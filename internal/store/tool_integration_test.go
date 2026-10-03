package store

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pocketbase/pocketbase/core"

	"github.com/gera2ld/prism/internal/gateway"
	"github.com/gera2ld/prism/internal/mcp"
)

// echoDefinition is a minimal valid conduit definition: one step, one output.
const echoDefinition = `
name: echo_weather
description: Reports the current weather for a city.
input_schema:
  type: object
  properties:
    city:
      type: string
  required: [city]
steps:
  - id: weather
    url: '"https://example.test/weather/" & $string(input.city)'
output_transform: '{ "summary": steps.weather.summary }'
`

func saveRecord(t *testing.T, app core.App, collection string, values map[string]any) *core.Record {
	t.Helper()
	c, err := app.FindCollectionByNameOrId(collection)
	if err != nil {
		t.Fatal(err)
	}
	record := core.NewRecord(c)
	for key, value := range values {
		record.Set(key, value)
	}
	if err := app.Save(record); err != nil {
		t.Fatalf("save %s: %v", collection, err)
	}
	return record
}

func listNames(tools []gateway.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, tool := range tools {
		out = append(out, tool.Name)
	}
	return out
}

// --- conduit tools ---

func TestConduitToolIsDerivedFromDefinition(t *testing.T) {
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	saveRecord(t, app, toolsCollection, map[string]any{
		"name": "weather", "definition": echoDefinition, "enabled": true,
	})

	tools, err := s.Tools().List(context.Background(), gateway.Key{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("got %d tools: %v", len(tools), listNames(tools))
	}
	tool := tools[0]
	// Description and schema come from the definition, never from a second
	// column that could disagree with it.
	if tool.Description != "Reports the current weather for a city." {
		t.Fatalf("description = %q", tool.Description)
	}
	if tool.Source != gateway.SourceConduit || tool.Server != "" {
		t.Fatalf("source = %q server = %q", tool.Source, tool.Server)
	}
	var schema map[string]any
	if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
		t.Fatalf("input schema is not valid JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Fatalf("schema = %#v", schema)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok || properties["city"] == nil {
		t.Fatalf("schema lost the city property: %#v", schema)
	}
}

func TestDisabledConduitToolIsHidden(t *testing.T) {
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	saveRecord(t, app, toolsCollection, map[string]any{"name": "off", "definition": echoDefinition, "enabled": false})

	tools, err := s.Tools().List(context.Background(), gateway.Key{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 0 {
		t.Fatalf("disabled tool was callable: %v", listNames(tools))
	}

	// The operator view still shows it, so a disabled tool can be found again.
	info, err := s.ConduitTools()
	if err != nil {
		t.Fatal(err)
	}
	if len(info) != 1 || info[0].Name != "off" || info[0].Enabled {
		t.Fatalf("info = %#v", info)
	}
	if info[0].Invalid {
		t.Fatal("valid definition reported as invalid")
	}
}

func TestConduitDefinitionRejectedAtSave(t *testing.T) {
	app := newTestApp(t)
	if _, err := Open(app, nil); err != nil {
		t.Fatal(err)
	}
	// conduitgo rejects an unknown top-level key, an empty step list, a missing
	// output_transform, a duplicate step id and an unknown method. Each must be
	// refused on the way in, so a broken definition never has to be skipped at
	// load time.
	cases := map[string]string{
		"unknown top-level key": "name: x\nsteps:\n  - {id: a, url: '\"http://x\"'}\noutput_transform: '{}'\nnope: 1\n",
		"no steps":              "name: x\nsteps: []\noutput_transform: '{}'\n",
		"no output transform":   "name: x\nsteps:\n  - {id: a, url: '\"http://x\"'}\n",
		"duplicate step id":     "name: x\nsteps:\n  - {id: a, url: '\"http://x\"'}\n  - {id: a, url: '\"http://y\"'}\noutput_transform: '{}'\n",
		"bad method":            "name: x\nsteps:\n  - {id: a, url: '\"http://x\"', method: FETCH}\noutput_transform: '{}'\n",
	}
	for name, definition := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := app.FindCollectionByNameOrId(toolsCollection)
			if err != nil {
				t.Fatal(err)
			}
			record := core.NewRecord(c)
			record.Set("name", "candidate")
			record.Set("definition", definition)
			record.Set("enabled", true)
			if err := app.Save(record); err == nil {
				t.Fatal("expected the invalid definition to be rejected")
			}
		})
	}
}

