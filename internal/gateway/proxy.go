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
		data = append(data, modelObject{Object: "model", ID: name, OwnedBy: target.ProviderName, SupportedEndpointTypes: []string{"openai"}})
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
		Outcome: OutcomeRejected,
		Error:   fmt.Sprintf("key %q is not allowed to use model %q", presented.Name, alias),
		Bodies:  p.captureBody(p.Capture(), string(raw), ""),
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
	Object                 string   `json:"object"`
	ID                     string   `json:"id"`
	OwnedBy                string   `json:"owned_by"`
	SupportedEndpointTypes []string `json:"supported_endpoint_types"`
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
		outcome := OutcomeGatewayError
		status := http.StatusInternalServerError
		if r.Context().Err() != nil {
			outcome = OutcomeClientDisconnected
			status = 0
		}
		rec := Record{
			StartedAt: start, KeyID: presented.ID, KeyName: presented.Name, Alias: alias,
			ProviderID: target.ProviderID, ProviderName: target.ProviderName, UpstreamModel: target.Model, Transformer: transformer, Stream: stream,
			TotalMS: p.Now().Sub(start).Milliseconds(), Status: status,
			Outcome: outcome,
			Error:   err.Error(),
			Bodies:  p.captureBody(captureEnabled, string(raw), transformCaptureError(transformer, err)),
		}
		p.write(rec)
		if outcome != OutcomeClientDisconnected {
			writeError(w, http.StatusInternalServerError, "transform_error", "request transform failed")
		}
		return
	}
	rec := Record{
		StartedAt: start, KeyID: presented.ID, KeyName: presented.Name, Alias: alias,
		ProviderID: target.ProviderID, ProviderName: target.ProviderName, UpstreamModel: target.Model, Transformer: transformer, Stream: stream,
	}

	upstreamReq, requestErr := newUpstreamRequest(r, shaped, target)
	if requestErr != nil {
		rec.TotalMS = p.Now().Sub(start).Milliseconds()
		rec.Status = http.StatusInternalServerError
		rec.Outcome = OutcomeGatewayError
		rec.Error = requestErr.Error()
		rec.Bodies = p.captureBody(captureEnabled, string(shaped), gatewayCaptureError(requestErr))
		p.write(rec)
		writeError(w, http.StatusInternalServerError, "upstream_error", "failed to build upstream request")
		return
	}

	upstream, upstreamErr := p.Client.Do(upstreamReq)
	if upstreamErr != nil {
		rec.TotalMS = p.Now().Sub(start).Milliseconds()
		rec.Outcome = OutcomeUpstreamError
		rec.Status = http.StatusBadGateway
		rec.Error = upstreamErr.Error()
		if r.Context().Err() != nil {
			rec.Outcome = OutcomeClientDisconnected
			rec.Status = 0
		}
		rec.Bodies = p.captureBody(captureEnabled, string(shaped), gatewayCaptureError(upstreamErr))
		p.write(rec)
		if rec.Outcome != OutcomeClientDisconnected {
			writeError(w, http.StatusBadGateway, "upstream_error", "upstream request failed")
		}
		return
	}
	defer upstream.Body.Close()

	defer func() { rec.TotalMS = p.Now().Sub(start).Milliseconds(); p.write(rec) }()

	if upstream.StatusCode >= 400 {
		snippet, readErr := limitedString(upstream.Body, maxBodyCapture)
		rec.Status = upstream.StatusCode
		rec.Outcome = OutcomeUpstreamError
		rec.Error = snippet
		if rec.Error == "" {
			rec.Error = fmt.Sprintf("upstream returned HTTP %d", upstream.StatusCode)
		}
		if readErr != nil {
			rec.Error += "\nresponse read failed: " + readErr.Error()
		}
		rec.Bodies = p.captureBody(captureEnabled, string(shaped), snippet)
		copyHeaders(w.Header(), upstream.Header)
		w.WriteHeader(upstream.StatusCode)
		if _, writeErr := io.Copy(w, strings.NewReader(snippet)); writeErr != nil {
			rec.Error += "\nresponse write failed: " + writeErr.Error()
		}
		return
	}

	if stream {
		p.relayStream(r.Context(), w, upstream, &rec, captureEnabled, shaped)
		return
	}
	p.relayOnce(r.Context(), w, upstream, &rec, captureEnabled, shaped)
}

