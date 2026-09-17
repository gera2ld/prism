package gateway_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gera2ld/prism/internal/gateway"
)

type fakeConfig struct {
	token     string
	key       gateway.Key
	targets   []gateway.Target
	transform func([]byte) ([]byte, string, error)
}

func (f *fakeConfig) Authenticate(_ context.Context, presented string) (gateway.Key, error) {
	if presented != f.token {
		return gateway.Key{}, gateway.ErrUnauthorized
	}
	key := f.key
	if key.ID == "" {
		key.ID = "key1"
	}
	return key, nil
}

func (f *fakeConfig) Resolve(_ context.Context, alias string) ([]gateway.Target, error) {
	return f.targets, nil
}

func (f *fakeConfig) Models(_ context.Context) ([]string, error) { return []string{"alias"}, nil }

func (f *fakeConfig) Transform(_ context.Context, _ gateway.Target, body []byte) ([]byte, string, error) {
	if f.transform != nil {
		return f.transform(body)
	}
	return body, "", nil
}

type sliceSink struct{ records []gateway.Record }

func (s *sliceSink) Write(_ context.Context, r gateway.Record) error {
	s.records = append(s.records, r)
	return nil
}

func newUpstream(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model         string `json:"model"`
			Stream        bool   `json:"stream"`
			StreamOptions *struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		_ = json.Unmarshal(body, &req)
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-key" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"bad key"}`))
			return
		}
		if req.Model != "gpt-x" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.Stream && (req.StreamOptions == nil || !req.StreamOptions.IncludeUsage) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"missing stream_options.include_usage"}`))
			return
		}
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n"))
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":3,\"total_tokens\":13,\"prompt_tokens_details\":{\"cached_tokens\":4}}}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "c2", "object": "chat.completion",
			"choices": []any{map[string]any{
				"finish_reason": "length",
				"message":       map[string]any{"role": "assistant", "content": "hi"},
			}},
			"usage": map[string]any{
				"prompt_tokens": 7, "completion_tokens": 2, "total_tokens": 9,
				"prompt_tokens_details": map[string]any{"cached_tokens": 2},
			},
		})
	})
	return httptest.NewServer(mux)
}

func TestStreamingRelaysPartialLinesUnchanged(t *testing.T) {
	first := "data: {\"choices\":"
	rest := "[]}\r\n\r\ndata: [DONE]\r\n\r\n"
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, first)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, rest)
	}))
	defer upstream.Close()
	proxy := gateway.New(&fakeConfig{token: "key", targets: []gateway.Target{{BaseURL: upstream.URL, Model: "x"}}}, nil, nil)
	server := httptest.NewServer(proxy)
	defer server.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"alias","stream":true}`))
	req.Header.Set("Authorization", "Bearer key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, len(first))
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("partial line was buffered: %v", err)
	}
	if string(buf) != first {
		t.Fatalf("partial bytes changed: %q", buf)
	}
	release <- struct{}{}
	remaining, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(remaining) != rest {
		t.Fatalf("SSE framing changed: %q", remaining)
	}
}

