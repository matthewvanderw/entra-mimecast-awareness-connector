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
	DisableUsers(ctx context.Context, userIDs []string) error
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
// pageSize controls the $top parameter (max 999 per Graph API constraints).
func (c *client) ListActiveUsers(ctx context.Context, pageSize int) ([]User, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return nil, err
	}

	var users []User
	q := url.Values{}
	q.Set("$top", fmt.Sprintf("%d", pageSize))
	q.Set("$count", "true")
	q.Set("$select", "id,displayName,userPrincipalName,mail,accountEnabled")
	q.Set("$filter", "accountEnabled eq true")
	nextURL := fmt.Sprintf("%s/users?%s", graphBase, q.Encode())

	for nextURL != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, nextURL, nil)
		if err != nil {
			return nil, fmt.Errorf("building users request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		// ConsistencyLevel + $count required when using $filter with advanced queries
		req.Header.Set("ConsistencyLevel", "eventual")

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
func (c *client) DisableUsers(ctx context.Context, userIDs []string) error {
	if len(userIDs) == 0 {
		return nil
	}

	token, err := c.getToken(ctx)
	if err != nil {
		return err
	}

	for i := 0; i < len(userIDs); i += batchMax {
		end := i + batchMax
		if end > len(userIDs) {
			end = len(userIDs)
		}
		if err := c.sendDisableBatch(ctx, token, userIDs[i:end]); err != nil {
			return fmt.Errorf("batch starting at index %d: %w", i, err)
		}
	}
	return nil
}

func (c *client) sendDisableBatch(ctx context.Context, token string, userIDs []string) error {
	items := make([]batchRequest, len(userIDs))
	for i, uid := range userIDs {
		items[i] = batchRequest{
			ID:     fmt.Sprintf("%d", i+1),
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

	var failures []string
	for _, r := range br.Responses {
		// Graph returns 204 No Content for a successful PATCH
		if r.Status != http.StatusNoContent {
			failures = append(failures, fmt.Sprintf("id=%s status=%d", r.ID, r.Status))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("batch had %d failure(s): %s", len(failures), strings.Join(failures, "; "))
	}
	return nil
}
