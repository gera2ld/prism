package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gera2ld/prism/internal/gateway"
)

type fakeRegistry struct {
	tools   []gateway.Tool
	result  gateway.ToolResult
	listErr error
	callErr error
	gotArgs string
	keyID   string
}

func (f *fakeRegistry) List(_ context.Context, key gateway.Key) ([]gateway.Tool, error) {
	f.keyID = key.ID
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.tools, nil
}

func (f *fakeRegistry) Invoke(_ context.Context, key gateway.Key, name string, args json.RawMessage) (gateway.ToolResult, error) {
	f.keyID = key.ID
	f.gotArgs = string(args)
	return f.result, f.callErr
}

type fakeSink struct{ records []gateway.ToolRecord }

func (s *fakeSink) WriteTool(_ context.Context, r gateway.ToolRecord) error {
	s.records = append(s.records, r)
	return nil
}

// serve wires a server to an in-process MCP client over an in-memory transport,
// so the real connect/list/call paths run with no network.
func serve(t *testing.T, registry *fakeRegistry) (*mcpsdk.ClientSession, *fakeSink) {
	t.Helper()
	return serveCapturing(t, registry, false)
}

func serveCapturing(t *testing.T, registry *fakeRegistry, capture bool) (*mcpsdk.ClientSession, *fakeSink) {
	t.Helper()
	sink := &fakeSink{}
	s := New(registry, sink, nil)
	s.now = func() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) }
	s.SetCapture(func() bool { return capture })

	server := s.build(gateway.Key{ID: "key1", Name: "test-key"})
	clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), serverTransport, nil); err != nil {
		t.Fatalf("connect server: %v", err)
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "1.0.0"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, sink
}

func weatherTool() gateway.Tool {
	return gateway.Tool{
		Name:        "weather",
		Description: "Reports the weather.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
	}
}

func TestListsPublishedTools(t *testing.T) {
	session, _ := serve(t, &fakeRegistry{tools: []gateway.Tool{weatherTool()}})

	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(listed.Tools) != 1 || listed.Tools[0].Name != "weather" {
		t.Fatalf("tools = %#v", listed.Tools)
	}
	if listed.Tools[0].Description != "Reports the weather." {
		t.Fatalf("description = %q", listed.Tools[0].Description)
	}
}

func TestCallsPublishedTool(t *testing.T) {
	registry := &fakeRegistry{
		tools:  []gateway.Tool{weatherTool()},
		result: gateway.ToolResult{Result: map[string]any{"temp": 21}},
	}
	session, sink := serveCapturing(t, registry, true)

	out, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "weather", Arguments: map[string]any{"city": "Oslo"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if out.IsError {
		t.Fatalf("unexpected tool error: %#v", out.Content)
	}
	if registry.gotArgs != `{"city":"Oslo"}` {
		t.Fatalf("arguments = %s", registry.gotArgs)
	}
	// Structured content plus text, so a text-only client still sees something.
	structured, ok := out.StructuredContent.(map[string]any)
	if !ok || structured["temp"] != float64(21) {
		t.Fatalf("structured = %#v", out.StructuredContent)
	}
	if len(out.Content) != 1 {
		t.Fatalf("content = %#v", out.Content)
	}
	if block, ok := out.Content[0].(*mcpsdk.TextContent); !ok || block.Text != `{"temp":21}` {
		t.Fatalf("text content = %#v", out.Content[0])
	}

	// Recorded against the calling key and tagged with the transport.
	if len(sink.records) != 1 {
		t.Fatalf("records = %d, want 1", len(sink.records))
	}
	rec := sink.records[0]
	if rec.Transport != TransportMCP {
		t.Fatalf("transport = %q, want %q", rec.Transport, TransportMCP)
	}
	if rec.KeyID != "key1" || rec.KeyName != "test-key" {
		t.Fatalf("key = %q/%q, want key1/test-key", rec.KeyID, rec.KeyName)
	}
	if rec.Tool != "weather" || rec.Outcome != gateway.ToolOutcomeCompleted {
		t.Fatalf("record = %+v", rec)
	}
	if rec.Args != `{"city":"Oslo"}` || rec.Result != `{"temp":21}` {
		t.Fatalf("captured args=%q result=%q", rec.Args, rec.Result)
	}
}

func TestToolFailureIsReportedAsToolError(t *testing.T) {
	// A failed tool is IsError, not a protocol error.
	registry := &fakeRegistry{
		tools:  []gateway.Tool{weatherTool()},
		result: gateway.ToolResult{Result: "upstream said 503", IsError: true},
	}
	session, sink := serve(t, registry)

	out, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "weather"})
	if err != nil {
		t.Fatalf("CallTool returned a protocol error: %v", err)
	}
	if !out.IsError {
		t.Fatal("expected IsError")
	}
	if sink.records[0].Outcome != gateway.ToolOutcomeToolError {
		t.Fatalf("outcome = %q", sink.records[0].Outcome)
	}
	if sink.records[0].Error == "" {
		t.Fatal("a failed tool should record why")
	}
}