func TestProxyChatAndModels(t *testing.T) {

	upstream := newUpstream(t)
	defer upstream.Close()

	sink := &sliceSink{}
	proxy := gateway.New(&fakeConfig{
		token:   "client-key",
		targets: []gateway.Target{{ProviderID: "p1", ProviderName: "prov", BaseURL: upstream.URL + "/v1", APIKey: "upstream-key", Model: "gpt-x"}},
	}, sink, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer client-key")
	proxy.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("non-streaming status = %d, body %s", rec.Code, rec.Body.String())
	}
	var completion struct {
		Usage struct {
			Total int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &completion); err != nil {
		t.Fatal(err)
	}
	if completion.Usage.Total != 9 {
		t.Fatalf("expected total 9, got %d", completion.Usage.Total)
	}
	if len(sink.records) != 1 || sink.records[0].Usage.TotalTokens == nil || *sink.records[0].Usage.TotalTokens != 9 {
		t.Fatalf("expected logged usage 9, got %+v", sink.records)
	}
	if sink.records[0].Usage.CachedTokens == nil || *sink.records[0].Usage.CachedTokens != 2 {
		t.Fatalf("expected logged cached tokens 2, got %+v", sink.records[0].Usage)
	}
	if sink.records[0].FinishReason == nil || *sink.records[0].FinishReason != "length" {
		t.Fatalf("expected logged finish reason length, got %+v", sink.records[0])
	}
	if sink.records[0].Status != 200 || sink.records[0].Stream {
		t.Fatalf("unexpected record %+v", sink.records[0])
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer client-key")
	proxy.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("streaming status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "data: ") || !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("stream body missing SSE framing: %q", rec.Body.String())
	}
	if len(sink.records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(sink.records))
	}
	streamRec := sink.records[1]
	if !streamRec.Stream || streamRec.TTFTMS == nil {
		t.Fatalf("expected stream record with TTFT, got %+v", streamRec)
	}
	if streamRec.Usage.TotalTokens == nil || *streamRec.Usage.TotalTokens != 13 {
		t.Fatalf("expected streamed usage 13, got %+v", streamRec.Usage)
	}
	if streamRec.Usage.CachedTokens == nil || *streamRec.Usage.CachedTokens != 4 {
		t.Fatalf("expected streamed cached tokens 4, got %+v", streamRec.Usage)
	}
	if streamRec.FinishReason == nil || *streamRec.FinishReason != "stop" {
		t.Fatalf("expected streamed finish reason stop, got %+v", streamRec)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer client-key")
	proxy.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"alias"`) {
		t.Fatalf("models response = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"owned_by":"prov"`) {
		t.Fatalf("models owned_by should name the winning provider: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	proxy.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestProxySlashAlias(t *testing.T) {
	var seen string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var in struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &in)
		seen = in.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	sink := &sliceSink{}
	proxy := gateway.New(&fakeConfig{
		token:   "k",
		targets: []gateway.Target{{ProviderID: "p1", ProviderName: "prov", BaseURL: upstream.URL, APIKey: "", Model: "qwen/qwen3-30b:free"}},
	}, sink, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen/qwen3:free","messages":[]}`))
	req.Header.Set("Authorization", "Bearer k")
	proxy.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if seen != "qwen/qwen3-30b:free" {
		t.Fatalf("upstream got model %q", seen)
	}
	if len(sink.records) != 1 || sink.records[0].Alias != "qwen/qwen3:free" {
		t.Fatalf("expected logged slash alias, got %+v", sink.records)
	}
}

func TestCaptureFollowsGlobal(t *testing.T) {
	upstream := newUpstream(t)
	defer upstream.Close()
	targets := []gateway.Target{{ProviderID: "p1", ProviderName: "prov", BaseURL: upstream.URL + "/v1", APIKey: "upstream-key", Model: "gpt-x"}}
	body := `{"model":"alias","messages":[{"role":"user","content":"hello"}]}`
	post := func(capture bool) (int, []gateway.Record) {
		sink := &sliceSink{}
		proxy := gateway.New(&fakeConfig{token: "k", targets: targets}, sink, nil)
		proxy.Capture = func() bool { return capture }
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer k")
		proxy.ServeHTTP(rec, req)
		return rec.Code, sink.records
	}

	if code, records := post(true); code != 200 || len(records) != 1 || records[0].Bodies == nil {
		t.Fatalf("expected captured bodies with global on, got code=%d rec=%+v", code, records)
	}
	if code, records := post(false); code != 200 || len(records) != 1 || records[0].Bodies != nil {
		t.Fatalf("expected no bodies with global off, got code=%d rec=%+v", code, records)
	}
}

func TestProxyAppliesTransformer(t *testing.T) {
	var seen string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	sink := &sliceSink{}
	cfg := &fakeConfig{
		token:   "k",
		targets: []gateway.Target{{ProviderID: "p1", ProviderName: "prov", BaseURL: upstream.URL, APIKey: "", Model: "gpt-x"}},
		transform: func(body []byte) ([]byte, string, error) {
			var in map[string]any
			if err := json.Unmarshal(body, &in); err != nil {
				return nil, "", err
			}
			in["shaped"] = true
			out, _ := json.Marshal(in)
			return out, "strip-params", nil
		},
	}
	proxy := gateway.New(cfg, sink, nil)
	proxy.Capture = func() bool { return true }
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[]}`))
	req.Header.Set("Authorization", "Bearer k")
	proxy.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(seen, `"shaped":true`) || !strings.Contains(seen, `"model":"gpt-x"`) {
		t.Fatalf("upstream got unshaped body: %s", seen)
	}
	if len(sink.records) != 1 || sink.records[0].Transformer != "strip-params" {
		t.Fatalf("expected logged transformer name, got %+v", sink.records)
	}
	// The logged request is the as-sent (shaped) body, not the client original.
	if bodies := sink.records[0].Bodies; bodies == nil || bodies.Request != seen {
		t.Fatalf("expected captured as-sent body %q, got %+v", seen, bodies)
	}
}

func TestProxyTransformErrorFailsClosed(t *testing.T) {
	upstream := newUpstream(t)
	defer upstream.Close()

	sink := &sliceSink{}
	cfg := &fakeConfig{
		token:   "k",
		targets: []gateway.Target{{ProviderID: "p1", ProviderName: "prov", BaseURL: upstream.URL + "/v1", APIKey: "upstream-key", Model: "gpt-x"}},
		transform: func([]byte) ([]byte, string, error) {
			return nil, "broken", errors.New("boom")
		},
	}
	proxy := gateway.New(cfg, sink, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[]}`))
	req.Header.Set("Authorization", "Bearer k")
	proxy.ServeHTTP(rec, req)
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "transform_error") {
		t.Fatalf("expected 500 transform_error, got %d %s", rec.Code, rec.Body.String())
	}
	if len(sink.records) != 1 || sink.records[0].Status != 500 || !strings.Contains(sink.records[0].Error, "boom") {
		t.Fatalf("expected logged 500 with cause, got %+v", sink.records)
	}
}

