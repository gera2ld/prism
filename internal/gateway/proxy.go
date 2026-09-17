package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	maxBodyCapture = 256 << 10
	modelsPath     = "/v1/models"
	chatPath       = "/v1/chat/completions"
)

type Proxy struct {
	Config  Config
	Logs    LogSink
	Client  *http.Client
	Now     func() time.Time
	Capture func() bool
	Logger  *slog.Logger
}

func New(cfg Config, logs LogSink, logger *slog.Logger) *Proxy {
	return &Proxy{
		Config:  cfg,
		Logs:    logs,
		Client:  &http.Client{Timeout: 10 * time.Minute},
		Now:     time.Now,
		Capture: func() bool { return false },
		Logger:  logger,
	}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == modelsPath && r.Method == http.MethodGet:
		p.handleModels(w, r)
	case r.URL.Path == chatPath && r.Method == http.MethodPost:
		p.handleChat(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (p *Proxy) handleModels(w http.ResponseWriter, r *http.Request) {
	presented, err := p.auth(r)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	names, err := p.Config.Models(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "config_error", "failed to load routes")
		return
	}
	// Clients enumerate only the aliases their key may use.
	data := make([]modelObject, 0, len(names))
	for _, name := range names {
		targets, err := p.Config.Resolve(r.Context(), name)
		if err != nil {
			continue
		}
		// One entry per alias: the winning target, so multi-provider aliases
		// list the provider that would actually serve the request.
		target, err := Authorize(presented, name, targets)
		if err != nil {
			continue
		}
		data = append(data, modelObject{Object: "model", ID: name, Owned_by: target.ProviderName})
	}
	writeJSON(w, http.StatusOK, modelsResponse{Object: "list", Data: data})
}

// deny rejects a request no route target of which the key may use. The denial
// is logged with the top route's provider/model so the operator can see
// exactly what was blocked.
func (p *Proxy) deny(w http.ResponseWriter, start time.Time, presented Key, alias string, raw []byte, blocked Target) {
	rec := Record{
		StartedAt: start, KeyID: presented.ID, KeyName: presented.Name, Alias: alias,
		ProviderID: blocked.ProviderID, ProviderName: blocked.ProviderName, UpstreamModel: blocked.Model,
		TotalMS: p.Now().Sub(start).Milliseconds(), Status: http.StatusForbidden,
		Error:  fmt.Sprintf("key %q is not allowed to use model %q", presented.Name, alias),
		Bodies: p.captureBody(p.Capture(), string(raw), ""),
	}
	p.write(rec)
	writeError(w, http.StatusForbidden, "forbidden", fmt.Sprintf("key %q is not allowed to use model %q", presented.Name, alias))
}

// writeAuthError maps authentication failures: unknown keys are 401, keys
// whose stored policy is broken are 403.
func writeAuthError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrForbidden) {
		writeError(w, http.StatusForbidden, "forbidden", "API key is not authorized")
		return
	}
	writeError(w, http.StatusUnauthorized, "invalid_api_key", "Invalid API key")
}

type modelObject struct {
	Object   string `json:"object"`
	ID       string `json:"id"`
	Owned_by string `json:"owned_by"`
}

type modelsResponse struct {
	Object string        `json:"object"`
	Data   []modelObject `json:"data"`
}

type errorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	var body errorBody
	body.Error.Message = message
	body.Error.Type = "invalid_request_error"
	body.Error.Code = code
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (p *Proxy) auth(r *http.Request) (Key, error) {
	header := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || token == "" {
		return Key{}, ErrUnauthorized
	}
	return p.Config.Authenticate(r.Context(), token)
}

type chatRequest struct {
	Model    string          `json:"model"`
	Stream   bool            `json:"stream"`
	Messages json.RawMessage `json:"messages"`
}

