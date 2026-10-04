package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/gera2ld/prism/internal/gateway"
)

type fakeConfig struct {
	token     string
	key       gateway.Key
	targets   []gateway.Target
	image     []gateway.Target
	models    []string
	named     map[string]map[gateway.EndpointType][]gateway.Target
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

func (f *fakeConfig) Resolve(_ context.Context, endpoint gateway.EndpointType, alias string) ([]gateway.Target, error) {
	if f.named != nil {
		targets := f.named[alias][endpoint]
		if len(targets) == 0 {
			return nil, gateway.ErrUnknownModel
		}
		return targets, nil
	}
	if endpoint == gateway.EndpointImage {
		if len(f.image) == 0 {
			return nil, gateway.ErrUnknownModel
		}
		return f.image, nil
	}
	if len(f.targets) == 0 {
		return nil, gateway.ErrUnknownModel
	}
	return f.targets, nil
}

func (f *fakeConfig) Models(_ context.Context) ([]string, error) {
	if f.models != nil {
		return f.models, nil
	}
	if len(f.targets) > 0 || len(f.image) > 0 {
		return []string{"alias"}, nil
	}
	return nil, nil
}

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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type terminalBody struct {
	data []byte
	err  error
	sent bool
}

func (b *terminalBody) Read(p []byte) (int, error) {
	if b.sent {
		return 0, io.EOF
	}
	b.sent = true
	return copy(p, b.data), b.err
}

func (b *terminalBody) Close() error { return nil }

type failingResponseWriter struct {
	header   http.Header
	code     int
	writeErr error
	flushErr error
	flushAt  int
	flushes  int
	onWrite  func([]byte)
}

func newFailingResponseWriter(writeErr, flushErr error, flushAt int) *failingResponseWriter {
	return &failingResponseWriter{
		header:   make(http.Header),
		writeErr: writeErr,
		flushErr: flushErr,
		flushAt:  flushAt,
	}
}

func (w *failingResponseWriter) Header() http.Header { return w.header }

func (w *failingResponseWriter) WriteHeader(code int) { w.code = code }

func (w *failingResponseWriter) Write(p []byte) (int, error) {
	if w.onWrite != nil {
		w.onWrite(p)
	}
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return len(p), nil
}

func (w *failingResponseWriter) Flush() {}

func (w *failingResponseWriter) FlushError() error {
	w.flushes++
	if w.flushErr != nil && (w.flushAt == 0 || w.flushes == w.flushAt) {
		return w.flushErr
	}
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
	if sink.records[0].Status != 200 || sink.records[0].Stream || sink.records[0].Outcome != gateway.OutcomeCompleted {
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
	if streamRec.Outcome != gateway.OutcomeCompleted || streamRec.Error != "" {
		t.Fatalf("expected completed stream outcome, got %+v", streamRec)
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
	if !strings.Contains(rec.Body.String(), `"supported_endpoint_types":["openai"]`) {
		t.Fatalf("models should advertise openai endpoint types: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	proxy.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestProxyLogsClientCancellationRecord(t *testing.T) {
	started := make(chan struct{})
	sink := &sliceSink{}
	proxy := gateway.New(&fakeConfig{
		token:   "k",
		targets: []gateway.Target{{BaseURL: "http://upstream.test", Model: "gpt-x"}},
	}, sink, nil)
	proxy.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","stream":true}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer k")
	done := make(chan struct{})
	go func() {
		proxy.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("upstream request did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("proxy did not stop after client cancellation")
	}

	if len(sink.records) != 1 {
		t.Fatalf("expected one record, got %d", len(sink.records))
	}
	got := sink.records[0]
	if got.Outcome != gateway.OutcomeClientDisconnected || got.Status != 0 || !strings.Contains(got.Error, "context canceled") {
		t.Fatalf("unexpected cancellation record %+v", got)
	}
	if got.FinishReason != nil {
		t.Fatalf("client cancellation must not synthesize finish reason: %+v", got)
	}
}

func TestProxyLogsUpstreamStreamDisconnects(t *testing.T) {
	tests := []struct {
		name      string
		readErr   error
		wantError string
	}{
		{name: "clean EOF", readErr: io.EOF, wantError: "[DONE]"},
		{name: "transport error", readErr: io.ErrUnexpectedEOF, wantError: "unexpected EOF"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := &sliceSink{}
			proxy := gateway.New(&fakeConfig{
				token:   "k",
				targets: []gateway.Target{{BaseURL: "http://upstream.test", Model: "gpt-x"}},
			}, sink, nil)
			proxy.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
					Body: &terminalBody{
						data: []byte("data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\n"),
						err:  tt.readErr,
					},
				}, nil
			})}

			writer := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","stream":true}`))
			req.Header.Set("Authorization", "Bearer k")
			proxy.ServeHTTP(writer, req)

			if len(sink.records) != 1 {
				t.Fatalf("expected one record, got %d", len(sink.records))
			}
			got := sink.records[0]
			if got.Outcome != gateway.OutcomeUpstreamDisconnected || got.Status != http.StatusOK || !strings.Contains(got.Error, tt.wantError) {
				t.Fatalf("unexpected upstream disconnect record %+v", got)
			}
			if got.FinishReason == nil || *got.FinishReason != "stop" {
				t.Fatalf("expected provider finish reason to be preserved, got %+v", got)
			}
		})
	}
}

