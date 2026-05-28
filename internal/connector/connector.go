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
	watchWindowDays  int
	dryRun           bool
}

func New(entraClient entra.Client, mimecastClient mimecast.Client, pageSize, disableThreshold, watchWindowDays int, dryRun bool) *Connector {
	return &Connector{
		entra:            entraClient,
		mimecast:         mimecastClient,
		pageSize:         pageSize,
		disableThreshold: disableThreshold,
		watchWindowDays:  watchWindowDays,
		dryRun:           dryRun,
	}
}

// Run performs one compliance cycle:
//  1. Fetch all Mimecast users with their training stats (paginated).
//  2. Look up each Mimecast email directly in Entra via the Graph batch API —
//     no full directory scan, no email-collision risk.
//  3. In dry-run mode: log every account with its watch progress and whether it
//     would be disabled — no changes are made.
//  4. In live mode: disable accounts that are active in both platforms and below
//     the watch threshold. Per-user failures (e.g. privileged accounts) are
//     logged and skipped so the remaining accounts are still processed.
func (c *Connector) Run(ctx context.Context) error {
	slog.Info("training compliance check started", "dry_run", c.dryRun)

	allRecords, err := c.mimecast.GetAllUserRecords(ctx, c.pageSize, c.disableThreshold, c.watchWindowDays)
	if err != nil {
		return fmt.Errorf("fetching mimecast user records: %w", err)
	}
	slog.Info("fetched mimecast users", "count", len(allRecords))

	type target struct {
		id    string
		email string
	}

	if c.dryRun {
		emails := emailList(allRecords)
		entraUsers, err := c.entra.GetUsersByEmail(ctx, emails)
		if err != nil {
			return fmt.Errorf("looking up entra users: %w", err)
		}
		slog.Info("resolved entra accounts",
			"mimecast_total", len(allRecords),
			"entra_matched", len(entraUsers),
		)

		var targets []target
		for _, r := range allRecords {
			eu, inEntra := entraUsers[strings.ToLower(r.Email)]
			entraEnabled := inEntra && eu.AccountEnabled
			wouldDisable := r.Incomplete && entraEnabled

			// Log the Entra account's own mail and UPN alongside the Mimecast
			// email so mismatches (wrong account targeted) are immediately visible.
			var entraMail, entraUPN, entraID string
			if inEntra {
				entraMail = eu.Mail
				entraUPN = eu.UserPrincipalName
				entraID = eu.ID
			}
			emailMatch := !inEntra || strings.EqualFold(r.Email, eu.Mail) || strings.EqualFold(r.Email, eu.UserPrincipalName)
			slog.Info("dry run — account status",
				"mimecast_email", r.Email,
				"name", r.Name,
				"mimecast_user_id", r.UserID,
				"mimecast_state", r.UserState,
				"watched", fmt.Sprintf("%d/%d", r.Watched, r.Total),
				"not_watched", r.NotWatched,
				"in_entra", inEntra,
				"entra_id", entraID,
				"entra_mail", entraMail,
				"entra_upn", entraUPN,
				"email_match", emailMatch,
				"entra_enabled", entraEnabled,
				"would_disable", wouldDisable,
			)
			if wouldDisable {
				targets = append(targets, target{id: eu.ID, email: r.Email})
			}
		}
		slog.Info("dry run complete — no accounts were modified",
			"total_mimecast_users", len(allRecords),
			"entra_matched", len(entraUsers),
			"would_disable", len(targets),
		)
		return nil
	}

	// Live mode: only look up emails for incomplete users — no point resolving
	// accounts we wouldn't disable anyway.
	var incomplete []mimecast.UserRecord
	for _, r := range allRecords {
		if r.Incomplete {
			incomplete = append(incomplete, r)
		}
	}
	if len(incomplete) == 0 {
		slog.Info("all users are compliant — no action taken")
		return nil
	}

	entraUsers, err := c.entra.GetUsersByEmail(ctx, emailList(incomplete))
	if err != nil {
		return fmt.Errorf("looking up entra users: %w", err)
	}

	var targets []target
	for _, r := range incomplete {
		eu, ok := entraUsers[strings.ToLower(r.Email)]
		if !ok {
			continue
		}
		if !eu.AccountEnabled {
			slog.Info("account already disabled in entra — skipping", "email", r.Email, "entra_id", eu.ID)
			continue
		}
		emailMatch := strings.EqualFold(r.Email, eu.Mail) || strings.EqualFold(r.Email, eu.UserPrincipalName)
		if !emailMatch {
			slog.Warn("email mismatch — mimecast email does not match entra mail or UPN; skipping to avoid disabling wrong account",
				"mimecast_email", r.Email,
				"entra_id", eu.ID,
				"entra_mail", eu.Mail,
				"entra_upn", eu.UserPrincipalName,
			)
			continue
		}
		slog.Info("matched account to disable",
			"mimecast_email", r.Email,
			"mimecast_user_id", r.UserID,
			"entra_id", eu.ID,
			"entra_mail", eu.Mail,
			"entra_upn", eu.UserPrincipalName,
		)
		targets = append(targets, target{id: eu.ID, email: r.Email})
	}
	slog.Info("accounts to disable", "count", len(targets))

	if len(targets) == 0 {
		slog.Info("no accounts to disable after entra lookup")
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

// emailList extracts the email address from each record into a flat slice.
func emailList(records []mimecast.UserRecord) []string {
	out := make([]string, len(records))
	for i, r := range records {
		out[i] = r.Email
	}
	return out
}
