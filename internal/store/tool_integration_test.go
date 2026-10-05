package store

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"

	"github.com/gera2ld/prism/internal/gateway"
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
	// Description and schema come from the definition, not a second column.
	if tool.Description != "Reports the current weather for a city." {
		t.Fatalf("description = %q", tool.Description)
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
	// Refused on the way in, so a bad definition never needs skipping at load.
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
	// No agent could address a name that is not a legal function name.
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

	// A tool that ran and failed, not a gateway fault, so IsError not an error.
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
	if got := seen.Get("User-Agent"); got != "Prism/"+gateway.Version {
		t.Fatalf("User-Agent = %q, want %q", got, "Prism/"+gateway.Version)
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

	// A step header still overrides the run-level default.
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
			Tool: "weather", Transport: gateway.TransportREST,
			Outcome: outcome, Status: 200, DurationMS: 5,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// The same tool over both transports stays one row. Grouping by transport
	// gave two rows sharing an id, which PocketBase rejects once such data
	// exists, and a rejected view takes the whole schema apply down with it.
	if err := sink.WriteTool(context.Background(), gateway.ToolRecord{
		Tool: "weather", Transport: "mcp",
		Outcome: gateway.ToolOutcomeCompleted, Status: 200, DurationMS: 5,
	}); err != nil {
		t.Fatal(err)
	}
	records, err := app.FindAllRecords("tools_usage")
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
	if row.GetInt("success_requests") != 3 || row.GetInt("fail_requests") != 1 {
		t.Fatalf("counts = %d/%d, want 3/1",
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
