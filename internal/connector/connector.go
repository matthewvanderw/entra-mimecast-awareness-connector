package connector

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/matthewvanderw/entra-mimecast-awareness-connector/internal/entra"
	"github.com/matthewvanderw/entra-mimecast-awareness-connector/internal/mimecast"
)

// Connector orchestrates the training-compliance check between
// Mimecast Awareness Training and Microsoft Entra ID.
type Connector struct {
	entra            entra.Client
	mimecast         mimecast.Client
	pageSize         int
	disableThreshold int
	dryRun           bool
}

func New(entraClient entra.Client, mimecastClient mimecast.Client, pageSize, disableThreshold int, dryRun bool) *Connector {
	return &Connector{
		entra:            entraClient,
		mimecast:         mimecastClient,
		pageSize:         pageSize,
		disableThreshold: disableThreshold,
		dryRun:           dryRun,
	}
}

// Run performs one compliance cycle:
//  1. Fetch all active Entra users (paginated).
//  2. Fetch all active Mimecast users with their training stats (paginated).
//  3. Intersect by email address to find accounts to disable.
//  4. In dry-run mode: log every account with its watch progress and whether it
//     would be disabled — no changes are made.
//  5. In live mode: disable matching accounts via batched Graph API calls.
//     Per-user failures (e.g. privileged accounts) are logged and skipped so
//     the remaining accounts are still processed.
func (c *Connector) Run(ctx context.Context) error {
	slog.Info("training compliance check started", "dry_run", c.dryRun)

	allRecords, err := c.mimecast.GetAllUserRecords(ctx, c.pageSize, c.disableThreshold)
	if err != nil {
		return fmt.Errorf("fetching mimecast user records: %w", err)
	}
	slog.Info("fetched mimecast users", "count", len(allRecords))

	type target struct {
		id    string
		email string
	}

	if c.dryRun {
		// Fetch every Entra account — including disabled ones — so the output
		// reflects the real state of both platforms.
		allEntraUsers, err := c.entra.ListAllUsers(ctx, c.pageSize)
		if err != nil {
			return fmt.Errorf("listing all entra users: %w", err)
		}
		entraByID, emailToUser, ambiguousEmails := buildEntraIndex(allEntraUsers)
		slog.Info("fetched entra users (all)", "total", len(allEntraUsers))

		var targets []target
		matchedEntraIDs := make(map[string]bool, len(allRecords))
		for _, r := range allRecords {
			eu, matchedBy := resolveEntraUser(r, entraByID, emailToUser, ambiguousEmails)
			inEntra := eu != nil
			entraEnabled := inEntra && eu.AccountEnabled
			wouldDisable := r.Incomplete && entraEnabled
			slog.Info("dry run — account status",
				"email", r.Email,
				"name", r.Name,
				"mimecast_user_id", r.UserID,
				"mimecast_state", r.UserState,
				"watched", fmt.Sprintf("%d/%d", r.Watched, r.Total),
				"not_watched", r.NotWatched,
				"matched_by", matchedBy,
				"in_entra", inEntra,
				"entra_enabled", entraEnabled,
				"would_disable", wouldDisable,
			)
			if wouldDisable {
				targets = append(targets, target{id: eu.ID, email: r.Email})
				matchedEntraIDs[eu.ID] = true
			} else if inEntra {
				matchedEntraIDs[eu.ID] = true
			}
		}

		// Log any Entra accounts that have no Mimecast record at all.
		for _, u := range allEntraUsers {
			if matchedEntraIDs[u.ID] {
				continue
			}
			slog.Info("dry run — entra only (not in mimecast)",
				"email", primaryEmail(u),
				"entra_id", u.ID,
				"name", u.DisplayName,
				"entra_enabled", u.AccountEnabled,
			)
		}

		slog.Info("dry run complete — no accounts were modified",
			"total_mimecast_users", len(allRecords),
			"total_entra_users", len(allEntraUsers),
			"would_disable", len(targets),
		)
		return nil
	}

	// Live mode: only active Entra accounts are candidates for disabling.
	entraUsers, err := c.entra.ListActiveUsers(ctx, c.pageSize)
	if err != nil {
		return fmt.Errorf("listing entra users: %w", err)
	}
	entraByID, emailToID, ambiguousEmails := buildEntraIndex(entraUsers)
	slog.Info("fetched entra users", "total", len(entraUsers), "with_email", len(emailToID))

	// Live mode: intersect incomplete Mimecast users with active Entra accounts.
	var targets []target
	for _, r := range allRecords {
		if !r.Incomplete {
			continue
		}
		eu, matchedBy := resolveEntraUser(r, entraByID, emailToID, ambiguousEmails)
		if eu == nil {
			continue
		}
		slog.Info("matched account to disable",
			"email", r.Email,
			"mimecast_user_id", r.UserID,
			"entra_id", eu.ID,
			"matched_by", matchedBy,
		)
		targets = append(targets, target{id: eu.ID, email: r.Email})
	}
	slog.Info("accounts to disable", "count", len(targets))

	if len(targets) == 0 {
		slog.Info("all users are compliant — no action taken")
		return nil
	}

	ids := make([]string, len(targets))
	idToEmail := make(map[string]string, len(targets))
	for i, t := range targets {
		ids[i] = t.id
		idToEmail[t.id] = t.email
	}

	results, err := c.entra.DisableUsers(ctx, ids)
	if err != nil {
		return fmt.Errorf("disabling accounts: %w", err)
	}

	var disabled, skipped int
	for _, r := range results {
		if r.Skipped {
			skipped++
			slog.Warn("could not disable account — skipping",
				"email", idToEmail[r.UserID],
				"entra_id", r.UserID,
				"reason", r.Err,
			)
		} else {
			disabled++
		}
	}
	slog.Info("training compliance check complete",
		"accounts_disabled", disabled,
		"accounts_skipped", skipped,
	)
	return nil
}