func TestGatewayFaultIsAProtocolError(t *testing.T) {
	registry := &fakeRegistry{
		tools:   []gateway.Tool{weatherTool()},
		callErr: errors.New("registry down"),
	}
	session, sink := serve(t, registry)

	if _, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "weather"}); err == nil {
		t.Fatal("expected an error when the gateway itself fails")
	}
	if sink.records[0].Outcome != gateway.ToolOutcomeGatewayError {
		t.Fatalf("outcome = %q", sink.records[0].Outcome)
	}
}

func TestStringResultTravelsAsBareText(t *testing.T) {
	registry := &fakeRegistry{tools: []gateway.Tool{weatherTool()}, result: gateway.ToolResult{Result: "sunny"}}
	session, _ := serve(t, registry)

	out, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "weather"})
	if err != nil {
		t.Fatal(err)
	}
	if out.StructuredContent != nil {
		t.Fatalf("structured = %#v, want none for a bare string", out.StructuredContent)
	}
	if block, ok := out.Content[0].(*mcpsdk.TextContent); !ok || block.Text != "sunny" {
		t.Fatalf("content = %#v", out.Content[0])
	}
}

func TestMissingArgumentsBecomeEmptyObject(t *testing.T) {
	registry := &fakeRegistry{tools: []gateway.Tool{weatherTool()}, result: gateway.ToolResult{Result: "ok"}}
	session, _ := serve(t, registry)

	if _, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "weather"}); err != nil {
		t.Fatal(err)
	}
	if registry.gotArgs != `{}` {
		t.Fatalf("arguments = %s, want {}", registry.gotArgs)
	}
}

// The SDK panics on a schema it cannot use. An admin-UI edit must not be able to
// take the gateway down, so an unusable schema is skipped and logged instead.
func TestUnusableSchemaIsSkippedNotFatal(t *testing.T) {
	cases := map[string]json.RawMessage{
		"no type":       json.RawMessage(`{"properties":{"city":{"type":"string"}}}`),
		"wrong type":    json.RawMessage(`{"type":"string"}`),
		"not an object": json.RawMessage(`["a"]`),
		"invalid json":  json.RawMessage(`{`),
	}
	for name, schema := range cases {
		t.Run(name, func(t *testing.T) {
			tool := weatherTool()
			tool.InputSchema = schema
			session, _ := serve(t, &fakeRegistry{tools: []gateway.Tool{tool}})

			listed, err := session.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatalf("ListTools: %v", err)
			}
			if len(listed.Tools) != 0 {
				t.Fatalf("unusable schema was published: %#v", listed.Tools)
			}
		})
	}
}