func TestProxyLogsProviderStreamError(t *testing.T) {
	sink := &sliceSink{}
	proxy := gateway.New(&fakeConfig{
		token:   "k",
		targets: []gateway.Target{{BaseURL: "http://upstream.test", Model: "gpt-x"}},
	}, sink, nil)
	proxy.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body: &terminalBody{
				data: []byte("data: {\"error\":{\"message\":\"generation failed\"}}\n\n"),
				err:  io.EOF,
			},
		}, nil
	})}

	writer := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","stream":true}`))
	req.Header.Set("Authorization", "Bearer k")
	proxy.ServeHTTP(writer, req)

	if len(sink.records) != 1 {
		t.Fatalf("expected one record, got %d", len(sink.records))
	}
	got := sink.records[0]
	if got.Outcome != gateway.OutcomeUpstreamError || got.Error != "stream error: generation failed" {
		t.Fatalf("unexpected provider stream error record %+v", got)
	}
	if got.FinishReason != nil {
		t.Fatalf("provider error must not synthesize finish reason: %+v", got)
	}
}

// Write and Flush are separate branches in the stream loop with separate error
// messages, so both are exercised here rather than assuming one covers the other.
func TestProxyLogsClientStreamFailures(t *testing.T) {
	cases := []struct {
		name       string
		chunk      string
		writer     *failingResponseWriter
		wantErr    string
		wantReason string
	}{
		{
			name:    "write fails",
			chunk:   `data: {"choices":[{"finish_reason":"stop"}]}`,
			writer:  newFailingResponseWriter(io.ErrClosedPipe, nil, 0),
			wantErr: "closed pipe",
			// The provider's finish reason arrived before the write failed, so it
			// is worth keeping on the record.
			wantReason: "stop",
		},
		{
			name:    "flush fails",
			chunk:   `data: {"choices":[{"delta":{"content":"hi"}}]}`,
			writer:  newFailingResponseWriter(nil, errors.New("connection reset by peer"), 2),
			wantErr: "connection reset by peer",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &sliceSink{}
			proxy := gateway.New(&fakeConfig{
				token:   "k",
				targets: []gateway.Target{{BaseURL: "http://upstream.test", Model: "gpt-x"}},
			}, sink, nil)
			proxy.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
					Body: &terminalBody{
						data: []byte(tc.chunk + "\n\n"),
						err:  io.EOF,
					},
				}, nil
			})}

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","stream":true}`))
			req.Header.Set("Authorization", "Bearer k")
			proxy.ServeHTTP(tc.writer, req)

			if len(sink.records) != 1 {
				t.Fatalf("expected one record, got %d", len(sink.records))
			}
			got := sink.records[0]
			if got.Outcome != gateway.OutcomeClientDisconnected || !strings.Contains(got.Error, tc.wantErr) {
				t.Fatalf("record = %+v, want a disconnect mentioning %q", got, tc.wantErr)
			}
			if tc.wantReason != "" {
				if got.Status != http.StatusOK {
					t.Fatalf("status = %d, want 200", got.Status)
				}
				if got.FinishReason == nil || *got.FinishReason != tc.wantReason {
					t.Fatalf("finish reason = %v, want %q", got.FinishReason, tc.wantReason)
				}
			}
		})
	}
}

func TestProxyCompletionWinsAfterDoneDelivered(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := &sliceSink{}
	proxy := gateway.New(&fakeConfig{
		token:   "k",
		targets: []gateway.Target{{BaseURL: "http://upstream.test", Model: "gpt-x"}},
	}, sink, nil)
	proxy.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body: &terminalBody{
				data: []byte("data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"),
				err:  io.EOF,
			},
		}, nil
	})}

	writer := newFailingResponseWriter(nil, nil, 0)
	writer.onWrite = func(p []byte) {
		if strings.Contains(string(p), "[DONE]") {
			cancel()
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","stream":true}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer k")
	proxy.ServeHTTP(writer, req)

	if len(sink.records) != 1 {
		t.Fatalf("expected one record, got %d", len(sink.records))
	}
	got := sink.records[0]
	if got.Outcome != gateway.OutcomeCompleted || got.Error != "" {
		t.Fatalf("late client cancellation must not overwrite completion: %+v", got)
	}
}

