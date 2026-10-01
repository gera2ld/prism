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
	ErrUnknownTool  = errors.New("unknown tool")
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

// ToolSource identifies where a tool definition came from.
type ToolSource string

const (
	SourceConduit ToolSource = "conduit"
	SourceMCP     ToolSource = "mcp"
)

// Tool is one callable tool as the agent sees it. Name is the callable
// identity: a conduit record name, or mcp__<server>__<tool> for MCP tools.
// Names are restricted to [a-zA-Z0-9_-] so they address a path segment and
// satisfy OpenAI's function-name rule.
type Tool struct {
	Name        string
	Description string
	// InputSchema is a JSON Schema object describing the arguments. It is
	// empty only for a source that publishes no schema at all.
	InputSchema json.RawMessage
	Source      ToolSource
	// Server is the MCP server name, empty for conduit tools.
	Server string
}

// ToolResult is the outcome of one invocation. IsError marks a tool that ran
// and failed, which is deliberately not a transport error: the agent feeds
// both cases back as a tool message, so a failing tool must still answer 200.
// Source and Server identify which source answered, so the caller can log the
// provenance without asking the registry a second time.
type ToolResult struct {
	Result  any
	IsError bool
	Source  ToolSource
	Server  string
}

// ToolRegistry resolves and executes tools. Key is the presented client key:
// Invoke logs it, and List takes it so narrowing the catalog per key later
// cannot change this interface. Policy is global today, so List ignores it.
type ToolRegistry interface {
	List(context.Context, Key) ([]Tool, error)
	Invoke(context.Context, Key, string, json.RawMessage) (ToolResult, error)
}

type ToolOutcome string

const (
	ToolOutcomeCompleted          ToolOutcome = "completed"
	ToolOutcomeToolError          ToolOutcome = "tool_error"
	ToolOutcomeClientDisconnected ToolOutcome = "client_disconnected"
	ToolOutcomeGatewayError       ToolOutcome = "gateway_error"
	ToolOutcomeRejected           ToolOutcome = "rejected"
)

// ToolRecord is one invocation written after it completes or fails. Args and
// Result carry the captured payload and are empty unless body capture is on.
type ToolRecord struct {
	StartedAt  time.Time
	KeyID      string
	KeyName    string
	Tool       string
	Source     ToolSource
	Server     string
	Args       string
	Result     string
	Truncated  bool
	DurationMS int64
	Status     int
	Outcome    ToolOutcome
	Error      string
}

type ToolLogSink interface {
	WriteTool(context.Context, ToolRecord) error
}
