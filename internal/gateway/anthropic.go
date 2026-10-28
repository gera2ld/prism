package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// defaultAnthropicVersion is injected only when the client did not send one.
// Anthropic requires the header; compatible upstreams generally ignore it.
const defaultAnthropicVersion = "2023-06-01"

const (
	messagesUpstreamSuffix    = "/messages"
	countTokensUpstreamSuffix = "/messages/count_tokens"
)

type messagesRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

type countTokensRequest struct {
	Model string `json:"model"`
}

// handleMessages proxies one Anthropic Messages request. The upstream must
// already speak the Anthropic shape; the gateway only routes, authorizes,
// rewrites the model and logs. Everything else in the body passes through.
func (p *Proxy) handleMessages(w http.ResponseWriter, r *http.Request) {
	start := p.Now()
	presented, err := p.auth(r)
	if err != nil {
		writeAnthropicAuthError(w, err)
		return
	}

	raw, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if readErr != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", readErr.Error())
		return
	}

	var req messagesRequest
	if err := json.Unmarshal(raw, &req); err != nil || req.Model == "" {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "body must be JSON with a model field")
		return
	}

	targets, err := p.Config.Resolve(r.Context(), EndpointAnthropic, req.Model)
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "Invalid API key")
			return
		}
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", fmt.Sprintf("no route for model %q", req.Model))
		return
	}
	target, err := Authorize(presented, req.Model, targets)
	if err != nil {
		p.denyAnthropic(w, start, presented, EndpointAnthropic, req.Model, raw, targets[0])
		return
	}

	alias := req.Model
	stream := req.Stream
	// Only the model is rewritten. Anthropic has no stream_options, so the
	// chat path's usage injection must not run here.
	mutated := rewriteMessagesBody(raw, target)

	captureEnabled := p.Capture()
	shaped, transformer, err := p.Config.Transform(r.Context(), target, mutated)
	if err != nil {
		outcome := OutcomeGatewayError
		status := http.StatusInternalServerError
		if r.Context().Err() != nil {
			outcome = OutcomeClientDisconnected
			status = 0
		}
		rec := Record{
			StartedAt: start, KeyID: presented.ID, KeyName: presented.Name, Alias: alias, Endpoint: EndpointAnthropic,
			ProviderID: target.ProviderID, ProviderName: target.ProviderName, UpstreamModel: target.Model, Transformer: transformer, Stream: stream,
			TotalMS: p.Now().Sub(start).Milliseconds(), Status: status,
			Outcome: outcome,
			Error:   err.Error(),
			Bodies:  p.captureBody(captureEnabled, string(raw), transformCaptureError(transformer, err)),
		}
		p.write(rec)
		if outcome != OutcomeClientDisconnected {
			writeAnthropicError(w, http.StatusInternalServerError, "api_error", "request transform failed")
		}
		return
	}
	rec := Record{
		StartedAt: start, KeyID: presented.ID, KeyName: presented.Name, Alias: alias, Endpoint: EndpointAnthropic,
		ProviderID: target.ProviderID, ProviderName: target.ProviderName, UpstreamModel: target.Model, Transformer: transformer, Stream: stream,
	}

	upstreamReq, requestErr := newUpstreamAnthropicRequest(r, shaped, target, messagesUpstreamSuffix)
	if requestErr != nil {
		rec.TotalMS = p.Now().Sub(start).Milliseconds()
		rec.Status = http.StatusInternalServerError
		rec.Outcome = OutcomeGatewayError
		rec.Error = requestErr.Error()
		rec.Bodies = p.captureBody(captureEnabled, string(shaped), gatewayCaptureError(requestErr))
		p.write(rec)
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "failed to build upstream request")
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
			writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream request failed")
		}
		return
	}
	defer upstream.Body.Close()

	defer func() { rec.TotalMS = p.Now().Sub(start).Milliseconds(); p.write(rec) }()

	// Upstream errors are Anthropic-shaped already, so relay them verbatim
	// rather than wrapping them in the gateway envelope.
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