func TestProxyLogsNonStreamingTransferFailures(t *testing.T) {
	tests := []struct {
		name        string
		readErr     error
		writeErr    error
		wantOutcome gateway.Outcome
		wantError   string
	}{
		{name: "upstream read", readErr: io.ErrUnexpectedEOF, wantOutcome: gateway.OutcomeUpstreamDisconnected, wantError: "unexpected EOF"},
		{name: "client write", writeErr: io.ErrClosedPipe, wantOutcome: gateway.OutcomeClientDisconnected, wantError: "closed pipe"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := &sliceSink{}
			proxy := gateway.New(&fakeConfig{
				token:   "k",
				targets: []gateway.Target{{BaseURL: "http://upstream.test", Model: "gpt-x"}},
			}, sink, nil)
			proxy.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body: &terminalBody{
						data: []byte(`{"choices":[{"finish_reason":"length"}]}`),
						err:  tt.readErr,
					},
				}, nil
			})}

			var writer http.ResponseWriter = httptest.NewRecorder()
			if tt.writeErr != nil {
				writer = newFailingResponseWriter(tt.writeErr, nil, 0)
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias"}`))
			req.Header.Set("Authorization", "Bearer k")
			proxy.ServeHTTP(writer, req)

			if len(sink.records) != 1 {
				t.Fatalf("expected one record, got %d", len(sink.records))
			}
			got := sink.records[0]
			if got.Outcome != tt.wantOutcome || !strings.Contains(got.Error, tt.wantError) {
				t.Fatalf("unexpected non-streaming failure record %+v", got)
			}
			if got.FinishReason == nil || *got.FinishReason != "length" {
				t.Fatalf("expected provider finish reason to be preserved, got %+v", got)
			}
		})
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
	if len(sink.records) != 1 || sink.records[0].Status != 500 || sink.records[0].Outcome != gateway.OutcomeGatewayError || !strings.Contains(sink.records[0].Error, "boom") {
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
	if denied.Outcome != gateway.OutcomeRejected {
		t.Fatalf("expected rejected outcome, got %+v", denied)
	}
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

func newImageUpstream(t *testing.T, seen *string, key string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/images" {
			t.Errorf("upstream path = %q, want /images", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
		}
		_ = json.Unmarshal(body, &req)
		*seen = req.Model
		if got := r.Header.Get("Authorization"); got != "Bearer "+key {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"bad key"}`))
			return
		}
		if req.Model != "img-x" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"bad model"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"created": 1748372400,
			"data": []any{map[string]any{
				"b64_json":   "aW1hZ2UtYnl0ZXM=",
				"media_type": "image/png",
			}},
			"usage": map[string]any{
				"prompt_tokens": 12, "completion_tokens": 4163, "total_tokens": 4175, "cost": 0.04,
			},
		})
	}))
}

func TestProxyImagesPassthrough(t *testing.T) {
	var seen string
	upstream := newImageUpstream(t, &seen, "upstream-key")
	defer upstream.Close()

	sink := &sliceSink{}
	proxy := gateway.New(&fakeConfig{
		token: "client-key",
		image: []gateway.Target{{ProviderID: "p1", ProviderName: "prov", BaseURL: upstream.URL, APIKey: "upstream-key", Model: "img-x"}},
	}, sink, nil)
	proxy.Capture = func() bool { return true }

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/images", strings.NewReader(`{"model":"alias","prompt":"a red panda astronaut"}`))
	req.Header.Set("Authorization", "Bearer client-key")
	proxy.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if seen != "img-x" {
		t.Fatalf("upstream got model %q, want rewritten img-x", seen)
	}
	var resp struct {
		Data []struct {
			B64 string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data) != 1 || resp.Data[0].B64 != "aW1hZ2UtYnl0ZXM=" {
		t.Fatalf("unexpected image body %s", rec.Body.String())
	}
	if len(sink.records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(sink.records))
	}
	got := sink.records[0]
	if got.Endpoint != gateway.EndpointImage || got.Alias != "alias" {
		t.Fatalf("unexpected endpoint/alias %+v", got)
	}
	if got.Usage.TotalTokens == nil || *got.Usage.TotalTokens != 4175 {
		t.Fatalf("expected logged total 4175, got %+v", got.Usage)
	}
	if got.Usage.Cost == nil || *got.Usage.Cost != 0.04 {
		t.Fatalf("expected logged cost 0.04, got %+v", got.Usage)
	}
	if len(got.Images) != 1 || string(got.Images[0].Data) != "image-bytes" {
		t.Fatalf("expected one decoded image file, got %+v", got.Images)
	}
	if got.Images[0].Name != "alias-0.png" || got.Images[0].MediaType != "image/png" {
		t.Fatalf("unexpected image file %+v", got.Images[0])
	}
	// Capture holds the redacted response: structure intact, payload blank.
	if got.Bodies == nil || strings.Contains(got.Bodies.Response, "aW1hZ2UtYnl0ZXM=") {
		t.Fatalf("captured response should redact saved base64, got %+v", got.Bodies)
	}
	if !strings.Contains(got.Bodies.Response, `"b64_json":""`) {
		t.Fatalf("captured response should keep the blanked key, got %s", got.Bodies.Response)
	}
	if got.Status != 200 || got.Outcome != gateway.OutcomeCompleted || got.Stream || got.TTFTMS != nil {
		t.Fatalf("unexpected image record %+v", got)
	}
	if got.FinishReason != nil {
		t.Fatalf("images have no finish reason, got %+v", got)
	}
}

func TestProxyImagesValidation(t *testing.T) {
	proxy := gateway.New(&fakeConfig{
		token: "k",
		image: []gateway.Target{{BaseURL: "http://upstream.test", Model: "img-x"}},
	}, &sliceSink{}, nil)
	for name, body := range map[string]string{
		"missing prompt": `{"model":"alias"}`,
		"missing model":  `{"prompt":"hi"}`,
		"not json":       `{{{`,
		"stream":         `{"model":"alias","prompt":"hi","stream":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/images", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer k")
			proxy.ServeHTTP(rec, req)
			if rec.Code != 400 {
				t.Fatalf("expected 400, got %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestProxyImagesAuthAndRouting(t *testing.T) {
	var seen string
	upstream := newImageUpstream(t, &seen, "upstream-key")
	defer upstream.Close()
	target := gateway.Target{ProviderID: "p1", ProviderName: "prov", BaseURL: upstream.URL, APIKey: "upstream-key", Model: "img-x"}

	t.Run("unauthorized", func(t *testing.T) {
		proxy := gateway.New(&fakeConfig{token: "k", image: []gateway.Target{target}}, &sliceSink{}, nil)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/images", strings.NewReader(`{"model":"alias","prompt":"hi"}`))
		proxy.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Fatalf("expected 401, got %d", rec.Code)
		}
	})

	t.Run("unknown model", func(t *testing.T) {
		sink := &sliceSink{}
		proxy := gateway.New(&fakeConfig{token: "k"}, sink, nil)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/images", strings.NewReader(`{"model":"alias","prompt":"hi"}`))
		req.Header.Set("Authorization", "Bearer k")
		proxy.ServeHTTP(rec, req)
		if rec.Code != 404 {
			t.Fatalf("expected 404, got %d %s", rec.Code, rec.Body.String())
		}
		if len(sink.records) != 0 {
			t.Fatalf("unknown models are not logged, got %+v", sink.records)
		}
	})

	t.Run("chat-only alias is unknown to images", func(t *testing.T) {
		sink := &sliceSink{}
		proxy := gateway.New(&fakeConfig{
			token:   "k",
			targets: []gateway.Target{{ProviderID: "p1", ProviderName: "prov", BaseURL: upstream.URL, Model: "gpt-x"}},
		}, sink, nil)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/images", strings.NewReader(`{"model":"alias","prompt":"hi"}`))
		req.Header.Set("Authorization", "Bearer k")
		proxy.ServeHTTP(rec, req)
		if rec.Code != 404 {
			t.Fatalf("expected 404 for a chat-only alias, got %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("denied key", func(t *testing.T) {
		sink := &sliceSink{}
		proxy := gateway.New(&fakeConfig{token: "k", key: policyKey(t, `^other`, "", ""), image: []gateway.Target{target}}, sink, nil)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/images", strings.NewReader(`{"model":"alias","prompt":"hi"}`))
		req.Header.Set("Authorization", "Bearer k")
		proxy.ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Fatalf("expected 403, got %d %s", rec.Code, rec.Body.String())
		}
		if len(sink.records) != 1 || sink.records[0].Outcome != gateway.OutcomeRejected || sink.records[0].Endpoint != gateway.EndpointImage {
			t.Fatalf("expected logged image rejection, got %+v", sink.records)
		}
	})
}

func TestProxyImagesUpstreamError(t *testing.T) {
	sink := &sliceSink{}
	proxy := gateway.New(&fakeConfig{
		token: "k",
		image: []gateway.Target{{ProviderID: "p1", ProviderName: "prov", BaseURL: "http://upstream.test", Model: "img-x"}},
	}, sink, nil)
	proxy.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":"bad prompt"}`)),
		}, nil
	})}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/images", strings.NewReader(`{"model":"alias","prompt":"hi"}`))
	req.Header.Set("Authorization", "Bearer k")
	proxy.ServeHTTP(rec, req)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "bad prompt") {
		t.Fatalf("expected upstream error relay, got %d %s", rec.Code, rec.Body.String())
	}
	if len(sink.records) != 1 || sink.records[0].Outcome != gateway.OutcomeUpstreamError || sink.records[0].Status != 400 {
		t.Fatalf("unexpected record %+v", sink.records)
	}
}

func TestProxyImagesAppliesTransformer(t *testing.T) {
	var seen string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[],"usage":{"total_tokens":1}}`))
	}))
	defer upstream.Close()

	sink := &sliceSink{}
	cfg := &fakeConfig{
		token: "k",
		image: []gateway.Target{{ProviderID: "p1", ProviderName: "prov", BaseURL: upstream.URL, Model: "img-x"}},
		transform: func(body []byte) ([]byte, string, error) {
			var in map[string]any
			if err := json.Unmarshal(body, &in); err != nil {
				return nil, "", err
			}
			in["quality"] = "high"
			out, _ := json.Marshal(in)
			return out, "hd", nil
		},
	}
	proxy := gateway.New(cfg, sink, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/images", strings.NewReader(`{"model":"alias","prompt":"hi"}`))
	req.Header.Set("Authorization", "Bearer k")
	proxy.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(seen, `"quality":"high"`) || !strings.Contains(seen, `"model":"img-x"`) {
		t.Fatalf("upstream got unshaped body: %s", seen)
	}
	if len(sink.records) != 1 || sink.records[0].Transformer != "hd" {
		t.Fatalf("expected logged transformer name, got %+v", sink.records)
	}
}

func TestProxyModelsAdvertisesEndpointTypes(t *testing.T) {
	chatOnly := gateway.Target{ProviderID: "p1", ProviderName: "prov", BaseURL: "http://x", Model: "gpt-x"}
	imgOnly := gateway.Target{ProviderID: "p1", ProviderName: "prov", BaseURL: "http://x", Model: "img-x"}
	proxy := gateway.New(&fakeConfig{
		token:  "k",
		models: []string{"both", "chat-only", "image-only"},
		named: map[string]map[gateway.EndpointType][]gateway.Target{
			"both":       {gateway.EndpointChat: {chatOnly}, gateway.EndpointImage: {imgOnly}},
			"chat-only":  {gateway.EndpointChat: {chatOnly}},
			"image-only": {gateway.EndpointImage: {imgOnly}},
		},
	}, &sliceSink{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer k")
	proxy.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"id":"both"`, `"supported_endpoint_types":["openai","image-generation"]`,
		`"id":"chat-only"`, `"supported_endpoint_types":["openai"]`,
		`"id":"image-only"`, `"supported_endpoint_types":["image-generation"]`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("models response missing %s: %s", want, body)
		}
	}
}