func TestConduitDefinitionRejectsBadName(t *testing.T) {
	app := newTestApp(t)
	if _, err := Open(app, nil); err != nil {
		t.Fatal(err)
	}
	c, err := app.FindCollectionByNameOrId(toolsCollection)
	if err != nil {
		t.Fatal(err)
	}
	record := core.NewRecord(c)
	record.Set("name", "has spaces/and-slash")
	record.Set("definition", echoDefinition)
	// A name that is not a legal function name would produce a tool no agent
	// could address, so the collection pattern rejects it.
	if err := app.Save(record); err == nil {
		t.Fatal("expected an illegal tool name to be rejected")
	}
}

func TestConduitCallReportsFailureAsToolError(t *testing.T) {
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	saveRecord(t, app, toolsCollection, map[string]any{"name": "weather", "definition": echoDefinition, "enabled": true})

	// The definition points at a host that does not resolve. That is a tool
	// that ran and failed, not a gateway fault, so it must be an IsError result
	// the agent can read rather than an error the agent cannot.
	result, err := s.Tools().Invoke(context.Background(), gateway.Key{}, "weather", json.RawMessage(`{"city":"Oslo"}`))
	if err != nil {
		t.Fatalf("Invoke returned a gateway error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError for a failing tool")
	}
	if result.Result == nil {
		t.Fatal("expected the failure reason in the result")
	}
}

func TestConduitCallUnknownTool(t *testing.T) {
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Tools().Invoke(context.Background(), gateway.Key{}, "nope", json.RawMessage(`{}`))
	if !errors.Is(err, gateway.ErrUnknownTool) {
		t.Fatalf("err = %v, want ErrUnknownTool", err)
	}
}

func TestValidateConduitDefinition(t *testing.T) {
	if err := ValidateConduitDefinition([]byte(echoDefinition)); err != nil {
		t.Fatalf("valid definition rejected: %v", err)
	}
	if err := ValidateConduitDefinition(nil); err == nil {
		t.Fatal("expected an empty definition to be rejected")
	}
	if err := ValidateConduitDefinition([]byte("   ")); err == nil {
		t.Fatal("expected a blank definition to be rejected")
	}
}

// A conduit request must identify itself: Go sends no User-Agent unless one is
// set, and public APIs answer an unidentified client with a bare 403.
func TestConduitSendsDefaultUserAgent(t *testing.T) {
	var seen http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"summary":"clear"}`))
	}))
	defer server.Close()

	definition := `
name: probe
description: probe
steps:
  - id: call
    url: '"` + server.URL + `/weather"'
output_transform: 'steps.call'
`
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	saveRecord(t, app, toolsCollection, map[string]any{
		"name": "probe", "definition": definition, "enabled": true,
	})

	result, err := s.Tools().Invoke(context.Background(), gateway.Key{}, "probe", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("probe failed: %#v", result.Result)
	}
	if got := seen.Get("User-Agent"); got != "Prism/"+Version {
		t.Fatalf("User-Agent = %q, want %q", got, "Prism/"+Version)
	}
}

func TestConduitStepHeaderOverridesDefault(t *testing.T) {
	var seen http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"summary":"clear"}`))
	}))
	defer server.Close()

	// A step that wants to identify itself differently still can: the engine
	// resolves step headers over the run-level defaults.
	definition := `
name: probe
description: probe
steps:
  - id: call
    url: '"` + server.URL + `/weather"'
    headers:
      User-Agent: '"my-tool/9"'
output_transform: 'steps.call'
`
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	saveRecord(t, app, toolsCollection, map[string]any{
		"name": "probe", "definition": definition, "enabled": true,
	})

	if _, err := s.Tools().Invoke(context.Background(), gateway.Key{}, "probe", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if got := seen.Get("User-Agent"); got != "my-tool/9" {
		t.Fatalf("User-Agent = %q, want the step's own value", got)
	}
}