func (p *Proxy) handleChat(w http.ResponseWriter, r *http.Request) {
	start := p.Now()
	presented, err := p.auth(r)
	if err != nil {
		writeAuthError(w, err)
		return
	}

	raw, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if readErr != nil {
		writeError(w, http.StatusBadRequest, "body_too_large", readErr.Error())
		return
	}

	var req chatRequest
	if err := json.Unmarshal(raw, &req); err != nil || req.Model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "body must be JSON with a model field")
		return
	}

	targets, err := p.Config.Resolve(r.Context(), req.Model)
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			writeError(w, http.StatusUnauthorized, "invalid_api_key", "Invalid API key")
			return
		}
		writeError(w, http.StatusNotFound, "unknown_model", fmt.Sprintf("no route for model %q", req.Model))
		return
	}
	// Key policy filters the priority-ordered targets before the winner is
	// picked, so a key denied on the top route still reaches lower routes
	// it is allowed to use.
	target, err := Authorize(presented, req.Model, targets)
	if err != nil {
		p.deny(w, start, presented, req.Model, raw, targets[0])
		return
	}
	alias := req.Model
	stream := req.Stream
	mutated := rewriteChatBody(raw, target, stream)

	// Capture is governed solely by the global gateway_settings toggle.
	captureEnabled := p.Capture()

	// Request shaping runs on the upstream-bound body (post model rewrite).
	// Every request resolves through the routing table, so this covers all
	// traffic uniformly.
	shaped, transformer, err := p.Config.Transform(r.Context(), target, mutated)
	if err != nil {
		rec := Record{
			StartedAt: start, KeyID: presented.ID, KeyName: presented.Name, Alias: alias,
			ProviderID: target.ProviderID, ProviderName: target.ProviderName, UpstreamModel: target.Model, Stream: stream,
			TotalMS: p.Now().Sub(start).Milliseconds(), Status: http.StatusInternalServerError,
			Error:  err.Error(),
			Bodies: p.captureBody(captureEnabled, string(raw), transformCaptureError(transformer, err)),
		}
		p.write(rec)
		writeError(w, http.StatusInternalServerError, "transform_error", "request transform failed")
		return
	}
	upstreamReq := newUpstreamRequest(r, shaped, target)

	upstream, upstreamErr := p.Client.Do(upstreamReq)
	if upstreamErr != nil {
		rec := Record{
			StartedAt: start, KeyID: presented.ID, Alias: alias,
			ProviderID: target.ProviderID, UpstreamModel: target.Model, Stream: stream,
			TotalMS: p.Now().Sub(start).Milliseconds(), Status: http.StatusBadGateway,
			Error:  upstreamErr.Error(),
			Bodies: p.captureBody(captureEnabled, string(shaped), gatewayCaptureError(upstreamErr)),
		}
		p.write(rec)
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream request failed")
		return
	}
	defer upstream.Body.Close()

	rec := Record{
		StartedAt: start, KeyID: presented.ID, KeyName: presented.Name, Alias: alias,
		ProviderID: target.ProviderID, ProviderName: target.ProviderName, UpstreamModel: target.Model, Transformer: transformer, Stream: stream,
	}
	defer func() { rec.TotalMS = p.Now().Sub(start).Milliseconds(); p.write(rec) }()

	if upstream.StatusCode >= 400 {
		snippet, _ := limitedString(upstream.Body, maxBodyCapture)
		rec.Status = upstream.StatusCode
		rec.Error = snippet
		rec.Bodies = p.captureBody(captureEnabled, string(shaped), snippet)
		copyHeaders(w.Header(), upstream.Header)
		w.WriteHeader(upstream.StatusCode)
		_, _ = io.Copy(w, strings.NewReader(snippet))
		return
	}

	if stream {
		p.relayStream(w, upstream, &rec, captureEnabled, shaped)
		return
	}
	p.relayOnce(w, upstream, &rec, captureEnabled, shaped)
}

func gatewayCaptureError(err error) string { return "upstream error: " + err.Error() }

type sseParser struct {
	carry []byte
}

