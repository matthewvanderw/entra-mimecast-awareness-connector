package mimecast

// performanceRecord is one element of the data array returned by
// /api/awareness-training/company/get-performance-details.
type performanceRecord struct {
	UserID        string       `json:"userId"`
	UserState     string       `json:"userState"`
	Department    string       `json:"department"`
	Email         string       `json:"email"`
	Name          string       `json:"name"`
	NumCorrect    int          `json:"numCorrect"`
	NumIncorrect  int          `json:"numIncorrect"`
	NumNotWatched int          `json:"numNotWatched"`
	UserDetails   []userDetail `json:"userDetails"`
}

type userDetail struct {
	Ack        bool   `json:"ack"`
	Category   string `json:"category"`
	Status     string `json:"status"`
	TimeSent   string `json:"timeSent"`
	TimeViewed string `json:"timeViewed"`
	Title      string `json:"title"`
}

// apiResponse is the top-level envelope returned by Mimecast API endpoints.
type apiResponse struct {
	Meta struct {
		Status     int         `json:"status"`
		Pagination *pagination `json:"pagination,omitempty"`
	} `json:"meta"`
	Data []performanceRecord `json:"data"`
	Fail []any               `json:"fail"`
}

type pagination struct {
	PageSize  int    `json:"pageSize"`
	Next      string `json:"next"`
	PageToken string `json:"pageToken"`
}

// requestEnvelope is the body sent to the Mimecast API.
type requestEnvelope struct {
	Meta *requestMeta       `json:"meta,omitempty"`
	Data []map[string]any   `json:"data"`
}

type requestMeta struct {
	Pagination requestPagination `json:"pagination"`
}

type requestPagination struct {
	PageSize  int    `json:"pageSize"`
	PageToken string `json:"pageToken"`
}

// tokenResponse is the OAuth 2.0 client-credentials response from /oauth/token.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}