// The default is a bare Name/version token, because some public APIs reject the
// conventional "App/1.0 (contact)" form for containing parentheses or an @.
func TestDefaultUserAgentAvoidsBlockedCharacters(t *testing.T) {
	for _, forbidden := range []string{"(", ")", "@"} {
		if strings.Contains(defaultConduitHeaders["User-Agent"], forbidden) {
			t.Fatalf("default User-Agent %q contains %q, which some APIs reject",
				defaultConduitHeaders["User-Agent"], forbidden)
		}
	}
}

// --- MCP approvals ---

// mcpFixture wires a store to an in-process MCP server so the approval ledger
// can be exercised with no subprocess. serve swaps what the server publishes,
// which is how a silently-changed definition looks from the gateway's side.
type mcpFixture struct {
	app    *core.BaseApp
	store  *Store
	server string

	mu    sync.Mutex
	tools []*mcpsdk.Tool
}

func newMCPFixture(t *testing.T, tools ...*mcpsdk.Tool) *mcpFixture {
	t.Helper()
	f := &mcpFixture{server: "fs", tools: tools}

	pool := mcp.NewPool(nil)
	t.Cleanup(pool.Close)

	dial := func(context.Context) (mcpsdk.Transport, error) {
		server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test", Version: "1.0.0"}, nil)
		for _, tool := range f.current() {
			tool := tool
			server.AddTool(tool, func(_ context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return &mcpsdk.CallToolResult{
					Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}},
					StructuredContent: map[string]any{"ok": true},
				}, nil
			})
		}
		clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()
		session, err := server.Connect(context.Background(), serverTransport, nil)
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { _ = session.Close() })
		return clientTransport, nil
	}

	f.app = newTestApp(t)
	f.store, _ = Open(f.app, nil,
		WithMCPPool(pool),
		WithMCPDial(func(string) func(context.Context) (mcpsdk.Transport, error) { return dial }),
	)
	if f.store == nil {
		t.Fatal("Open returned no store")
	}
	saveRecord(t, f.app, mcpServersCollection, map[string]any{
		"name": "fs", "transport": "stdio", "command": "placeholder",
		"enabled": true, "args": []string{},
	})
	return f
}

func (f *mcpFixture) serve(tools ...*mcpsdk.Tool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tools = tools
}

func (f *mcpFixture) current() []*mcpsdk.Tool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tools
}

func testMCPTool(description string) *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        "read_file",
		Description: description,
		InputSchema: map[string]any{"type": "object"},
	}
}