// The SDK rejects a nil schema outright.
func TestAbsentSchemaGetsAnObjectSubstitute(t *testing.T) {
	tool := weatherTool()
	tool.InputSchema = nil
	session, _ := serve(t, &fakeRegistry{tools: []gateway.Tool{tool}})

	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(listed.Tools) != 1 {
		t.Fatalf("tools = %#v", listed.Tools)
	}
	if listed.Tools[0].InputSchema == nil {
		t.Fatal("published tool has no input schema")
	}
	encoded, err := json.Marshal(listed.Tools[0].InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"type":"object"}` {
		t.Fatalf("substitute schema = %s, want an empty object schema", encoded)
	}
}

func TestOneBadToolDoesNotHideTheRest(t *testing.T) {
	bad := weatherTool()
	bad.InputSchema = json.RawMessage(`{"type":"string"}`)
	good := weatherTool()
	good.Name = "forecast"

	session, _ := serve(t, &fakeRegistry{tools: []gateway.Tool{bad, good}})
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(listed.Tools) != 1 || listed.Tools[0].Name != "forecast" {
		t.Fatalf("tools = %#v", listed.Tools)
	}
}

func TestRegistryFailureYieldsAnEmptyServer(t *testing.T) {
	session, _ := serve(t, &fakeRegistry{listErr: errors.New("catalog unavailable")})
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(listed.Tools) != 0 {
		t.Fatalf("tools = %#v", listed.Tools)
	}
}

// A fresh server per request, so a catalog edit applies without a restart.
func TestHandlerRebuildsCatalogPerRequest(t *testing.T) {
	registry := &fakeRegistry{tools: []gateway.Tool{weatherTool()}}
	s := New(registry, &fakeSink{}, nil)
	handler := s.Handler(gateway.Key{ID: "k", Name: "n"})

	if got := countTools(t, handler); got != 1 {
		t.Fatalf("tools = %d, want 1", got)
	}
	registry.tools = nil
	if got := countTools(t, handler); got != 0 {
		t.Fatalf("tools = %d after the catalog was emptied, want 0", got)
	}
	registry.tools = []gateway.Tool{weatherTool()}
	if got := countTools(t, handler); got != 1 {
		t.Fatalf("tools = %d after the catalog was refilled, want 1", got)
	}
}

// A real client, so the stateless path runs end to end.
func countTools(t *testing.T, handler http.Handler) int {
	t.Helper()
	front := httptest.NewServer(authedHandler(handler))
	defer front.Close()

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "1.0.0"}, nil)
	session, err := client.Connect(context.Background(), bearerClient(front.URL), nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()

	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	return len(listed.Tools)
}

// authedHandler stands in for the router's bearer check.
func authedHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerClient talks to a handler that demands a bearer token, standing in for
// the key the router would have resolved.
func bearerClient(endpoint string) *mcpsdk.StreamableClientTransport {
	return &mcpsdk.StreamableClientTransport{
		Endpoint:   endpoint,
		MaxRetries: -1,
		HTTPClient: &http.Client{Transport: bearerTransport{token: "test-key"}},
	}
}

type bearerTransport struct{ token string }

func (t bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(clone)
}

// Capture is off by default so nothing is stored unless the operator asked.
func TestCaptureDefaultsOff(t *testing.T) {
	registry := &fakeRegistry{
		tools:  []gateway.Tool{weatherTool()},
		result: gateway.ToolResult{Result: map[string]any{"temp": 21}},
	}
	session, sink := serve(t, registry)
	if _, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "weather", Arguments: map[string]any{"city": "Oslo"},
	}); err != nil {
		t.Fatal(err)
	}
	if sink.records[0].Args != "" || sink.records[0].Result != "" {
		t.Fatalf("captured with capture off: args=%q result=%q",
			sink.records[0].Args, sink.records[0].Result)
	}
}

func TestPublishableSchema(t *testing.T) {
	ok := map[string]struct {
		raw  json.RawMessage
		good bool
	}{
		"object":            {json.RawMessage(`{"type":"object"}`), true},
		"object with props": {json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`), true},
		// Published with a substitute rather than hidden: still callable.
		"nil":          {nil, true},
		"empty":        {json.RawMessage(``), true},
		"blank object": {json.RawMessage(`{}`), true},
		"missing type": {json.RawMessage(`{"properties":{}}`), false},
		"array type":   {json.RawMessage(`{"type":"array"}`), false},
		"malformed":    {json.RawMessage(`{`), false},
		"not a schema": {json.RawMessage(`"hello"`), false},
	}
	for name, tc := range ok {
		t.Run(name, func(t *testing.T) {
			_, good := publishableSchema(tc.raw)
			if good != tc.good {
				t.Fatalf("publishableSchema(%s) = %v, want %v", tc.raw, good, tc.good)
			}
		})
	}
}
