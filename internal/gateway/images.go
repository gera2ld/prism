package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// imagesPath serves image generation in OpenRouter's shape
// (POST {"model","prompt",...} -> {data:[{b64_json}], usage}).
// Kept for OpenRouter-backed providers; the OpenAI shape lives at
// generationsPath below. Both resolve the same image routes, so the
// operator picks the surface whose upstream speaks that shape.
const imagesPath = "/v1/images"

// generationsPath serves image generation in OpenAI's shape
// (POST {"model","prompt","size",...} -> {data:[{url|b64_json}]}).
// Clients name a Prism alias in model, exactly like chat; the gateway
// rewrites it to the winning image route's upstream model and forwards to
// {base_url}/images/generations.
const generationsPath = "/v1/images/generations"

type imageRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

// openAIImageRequest is the OpenAI images/generations shape. Only model
// and prompt are read by the gateway; size, n, quality, style and
// response_format pass through untouched.
type openAIImageRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type imageDatum struct {
	// B64JSON deliberately lacks omitempty: redacted captures marshal the
	// saved payload as an explicit blank rather than dropping the key.
	B64JSON       string `json:"b64_json"`
	URL           string `json:"url,omitempty"`
	MediaType     string `json:"media_type,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

type imageCompletion struct {
	Data  []imageDatum `json:"data"`
	Usage Usage        `json:"usage"`
}

// handleImages proxies one image generation request in OpenRouter's shape.
// It mirrors handleChat but is deliberately non-streaming: usage arrives in
// the JSON body and there is no TTFT or finish reason, while stream:true is
// rejected rather than half-relayed.
func (p *Proxy) handleImages(w http.ResponseWriter, r *http.Request) {
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

	var req imageRequest
	if err := json.Unmarshal(raw, &req); err != nil || req.Model == "" || req.Prompt == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "body must be JSON with model and prompt fields")
		return
	}
	if req.Stream {
		writeError(w, http.StatusBadRequest, "invalid_request", "image streaming is not supported")
		return
	}

	p.serveImage(w, r, presented, raw, req.Model, "/images")
}

// handleImageGenerations proxies one image generation request in OpenAI's
// images/generations shape. Routing, policy, shaping, logging and the relay
// are shared with handleImages; only the surface path, the accepted fields
// and the upstream suffix differ.
func (p *Proxy) handleImageGenerations(w http.ResponseWriter, r *http.Request) {
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

	var req openAIImageRequest
	if err := json.Unmarshal(raw, &req); err != nil || req.Model == "" || req.Prompt == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "body must be JSON with model and prompt fields")
		return
	}

	p.serveImage(w, r, presented, raw, req.Model, "/images/generations")
}

// serveImage runs the shared image pipeline: resolve the alias against the
// image routes, authorize the key, rewrite the model, shape, forward to the
// upstream path suffix and relay the response byte-identical.
func (p *Proxy) serveImage(w http.ResponseWriter, r *http.Request, presented Key, raw []byte, alias, upstreamSuffix string) {
	start := p.Now()

	targets, err := p.Config.Resolve(r.Context(), EndpointImage, alias)
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			writeError(w, http.StatusUnauthorized, "invalid_api_key", "Invalid API key")
			return
		}
		writeError(w, http.StatusNotFound, "unknown_model", fmt.Sprintf("no route for model %q", alias))
		return
	}
	// Key policy filters the priority-ordered targets before the winner is
	// picked, so a key denied on the top route still reaches lower routes
	// it is allowed to use.
	target, err := Authorize(presented, alias, targets)
	if err != nil {
		p.deny(w, start, presented, EndpointImage, alias, raw, targets[0])
		return
	}
	mutated := rewriteImageBody(raw, target)

	// Capture is governed solely by the global gateway_settings toggle.
	// Note the response can hold base64 image bytes: capture truncates at
	// the shared cap with a flag, same as chat.
	captureEnabled := p.Capture()

	// Request shaping runs on the upstream-bound body (post model rewrite),
	// identical to chat: matching is provider-exact plus model pattern.
	shaped, transformer, err := p.Config.Transform(r.Context(), target, mutated)
	if err != nil {
		outcome := OutcomeGatewayError
		status := http.StatusInternalServerError
		if r.Context().Err() != nil {
			outcome = OutcomeClientDisconnected
			status = 0
		}
		rec := Record{
			StartedAt: start, KeyID: presented.ID, KeyName: presented.Name, Alias: alias, Endpoint: EndpointImage,
			ProviderID: target.ProviderID, ProviderName: target.ProviderName, UpstreamModel: target.Model, Transformer: transformer,
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
		StartedAt: start, KeyID: presented.ID, KeyName: presented.Name, Alias: alias, Endpoint: EndpointImage,
		ProviderID: target.ProviderID, ProviderName: target.ProviderName, UpstreamModel: target.Model, Transformer: transformer,
	}

	upstreamReq, requestErr := newUpstreamImagesRequest(r, shaped, target, upstreamSuffix)
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

	p.relayImage(r.Context(), w, upstream, &rec, captureEnabled, shaped)
}

func (p *Proxy) relayImage(ctx context.Context, w http.ResponseWriter, upstream *http.Response, rec *Record, capture bool, request []byte) {
	body, readErr := io.ReadAll(io.LimitReader(upstream.Body, 32<<20))
	completion := imageCompletion{}
	if err := json.Unmarshal(body, &completion); err == nil {
		rec.Usage = completion.Usage
	}
	if capture {
		// Inline base64 outputs are persisted as files by the store, so the
		// captured response text carries blanks instead of megabytes of
		// base64. Items that failed to decode keep their payload in text.
		files, redacted := extractImageFiles(rec.Alias, completion.Data)
		rec.Images = files
		responseBody := body
		if redacted != nil {
			redactedCompletion := completion
			redactedCompletion.Data = redacted
			if encoded, err := json.Marshal(redactedCompletion); err == nil {
				responseBody = encoded
			}
		}
		reqBody, reqTrunc := truncateCapture(string(request))
		response, respTrunc := truncateCapture(string(responseBody))
		rec.Bodies = &Bodies{Request: reqBody, Response: response, Truncated: reqTrunc || respTrunc}
	}

	if readErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			rec.Outcome = OutcomeClientDisconnected
			rec.Error = "request canceled while reading upstream response: " + ctxErr.Error()
		} else {
			rec.Outcome = OutcomeUpstreamDisconnected
			rec.Error = "upstream response read failed: " + readErr.Error()
		}
	} else if ctx.Err() != nil {
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

// extractImageFiles decodes inline base64 outputs into files for the store
// to persist, and returns a redacted datum copy with saved payloads blanked
// for the captured response text. It returns nil redacted when nothing was
// saved, so the caller keeps the original bytes. URL outputs are never
// downloaded; undecodable items keep their payload in text.
func extractImageFiles(alias string, data []imageDatum) ([]GeneratedImage, []imageDatum) {
	var files []GeneratedImage
	var redacted []imageDatum
	base := sanitizeFilename(alias)
	for i, datum := range data {
		if datum.B64JSON == "" {
			continue
		}
		raw, err := decodeImageData(datum.B64JSON)
		if err != nil {
			continue
		}
		mediaType := datum.MediaType
		if mediaType == "" {
			mediaType = http.DetectContentType(raw)
		}
		ext := imageExtension(mediaType)
		if ext == "" {
			continue
		}
		if redacted == nil {
			redacted = append([]imageDatum(nil), data...)
		}
		files = append(files, GeneratedImage{
			Data:      raw,
			Name:      fmt.Sprintf("%s-%d.%s", base, i, ext),
			MediaType: mediaType,
		})
		redacted[i].B64JSON = ""
	}
	return files, redacted
}

// decodeImageData decodes a b64_json payload, tolerating data-URL prefixes
// and embedded whitespace.
func decodeImageData(payload string) ([]byte, error) {
	if i := strings.LastIndex(payload, ","); i >= 0 && strings.Contains(payload[:i], ";base64") {
		payload = payload[i+1:]
	}
	payload = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\n', '\r', '\t':
			return -1
		}
		return r
	}, payload)
	return base64.StdEncoding.DecodeString(payload)
}

// imageExtension maps an image MIME type to a filename extension. Empty
// means not a savable image.
func imageExtension(mediaType string) string {
	// Parameters (e.g. ";charset") never appear here, but strip them anyway.
	if i := strings.Index(mediaType, ";"); i >= 0 {
		mediaType = mediaType[:i]
	}
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image/png":
		return "png"
	case "image/jpeg":
		return "jpg"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	case "image/svg+xml":
		return "svg"
	default:
		return ""
	}
}

// sanitizeFilename keeps filenames readable in the admin UI. Aliases may
// contain slashes (e.g. qwen/qwen3:free); PocketBase appends its own random
// suffix to stored files, so this only needs to be safe, not unique.
func sanitizeFilename(alias string) string {
	var out strings.Builder
	for _, r := range alias {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out.WriteRune(r)
		default:
			out.WriteByte('_')
		}
	}
	if out.Len() == 0 {
		return "image"
	}
	return out.String()
}

// rewriteImageBody applies the gateway's own mutation: the upstream model
// name. Every other field passes through untouched.
func rewriteImageBody(raw []byte, target Target) []byte {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err == nil && body != nil {
		body["model"], _ = json.Marshal(target.Model)
		raw, _ = json.Marshal(body)
	}
	return raw
}

func newUpstreamImagesRequest(r *http.Request, raw []byte, target Target, suffix string) (*http.Request, error) {
	url := strings.TrimSuffix(target.BaseURL, "/") + suffix
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