// handleCountTokens proxies Anthropic's token-counting endpoint. It follows
// the same routing and policy path as messages, including logging; the
// response carries no usage or stop reason, so the log row records the call
// without token counts.
func (p *Proxy) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	start := p.Now()
	presented, err := p.auth(r)
	if err != nil {
		writeAnthropicAuthError(w, err)
		return
	}

	raw, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if readErr != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", readErr.Error())
		return
	}

	var req countTokensRequest
	if err := json.Unmarshal(raw, &req); err != nil || req.Model == "" {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "body must be JSON with a model field")
		return
	}

	targets, err := p.Config.Resolve(r.Context(), EndpointAnthropic, req.Model)
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "Invalid API key")
			return
		}
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", fmt.Sprintf("no route for model %q", req.Model))
		return
	}
	target, err := Authorize(presented, req.Model, targets)
	if err != nil {
		p.denyAnthropic(w, start, presented, EndpointAnthropic, req.Model, raw, targets[0])
		return
	}

	mutated := rewriteMessagesBody(raw, target)
	captureEnabled := p.Capture()
	shaped, transformer, err := p.Config.Transform(r.Context(), target, mutated)
	if err != nil {
		outcome := OutcomeGatewayError
		status := http.StatusInternalServerError
		if r.Context().Err() != nil {
			outcome = OutcomeClientDisconnected
			status = 0
		}
		rec := Record{
			StartedAt: start, KeyID: presented.ID, KeyName: presented.Name, Alias: req.Model, Endpoint: EndpointAnthropic,
			ProviderID: target.ProviderID, ProviderName: target.ProviderName, UpstreamModel: target.Model, Transformer: transformer,
			TotalMS: p.Now().Sub(start).Milliseconds(), Status: status,
			Outcome: outcome,
			Error:   err.Error(),
			Bodies:  p.captureBody(captureEnabled, string(raw), transformCaptureError(transformer, err)),
		}
		p.write(rec)
		if outcome != OutcomeClientDisconnected {
			writeAnthropicError(w, http.StatusInternalServerError, "api_error", "request transform failed")
		}
		return
	}
	rec := Record{
		StartedAt: start, KeyID: presented.ID, KeyName: presented.Name, Alias: req.Model, Endpoint: EndpointAnthropic,
		ProviderID: target.ProviderID, ProviderName: target.ProviderName, UpstreamModel: target.Model, Transformer: transformer,
	}

	upstreamReq, requestErr := newUpstreamAnthropicRequest(r, shaped, target, countTokensUpstreamSuffix)
	if requestErr != nil {
		rec.TotalMS = p.Now().Sub(start).Milliseconds()
		rec.Status = http.StatusInternalServerError
		rec.Outcome = OutcomeGatewayError
		rec.Error = requestErr.Error()
		rec.Bodies = p.captureBody(captureEnabled, string(shaped), gatewayCaptureError(requestErr))
		p.write(rec)
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "failed to build upstream request")
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
			writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream request failed")
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

	p.relayOnce(r.Context(), w, upstream, &rec, captureEnabled, shaped)
}

// rewriteMessagesBody applies the gateway's own mutation: the upstream model
// name. Every other field, including system, tools and thinking, passes
// through untouched.
func rewriteMessagesBody(raw []byte, target Target) []byte {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err == nil && body != nil {
		body["model"], _ = json.Marshal(target.Model)
		raw, _ = json.Marshal(body)
	}
	return raw
}

func newUpstreamAnthropicRequest(r *http.Request, raw []byte, target Target, suffix string) (*http.Request, error) {
	upstream, err := newUpstreamJSONRequest(r, raw, target, suffix)
	if err != nil {
		return nil, err
	}
	if upstream.Header.Get("anthropic-version") == "" {
		upstream.Header.Set("anthropic-version", defaultAnthropicVersion)
	}
	return upstream, nil
}

type anthropicErrorBody struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// writeAnthropicError renders a gateway-level failure in Anthropic's error
// envelope. Upstream error bodies are relayed as-is and do not come here.
func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	var body anthropicErrorBody
	body.Type = "error"
	body.Error.Type = errType
	body.Error.Message = message
	writeJSON(w, status, body)
}

func writeAnthropicAuthError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrForbidden) {
		writeAnthropicError(w, http.StatusForbidden, "permission_error", "API key is not authorized")
		return
	}
	writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "Invalid API key")
}
