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
//  2. Fetch all Mimecast users who have not completed training (paginated).
//  3. Intersect by email address.
//  4. Disable the matching Entra accounts via batched Graph API calls.
func (c *Connector) Run(ctx context.Context) error {
	slog.Info("training compliance check started", "dry_run", c.dryRun)

	entraUsers, err := c.entra.ListActiveUsers(ctx, c.pageSize)
	if err != nil {
		return fmt.Errorf("listing entra users: %w", err)
	}

	// Build a lowercase-email → entra-user-ID lookup.
	emailToID := make(map[string]string, len(entraUsers))
	for _, u := range entraUsers {
		email := primaryEmail(u)
		if email != "" {
			emailToID[email] = u.ID
		}
	}
	slog.Info("fetched entra users", "total", len(entraUsers), "with_email", len(emailToID))

	incompleteEmails, err := c.mimecast.GetIncompleteUsers(ctx, c.pageSize, c.disableThreshold)
	if err != nil {
		return fmt.Errorf("fetching incomplete mimecast users: %w", err)
	}
	slog.Info("fetched incomplete training users", "count", len(incompleteEmails))

	// Intersect: only disable users who exist in Entra and are incomplete.
	type target struct {
		id    string
		email string
	}
	targets := make([]target, 0, len(incompleteEmails))
	for _, email := range incompleteEmails {
		if id, ok := emailToID[strings.ToLower(email)]; ok {
			targets = append(targets, target{id: id, email: email})
		}
	}
	slog.Info("accounts to disable", "count", len(targets))

	if len(targets) == 0 {
		slog.Info("all users are compliant — no action taken")
		return nil
	}

	if c.dryRun {
		for _, t := range targets {
			slog.Info("dry run — would disable account", "email", t.email, "entra_id", t.id)
		}
		slog.Info("dry run complete — no accounts were modified", "would_disable", len(targets))
		return nil
	}

	ids := make([]string, len(targets))
	for i, t := range targets {
		ids[i] = t.id
	}
	if err := c.entra.DisableUsers(ctx, ids); err != nil {
		return fmt.Errorf("disabling accounts: %w", err)
	}

	slog.Info("training compliance check complete", "accounts_disabled", len(targets))
	return nil
}

// primaryEmail returns the lowercase email to use for matching.
// Prefers the mail field; falls back to userPrincipalName.
func primaryEmail(u entra.User) string {
	if u.Mail != "" {
		return strings.ToLower(u.Mail)
	}
	return strings.ToLower(u.UserPrincipalName)
}
