package gateway_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gera2ld/prism/internal/gateway"
)

type fakeRegistry struct {
	tools   []gateway.Tool
	result  gateway.ToolResult
	err     error
	gotArgs string
	calls   int
}

func (f *fakeRegistry) List(context.Context, gateway.Key) ([]gateway.Tool, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.tools, nil
}

func (f *fakeRegistry) Invoke(_ context.Context, _ gateway.Key, _ string, args json.RawMessage) (gateway.ToolResult, error) {
	f.calls++
	f.gotArgs = string(args)
	return f.result, f.err
}

type fakeToolSink struct{ records []gateway.ToolRecord }

func (s *fakeToolSink) WriteTool(_ context.Context, r gateway.ToolRecord) error {
	s.records = append(s.records, r)
	return nil
}

func toolProxy(t *testing.T, registry *fakeRegistry, capture bool) (*httptest.Server, *fakeToolSink) {
	t.Helper()
	sink := &fakeToolSink{}
	proxy := gateway.New(&fakeConfig{token: "key"}, nil, nil)
	proxy.Tools = registry
	proxy.ToolLogs = sink
	proxy.Capture = func() bool { return capture }
	proxy.Now = func() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) }

	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	return server, sink
}

func do(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(payload)
}

func sampleTools() []gateway.Tool {
	return []gateway.Tool{
		{
			Name:        "weather",
			Description: "Reports the weather.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		},
		{
			Name:        "reads_file",
			Description: "Reads a file.",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
	}
}

func TestListToolsRequiresAuth(t *testing.T) {
	server, _ := toolProxy(t, &fakeRegistry{tools: sampleTools()}, false)

	req, err := http.NewRequest(http.MethodGet, server.URL+"/v1/tools", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without a key", resp.StatusCode)
	}

	if code, _ := do(t, http.MethodGet, server.URL+"/v1/tools", ""); code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with a key", code)
	}
}

func TestListToolsReturnsOpenAIShape(t *testing.T) {
	server, _ := toolProxy(t, &fakeRegistry{tools: sampleTools()}, false)

	code, body := do(t, http.MethodGet, server.URL+"/v1/tools", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, body)
	}

	var listing struct {
		Object string `json:"object"`
		Data   []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &listing); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, body)
	}
	if listing.Object != "list" || len(listing.Data) != 2 {
		t.Fatalf("listing = %s", body)
	}
	first := listing.Data[0]
	if first.Type != "function" || first.ID != "weather" {
		t.Fatalf("entry = %+v", first)
	}
	if first.Function.Name != "weather" || first.Function.Description != "Reports the weather." {
		t.Fatalf("function = %+v", first.Function)
	}
	if !strings.Contains(string(first.Function.Parameters), `"city"`) {
		t.Fatalf("parameters = %s", first.Function.Parameters)
	}
	if listing.Data[1].ID != "reads_file" {
		t.Fatalf("second entry = %+v", listing.Data[1])
	}
}

func TestListToolsEmptyCatalogIsAnEmptyList(t *testing.T) {
	server, _ := toolProxy(t, &fakeRegistry{}, false)
	code, body := do(t, http.MethodGet, server.URL+"/v1/tools", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, body)
	}
	if !strings.Contains(body, `"data":[]`) {
		t.Fatalf("body = %s, want an empty array", body)
	}
}

func TestListToolsWhenRegistryAbsent(t *testing.T) {
	sink := &fakeToolSink{}
	proxy := gateway.New(&fakeConfig{token: "key"}, nil, nil)
	proxy.ToolLogs = sink
	server := httptest.NewServer(proxy)
	defer server.Close()

	code, body := do(t, http.MethodGet, server.URL+"/v1/tools", "")
	if code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 when tools are unconfigured: %s", code, body)
	}
}