func TestMCPToolIsWithheldUntilApproved(t *testing.T) {
	f := newMCPFixture(t, testMCPTool("Reads a file."))

	// Nothing is approved yet, so the catalog must be empty: MCP tools are
	// disabled by default.
	tools, err := f.store.Tools().List(context.Background(), gateway.Key{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("unapproved tool was callable: %v", listNames(tools))
	}

	views, err := f.store.MCPTools(context.Background(), "fs")
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Status != MCPToolUnapproved {
		t.Fatalf("views = %#v", views)
	}
	if len(views[0].Hash) != 64 {
		t.Fatalf("hash = %q", views[0].Hash)
	}
	// The namespaced name is what an agent would call.
	if views[0].Name != "mcp__fs__read_file" {
		t.Fatalf("name = %q", views[0].Name)
	}

	if err := f.store.ApproveMCPTool(context.Background(), "fs", "read_file", views[0].Hash); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	tools, err = f.store.Tools().List(context.Background(), gateway.Key{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := listNames(tools); len(got) != 1 || got[0] != "mcp__fs__read_file" {
		t.Fatalf("after approval: %v", got)
	}
}

func TestMCPToolWithheldWhenDefinitionChanges(t *testing.T) {
	f := newMCPFixture(t, testMCPTool("Reads a file."))

	views, err := f.store.MCPTools(context.Background(), "fs")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.ApproveMCPTool(context.Background(), "fs", "read_file", views[0].Hash); err != nil {
		t.Fatal(err)
	}

	// The server now publishes a different description, which is exactly the
	// quiet-edit vector the hash pin exists to catch.
	f.serve(testMCPTool("Reads a file and then deletes it."))
	if err := f.store.RefreshMCPServer("fs"); err != nil {
		t.Fatal(err)
	}

	tools, err := f.store.Tools().List(context.Background(), gateway.Key{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 0 {
		t.Fatalf("changed tool stayed callable: %v", listNames(tools))
	}

	views, err = f.store.MCPTools(context.Background(), "fs")
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Status != MCPToolChanged {
		t.Fatalf("status = %#v, want changed", views)
	}
	if views[0].Hash == views[0].ApprovedHash {
		t.Fatal("live and approved hashes are identical, so the change was not detected")
	}
}

func TestApproveRefusesStaleHash(t *testing.T) {
	f := newMCPFixture(t, testMCPTool("Reads a file."))

	views, err := f.store.MCPTools(context.Background(), "fs")
	if err != nil {
		t.Fatal(err)
	}
	err = f.store.ApproveMCPTool(context.Background(), "fs", "read_file", strings.Repeat("0", 64))
	if !errors.Is(err, ErrApprovalStale) {
		t.Fatalf("err = %v, want ErrApprovalStale", err)
	}
	// A refused approval must leave the tool withheld.
	tools, err := f.store.Tools().List(context.Background(), gateway.Key{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 0 {
		t.Fatalf("tool became callable after a refused approval: %v", listNames(tools))
	}

	// The right hash still works.
	if err := f.store.ApproveMCPTool(context.Background(), "fs", "read_file", views[0].Hash); err != nil {
		t.Fatalf("Approve with the reviewed hash: %v", err)
	}
}

func TestApproveUnknownTool(t *testing.T) {
	f := newMCPFixture(t, testMCPTool("Reads a file."))
	err := f.store.ApproveMCPTool(context.Background(), "fs", "absent", strings.Repeat("a", 64))
	if !errors.Is(err, gateway.ErrUnknownTool) {
		t.Fatalf("err = %v, want ErrUnknownTool", err)
	}
}

func TestRevokeHidesToolAgain(t *testing.T) {
	f := newMCPFixture(t, testMCPTool("Reads a file."))

	views, err := f.store.MCPTools(context.Background(), "fs")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.ApproveMCPTool(context.Background(), "fs", "read_file", views[0].Hash); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RevokeMCPTool("fs", "read_file"); err != nil {
		t.Fatal(err)
	}
	tools, err := f.store.Tools().List(context.Background(), gateway.Key{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 0 {
		t.Fatalf("revoked tool still callable: %v", listNames(tools))
	}
	if err := f.store.RevokeMCPTool("fs", "read_file"); err == nil {
		t.Fatal("expected revoking a missing approval to fail")
	}
}

func TestMCPInvokeRequiresApproval(t *testing.T) {
	f := newMCPFixture(t, testMCPTool("Reads a file."))

	// Even the exact namespaced name is not callable before approval.
	_, err := f.store.Tools().Invoke(context.Background(), gateway.Key{}, "mcp__fs__read_file", json.RawMessage(`{}`))
	if !errors.Is(err, gateway.ErrUnknownTool) {
		t.Fatalf("err = %v, want ErrUnknownTool for an unapproved tool", err)
	}

	views, err := f.store.MCPTools(context.Background(), "fs")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.ApproveMCPTool(context.Background(), "fs", "read_file", views[0].Hash); err != nil {
		t.Fatal(err)
	}
	result, err := f.store.Tools().Invoke(context.Background(), gateway.Key{}, "mcp__fs__read_file", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %#v", result.Result)
	}
	structured, ok := result.Result.(map[string]any)
	if !ok || structured["ok"] != true {
		t.Fatalf("result = %#v", result.Result)
	}
}

func TestDisabledMCPServerContributesNothing(t *testing.T) {
	f := newMCPFixture(t, testMCPTool("Reads a file."))

	views, err := f.store.MCPTools(context.Background(), "fs")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.ApproveMCPTool(context.Background(), "fs", "read_file", views[0].Hash); err != nil {
		t.Fatal(err)
	}

	row, err := f.app.FindFirstRecordByFilter(mcpServersCollection, "name = 'fs'")
	if err != nil {
		t.Fatal(err)
	}
	row.Set("enabled", false)
	if err := f.app.Save(row); err != nil {
		t.Fatal(err)
	}

	// The master switch withdraws the server's tools even though the approval
	// row is untouched.
	tools, err := f.store.Tools().List(context.Background(), gateway.Key{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 0 {
		t.Fatalf("disabled server still contributed tools: %v", listNames(tools))
	}
}

func TestOrphanedGrantIsReportedNotDropped(t *testing.T) {
	f := newMCPFixture(t, testMCPTool("Reads a file."))

	views, err := f.store.MCPTools(context.Background(), "fs")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.ApproveMCPTool(context.Background(), "fs", "read_file", views[0].Hash); err != nil {
		t.Fatal(err)
	}

	// The server stops publishing the tool entirely. The approval is kept so a
	// server flapping on a flaky network does not lose it.
	f.serve(&mcpsdk.Tool{
		Name:        "other",
		Description: "Something else.",
		InputSchema: map[string]any{"type": "object"},
	})
	if err := f.store.RefreshMCPServer("fs"); err != nil {
		t.Fatal(err)
	}

	views, err = f.store.MCPTools(context.Background(), "fs")
	if err != nil {
		t.Fatal(err)
	}
	var orphan *MCPToolView
	for i := range views {
		if views[i].Tool == "read_file" {
			orphan = &views[i]
		}
	}
	if orphan == nil {
		t.Fatalf("orphan grant was dropped: %#v", views)
	}
	if orphan.Status != MCPToolOrphaned || orphan.Offered {
		t.Fatalf("orphan = %#v", orphan)
	}
	if orphan.ApprovedHash == "" {
		t.Fatal("orphan lost its approved hash, so re-approval would not be checked")
	}
}

func TestMCPServerSecretsAreEncryptedAtRest(t *testing.T) {
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	saveRecord(t, app, mcpServersCollection, map[string]any{
		"name": "remote", "transport": "http", "url": "https://mcp.example/mcp",
		"enabled": true,
		"env":     map[string]string{"TOKEN": "super-secret"},
		"headers": map[string]string{"Authorization": "Bearer top-secret"},
	})

	row, err := app.FindFirstRecordByFilter(mcpServersCollection, "name = 'remote'")
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"env", "headers"} {
		stored := row.GetString(field)
		for _, secret := range []string{"super-secret", "top-secret"} {
			if strings.Contains(stored, secret) {
				t.Fatalf("%s stored in plaintext: %s", field, stored)
			}
		}
		if !strings.Contains(stored, "enc:") {
			t.Fatalf("%s is not encrypted: %s", field, stored)
		}
	}

	// Reveal is the only way to read them back, and it must round-trip.
	secrets, err := s.RevealMCPSecrets("remote")
	if err != nil {
		t.Fatal(err)
	}
	if secrets["env.TOKEN"] != "super-secret" {
		t.Fatalf("env.TOKEN = %q", secrets["env.TOKEN"])
	}
	if secrets["headers.Authorization"] != "Bearer top-secret" {
		t.Fatalf("headers.Authorization = %q", secrets["headers.Authorization"])
	}

	// Listing must never carry the values, only the fact that they exist.
	servers, err := s.MCPServers()
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || !servers[0].HasSecrets {
		t.Fatalf("servers = %#v", servers)
	}
	encoded, err := json.Marshal(servers)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"super-secret", "top-secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("server listing leaked %q: %s", secret, encoded)
		}
	}
}

func TestMCPServerValidation(t *testing.T) {
	app := newTestApp(t)
	if _, err := Open(app, nil); err != nil {
		t.Fatal(err)
	}
	cases := map[string]map[string]any{
		"stdio without command": {"name": "a", "transport": "stdio"},
		"http without url":      {"name": "b", "transport": "http"},
		"unknown transport":     {"name": "c", "transport": "smoke-signal"},
		"args not an array":     {"name": "d", "transport": "stdio", "command": "x", "args": map[string]string{"a": "b"}},
		"env not an object":     {"name": "e", "transport": "stdio", "command": "x", "env": "nope"},
	}
	for name, values := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := app.FindCollectionByNameOrId(mcpServersCollection)
			if err != nil {
				t.Fatal(err)
			}
			record := core.NewRecord(c)
			for key, value := range values {
				record.Set(key, value)
			}
			if err := app.Save(record); err == nil {
				t.Fatal("expected the invalid server to be rejected")
			}
		})
	}
}

// --- logging ---

func TestToolLogsViewReportsUsage(t *testing.T) {
	app := newTestApp(t)
	if _, err := Open(app, nil); err != nil {
		t.Fatal(err)
	}
	sink := NewToolLogSink(app)
	for _, outcome := range []gateway.ToolOutcome{
		gateway.ToolOutcomeCompleted, gateway.ToolOutcomeCompleted, gateway.ToolOutcomeToolError,
	} {
		if err := sink.WriteTool(context.Background(), gateway.ToolRecord{
			Tool: "weather", Source: gateway.SourceConduit,
			Outcome: outcome, Status: 200, DurationMS: 5,
		}); err != nil {
			t.Fatal(err)
		}
	}
	records, err := app.FindAllRecords(toolsUsageView.name)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d rows, want 1", len(records))
	}
	row := records[0]
	if row.GetString("id") != "weather" {
		t.Fatalf("id = %q", row.GetString("id"))
	}
	if row.GetInt("success_requests") != 2 || row.GetInt("fail_requests") != 1 {
		t.Fatalf("counts = %d/%d, want 2/1",
			row.GetInt("success_requests"), row.GetInt("fail_requests"))
	}
}

func TestCatalogMergesSourcesAndSortsNames(t *testing.T) {
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	saveRecord(t, app, toolsCollection, map[string]any{"name": "alpha", "definition": echoDefinition, "enabled": true})
	saveRecord(t, app, toolsCollection, map[string]any{"name": "zeta", "definition": echoDefinition, "enabled": true})

	tools, err := s.Tools().List(context.Background(), gateway.Key{})
	if err != nil {
		t.Fatal(err)
	}
	names := listNames(tools)
	if len(names) != 2 || names[0] != "alpha" || names[1] != "zeta" {
		t.Fatalf("names = %v, want sorted [alpha zeta]", names)
	}
}

// A call must report which source answered, so the invocation log can attribute
// it without the handler asking the registry a second time.
func TestConduitInvokeReportsSource(t *testing.T) {
	app := newTestApp(t)
	s, err := Open(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	saveRecord(t, app, toolsCollection, map[string]any{"name": "weather", "definition": echoDefinition, "enabled": true})

	result, err := s.Tools().Invoke(context.Background(), gateway.Key{}, "weather", json.RawMessage(`{"city":"Oslo"}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.Source != gateway.SourceConduit || result.Server != "" {
		t.Fatalf("source = %q server = %q", result.Source, result.Server)
	}
	// A failing call must still carry its provenance, so the log attributes it.
	if !result.IsError {
		t.Skip("conduit call unexpectedly succeeded; provenance of failures covered below")
	}
	if result.Source != gateway.SourceConduit {
		t.Fatalf("failed call lost its source: %q", result.Source)
	}
}

func TestMCPInvokeReportsSource(t *testing.T) {
	f := newMCPFixture(t, testMCPTool("Reads a file."))
	views, err := f.store.MCPTools(context.Background(), "fs")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.ApproveMCPTool(context.Background(), "fs", "read_file", views[0].Hash); err != nil {
		t.Fatal(err)
	}
	result, err := f.store.Tools().Invoke(context.Background(), gateway.Key{}, "mcp__fs__read_file", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.Source != gateway.SourceMCP || result.Server != "fs" {
		t.Fatalf("source = %q server = %q, want mcp/fs", result.Source, result.Server)
	}
}
