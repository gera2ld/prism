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
	// Endpoint is the route kind this target was resolved for. It lets
	// endpoint-scoped transformers stay out of other surfaces.
	Endpoint EndpointType
	// AuthStyle overrides how the upstream credential is sent. Empty means
	// auto: x-api-key for Anthropic routes, Bearer for everything else.
	AuthStyle string
}

// Upstream credential styles. Anthropic speaks x-api-key; OpenAI-compatible
// providers, including most Anthropic-compatible gateways, speak Bearer.
const (
	AuthStyleBearer  = "bearer"
	AuthStyleXAPIKey = "x-api-key"
)

// EndpointType names one gateway surface an alias can serve. Routes carry
// one, so a chat alias and an image alias never resolve to each other's
// targets even when they share a name. These are storage values; what
// clients see in GET /v1/models is ModelsValue.
type EndpointType string

const (
	EndpointChat      EndpointType = "chat"
	EndpointImage     EndpointType = "image"
	EndpointAnthropic EndpointType = "anthropic"
)

// Valid reports whether e is a route kind the gateway can resolve.
func (e EndpointType) Valid() bool {
	switch e {
	case EndpointChat, EndpointImage, EndpointAnthropic:
		return true
	}
	return false
}

// ModelsValue reports the token advertised in GET /v1/models
// supported_endpoint_types for this endpoint kind. The vocabulary follows
// new-api (openai, image-generation, anthropic, ...) so clients that already
// speak it can route models without learning Prism-specific names.
func (e EndpointType) ModelsValue() string {
	switch e {
	case EndpointImage:
		return "image-generation"
	case EndpointAnthropic:
		return "anthropic"
	default:
		return "openai"
	}
}

type Config interface {
	Authenticate(context.Context, string) (Key, error)
	Resolve(ctx context.Context, endpoint EndpointType, alias string) ([]Target, error)
	// Models lists every alias with at least one enabled route, across all
	// endpoint types. Callers resolve per endpoint to advertise or authorize.
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
	// Cost is usage.cost as reported by image providers (e.g. OpenRouter).
	// Chat providers do not report it; absent stays nil.
	Cost *float64 `json:"cost"`
}

// UnmarshalJSON reads OpenAI's token counts, the nested
// prompt_tokens_details.cached_tokens field, and Anthropic's input/output
// counts with their separate cache counters. Anthropic does not report a
// total, so Finalize computes it from the parts.
func (u *Usage) UnmarshalJSON(data []byte) error {
	var raw struct {
		PromptTokens     *int64   `json:"prompt_tokens"`
		CompletionTokens *int64   `json:"completion_tokens"`
		TotalTokens      *int64   `json:"total_tokens"`
		Cost             *float64 `json:"cost"`
		Details          *struct {
			CachedTokens *int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		InputTokens              *int64 `json:"input_tokens"`
		OutputTokens             *int64 `json:"output_tokens"`
		CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	u.PromptTokens = raw.PromptTokens
	u.CompletionTokens = raw.CompletionTokens
	u.TotalTokens = raw.TotalTokens
	u.Cost = raw.Cost
	u.CachedTokens = nil
	if raw.Details != nil {
		u.CachedTokens = raw.Details.CachedTokens
	}
	// Anthropic bills input_tokens, cache creation and cache reads
	// separately. Summing them matches OpenAI's prompt_tokens, which counts
	// cached tokens as a subset of the prompt.
	if u.PromptTokens == nil && raw.InputTokens != nil {
		prompt := *raw.InputTokens
		if raw.CacheCreationInputTokens != nil {
			prompt += *raw.CacheCreationInputTokens
		}
		if raw.CacheReadInputTokens != nil {
			prompt += *raw.CacheReadInputTokens
		}
		u.PromptTokens = &prompt
	}
	if u.CompletionTokens == nil && raw.OutputTokens != nil {
		u.CompletionTokens = raw.OutputTokens
	}
	if u.CachedTokens == nil && raw.CacheReadInputTokens != nil {
		u.CachedTokens = raw.CacheReadInputTokens
	}
	return nil
}

// Merge folds a partial usage report into u. Streaming providers report
// usage in pieces: Anthropic puts input counts on message_start and output
// counts on message_delta, so assigning the last report would lose the
// input side. Nil fields never overwrite known values.
func (u *Usage) Merge(other Usage) {
	if other.PromptTokens != nil {
		u.PromptTokens = other.PromptTokens
	}
	if other.CompletionTokens != nil {
		u.CompletionTokens = other.CompletionTokens
	}
	if other.TotalTokens != nil {
		u.TotalTokens = other.TotalTokens
	}
	if other.CachedTokens != nil {
		u.CachedTokens = other.CachedTokens
	}
	if other.Cost != nil {
		u.Cost = other.Cost
	}
}

// Finalize fills the total when the provider reports only the parts. It is
// idempotent, so logging every record through it is safe.
func (u *Usage) Finalize() {
	if u.TotalTokens == nil && u.PromptTokens != nil && u.CompletionTokens != nil {
		total := *u.PromptTokens + *u.CompletionTokens
		u.TotalTokens = &total
	}
}

type Bodies struct {
	Request   string
	Response  string
	Truncated bool
}

// GeneratedImage is one decoded image output awaiting persistence. Data
// holds the raw bytes, Name is a sanitized filename with an extension
// derived from the media type, and MediaType is the provider-reported or
// sniffed MIME type. Kind is "input" for uploaded edit images and "output"
// for generated ones.
type GeneratedImage struct {
	Data      []byte
	Name      string
	MediaType string
	Kind      string
}

// Image file kinds stored in request_images.
const (
	ImageKindInput  = "input"
	ImageKindOutput = "output"
)

type Record struct {
	StartedAt     time.Time
	KeyID         string
	KeyName       string
	Alias         string
	Endpoint      EndpointType
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
	// Images carries decoded image outputs for the store to persist as
	// files. Populated only when body capture is on; url-based outputs are
	// never downloaded, so only inline base64 items appear here.
	Images []GeneratedImage
}

type LogSink interface {
	Write(context.Context, Record) error
}

// Version identifies Prism to the systems it calls: the User-Agent a conduit
// tool sends, and the client identity an MCP peer sees. It lives here because
// both the store and the MCP server need it and gateway is the one package
// neither has to avoid.
const Version = "1.0.0"

// Tool is one callable tool as the agent sees it. Name is the callable identity:
// a conduit record name. Names are restricted to [a-zA-Z0-9_-] so they address a
// path segment and satisfy OpenAI's function-name rule.
type Tool struct {
	Name        string
	Description string
	// InputSchema is a JSON Schema object describing the arguments. It is
	// empty only for a source that publishes no schema at all.
	InputSchema json.RawMessage
}

// ToolResult is the outcome of one invocation. IsError marks a tool that ran
// and failed, which is deliberately not a transport error: the agent feeds
// both cases back as a tool message, so a failing tool must still answer 200.
type ToolResult struct {
	Result  any
	IsError bool
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
// Transport records which surface the call arrived on.
type ToolRecord struct {
	StartedAt  time.Time
	KeyID      string
	KeyName    string
	Tool       string
	Transport  string
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
