package entra

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	graphBase  = "https://graph.microsoft.com/v1.0"
	loginBase  = "https://login.microsoftonline.com"
	batchMax   = 20 // Graph API hard limit per $batch call
)

// Client defines the Entra ID operations required by the connector.
type Client interface {
	// GetUsersByEmail resolves Entra accounts for a list of email addresses.
	// Each address is matched against both the mail and userPrincipalName fields.
	// The returned map is keyed by the lowercase input email. Addresses that match
	// zero accounts are omitted; addresses that match more than one account are
	// logged as ambiguous and omitted.
	GetUsersByEmail(ctx context.Context, emails []string) (map[string]User, error)
	DisableUsers(ctx context.Context, userIDs []string) ([]DisableResult, error)
}

type client struct {
	tenantID     string
	clientID     string
	clientSecret string
	http         *http.Client

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
}

func NewClient(tenantID, clientID, clientSecret string) Client {
	return &client{
		tenantID:     tenantID,
		clientID:     clientID,
		clientSecret: clientSecret,
		http:         &http.Client{Timeout: 30 * time.Second},
	}
}

// getToken fetches a cached OAuth2 client-credentials token, refreshing when
// within 60 seconds of expiry to avoid mid-run token failures.
func (c *client) getToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if time.Now().Before(c.tokenExpiry) {
		return c.token, nil
	}

	tokenURL := fmt.Sprintf("%s/%s/oauth2/v2.0/token", loginBase, c.tenantID)
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
		"scope":         {"https://graph.microsoft.com/.default"},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("building token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("token endpoint %d: %s", resp.StatusCode, body)
	}

	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("decoding token: %w", err)
	}

	c.token = tr.AccessToken
	c.tokenExpiry = time.Now().Add(time.Duration(tr.ExpiresIn-60) * time.Second)
	return c.token, nil
}

// GetUsersByEmail resolves Entra accounts for a list of email addresses using
// the Graph $batch API (up to 20 lookups per call). Each address is matched
// against both the mail and userPrincipalName fields. The returned map is keyed
// by the lowercase input email. Zero-match addresses are omitted silently;
// addresses matching more than one account are logged as ambiguous and omitted.
func (c *client) GetUsersByEmail(ctx context.Context, emails []string) (map[string]User, error) {
	if len(emails) == 0 {
		return map[string]User{}, nil
	}

	// Deduplicate while preserving order.
	seen := make(map[string]bool, len(emails))
	unique := make([]string, 0, len(emails))
	for _, e := range emails {
		lc := strings.ToLower(e)
		if !seen[lc] {
			seen[lc] = true
			unique = append(unique, lc)
		}
	}

	token, err := c.getToken(ctx)
	if err != nil {
		return nil, err
	}

	out := make(map[string]User, len(unique))
	for i := 0; i < len(unique); i += batchMax {
		end := i + batchMax
		if end > len(unique) {
			end = len(unique)
		}
		if err := c.resolveEmailBatch(ctx, token, unique[i:end], out); err != nil {
			return nil, fmt.Errorf("email lookup batch at index %d: %w", i, err)
		}
	}
	return out, nil
}

