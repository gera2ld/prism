package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// testServer serves a fixed tool set over in-process pipes. Using the SDK's
// in-memory transport means these tests exercise the real connect, list and
// call paths with no subprocess and no network.
type testServer struct {
	t     *testing.T
	tools []*mcpsdk.Tool
}

func newTestServer(t *testing.T, tools ...*mcpsdk.Tool) *testServer {
	return &testServer{t: t, tools: tools}
}

// dial returns a fresh client end. An SDK transport is single-use, so a redial
// needs a new pipe, which means a newly served instance of the same tool set.
func (s *testServer) dial() (mcpsdk.Transport, error) {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test", Version: "1.0.0"}, nil)
	for _, tool := range s.tools {
		server.AddTool(tool, echoHandler)
	}
	clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()
	session, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		return nil, err
	}
	s.t.Cleanup(func() { _ = session.Close() })
	return clientTransport, nil
}

// config returns a ServerConfig wired to this server, counting dials so a test
// can assert that connecting is lazy and that a refresh redials.
func (s *testServer) config(name string, dials *int) ServerConfig {
	var mu sync.Mutex
	return ServerConfig{
		Name:      name,
		Transport: TransportStdio,
		Enabled:   true,
		Dial: func(context.Context) (mcpsdk.Transport, error) {
			mu.Lock()
			if dials != nil {
				*dials++
			}
			mu.Unlock()
			return s.dial()
		},
	}
}

func echoHandler(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	return &mcpsdk.CallToolResult{
		Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok:" + req.Params.Name}},
		StructuredContent: map[string]any{"echoed": req.Params.Name},
	}, nil
}

func echoTool(description string) *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        "echo",
		Description: description,
		InputSchema: map[string]any{"type": "object"},
	}
}

func TestPoolListDiscoversToolsWithHashes(t *testing.T) {
	srv := newTestServer(t, echoTool("Echoes its input."))
	pool := NewPool(nil)
	defer pool.Close()

	pool.Apply([]ServerConfig{srv.config("echo-server", nil)})

	tools, err := pool.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(tools))
	}
	got := tools[0]
	if got.Server != "echo-server" || got.Tool != "echo" {
		t.Fatalf("unexpected tool %+v", got)
	}
	if got.Description != "Echoes its input." {
		t.Fatalf("description = %q", got.Description)
	}
	if len(got.Hash) != 64 {
		t.Fatalf("hash = %q, want 64 hex chars", got.Hash)
	}
	if !strings.Contains(string(got.InputSchema), "object") {
		t.Fatalf("input schema = %s, want an object schema", got.InputSchema)
	}
}

func TestPoolCallReturnsResult(t *testing.T) {
	srv := newTestServer(t, echoTool("Echoes."))
	pool := NewPool(nil)
	defer pool.Close()
	pool.Apply([]ServerConfig{srv.config("s", nil)})

	result, err := pool.Call(context.Background(), "s", "echo", map[string]any{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result.IsError {
		t.Fatal("unexpected tool error")
	}
	structured, ok := result.Value.(map[string]any)
	if !ok {
		t.Fatalf("value = %#v, want structured content", result.Value)
	}
	if structured["echoed"] != "echo" {
		t.Fatalf("structured = %#v", structured)
	}
}

func TestPoolCallUnknownServer(t *testing.T) {
	pool := NewPool(nil)
	defer pool.Close()
	if _, err := pool.Call(context.Background(), "nope", "echo", nil); !errors.Is(err, ErrUnknownServer) {
		t.Fatalf("err = %v, want ErrUnknownServer", err)
	}
}

func TestPoolDialsLazily(t *testing.T) {
	srv := newTestServer(t, echoTool("Echoes."))
	pool := NewPool(nil)
	defer pool.Close()

	dials := 0
	pool.Apply([]ServerConfig{srv.config("s", &dials)})
	// Apply must not connect: a broken server cannot be allowed to delay
	// startup or block the chat proxy.
	if dials != 0 {
		t.Fatalf("Apply dialed %d times, want 0", dials)
	}
	if _, err := pool.List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	}
	if dials != 1 {
		t.Fatalf("after List, dials = %d, want 1", dials)
	}
	// A second List reuses the live session rather than redialing.
	if _, err := pool.List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	}
	if dials != 1 {
		t.Fatalf("after second List, dials = %d, want 1", dials)
	}
}

func TestPoolRefreshRedials(t *testing.T) {
	srv := newTestServer(t, echoTool("Echoes."))
	pool := NewPool(nil)
	defer pool.Close()

	dials := 0
	pool.Apply([]ServerConfig{srv.config("s", &dials)})
	if _, err := pool.List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := pool.Refresh("s"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if _, err := pool.List(context.Background()); err != nil {
		t.Fatalf("List after refresh: %v", err)
	}
	if dials != 2 {
		t.Fatalf("dials = %d, want 2 (initial plus refresh)", dials)
	}
}

func TestPoolRefreshUnknownServer(t *testing.T) {
	pool := NewPool(nil)
	defer pool.Close()
	if err := pool.Refresh("nope"); !errors.Is(err, ErrUnknownServer) {
		t.Fatalf("err = %v, want ErrUnknownServer", err)
	}
}

func TestPoolDisabledServerIsNotPooled(t *testing.T) {
	newTestServer(t, echoTool("Echoes."))
	pool := NewPool(nil)
	defer pool.Close()

	// A disabled server never enters the pool, so it cannot be reached at all.
	pool.Apply([]ServerConfig{{Name: "off", Transport: TransportStdio, Enabled: false}})
	tools, err := pool.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("got %d tools from a disabled server", len(tools))
	}
	if _, err := pool.Call(context.Background(), "off", "echo", nil); !errors.Is(err, ErrUnknownServer) {
		t.Fatalf("err = %v, want ErrUnknownServer", err)
	}
}