func TestInvokeToolReturnsResult(t *testing.T) {
	registry := &fakeRegistry{result: gateway.ToolResult{Result: map[string]any{"temp": 21}}}
	server, sink := toolProxy(t, registry, true)

	code, body := do(t, http.MethodPost, server.URL+"/v1/tools/weather/invoke", `{"arguments":{"city":"Oslo"}}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, body)
	}
	if registry.gotArgs != `{"city":"Oslo"}` {
		t.Fatalf("arguments = %s", registry.gotArgs)
	}

	var out struct {
		Object  string         `json:"object"`
		Tool    string         `json:"tool"`
		IsError bool           `json:"is_error"`
		Result  map[string]any `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, body)
	}
	if out.Object != "tool.result" || out.Tool != "weather" || out.IsError {
		t.Fatalf("response = %+v", out)
	}
	if out.Result["temp"] != float64(21) {
		t.Fatalf("result = %#v", out.Result)
	}

	if len(sink.records) != 1 {
		t.Fatalf("tool logs = %d, want 1", len(sink.records))
	}
	rec := sink.records[0]
	if rec.Outcome != gateway.ToolOutcomeCompleted || rec.Status != http.StatusOK {
		t.Fatalf("record = %+v", rec)
	}
	if rec.Transport != gateway.TransportREST {
		t.Fatalf("transport = %q, want %q", rec.Transport, gateway.TransportREST)
	}
	if rec.Args != `{"city":"Oslo"}` || !strings.Contains(rec.Result, "temp") {
		t.Fatalf("captured args=%q result=%q", rec.Args, rec.Result)
	}
}

func TestInvokeToolFailureIsStillTwoHundred(t *testing.T) {
	registry := &fakeRegistry{result: gateway.ToolResult{Result: "upstream said 503", IsError: true}}
	server, sink := toolProxy(t, registry, true)

	code, body := do(t, http.MethodPost, server.URL+"/v1/tools/weather/invoke", `{"arguments":{}}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a tool-level failure: %s", code, body)
	}
	if !strings.Contains(body, `"is_error":true`) || !strings.Contains(body, "upstream said 503") {
		t.Fatalf("body = %s", body)
	}
	if sink.records[0].Outcome != gateway.ToolOutcomeToolError {
		t.Fatalf("outcome = %q, want tool_error", sink.records[0].Outcome)
	}
	if sink.records[0].Error == "" {
		t.Fatal("a failed tool should record why")
	}
}

func TestInvokeUnknownToolIsNotFound(t *testing.T) {
	registry := &fakeRegistry{err: fmt.Errorf("%w: %q", gateway.ErrUnknownTool, "nope")}
	server, sink := toolProxy(t, registry, false)

	code, body := do(t, http.MethodPost, server.URL+"/v1/tools/nope/invoke", `{"arguments":{}}`)
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", code, body)
	}
	if !strings.Contains(body, `"unknown_tool"`) {
		t.Fatalf("body = %s", body)
	}
	if len(sink.records) != 1 || sink.records[0].Outcome != gateway.ToolOutcomeRejected {
		t.Fatalf("records = %+v", sink.records)
	}
	if sink.records[0].Status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", sink.records[0].Status)
	}
}

func TestInvokeForbiddenTool(t *testing.T) {
	registry := &fakeRegistry{err: gateway.ErrForbidden}
	server, _ := toolProxy(t, registry, false)
	code, body := do(t, http.MethodPost, server.URL+"/v1/tools/weather/invoke", `{"arguments":{}}`)
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", code, body)
	}
}

func TestInvokeGatewayFailureIsFiveHundred(t *testing.T) {
	registry := &fakeRegistry{err: errors.New("pool exploded")}
	server, sink := toolProxy(t, registry, false)
	code, body := do(t, http.MethodPost, server.URL+"/v1/tools/weather/invoke", `{"arguments":{}}`)
	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", code, body)
	}
	if sink.records[0].Outcome != gateway.ToolOutcomeGatewayError {
		t.Fatalf("outcome = %q", sink.records[0].Outcome)
	}
}

func TestInvokeRejectsMalformedBody(t *testing.T) {
	registry := &fakeRegistry{result: gateway.ToolResult{Result: "unused"}}
	server, sink := toolProxy(t, registry, false)

	for _, body := range []string{`not json`, `{"arguments": }`, `[1,2,3]`} {
		code, _ := do(t, http.MethodPost, server.URL+"/v1/tools/weather/invoke", body)
		if code != http.StatusBadRequest {
			t.Fatalf("body %s: status = %d, want 400", body, code)
		}
	}
	if registry.calls != 0 {
		t.Fatalf("registry was called %d times for malformed bodies", registry.calls)
	}
	if len(sink.records) != 0 {
		t.Fatalf("malformed bodies were logged: %+v", sink.records)
	}
}

func TestInvokeDefaultsMissingArgumentsToEmptyObject(t *testing.T) {
	registry := &fakeRegistry{result: gateway.ToolResult{Result: "pong"}}
	server, _ := toolProxy(t, registry, false)

	for _, body := range []string{"", "{}", `{"arguments":null}`, `null`} {
		registry.gotArgs = ""
		code, resp := do(t, http.MethodPost, server.URL+"/v1/tools/ping/invoke", body)
		if code != http.StatusOK {
			t.Fatalf("body %q: status = %d: %s", body, code, resp)
		}
		if registry.gotArgs != `{}` {
			t.Fatalf("body %q: arguments = %s, want {}", body, registry.gotArgs)
		}
	}
}

func TestInvokeOmitsPayloadsWhenCaptureIsOff(t *testing.T) {
	registry := &fakeRegistry{result: gateway.ToolResult{Result: map[string]any{"a": 1}}}
	server, sink := toolProxy(t, registry, false)

	code, _ := do(t, http.MethodPost, server.URL+"/v1/tools/weather/invoke", `{"arguments":{"city":"Oslo"}}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	rec := sink.records[0]
	if rec.Args != "" || rec.Result != "" {
		t.Fatalf("captured args=%q result=%q with capture off", rec.Args, rec.Result)
	}
	if rec.Outcome != gateway.ToolOutcomeCompleted || rec.Tool != "weather" {
		t.Fatalf("record = %+v", rec)
	}
}

func TestInvokeTruncatesCapturedPayloads(t *testing.T) {
	huge := strings.Repeat("x", 300<<10)
	registry := &fakeRegistry{result: gateway.ToolResult{Result: huge}}
	server, sink := toolProxy(t, registry, true)

	body, err := json.Marshal(map[string]any{"arguments": map[string]any{"blob": huge}})
	if err != nil {
		t.Fatal(err)
	}
	code, resp := do(t, http.MethodPost, server.URL+"/v1/tools/weather/invoke", string(body))
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, resp)
	}
	rec := sink.records[0]
	if !rec.Truncated {
		t.Fatal("expected the capture to be marked truncated")
	}
	if len(rec.Args) > 256<<10 || len(rec.Result) > 256<<10 {
		t.Fatalf("captured args=%d result=%d, want each capped at 256KB", len(rec.Args), len(rec.Result))
	}
	if !strings.Contains(resp, huge[:1000]) {
		t.Fatal("the client response was truncated, but only the capture should be")
	}
}

