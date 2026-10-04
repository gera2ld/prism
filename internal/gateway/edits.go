package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
)

// editsPath serves image edits in OpenAI's images/edits shape
// (multipart/form-data with image, mask, prompt, model, ...).
// Clients name a Prism alias in the model field; the gateway rewrites it to
// the winning image route's upstream model and forwards the parts to
// {base_url}/images/edits.
const editsPath = "/v1/images/edits"

// maxEditBody bounds an edits request. Parts stream through a rebuilt
// multipart body, so this caps total memory and disk fluctuation alike.
const maxEditBody = 32 << 20

// handleImageEdits proxies one image edit request. Auth, routing, policy
// and logging mirror the generation surfaces; only the body shape differs.
// Transformers do not apply here: they evaluate JSONata over a JSON request
// body, and a multipart payload has no JSON body to shape.
func (p *Proxy) handleImageEdits(w http.ResponseWriter, r *http.Request) {
	presented, err := p.auth(r)
	if err != nil {
		writeAuthError(w, err)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxEditBody)
	if err := r.ParseMultipartForm(maxEditBody); err != nil || r.MultipartForm == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "body must be multipart/form-data")
		return
	}
	form := r.MultipartForm
	alias := firstFormValue(form, "model")
	prompt := firstFormValue(form, "prompt")
	if alias == "" || prompt == "" || len(form.File["image"]) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "multipart body must include model, prompt and image fields")
		return
	}

	// The captured request is a summary, not the bytes: binary parts live
	// as files in request_images, never as text.
	summary := editsSummary(alias, form)

	p.serveEdits(w, r, presented, form, summary, alias)
}

// serveEdits runs the edits pipeline: resolve the alias against the image
// routes, authorize the key, rebuild the multipart body with the rewritten
// model, forward it and relay the response byte-identical. Response outputs
// flow through relayImage, so generated images are saved and redacted
// exactly like generations.
func (p *Proxy) serveEdits(w http.ResponseWriter, r *http.Request, presented Key, form *multipart.Form, summary []byte, alias string) {
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
		p.deny(w, start, presented, EndpointImage, alias, summary, targets[0])
		return
	}

	// Capture is governed solely by the global gateway_settings toggle.
	captureEnabled := p.Capture()

	rebuilt, buildErr := rebuildEditBody(form, target)
	if buildErr != nil {
		rec := Record{
			StartedAt: start, KeyID: presented.ID, KeyName: presented.Name, Alias: alias, Endpoint: EndpointImage,
			ProviderID: target.ProviderID, ProviderName: target.ProviderName, UpstreamModel: target.Model,
			TotalMS: p.Now().Sub(start).Milliseconds(), Status: http.StatusInternalServerError,
			Outcome: OutcomeGatewayError, Error: buildErr.Error(),
			Bodies: p.captureBody(captureEnabled, string(summary), gatewayCaptureError(buildErr)),
		}
		p.write(rec)
		writeError(w, http.StatusInternalServerError, "upstream_error", "failed to build upstream request")
		return
	}
	rec := Record{
		StartedAt: start, KeyID: presented.ID, KeyName: presented.Name, Alias: alias, Endpoint: EndpointImage,
		ProviderID: target.ProviderID, ProviderName: target.ProviderName, UpstreamModel: target.Model,
	}
	if captureEnabled {
		rec.Images = rebuilt.inputs
	}

	upstreamReq, requestErr := newUpstreamEditRequest(r, rebuilt, target)
	if requestErr != nil {
		rec.TotalMS = p.Now().Sub(start).Milliseconds()
		rec.Status = http.StatusInternalServerError
		rec.Outcome = OutcomeGatewayError
		rec.Error = requestErr.Error()
		rec.Bodies = p.captureBody(captureEnabled, string(summary), gatewayCaptureError(requestErr))
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
		rec.Bodies = p.captureBody(captureEnabled, string(summary), gatewayCaptureError(upstreamErr))
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
		rec.Bodies = p.captureBody(captureEnabled, string(summary), snippet)
		copyHeaders(w.Header(), upstream.Header)
		w.WriteHeader(upstream.StatusCode)
		if _, writeErr := io.Copy(w, strings.NewReader(snippet)); writeErr != nil {
			rec.Error += "\nresponse write failed: " + writeErr.Error()
		}
		return
	}

	p.relayImage(r.Context(), w, upstream, &rec, captureEnabled, summary)
}