func (s *sseParser) Feed(chunk []byte, rec *Record) {
	data := chunk
	if len(s.carry) > 0 {
		data = append(s.carry, chunk...)
		s.carry = nil
	}
	for {
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			if len(data) > 0 && len(data) <= 1<<20 {
				s.carry = append([]byte(nil), data...)
			}
			return
		}
		line := data[:idx]
		data = data[idx+1:]
		if payload, ok := bytes.CutPrefix(line, []byte("data: ")); ok {
			var parsed struct {
				Usage   *Usage `json:"usage"`
				Choices []struct {
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(payload, &parsed); err != nil {
				continue
			}
			if parsed.Usage != nil {
				rec.Usage = *parsed.Usage
			}
			// First choice only; last non-empty reason across chunks wins.
			if len(parsed.Choices) > 0 {
				if fr := parsed.Choices[0].FinishReason; fr != nil && *fr != "" {
					reason := *fr
					rec.FinishReason = &reason
				}
			}
		}
	}
}

func ptr[T any](v T) *T { return &v }

func (p *Proxy) relayStream(w http.ResponseWriter, upstream *http.Response, rec *Record, capture bool, request []byte) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming_unsupported", "streaming requires a flushable response writer")
		return
	}
	copyHeaders(w.Header(), upstream.Header)
	w.WriteHeader(upstream.StatusCode)
	flusher.Flush()

	var responseBuf bytes.Buffer
	var parser sseParser
	buf := make([]byte, 32<<10)
	first := true
	for {
		n, err := upstream.Body.Read(buf)
		if n > 0 {
			if first {
				rec.TTFTMS = ptr(p.Now().Sub(rec.StartedAt).Milliseconds())
				first = false
			}
			chunk := buf[:n]
			parser.Feed(chunk, rec)
			if capture && responseBuf.Len() < maxBodyCapture {
				responseBuf.Write(chunk)
			}
			if _, werr := w.Write(chunk); werr != nil {
				break
			}
			flusher.Flush()
		}
		if err != nil {
			break
		}
	}
	if capture {
		response, truncated := truncateCapture(responseBuf.String())
		rec.Bodies = &Bodies{Request: string(request), Response: response, Truncated: truncated}
	}
	rec.Status = upstream.StatusCode
}

func (p *Proxy) relayOnce(w http.ResponseWriter, upstream *http.Response, rec *Record, capture bool, request []byte) {
	body, _ := io.ReadAll(io.LimitReader(upstream.Body, 32<<20))
	completion := chatCompletion{}
	if err := json.Unmarshal(body, &completion); err == nil {
		rec.Usage = completion.Usage
		if len(completion.Choices) > 0 {
			if fr := completion.Choices[0].FinishReason; fr != nil && *fr != "" {
				rec.FinishReason = fr
			}
		}
	}
	if capture {
		reqBody, reqTrunc := truncateCapture(string(request))
		response, respTrunc := truncateCapture(string(body))
		rec.Bodies = &Bodies{Request: reqBody, Response: response, Truncated: reqTrunc || respTrunc}
	}
	rec.Status = upstream.StatusCode
	copyHeaders(w.Header(), upstream.Header)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(upstream.StatusCode)
	_, _ = w.Write(body)
}

type chatCompletion struct {
	Usage   Usage `json:"usage"`
	Choices []struct {
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// rewriteChatBody applies the gateway's own mutation: the upstream model
// name and, for streams, the usage-reporting option.
func rewriteChatBody(raw []byte, target Target, stream bool) []byte {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err == nil && body != nil {
		body["model"], _ = json.Marshal(target.Model)
		if stream {
			body["stream_options"], _ = json.Marshal(map[string]bool{"include_usage": true})
		}
		raw, _ = json.Marshal(body)
	}
	return raw
}

func transformCaptureError(name string, err error) string {
	if name == "" {
		return "transform error: " + err.Error()
	}
	return "transformer " + name + ": " + err.Error()
}

func newUpstreamRequest(r *http.Request, raw []byte, target Target) *http.Request {
	url := strings.TrimSuffix(target.BaseURL, "/") + "/chat/completions"
	upstream, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	upstream.Header = http.Header{}
	for key, values := range r.Header {
		if !hopByHop(key) && !strings.EqualFold(key, "Authorization") && !strings.EqualFold(key, "Host") {
			upstream.Header[key] = values
		}
	}
	if target.APIKey != "" {
		upstream.Header.Set("Authorization", "Bearer "+target.APIKey)
	}
	upstream.Host = upstream.URL.Host
	return upstream
}

func hopByHop(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailers", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		if !hopByHop(key) {
			for _, v := range values {
				dst.Add(key, v)
			}
		}
	}
}

func limitedString(r io.Reader, limit int64) (string, error) {
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.LimitReader(r, limit)); err != nil {
		return buf.String(), err
	}
	return buf.String(), nil
}

func truncateCapture(s string) (string, bool) {
	if len(s) <= maxBodyCapture {
		return s, false
	}
	return s[:maxBodyCapture], true
}

func (p *Proxy) captureBody(enabled bool, request, response string) *Bodies {
	if !enabled {
		return nil
	}
	req, truncated := truncateCapture(request)
	resp, respTruncated := truncateCapture(response)
	return &Bodies{Request: req, Response: resp, Truncated: truncated || respTruncated}
}

func (p *Proxy) write(rec Record) {
	if p.Logs == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.Logs.Write(ctx, rec); err != nil && p.Logger != nil {
		p.Logger.Error("failed to write request log", "error", err)
	}
}