func TestInvokeRequiresPostAndKnownShape(t *testing.T) {
	server, _ := toolProxy(t, &fakeRegistry{result: gateway.ToolResult{}}, false)

	if code, _ := do(t, http.MethodGet, server.URL+"/v1/tools/weather/invoke", ""); code != http.StatusNotFound {
		t.Fatalf("GET invoke status = %d, want 404", code)
	}
	if code, _ := do(t, http.MethodPost, server.URL+"/v1/tools", ""); code != http.StatusNotFound {
		t.Fatalf("POST /v1/tools status = %d, want 404", code)
	}
	if code, _ := do(t, http.MethodPost, server.URL+"/v1/tools//invoke", `{"arguments":{}}`); code != http.StatusNotFound {
		t.Fatalf("empty tool name status = %d, want 404", code)
	}
}

func TestInvokeSecondToolName(t *testing.T) {
	registry := &fakeRegistry{result: gateway.ToolResult{Result: "file contents"}}
	server, _ := toolProxy(t, registry, false)

	code, body := do(t, http.MethodPost, server.URL+"/v1/tools/reads_file/invoke", `{"arguments":{"path":"a.txt"}}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, body)
	}
	if !strings.Contains(body, "file contents") {
		t.Fatalf("body = %s", body)
	}
}

func TestToolsRequireAuthOnInvoke(t *testing.T) {
	server, sink := toolProxy(t, &fakeRegistry{result: gateway.ToolResult{}}, false)
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/tools/weather/invoke", strings.NewReader(`{"arguments":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if len(sink.records) != 0 {
		t.Fatalf("unauthenticated invoke was logged: %+v", sink.records)
	}
}
