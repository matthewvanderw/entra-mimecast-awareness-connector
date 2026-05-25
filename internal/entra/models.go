package entra

import "encoding/json"

// DisableResult is the outcome of one account-disable attempt.
type DisableResult struct {
	UserID  string
	Skipped bool  // true when Graph denied the operation (e.g. privileged account)
	Err     error // non-nil for skipped or otherwise failed entries
}

// User represents a Microsoft Entra ID (Azure AD) user from the Graph API.
type User struct {
	ID                string `json:"id"`
	DisplayName       string `json:"displayName"`
	UserPrincipalName string `json:"userPrincipalName"`
	Mail              string `json:"mail"`
	AccountEnabled    bool   `json:"accountEnabled"`
}

type usersPage struct {
	Value    []User `json:"value"`
	NextLink string `json:"@odata.nextLink"`
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// Graph API $batch structures — max 20 requests per batch call.
type batchEnvelope struct {
	Requests []batchRequest `json:"requests"`
}

type batchRequest struct {
	ID      string            `json:"id"`
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    any               `json:"body,omitempty"`
}

type batchResponse struct {
	Responses []batchResponseItem `json:"responses"`
}

type batchResponseItem struct {
	ID     string          `json:"id"`
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body,omitempty"`
}