func TestProxyImageGenerationsPassthrough(t *testing.T) {
	var seenPath, seenModel, seenSize string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model          string `json:"model"`
			Prompt         string `json:"prompt"`
			Size           string `json:"size"`
			ResponseFormat string `json:"response_format"`
		}
		_ = json.Unmarshal(body, &req)
		seenModel, seenSize = req.Model, req.Size
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if req.Model != "dall-e-3" || req.Prompt == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"created": 1748372400,
			"data": []any{map[string]any{
				"url":            "https://images.example/1.png",
				"revised_prompt": "a revised panda",
			}},
		})
	}))
	defer upstream.Close()

	sink := &sliceSink{}
	proxy := gateway.New(&fakeConfig{
		token: "client-key",
		image: []gateway.Target{{ProviderID: "p1", ProviderName: "prov", BaseURL: upstream.URL, APIKey: "upstream-key", Model: "dall-e-3"}},
	}, sink, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(
		`{"model":"alias","prompt":"a red panda","n":1,"size":"1024x1024","response_format":"url"}`))
	req.Header.Set("Authorization", "Bearer client-key")
	proxy.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if seenPath != "/images/generations" {
		t.Fatalf("upstream path = %q, want /images/generations", seenPath)
	}
	if seenModel != "dall-e-3" || seenSize != "1024x1024" {
		t.Fatalf("upstream got model %q size %q", seenModel, seenSize)
	}
	if !strings.Contains(rec.Body.String(), "https://images.example/1.png") {
		t.Fatalf("unexpected body %s", rec.Body.String())
	}
	if len(sink.records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(sink.records))
	}
	got := sink.records[0]
	if got.Endpoint != gateway.EndpointImage || got.Outcome != gateway.OutcomeCompleted || got.Status != 200 {
		t.Fatalf("unexpected record %+v", got)
	}
}