func firstFormValue(form *multipart.Form, key string) string {
	if form == nil || len(form.Value[key]) == 0 {
		return ""
	}
	return form.Value[key][0]
}

// editsSummary renders the captured request: text fields verbatim plus the
// original filenames of the uploaded parts. Binary bytes never enter the
// log; they persist as input files when capture is on.
func editsSummary(alias string, form *multipart.Form) []byte {
	summary := make(map[string]any, len(form.Value)+len(form.File)+1)
	summary["model"] = alias
	for field, vals := range form.Value {
		if field == "model" {
			continue
		}
		if len(vals) == 1 {
			summary[field] = vals[0]
		} else {
			summary[field] = vals
		}
	}
	for field, headers := range form.File {
		names := make([]string, 0, len(headers))
		for _, h := range headers {
			names = append(names, h.Filename)
		}
		if len(names) == 1 {
			summary[field] = names[0]
		} else {
			summary[field] = names
		}
	}
	encoded, err := json.Marshal(summary)
	if err != nil {
		return []byte("{}")
	}
	return encoded
}

// rebuiltEditBody carries a rebuilt multipart payload plus the decoded input
// images awaiting persistence.
type rebuiltEditBody struct {
	contentType string
	body        []byte
	inputs      []GeneratedImage
}

// rebuildEditBody copies the client's multipart body with the model field
// rewritten to the upstream model. Every other field and file passes
// through untouched. Uploaded images decode into input files for the store;
// undecodable or non-image parts still forward, they just are not saved.
func rebuildEditBody(form *multipart.Form, target Target) (rebuiltEditBody, error) {
	var out rebuiltEditBody
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	for field, vals := range form.Value {
		for _, v := range vals {
			if field == "model" {
				v = target.Model
			}
			if err := writer.WriteField(field, v); err != nil {
				return out, err
			}
		}
	}
	base := sanitizeFilename(target.Model)
	n := 0
	for field, headers := range form.File {
		for _, h := range headers {
			src, err := h.Open()
			if err != nil {
				return out, err
			}
			data, err := io.ReadAll(src)
			_ = src.Close()
			if err != nil {
				return out, err
			}
			mediaType := h.Header.Get("Content-Type")
			if mediaType == "" {
				mediaType = http.DetectContentType(data)
			}
			if ext := imageExtension(mediaType); ext != "" {
				out.inputs = append(out.inputs, GeneratedImage{
					Data:      data,
					Name:      fmt.Sprintf("%s-input-%s-%d.%s", base, sanitizeFilename(field), n, ext),
					MediaType: mediaType,
					Kind:      ImageKindInput,
				})
				n++
			}
			partHeader := make(textproto.MIMEHeader)
			partHeader.Set("Content-Disposition",
				fmt.Sprintf(`form-data; name="%s"; filename="%s"`,
					escapeQuotes(field), escapeQuotes(h.Filename)))
			if ct := h.Header.Get("Content-Type"); ct != "" {
				partHeader.Set("Content-Type", ct)
			}
			part, err := writer.CreatePart(partHeader)
			if err != nil {
				return out, err
			}
			if _, err := part.Write(data); err != nil {
				return out, err
			}
		}
	}
	if err := writer.Close(); err != nil {
		return out, err
	}
	out.contentType = writer.FormDataContentType()
	out.body = buf.Bytes()
	return out, nil
}

func escapeQuotes(s string) string {
	return strings.NewReplacer("\\", "\\\\", `"`, `\\"`).Replace(s)
}

func newUpstreamEditRequest(r *http.Request, rebuilt rebuiltEditBody, target Target) (*http.Request, error) {
	url := strings.TrimSuffix(target.BaseURL, "/") + "/images/edits"
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(rebuilt.body))
	if err != nil {
		return nil, err
	}
	upstream.Header = http.Header{}
	for key, values := range r.Header {
		if !hopByHop(key) && !strings.EqualFold(key, "Authorization") && !strings.EqualFold(key, "Host") &&
			!strings.EqualFold(key, "Content-Type") && !strings.EqualFold(key, "Content-Length") {
			upstream.Header[key] = values
		}
	}
	upstream.Header.Set("Content-Type", rebuilt.contentType)
	if target.APIKey != "" {
		upstream.Header.Set("Authorization", "Bearer "+target.APIKey)
	}
	upstream.Host = upstream.URL.Host
	return upstream, nil
}