func gatewayCaptureError(err error) string { return "upstream error: " + err.Error() }

type providerError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

func providerErrorMessage(err *providerError) string {
	if err.Message != "" {
		return err.Message
	}
	if err.Code != "" {
		return err.Code
	}
	if err.Type != "" {
		return err.Type
	}
	return "provider error"
}

type sseParser struct {
	carry     []byte
	done      bool
	streamErr string
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
		s.parseLine(data[:idx], rec)
		data = data[idx+1:]
	}
}

func (s *sseParser) Close(rec *Record) {
	if len(s.carry) == 0 {
		return
	}
	s.parseLine(s.carry, rec)
	s.carry = nil
}

func (s *sseParser) parseLine(line []byte, rec *Record) {
	payload, ok := bytes.CutPrefix(line, []byte("data: "))
	if !ok {
		return
	}
	payload = bytes.TrimSpace(payload)
	if bytes.Equal(payload, []byte("[DONE]")) {
		s.done = true
		return
	}
	var parsed struct {
		Usage   *Usage `json:"usage"`
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Error *providerError `json:"error"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return
	}
	if parsed.Usage != nil {
		rec.Usage = *parsed.Usage
	}
	if len(parsed.Choices) > 0 {
		if fr := parsed.Choices[0].FinishReason; fr != nil && *fr != "" {
			reason := *fr
			rec.FinishReason = &reason
		}
	}
	if parsed.Error != nil && s.streamErr == "" {
		s.streamErr = providerErrorMessage(parsed.Error)
	}
}

func ptr[T any](v T) *T { return &v }

func (p *Proxy) relayStream(ctx context.Context, w http.ResponseWriter, upstream *http.Response, rec *Record, capture bool, request []byte) {
	if _, ok := w.(http.Flusher); !ok {
		rec.Status = http.StatusInternalServerError
		rec.Outcome = OutcomeGatewayError
		rec.Error = "streaming response writer does not support flushing"
		rec.Bodies = p.captureBody(capture, string(request), "")
		writeError(w, http.StatusInternalServerError, "streaming_unsupported", "streaming requires a flushable response writer")
		return
	}

	rec.Status = upstream.StatusCode
	copyHeaders(w.Header(), upstream.Header)
	w.WriteHeader(upstream.StatusCode)
	controller := http.NewResponseController(w)
	if err := controller.Flush(); err != nil {
		if errors.Is(err, http.ErrNotSupported) {
			rec.Outcome = OutcomeGatewayError
			rec.Error = "streaming response writer does not support flushing"
		} else {
			rec.Outcome = OutcomeClientDisconnected
			rec.Error = "response flush failed before first chunk: " + err.Error()
		}
		return
	}

	var responseBuf bytes.Buffer
	var parser sseParser
	buf := make([]byte, 32<<10)
	first := true
	for {
		if err := ctx.Err(); err != nil {
			rec.Outcome = OutcomeClientDisconnected
			rec.Error = "request canceled while reading upstream response: " + err.Error()
			break
		}

		n, readErr := upstream.Body.Read(buf)
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
			written, writeErr := w.Write(chunk)
			if writeErr == nil && written != len(chunk) {
				writeErr = io.ErrShortWrite
			}
			if writeErr != nil {
				rec.Outcome = OutcomeClientDisconnected
				rec.Error = "response write failed: " + writeErr.Error()
				break
			}
			if err := controller.Flush(); err != nil {
				rec.Outcome = OutcomeClientDisconnected
				rec.Error = "response flush failed: " + err.Error()
				break
			}
			if parser.streamErr != "" {
				rec.Outcome = OutcomeUpstreamError
				rec.Error = "stream error: " + parser.streamErr
				break
			}
			if parser.done {
				rec.Outcome = OutcomeCompleted
				break
			}
		}
		if readErr != nil {
			parser.Close(rec)
			if parser.done {
				rec.Outcome = OutcomeCompleted
				break
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				rec.Outcome = OutcomeClientDisconnected
				rec.Error = "request canceled while reading upstream response: " + ctxErr.Error()
				break
			}
			if parser.streamErr != "" {
				rec.Outcome = OutcomeUpstreamError
				rec.Error = "stream error: " + parser.streamErr
				break
			}
			if !errors.Is(readErr, io.EOF) {
				rec.Outcome = OutcomeUpstreamDisconnected
				rec.Error = "upstream response read failed: " + readErr.Error()
				break
			}
			rec.Outcome = OutcomeUpstreamDisconnected
			rec.Error = "stream ended before [DONE]"
			break
		}
	}
	if capture {
		response, truncated := truncateCapture(responseBuf.String())
		rec.Bodies = &Bodies{Request: string(request), Response: response, Truncated: truncated}
	}
}

func (p *Proxy) relayOnce(ctx context.Context, w http.ResponseWriter, upstream *http.Response, rec *Record, capture bool, request []byte) {
	body, readErr := io.ReadAll(io.LimitReader(upstream.Body, 32<<20))
	completion := chatCompletion{}
	if err := json.Unmarshal(body, &completion); err == nil {
		rec.Usage = completion.Usage
		if len(completion.Choices) > 0 {
			if fr := completion.Choices[0].FinishReason; fr != nil && *fr != "" {
				rec.FinishReason = fr
			}
		}
		if completion.Error != nil {
			rec.Outcome = OutcomeUpstreamError
			rec.Error = providerErrorMessage(completion.Error)
		}
	}
	if capture {
		reqBody, reqTrunc := truncateCapture(string(request))
		response, respTrunc := truncateCapture(string(body))
		rec.Bodies = &Bodies{Request: reqBody, Response: response, Truncated: reqTrunc || respTrunc}
	}

	if readErr != nil {
		if rec.Outcome == "" {
			if ctxErr := ctx.Err(); ctxErr != nil {
				rec.Outcome = OutcomeClientDisconnected
				rec.Error = "request canceled while reading upstream response: " + ctxErr.Error()
			} else {
				rec.Outcome = OutcomeUpstreamDisconnected
				rec.Error = "upstream response read failed: " + readErr.Error()
			}
		} else {
			if rec.Error != "" {
				rec.Error += "\n"
			}
			rec.Error += "upstream response read failed: " + readErr.Error()
		}
	} else if ctx.Err() != nil && rec.Outcome == "" {
		rec.Outcome = OutcomeClientDisconnected
		rec.Error = "request canceled before upstream response delivery: " + ctx.Err().Error()
		return
	}

	rec.Status = upstream.StatusCode
	copyHeaders(w.Header(), upstream.Header)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(upstream.StatusCode)
	written, writeErr := w.Write(body)
	if writeErr == nil && written != len(body) {
		writeErr = io.ErrShortWrite
	}
	if writeErr != nil {
		if rec.Outcome == "" {
			rec.Outcome = OutcomeClientDisconnected
		}
		if rec.Error != "" {
			rec.Error += "\nresponse write failed: "
		} else {
			rec.Error = "response write failed: "
		}
		rec.Error += writeErr.Error()
		return
	}
	if rec.Outcome == "" {
		rec.Outcome = OutcomeCompleted
	}
}

type chatCompletion struct {
	Usage   Usage `json:"usage"`
	Choices []struct {
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Error *providerError `json:"error"`
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

func newUpstreamRequest(r *http.Request, raw []byte, target Target) (*http.Request, error) {
	url := strings.TrimSuffix(target.BaseURL, "/") + "/chat/completions"
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
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
	return upstream, nil
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