func TestProxyImageGenerationsValidation(t *testing.T) {
	proxy := gateway.New(&fakeConfig{
		token: "k",
		image: []gateway.Target{{BaseURL: "http://upstream.test", Model: "img-x"}},
	}, &sliceSink{}, nil)
	for name, body := range map[string]string{
		"missing prompt": `{"model":"alias"}`,
		"missing model":  `{"prompt":"hi"}`,
		"not json":       `{{{`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer k")
			proxy.ServeHTTP(rec, req)
			if rec.Code != 400 {
				t.Fatalf("expected 400, got %d %s", rec.Code, rec.Body.String())
			}
		})
	}

	t.Run("chat-only alias is unknown", func(t *testing.T) {
		sink := &sliceSink{}
		chatOnly := gateway.New(&fakeConfig{
			token:   "k",
			targets: []gateway.Target{{ProviderID: "p1", ProviderName: "prov", BaseURL: "http://upstream.test", Model: "gpt-x"}},
		}, sink, nil)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"alias","prompt":"hi"}`))
		req.Header.Set("Authorization", "Bearer k")
		chatOnly.ServeHTTP(rec, req)
		if rec.Code != 404 {
			t.Fatalf("expected 404, got %d %s", rec.Code, rec.Body.String())
		}
	})
}

func TestProxyImagesNoFilesWithoutCapture(t *testing.T) {
	var seen string
	upstream := newImageUpstream(t, &seen, "upstream-key")
	defer upstream.Close()

	sink := &sliceSink{}
	proxy := gateway.New(&fakeConfig{
		token: "client-key",
		image: []gateway.Target{{ProviderID: "p1", ProviderName: "prov", BaseURL: upstream.URL, APIKey: "upstream-key", Model: "img-x"}},
	}, sink, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/images", strings.NewReader(`{"model":"alias","prompt":"hi"}`))
	req.Header.Set("Authorization", "Bearer client-key")
	proxy.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(sink.records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(sink.records))
	}
	got := sink.records[0]
	if len(got.Images) != 0 || got.Bodies != nil {
		t.Fatalf("capture off must store neither files nor bodies, got %+v", got)
	}
	// The client still receives the full payload.
	if !strings.Contains(rec.Body.String(), "aW1hZ2UtYnl0ZXM=") {
		t.Fatalf("relay must be byte-identical, got %s", rec.Body.String())
	}
}

func TestProxyImagesFileExtractionCases(t *testing.T) {
	pngB64 := "aW1hZ2UtYnl0ZXM=" // "image-bytes"
	cases := []struct {
		name       string
		data       string
		wantFiles  int
		wantName   string
		wantMedia  string
		wantData   string
		wantText   string
		wantAbsent string
	}{
		{
			name:       "plain base64",
			data:       `{"b64_json":"` + pngB64 + `","media_type":"image/png"}`,
			wantFiles:  1,
			wantName:   "alias-0.png",
			wantMedia:  "image/png",
			wantData:   "image-bytes",
			wantText:   `"b64_json":""`,
			wantAbsent: pngB64,
		},
		{
			name: "data url prefix and whitespace",
			// \n is a JSON escape here, so the payload parses with whitespace.
			data:       `{"b64_json":"  data:image/png;base64,` + pngB64[:4] + `\n` + pngB64[4:] + `  ","media_type":"image/png"}`,
			wantFiles:  1,
			wantName:   "alias-0.png",
			wantMedia:  "image/png",
			wantData:   "image-bytes",
			wantText:   `"b64_json":""`,
			wantAbsent: pngB64,
		},
		{
			name:       "sniffed media type",
			data:       `{"b64_json":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="}`,
			wantFiles:  1,
			wantName:   "alias-0.png",
			wantMedia:  "image/png",
			wantData:   "",
			wantText:   `"b64_json":""`,
			wantAbsent: "iVBORw0KGgo",
		},
		{
			name:       "invalid base64 keeps text",
			data:       `{"b64_json":"!!!not-base64!!!","media_type":"image/png"}`,
			wantFiles:  0,
			wantText:   "!!!not-base64!!!",
			wantAbsent: "",
		},
		{
			name:       "url output is never downloaded",
			data:       `{"url":"https://images.example/1.png"}`,
			wantFiles:  0,
			wantText:   "https://images.example/1.png",
			wantAbsent: "",
		},
		{
			name:       "non-image media type skipped",
			data:       `{"b64_json":"` + pngB64 + `","media_type":"application/octet-stream"}`,
			wantFiles:  0,
			wantText:   pngB64,
			wantAbsent: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"created":1,"data":[`+tc.data+`],"usage":{}}`)
			}))
			defer upstream.Close()

			sink := &sliceSink{}
			proxy := gateway.New(&fakeConfig{
				token: "k",
				image: []gateway.Target{{ProviderID: "p1", ProviderName: "prov", BaseURL: upstream.URL, Model: "img-x"}},
			}, sink, nil)
			proxy.Capture = func() bool { return true }

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/images", strings.NewReader(`{"model":"alias","prompt":"hi"}`))
			req.Header.Set("Authorization", "Bearer k")
			proxy.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("status = %d", rec.Code)
			}
			if len(sink.records) != 1 {
				t.Fatalf("expected 1 record, got %d", len(sink.records))
			}
			got := sink.records[0]
			if len(got.Images) != tc.wantFiles {
				t.Fatalf("files = %d, want %d (%+v)", len(got.Images), tc.wantFiles, got.Images)
			}
			if tc.wantFiles == 1 {
				if got.Images[0].Name != tc.wantName || got.Images[0].MediaType != tc.wantMedia {
					t.Fatalf("unexpected file %+v", got.Images[0])
				}
				if tc.wantData != "" && string(got.Images[0].Data) != tc.wantData {
					t.Fatalf("unexpected file bytes %q", got.Images[0].Data)
				}
				if len(got.Images[0].Data) == 0 {
					t.Fatal("empty file bytes")
				}
			}
			if got.Bodies == nil {
				t.Fatal("expected captured bodies")
			}
			if !strings.Contains(got.Bodies.Response, tc.wantText) {
				t.Fatalf("captured response missing %q in %s", tc.wantText, got.Bodies.Response)
			}
			if tc.wantAbsent != "" && strings.Contains(got.Bodies.Response, tc.wantAbsent) {
				t.Fatalf("captured response should redact %q in %s", tc.wantAbsent, got.Bodies.Response)
			}
		})
	}
}

func TestProxyImagesFilenameSanitizesAlias(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"created":1,"data":[{"b64_json":"aGk=","media_type":"image/jpeg"}],"usage":{}}`)
	}))
	defer upstream.Close()

	sink := &sliceSink{}
	proxy := gateway.New(&fakeConfig{
		token: "k",
		image: []gateway.Target{{ProviderID: "p1", ProviderName: "prov", BaseURL: upstream.URL, Model: "img-x"}},
	}, sink, nil)
	proxy.Capture = func() bool { return true }

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/images", strings.NewReader(`{"model":"qwen/qwen3:free","prompt":"hi"}`))
	req.Header.Set("Authorization", "Bearer k")
	proxy.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	got := sink.records[0]
	if len(got.Images) != 1 || got.Images[0].Name != "qwen_qwen3_free-0.jpg" {
		t.Fatalf("unexpected files %+v", got.Images)
	}
}