func (c *client) resolveEmailBatch(ctx context.Context, token string, emails []string, out map[string]User) error {
	items := make([]batchRequest, len(emails))
	idToEmail := make(map[string]string, len(emails))

	for i, email := range emails {
		id := fmt.Sprintf("%d", i+1)
		idToEmail[id] = email
		// Escape single quotes for OData string literals.
		escaped := strings.ReplaceAll(email, "'", "''")
		q := url.Values{}
		q.Set("$filter", fmt.Sprintf("mail eq '%s' or userPrincipalName eq '%s'", escaped, escaped))
		q.Set("$select", "id,displayName,userPrincipalName,mail,accountEnabled")
		q.Set("$count", "true")
		q.Set("$top", "2") // only need to know: 0, 1, or 2+
		items[i] = batchRequest{
			ID:     id,
			Method: "GET",
			URL:    "/users?" + q.Encode(),
			Headers: map[string]string{
				"ConsistencyLevel": "eventual",
			},
		}
	}

	payload, err := json.Marshal(batchEnvelope{Requests: items})
	if err != nil {
		return fmt.Errorf("marshalling batch: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, graphBase+"/$batch", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("building batch request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("sending batch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("batch endpoint %d: %s", resp.StatusCode, body)
	}

	var br batchResponse
	if err := json.NewDecoder(resp.Body).Decode(&br); err != nil {
		return fmt.Errorf("decoding batch response: %w", err)
	}

	for _, r := range br.Responses {
		email := idToEmail[r.ID]
		if r.Status != http.StatusOK {
			slog.Warn("entra email lookup failed", "email", email, "graph_status", r.Status)
			continue
		}
		var page usersPage
		if err := json.Unmarshal(r.Body, &page); err != nil {
			slog.Warn("decoding entra email lookup response", "email", email, "error", err)
			continue
		}
		switch len(page.Value) {
		case 0:
			// not found in Entra — omit silently
		case 1:
			out[email] = page.Value[0]
		default:
			slog.Warn("ambiguous email — multiple Entra accounts match, skipping",
				"email", email,
				"count", len(page.Value),
			)
		}
	}
	return nil
}

// DisableUsers sets accountEnabled=false for each userID using the Graph $batch
// endpoint (max 20 items per call) to stay within API rate limits.
// Fatal errors (e.g. 401 Unauthorized on the batch endpoint) are returned as the
// error value. Per-user failures (e.g. 403 for privileged accounts) are recorded
// in the returned slice so callers can continue processing the remaining users.
func (c *client) DisableUsers(ctx context.Context, userIDs []string) ([]DisableResult, error) {
	if len(userIDs) == 0 {
		return nil, nil
	}

	token, err := c.getToken(ctx)
	if err != nil {
		return nil, err
	}

	var all []DisableResult
	for i := 0; i < len(userIDs); i += batchMax {
		end := i + batchMax
		if end > len(userIDs) {
			end = len(userIDs)
		}
		results, err := c.sendDisableBatch(ctx, token, userIDs[i:end])
		if err != nil {
			return nil, fmt.Errorf("batch starting at index %d: %w", i, err)
		}
		all = append(all, results...)
	}
	return all, nil
}

func (c *client) sendDisableBatch(ctx context.Context, token string, userIDs []string) ([]DisableResult, error) {
	idToUser := make(map[string]string, len(userIDs))
	items := make([]batchRequest, len(userIDs))
	for i, uid := range userIDs {
		itemID := fmt.Sprintf("%d", i+1)
		idToUser[itemID] = uid
		items[i] = batchRequest{
			ID:     itemID,
			Method: "PATCH",
			URL:    fmt.Sprintf("/users/%s", uid),
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
			Body: map[string]bool{"accountEnabled": false},
		}
	}

	payload, err := json.Marshal(batchEnvelope{Requests: items})
	if err != nil {
		return nil, fmt.Errorf("marshalling batch: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, graphBase+"/$batch", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("building batch request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending batch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		// 401 means our credentials are invalid — nothing further will succeed.
		// Any other HTTP-level failure from the batch endpoint is also fatal.
		return nil, fmt.Errorf("batch endpoint %d: %s", resp.StatusCode, body)
	}

	var br batchResponse
	if err := json.NewDecoder(resp.Body).Decode(&br); err != nil {
		return nil, fmt.Errorf("decoding batch response: %w", err)
	}

	results := make([]DisableResult, 0, len(userIDs))
	for _, r := range br.Responses {
		uid := idToUser[r.ID]
		if r.Status == http.StatusNoContent {
			results = append(results, DisableResult{UserID: uid})
			continue
		}
		// 403 Forbidden: account has elevated privileges the app cannot override.
		// 404 Not Found: account was removed between the list and disable steps.
		// Any other unexpected status: record and skip rather than aborting the run.
		results = append(results, DisableResult{
			UserID:  uid,
			Skipped: true,
			Err:     fmt.Errorf("Graph status %d: %s", r.Status, r.Body),
		})
	}
	return results, nil
}
