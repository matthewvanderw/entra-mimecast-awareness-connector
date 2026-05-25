package entra

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	ListActiveUsers(ctx context.Context, pageSize int) ([]User, error)
	ListAllUsers(ctx context.Context, pageSize int) ([]User, error)
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

// ListActiveUsers pages through all accountEnabled=true users in the directory.
func (c *client) ListActiveUsers(ctx context.Context, pageSize int) ([]User, error) {
	return c.listUsers(ctx, pageSize, "accountEnabled eq true")
}

// ListAllUsers pages through every user in the directory regardless of account
// state. Used by dry-run mode to surface disabled accounts in the output.
func (c *client) ListAllUsers(ctx context.Context, pageSize int) ([]User, error) {
	return c.listUsers(ctx, pageSize, "")
}

// listUsers is the shared paginator. When filter is non-empty it is applied as
// a $filter query parameter and ConsistencyLevel/eventual is set accordingly.
func (c *client) listUsers(ctx context.Context, pageSize int, filter string) ([]User, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return nil, err
	}

	var users []User
	q := url.Values{}
	q.Set("$top", fmt.Sprintf("%d", pageSize))
	q.Set("$select", "id,displayName,userPrincipalName,mail,accountEnabled")
	if filter != "" {
		q.Set("$count", "true")
		q.Set("$filter", filter)
	}
	nextURL := fmt.Sprintf("%s/users?%s", graphBase, q.Encode())

	for nextURL != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, nextURL, nil)
		if err != nil {
			return nil, fmt.Errorf("building users request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if filter != "" {
			// ConsistencyLevel + $count required when using $filter with advanced queries.
			req.Header.Set("ConsistencyLevel", "eventual")
		}

		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("listing users: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("list users %d: %s", resp.StatusCode, body)
		}

		var page usersPage
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("decoding users page: %w", err)
		}
		resp.Body.Close()

		users = append(users, page.Value...)
		nextURL = page.NextLink
	}

	return users, nil
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