func newEditRequest(t *testing.T, fields map[string]string, files map[string]editFile) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := writer.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	for field, f := range files {
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, field, f.filename))
		h.Set("Content-Type", f.contentType)
		part, err := writer.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, writer.FormDataContentType()
}

type editFile struct {
	filename    string
	contentType string
	data        []byte
}

func TestProxyImageEditsPassthrough(t *testing.T) {
	var seenModel, seenPrompt, seenSize, seenFilename, seenContentType string
	var seenImage []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/images/edits" {
			t.Errorf("upstream path = %q, want /images/edits", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			t.Errorf("parse upstream multipart: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		seenModel = r.FormValue("model")
		seenPrompt = r.FormValue("prompt")
		seenSize = r.FormValue("size")
		fhs := r.MultipartForm.File["image"]
		if len(fhs) != 1 {
			t.Errorf("upstream got %d image files", len(fhs))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		seenFilename = fhs[0].Filename
		seenContentType = fhs[0].Header.Get("Content-Type")
		src, _ := fhs[0].Open()
		seenImage, _ = io.ReadAll(src)
		_ = src.Close()
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"created": 1748372400,
			"data": []any{map[string]any{
				"b64_json":   "ZWRpdGVk",
				"media_type": "image/png",
			}},
			"usage": map[string]any{"total_tokens": 100, "cost": 0.02},
		})
	}))
	defer upstream.Close()

	sink := &sliceSink{}
	proxy := gateway.New(&fakeConfig{
		token: "client-key",
		image: []gateway.Target{{ProviderID: "p1", ProviderName: "prov", BaseURL: upstream.URL, APIKey: "upstream-key", Model: "img-edit-x"}},
	}, sink, nil)
	proxy.Capture = func() bool { return true }

	imageBytes := []byte("fake-png-bytes")
	buf, contentType := newEditRequest(t,
		map[string]string{"model": "alias", "prompt": "add a hat", "size": "1024x1024"},
		map[string]editFile{"image": {"orig.png", "image/png", imageBytes}},
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", buf)
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", contentType)
	proxy.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if seenModel != "img-edit-x" || seenPrompt != "add a hat" || seenSize != "1024x1024" {
		t.Fatalf("upstream got model %q prompt %q size %q", seenModel, seenPrompt, seenSize)
	}
	if seenFilename != "orig.png" || seenContentType != "image/png" || string(seenImage) != string(imageBytes) {
		t.Fatalf("upstream got file %q %q %q", seenFilename, seenContentType, seenImage)
	}
	if !strings.Contains(rec.Body.String(), "ZWRpdGVk") {
		t.Fatalf("relay missing output payload: %s", rec.Body.String())
	}
	if len(sink.records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(sink.records))
	}
	got := sink.records[0]
	if got.Endpoint != gateway.EndpointImage || got.Outcome != gateway.OutcomeCompleted || got.Status != 200 {
		t.Fatalf("unexpected record %+v", got)
	}
	if got.Transformer != "" {
		t.Fatalf("transformers do not apply to multipart edits, got %q", got.Transformer)
	}
	if got.Usage.Cost == nil || *got.Usage.Cost != 0.02 {
		t.Fatalf("expected cost 0.02, got %+v", got.Usage)
	}
	// One input file plus one output file, in that order.
	if len(got.Images) != 2 {
		t.Fatalf("expected input+output files, got %+v", got.Images)
	}
	in, out := got.Images[0], got.Images[1]
	if in.MediaType != "image/png" || !strings.HasSuffix(in.Name, ".png") || string(in.Data) != string(imageBytes) {
		t.Fatalf("unexpected input file %+v", in)
	}
	if out.MediaType != "image/png" || string(out.Data) != "edited" {
		t.Fatalf("unexpected output file %+v", out)
	}
	// Captured request is a summary: fields and filenames, no binary.
	if got.Bodies == nil {
		t.Fatal("expected captured bodies")
	}
	for _, want := range []string{`"prompt":"add a hat"`, "orig.png"} {
		if !strings.Contains(got.Bodies.Request, want) {
			t.Fatalf("captured request missing %s: %s", want, got.Bodies.Request)
		}
	}
	if strings.Contains(got.Bodies.Request, "fake-png-bytes") {
		t.Fatalf("captured request must not hold binary: %s", got.Bodies.Request)
	}
	if strings.Contains(got.Bodies.Response, "ZWRpdGVk") {
		t.Fatalf("captured response must redact saved output: %s", got.Bodies.Response)
	}
}