func TestPoolRemovedServerIsDropped(t *testing.T) {
	srv := newTestServer(t, echoTool("Echoes."))
	pool := NewPool(nil)
	defer pool.Close()

	pool.Apply([]ServerConfig{srv.config("s", nil)})
	if _, err := pool.List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	}
	// Apply with no configs is how a deleted server row reaches the pool.
	pool.Apply(nil)
	if _, err := pool.Call(context.Background(), "s", "echo", nil); !errors.Is(err, ErrUnknownServer) {
		t.Fatalf("err = %v, want ErrUnknownServer after removal", err)
	}
}

func TestPoolCachedReflectsSessionAndNotifiesOnChange(t *testing.T) {
	srv := newTestServer(t, echoTool("Echoes."))
	pool := NewPool(nil)
	defer pool.Close()

	var mu sync.Mutex
	changes := 0
	pool.OnChange(func() {
		mu.Lock()
		defer mu.Unlock()
		changes++
	})
	pool.Apply([]ServerConfig{srv.config("s", nil)})

	if got := pool.Cached("s"); got != nil {
		t.Fatalf("Cached before connect = %#v, want nil", got)
	}
	if _, err := pool.List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	}
	mu.Lock()
	afterFirst := changes
	mu.Unlock()
	if afterFirst != 1 {
		t.Fatalf("changes = %d after first discovery, want 1", afterFirst)
	}

	// An unchanged list must not fire the callback, so repeated polling cannot
	// churn everything derived from discovery.
	if _, err := pool.List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	}
	mu.Lock()
	afterSecond := changes
	mu.Unlock()
	if afterSecond != afterFirst {
		t.Fatalf("changes = %d after an unchanged relist, want %d", afterSecond, afterFirst)
	}

	cached := pool.Cached("s")
	if len(cached) != 1 || cached[0].Tool != "echo" {
		t.Fatalf("Cached = %#v", cached)
	}
}

func TestPoolSkipsServerWhoseDialFails(t *testing.T) {
	pool := NewPool(nil)
	defer pool.Close()
	pool.Apply([]ServerConfig{{
		Name:      "broken",
		Transport: TransportStdio,
		Enabled:   true,
		Dial: func(context.Context) (mcpsdk.Transport, error) {
			return nil, errors.New("spawn refused")
		},
	}})
	// One broken server must not turn the whole listing into a failure.
	if _, err := pool.List(context.Background()); err == nil {
		t.Fatal("expected an error for a server that cannot dial")
	}
}

func TestPoolRejectsIncompleteConfig(t *testing.T) {
	pool := NewPool(nil)
	defer pool.Close()

	cases := []ServerConfig{
		{Name: "a", Transport: TransportStdio, Enabled: true},
		{Name: "b", Transport: TransportHTTP, Enabled: true},
		{Name: "c", Transport: "carrier-pigeon", Enabled: true},
	}
	for _, cfg := range cases {
		pool.Apply([]ServerConfig{cfg})
		if _, err := pool.List(context.Background()); err == nil {
			t.Fatalf("server %q: expected a dial error", cfg.Name)
		}
	}
}

func TestToResultFlattensContent(t *testing.T) {
	// Structured content wins: it is the form the server committed to.
	structured := toResult(&mcpsdk.CallToolResult{
		Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: "ignored"}},
		StructuredContent: map[string]any{"a": 1},
	})
	if _, ok := structured.Value.(map[string]any); !ok {
		t.Fatalf("structured = %#v", structured.Value)
	}

	// Without it, text blocks join; one stays bare rather than being split.
	single := toResult(&mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "one"}}})
	if single.Value != "one" {
		t.Fatalf("single = %#v", single.Value)
	}
	multi := toResult(&mcpsdk.CallToolResult{Content: []mcpsdk.Content{
		&mcpsdk.TextContent{Text: "a"},
		&mcpsdk.TextContent{Text: "b"},
	}})
	if multi.Value != "a\nb" {
		t.Fatalf("multi = %#v", multi.Value)
	}

	// A non-text block has no rendering, so the result is empty but is_error
	// still carries the signal that something came back.
	image := toResult(&mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{&mcpsdk.ImageContent{Data: []byte{0, 0, 0, 0}, MIMEType: "image/png"}},
		IsError: true,
	})
	if image.Value != nil || !image.IsError {
		t.Fatalf("image = %#v", image)
	}

	if got := toResult(nil); got.Value != nil || got.IsError {
		t.Fatalf("nil = %#v", got)
	}
}

func TestDiscoveredMarshalsSchema(t *testing.T) {
	tools, err := toDiscovered("s", []*mcpsdk.Tool{echoTool("Echoes.")})
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(tools[0].InputSchema, &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Fatalf("schema = %#v", schema)
	}
}
