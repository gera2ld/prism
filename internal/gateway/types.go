package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"
)

var (
	ErrUnauthorized = errors.New("invalid API key")
	ErrUnknownModel = errors.New("unknown model")
	ErrForbidden    = errors.New("forbidden")
)

type Outcome string

const (
	OutcomeCompleted            Outcome = "completed"
	OutcomeClientDisconnected   Outcome = "client_disconnected"
	OutcomeUpstreamDisconnected Outcome = "upstream_disconnected"
	OutcomeUpstreamError        Outcome = "upstream_error"
	OutcomeGatewayError         Outcome = "gateway_error"
	OutcomeRejected             Outcome = "rejected"
)

type Key struct {
	ID   string
	Name string
	// Optional RE2 whitelist policy. Nil means unrestricted.
	AliasPattern    *regexp.Regexp
	ProviderPattern *regexp.Regexp
	ModelPattern    *regexp.Regexp
}

type Target struct {
	ProviderID   string
	ProviderName string
	BaseURL      string
	APIKey       string
	Model        string
}

type Config interface {
	Authenticate(context.Context, string) (Key, error)
	Resolve(context.Context, string) ([]Target, error)
	Models(context.Context) ([]string, error)
	// Transform applies the winning transformer for target, if any.
	// Returns the (possibly unchanged) body and the applied transformer's
	// name ("" when nothing matched).
	Transform(context.Context, Target, []byte) ([]byte, string, error)
}

type Usage struct {
	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
	TotalTokens      *int64 `json:"total_tokens"`
	// CachedTokens is usage.prompt_tokens_details.cached_tokens, a subset of
	// PromptTokens. Logged for cost visibility, never double-counted.
	CachedTokens *int64 `json:"-"`
}

// UnmarshalJSON reads the standard token counts plus the nested OpenAI
// prompt_tokens_details.cached_tokens field.
func (u *Usage) UnmarshalJSON(data []byte) error {
	var raw struct {
		PromptTokens     *int64 `json:"prompt_tokens"`
		CompletionTokens *int64 `json:"completion_tokens"`
		TotalTokens      *int64 `json:"total_tokens"`
		Details          *struct {
			CachedTokens *int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	u.PromptTokens = raw.PromptTokens
	u.CompletionTokens = raw.CompletionTokens
	u.TotalTokens = raw.TotalTokens
	if raw.Details != nil {
		u.CachedTokens = raw.Details.CachedTokens
	}
	return nil
}

type Bodies struct {
	Request   string
	Response  string
	Truncated bool
}

type Record struct {
	StartedAt     time.Time
	KeyID         string
	KeyName       string
	Alias         string
	ProviderID    string
	ProviderName  string
	UpstreamModel string
	Transformer   string
	Stream        bool
	Usage         Usage
	Outcome       Outcome
	FinishReason  *string
	TTFTMS        *int64
	TotalMS       int64
	Status        int
	Error         string
	Bodies        *Bodies
}

type LogSink interface {
	Write(context.Context, Record) error
}