func TestProxyImageEditsValidation(t *testing.T) {
	proxy := gateway.New(&fakeConfig{
		token: "k",
		image: []gateway.Target{{BaseURL: "http://upstream.test", Model: "img-x"}},
	}, &sliceSink{}, nil)
	goodFiles := map[string]editFile{"image": {"a.png", "image/png", []byte("x")}}

	t.Run("not multipart", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(`{"model":"alias"}`))
		req.Header.Set("Authorization", "Bearer k")
		req.Header.Set("Content-Type", "application/json")
		proxy.ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
	})
	for name, fields := range map[string]map[string]string{
		"missing model":  {"prompt": "hi"},
		"missing prompt": {"model": "alias"},
	} {
		t.Run(name, func(t *testing.T) {
			buf, contentType := newEditRequest(t, fields, goodFiles)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", buf)
			req.Header.Set("Authorization", "Bearer k")
			req.Header.Set("Content-Type", contentType)
			proxy.ServeHTTP(rec, req)
			if rec.Code != 400 {
				t.Fatalf("expected 400, got %d", rec.Code)
			}
		})
	}
	t.Run("missing image", func(t *testing.T) {
		buf, contentType := newEditRequest(t, map[string]string{"model": "alias", "prompt": "hi"}, nil)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", buf)
		req.Header.Set("Authorization", "Bearer k")
		req.Header.Set("Content-Type", contentType)
		proxy.ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
	})
}

