package mimecast

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
	performanceDetailsPath = "/api/awareness-training/company/get-performance-details"
	tokenPath              = "/oauth/token"

	// tokenRefreshSkew refreshes the bearer slightly before its real expiry
	// to avoid races where a request is sent with a token about to expire.
	tokenRefreshSkew = 30 * time.Second
)

// Client defines the Mimecast Awareness Training operations required by the connector.
type Client interface {
	// GetAllUserRecords returns training stats for every active Mimecast user.
	// Incomplete is set when a user has at least threshold unwatched videos that
	// were sent more than watchWindowDays ago (matching the Mimecast dashboard
	// definition of overdue — videos still within the watch window are excluded).
	GetAllUserRecords(ctx context.Context, pageSize, threshold, watchWindowDays int) ([]UserRecord, error)
}

type client struct {
	baseURL      string
	clientID     string
	clientSecret string
	http         *http.Client

	tokenMu  sync.Mutex
	token    string
	tokenExp time.Time
}

// NewClient creates a Mimecast Awareness Training client using OAuth 2.0
// client-credentials authentication (Mimecast 2.0 API).
func NewClient(baseURL, clientID, clientSecret string) Client {
	return &client{
		baseURL:      baseURL,
		clientID:     clientID,
		clientSecret: clientSecret,
		http:         &http.Client{Timeout: 30 * time.Second},
	}
}

// timeSentLayout is the timestamp format Mimecast uses in userDetails.timeSent.
const timeSentLayout = "2006-01-02T15:04:05-0700"

// GetAllUserRecords pages through the company performance report and returns
// training stats for every active user. Incomplete is set when the number of
// overdue unwatched videos is at or above threshold. A video is considered
// overdue when it was sent more than watchWindowDays ago, matching the
// Mimecast dashboard definition (videos still within the watch window are not
// yet counted as a problem).
func (c *client) GetAllUserRecords(ctx context.Context, pageSize, threshold, watchWindowDays int) ([]UserRecord, error) {
	cutoff := time.Now().UTC().Add(-time.Duration(watchWindowDays) * 24 * time.Hour)

	var out []UserRecord
	var pageToken string

	for {
		records, nextToken, err := c.fetchPage(ctx, pageSize, pageToken)
		if err != nil {
			return nil, err
		}

		for _, r := range records {
			if r.Email == "" {
				continue
			}
			notWatched := 0
			for _, d := range r.UserDetails {
				if !strings.EqualFold(d.Status, "Not Watched") {
					continue
				}
				sent, err := time.Parse(timeSentLayout, d.TimeSent)
				if err != nil || sent.Before(cutoff) {
					notWatched++
				}
			}
			watched := r.NumCorrect + r.NumIncorrect
			total := watched + notWatched
			out = append(out, UserRecord{
				UserID:     r.UserID,
				Email:      r.Email,
				Name:       r.Name,
				UserState:  r.UserState,
				Watched:    watched,
				NotWatched: notWatched,
				Total:      total,
				Incomplete: strings.EqualFold(r.UserState, "active") && notWatched >= threshold,
			})
		}

		if nextToken == "" {
			break
		}
		pageToken = nextToken
	}

	return out, nil
}

// fetchPage calls get-performance-details for one page, returning the records
// and the next-page token (empty string when this is the last page).
func (c *client) fetchPage(ctx context.Context, pageSize int, pageToken string) ([]performanceRecord, string, error) {
	meta := requestMeta{Pagination: requestPagination{PageSize: pageSize}}
	if pageToken != "" {
		meta.Pagination.PageToken = pageToken
	}
	body := requestEnvelope{
		Meta: &meta,
		Data: []map[string]any{
			{"includeUserDetails": true},
		},
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, "", fmt.Errorf("marshalling request: %w", err)
	}

	token, err := c.getToken(ctx)
	if err != nil {
		return nil, "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+performanceDetailsPath, bytes.NewReader(payload))
	if err != nil {
		return nil, "", fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("mimecast request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, "", fmt.Errorf("mimecast API %d: %s", resp.StatusCode, raw)
	}

	var envelope apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, "", fmt.Errorf("decoding response: %w", err)
	}

	if envelope.Meta.Status != http.StatusOK {
		return nil, "", fmt.Errorf("mimecast meta status %d", envelope.Meta.Status)
	}

	var nextToken string
	if p := envelope.Meta.Pagination; p != nil {
		nextToken = p.Next
	}

	return envelope.Data, nextToken, nil
}

// getToken returns a valid bearer token, fetching a new one if the cached
// token is missing or close to expiry.
func (c *client) getToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()

	if c.token != "" && time.Now().Before(c.tokenExp) {
		return c.token, nil
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", c.clientID)
	form.Set("client_secret", c.clientSecret)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+tokenPath, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("building token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("mimecast token request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("mimecast token API %d: %s", resp.StatusCode, raw)
	}

	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("decoding token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("mimecast token response missing access_token")
	}

	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= tokenRefreshSkew {
		ttl = tokenRefreshSkew * 2
	}
	c.token = tr.AccessToken
	c.tokenExp = time.Now().Add(ttl - tokenRefreshSkew)

	return c.token, nil
}