// primaryEmail returns the lowercase email to use for matching.
// Prefers the mail field; falls back to userPrincipalName when mail is unset.
// Returns an empty string for guest-style UPNs (contains "#EXT#") because those
// never match a real Mimecast email address.
func primaryEmail(u entra.User) string {
	if u.Mail != "" {
		return strings.ToLower(u.Mail)
	}
	upn := strings.ToLower(u.UserPrincipalName)
	if strings.Contains(upn, "#ext#") {
		return ""
	}
	return upn
}

// buildEntraIndex constructs three lookup structures from a slice of Entra users:
//   - byID: Entra object ID → User (always unambiguous)
//   - byEmail: resolved email → User (only populated when the email is unique)
//   - ambiguous: emails that map to more than one user (logged as warnings, skipped for matching)
func buildEntraIndex(users []entra.User) (byID map[string]*entra.User, byEmail map[string]*entra.User, ambiguous map[string]bool) {
	byID = make(map[string]*entra.User, len(users))
	byEmail = make(map[string]*entra.User, len(users))
	ambiguous = make(map[string]bool)

	for i := range users {
		u := &users[i]
		byID[u.ID] = u

		email := primaryEmail(*u)
		if email == "" {
			continue
		}
		if ambiguous[email] {
			continue
		}
		if _, exists := byEmail[email]; exists {
			slog.Warn("duplicate email across Entra accounts — both will be skipped for email matching",
				"email", email,
				"entra_id_1", byEmail[email].ID,
				"entra_id_2", u.ID,
			)
			delete(byEmail, email)
			ambiguous[email] = true
			continue
		}
		byEmail[email] = u
	}
	return
}

// resolveEntraUser finds the Entra user for a Mimecast record.
// It tries the Mimecast userId as an Entra object ID first (works when the tenant
// was provisioned via Azure AD sync), then falls back to email matching.
// Returns nil and an empty matchedBy string when no match is found.
func resolveEntraUser(r mimecast.UserRecord, byID map[string]*entra.User, byEmail map[string]*entra.User, ambiguous map[string]bool) (u *entra.User, matchedBy string) {
	if r.UserID != "" {
		if u = byID[r.UserID]; u != nil {
			return u, "entra_id"
		}
	}
	email := strings.ToLower(r.Email)
	if ambiguous[email] {
		slog.Warn("skipping match — email is ambiguous in Entra", "email", r.Email, "mimecast_user_id", r.UserID)
		return nil, ""
	}
	if u = byEmail[email]; u != nil {
		return u, "email"
	}
	return nil, ""
}