func TestProxyImageEditsAuthAndRouting(t *testing.T) {
	target := gateway.Target{ProviderID: "p1", ProviderName: "prov", BaseURL: "http://upstream.test", Model: "img-x"}
	files := map[string]editFile{"image": {"a.png", "image/png", []byte("x")}}
	post := func(proxy *gateway.Proxy, key string) *httptest.ResponseRecorder {
		buf, contentType := newEditRequest(t, map[string]string{"model": "alias", "prompt": "hi"}, files)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", buf)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		req.Header.Set("Content-Type", contentType)
		proxy.ServeHTTP(rec, req)
		return rec
	}

	t.Run("unauthorized", func(t *testing.T) {
		proxy := gateway.New(&fakeConfig{token: "k", image: []gateway.Target{target}}, &sliceSink{}, nil)
		if rec := post(proxy, ""); rec.Code != 401 {
			t.Fatalf("expected 401, got %d", rec.Code)
		}
	})
	t.Run("unknown model", func(t *testing.T) {
		sink := &sliceSink{}
		proxy := gateway.New(&fakeConfig{token: "k"}, sink, nil)
		if rec := post(proxy, "k"); rec.Code != 404 {
			t.Fatalf("expected 404, got %d", rec.Code)
		}
		if len(sink.records) != 0 {
			t.Fatalf("unknown models are not logged, got %+v", sink.records)
		}
	})
	t.Run("chat-only alias is unknown", func(t *testing.T) {
		proxy := gateway.New(&fakeConfig{
			token:   "k",
			targets: []gateway.Target{{ProviderID: "p1", ProviderName: "prov", BaseURL: "http://x", Model: "gpt-x"}},
		}, &sliceSink{}, nil)
		if rec := post(proxy, "k"); rec.Code != 404 {
			t.Fatalf("expected 404, got %d", rec.Code)
		}
	})
	t.Run("denied key", func(t *testing.T) {
		sink := &sliceSink{}
		proxy := gateway.New(&fakeConfig{token: "k", key: policyKey(t, `^other`, "", ""), image: []gateway.Target{target}}, sink, nil)
		if rec := post(proxy, "k"); rec.Code != 403 {
			t.Fatalf("expected 403, got %d", rec.Code)
		}
		if len(sink.records) != 1 || sink.records[0].Outcome != gateway.OutcomeRejected || sink.records[0].Endpoint != gateway.EndpointImage {
			t.Fatalf("expected logged rejection, got %+v", sink.records)
		}
	})
}

func TestProxyImageEditsSkipsTransform(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"created":1,"data":[],"usage":{}}`)
	}))
	defer upstream.Close()

	sink := &sliceSink{}
	proxy := gateway.New(&fakeConfig{
		token: "k",
		image: []gateway.Target{{ProviderID: "p1", ProviderName: "prov", BaseURL: upstream.URL, Model: "img-x"}},
		transform: func([]byte) ([]byte, string, error) {
			return nil, "broken", errors.New("boom")
		},
	}, sink, nil)

	buf, contentType := newEditRequest(t,
		map[string]string{"model": "alias", "prompt": "hi"},
		map[string]editFile{"image": {"a.png", "image/png", []byte("x")}},
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", buf)
	req.Header.Set("Authorization", "Bearer k")
	req.Header.Set("Content-Type", contentType)
	proxy.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("edits must not fail on JSON transformers, got %d %s", rec.Code, rec.Body.String())
	}
	if len(sink.records) != 1 || sink.records[0].Transformer != "" {
		t.Fatalf("expected empty transformer, got %+v", sink.records)
	}
}

func TestProxyImageEditsNoFilesWithoutCapture(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"created":1,"data":[{"b64_json":"aGk=","media_type":"image/png"}],"usage":{}}`)
	}))
	defer upstream.Close()

	sink := &sliceSink{}
	proxy := gateway.New(&fakeConfig{
		token: "k",
		image: []gateway.Target{{ProviderID: "p1", ProviderName: "prov", BaseURL: upstream.URL, Model: "img-x"}},
	}, sink, nil)

	buf, contentType := newEditRequest(t,
		map[string]string{"model": "alias", "prompt": "hi"},
		map[string]editFile{"image": {"a.png", "image/png", []byte("x")}},
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", buf)
	req.Header.Set("Authorization", "Bearer k")
	req.Header.Set("Content-Type", contentType)
	proxy.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	got := sink.records[0]
	if len(got.Images) != 0 || got.Bodies != nil {
		t.Fatalf("capture off must store neither files nor bodies, got %+v", got)
	}
}
