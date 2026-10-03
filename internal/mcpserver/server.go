// Package mcpserver exposes Prism's tools over the Model Context Protocol, so
// an MCP-capable agent can call them directly instead of driving the REST
// tools endpoints. It knows nothing about PocketBase: callers hand it a tool
// registry and a log sink, which keeps it testable against in-process
// transports with no network.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gera2ld/prism/internal/gateway"
)

// TransportMCP names this surface in tool_logs, so an invocation can be told
// apart from one that arrived over REST.
const TransportMCP = "mcp"

// Path is where the handler is mounted.
const Path = "/mcp"

// callTimeout bounds one tool invocation. It matches the gateway's REST budget
// so the two surfaces fail a slow tool at the same point.
const callTimeout = 120 * time.Second

// Server publishes a tool catalog over MCP.
type Server struct {
	registry gateway.ToolRegistry
	logs     gateway.ToolLogSink
	logger   *slog.Logger
	now      func() time.Time
	capture  func() bool
}

func New(registry gateway.ToolRegistry, logs gateway.ToolLogSink, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		registry: registry,
		logs:     logs,
		logger:   logger,
		now:      time.Now,
		// Capture defaults off, matching the REST surface. main.go sets it from
		// gateway_settings so both agree.
		capture: func() bool { return false },
	}
}

// SetCapture installs the body-capture toggle.
func (s *Server) SetCapture(fn func() bool) { s.capture = fn }

// Handler returns the MCP endpoint for a presented key.
//
// The session is stateless: the SDK builds a server per request and closes it
// when the request ends, so the catalog is rebuilt every time and an admin UI
// edit applies without a restart. It also means a tool handler runs inside the
// caller's request, which is what lets an invocation be attributed to the key
// that made it.
func (s *Server) Handler(key gateway.Key) http.Handler {
	handler := mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return s.build(key) },
		&mcpsdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		handler.ServeHTTP(w, r)
	})
}

// build assembles a one-request server from the current catalog.
func (s *Server) build(key gateway.Key) *mcpsdk.Server {
	server := mcpsdk.NewServer(
		&mcpsdk.Implementation{Name: "prism", Version: gateway.Version},
		nil,
	)
	tools, err := s.registry.List(context.Background(), key)
	if err != nil {
		// A catalog that will not load leaves the server with no tools rather
		// than failing the whole endpoint; the agent sees an empty list and
		// Prism logs why.
		s.logger.Error("failed to list tools for mcp", "error", err)
		return server
	}
	for _, tool := range tools {
		schema, ok := publishableSchema(tool.InputSchema)
		if !ok {
			// The SDK panics on a schema it cannot use, which would take the
			// gateway down from an admin-UI edit. Skip and say so instead.
			s.logger.Error("skipping tool with unusable input schema",
				"tool", tool.Name, "error", "MCP requires input_schema.type to be \"object\"")
			continue
		}
		server.AddTool(&mcpsdk.Tool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: schema,
		}, s.handler(key, tool.Name))
	}
	return server
}

// publishableSchema prepares a tool's schema for the MCP SDK, which requires a
// non-nil object schema. A definition that declares no constraints still needs
// one published, since the SDK rejects nil outright, so it is described as an
// empty object — the closest honest statement, though not strictly equivalent to
// the engine's own treatment of an absent schema.
//
// Anything the SDK would panic on is refused instead: malformed JSON, a schema
// that is not an object, or a type other than "object". A schema carrying
// properties but no type is refused rather than coerced, because silently
// calling it an object could misdescribe what the tool accepts.
func publishableSchema(raw json.RawMessage) (any, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{"type": "object"}, true
	}
	if !json.Valid(raw) {
		return nil, false
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, false
	}
	if len(probe) == 0 {
		return map[string]any{"type": "object"}, true
	}
	kind, ok := probe["type"].(string)
	if !ok || kind != "object" {
		return nil, false
	}
	return probe, true
}

// handler runs one tool and records the outcome. A tool that failed is reported
// through CallToolResult.IsError with the reason as its content, which is how
// an MCP client expects a tool-level failure; only a gateway fault becomes a
// protocol error.
func (s *Server) handler(key gateway.Key, name string) mcpsdk.ToolHandler {
	return func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		start := s.now()
		rec := gateway.ToolRecord{
			StartedAt: start, KeyID: key.ID, KeyName: key.Name,
			Tool: name, Transport: TransportMCP,
		}

		callCtx, cancel := context.WithTimeout(ctx, callTimeout)
		defer cancel()

		result, err := s.registry.Invoke(callCtx, key, name, arguments(req))
		if err != nil {
			rec.DurationMS = s.now().Sub(start).Milliseconds()
			rec.Status = http.StatusInternalServerError
			rec.Outcome = gateway.ToolOutcomeGatewayError
			rec.Error = err.Error()
			s.write(rec)
			// The SDK turns an error into a protocol-level failure, which the
			// client sees as a broken call rather than a tool that said no.
			return nil, err
		}

		out := &mcpsdk.CallToolResult{}
		if structured, ok := result.Result.(map[string]any); ok {
			out.StructuredContent = structured
		}
		out.Content = []mcpsdk.Content{&mcpsdk.TextContent{Text: text(result.Result)}}
		out.IsError = result.IsError

		rec.DurationMS = s.now().Sub(start).Milliseconds()
		rec.Status = http.StatusOK
		rec.Outcome = gateway.ToolOutcomeCompleted
		if encoded, err := json.Marshal(result.Result); err == nil {
			rec.Result = string(encoded)
		}
		if result.IsError {
			rec.Outcome = gateway.ToolOutcomeToolError
			rec.Error = rec.Result
		}
		rec.Args, rec.Result, rec.Truncated = s.capturePayload(string(arguments(req)), rec.Result)
		s.write(rec)
		return out, nil
	}
}

// arguments returns the call's arguments as raw JSON, or an empty object when
// the client sent none.
func arguments(req *mcpsdk.CallToolRequest) json.RawMessage {
	if req == nil || len(req.Params.Arguments) == 0 {
		return json.RawMessage(`{}`)
	}
	if !json.Valid(req.Params.Arguments) {
		return json.RawMessage(`{}`)
	}
	return req.Params.Arguments
}

// text renders a result for the content block: JSON when it is a structure, and
// the bare string when the tool returned one.
func text(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprint(typed)
		}
		return string(encoded)
	}
}

// capturePayload applies the capture toggle and the same 256KB per-body cap the
// REST surface uses, so the two cannot disagree about what is stored.
func (s *Server) capturePayload(args, result string) (string, string, bool) {
	if !s.capture() {
		return "", "", false
	}
	trimmedArgs, argsTruncated := truncate(args)
	trimmedResult, resultTruncated := truncate(result)
	return trimmedArgs, trimmedResult, argsTruncated || resultTruncated
}

const maxCapture = 256 << 10

func truncate(s string) (string, bool) {
	if len(s) <= maxCapture {
		return s, false
	}
	return s[:maxCapture], true
}

func (s *Server) write(rec gateway.ToolRecord) {
	if s.logs == nil {
		return
	}
	// Written on a detached context: a client that hangs up mid-call must still
	// leave an audit trail.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.logs.WriteTool(ctx, rec); err != nil {
		s.logger.Error("failed to write tool log", "tool", rec.Tool, "error", err)
	}
}