func policyKey(t *testing.T, alias, provider, model string) gateway.Key {
	t.Helper()
	a, p, m, err := gateway.CompilePolicy(alias, provider, model)
	if err != nil {
		t.Fatal(err)
	}
	return gateway.Key{ID: "key1", Name: "laptop", AliasPattern: a, ProviderPattern: p, ModelPattern: m}
}

func TestProxyDeniesDisallowedModel(t *testing.T) {
	upstream := newUpstream(t)
	defer upstream.Close()

	sink := &sliceSink{}
	proxy := gateway.New(&fakeConfig{
		token:   "k",
		key:     policyKey(t, `^other`, "", ""),
		targets: []gateway.Target{{ProviderID: "p1", ProviderName: "prov", BaseURL: upstream.URL + "/v1", APIKey: "upstream-key", Model: "gpt-x"}},
	}, sink, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[]}`))
	req.Header.Set("Authorization", "Bearer k")
	proxy.ServeHTTP(rec, req)
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "forbidden") {
		t.Fatalf("expected 403 forbidden, got %d %s", rec.Code, rec.Body.String())
	}
	if len(sink.records) != 1 || sink.records[0].Status != 403 {
		t.Fatalf("expected logged 403, got %+v", sink.records)
	}
	denied := sink.records[0]
	if denied.ProviderName != "prov" || denied.UpstreamModel != "gpt-x" || denied.KeyName != "laptop" || denied.Alias != "alias" {
		t.Fatalf("denial must identify key/alias/blocked route, got %+v", denied)
	}
}

func TestProxyFallsThroughToAllowedRoute(t *testing.T) {
	var seen string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var in struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &in)
		seen = in.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	sink := &sliceSink{}
	proxy := gateway.New(&fakeConfig{
		token: "k",
		key:   policyKey(t, "", "", `mirror`),
		targets: []gateway.Target{
			{ProviderID: "p1", ProviderName: "prov", BaseURL: upstream.URL, APIKey: "", Model: "gpt-x"},
			{ProviderID: "p1", ProviderName: "prov", BaseURL: upstream.URL, APIKey: "", Model: "gpt-x-mirror"},
		},
	}, sink, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[]}`))
	req.Header.Set("Authorization", "Bearer k")
	proxy.ServeHTTP(rec, req)
	if rec.Code != 200 || seen != "gpt-x-mirror" {
		t.Fatalf("expected fallthrough to mirror route, got %d upstream=%q", rec.Code, seen)
	}
}

func TestProxyModelsRespectsPolicy(t *testing.T) {
	sink := &sliceSink{}
	proxy := gateway.New(&fakeConfig{token: "k", key: policyKey(t, `^other`, "", "")}, sink, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer k")
	proxy.ServeHTTP(rec, req)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), `"id":"alias"`) {
		t.Fatalf("disallowed alias must be hidden, got %d %s", rec.Code, rec.Body.String())
	}
}
