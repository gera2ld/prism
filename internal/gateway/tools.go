package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	toolsPath = "/v1/tools"
	// invokeSuffix separates the tool name from the invoke verb.
	invokeSuffix = "/invoke"
	// maxToolBody bounds an invocation request. Arguments are small; a large
	// one is a client bug or an attempt to push a payload past the capture cap.
	maxToolBody = 4 << 20
)

// toolObject is the OpenAI tool shape, so the array from GET /v1/tools can be
// handed straight to a chat request's tools field. The id is redundant with
// function.name but makes the listing addressable, matching /v1/models.
type toolObject struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function functionObject `json:"function"`
}

type functionObject struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type toolsResponse struct {
	Object string       `json:"object"`
	Data   []toolObject `json:"data"`
}

type invokeRequest struct {
	Arguments json.RawMessage `json:"arguments"`
}

type invokeResponse struct {
	Object  string `json:"object"`
	Tool    string `json:"tool"`
	IsError bool   `json:"is_error"`
	Result  any    `json:"result"`
}

// serveToolInvoke dispatches the invoke path, returning false when path is not
// an invocation so the caller can keep routing.
func (p *Proxy) serveToolInvoke(w http.ResponseWriter, r *http.Request) bool {
	name, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, toolsPath+"/"), invokeSuffix)
	if !ok || name == "" || r.Method != http.MethodPost {
		return false
	}
	p.handleToolInvoke(w, r, name)
	return true
}

func (p *Proxy) handleTools(w http.ResponseWriter, r *http.Request) {
	presented, err := p.auth(r)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	if p.Tools == nil {
		writeError(w, http.StatusNotImplemented, "tools_disabled", "tools are not configured on this gateway")
		return
	}
	tools, err := p.Tools.List(r.Context(), presented)
	if err != nil {
		if errors.Is(err, ErrForbidden) {
			writeError(w, http.StatusForbidden, "forbidden", "not allowed to use tools")
			return
		}
		writeError(w, http.StatusInternalServerError, "tools_error", "failed to load tools")
		return
	}
	data := make([]toolObject, 0, len(tools))
	for _, t := range tools {
		data = append(data, toolObject{
			ID:   t.Name,
			Type: "function",
			Function: functionObject{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}
	writeJSON(w, http.StatusOK, toolsResponse{Object: "list", Data: data})
}

func (p *Proxy) handleToolInvoke(w http.ResponseWriter, r *http.Request, name string) {
	start := p.Now()
	presented, err := p.auth(r)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	if p.Tools == nil {
		writeError(w, http.StatusNotImplemented, "tools_disabled", "tools are not configured on this gateway")
		return
	}

	raw, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, maxToolBody))
	if readErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", readErr.Error())
		return
	}
	// Arguments are required as an explicit envelope, so there is no ambiguity
	// between a wrapper and the argument object itself. Absent or null
	// arguments mean an empty object, which is what a no-argument tool wants.
	args := json.RawMessage(`{}`)
	if trimmed := strings.TrimSpace(string(raw)); trimmed != "" && trimmed != "null" {
		var body invokeRequest
		if err := json.Unmarshal(raw, &body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", `body must be {"arguments": {...}}`)
			return
		}
		if len(body.Arguments) > 0 {
			// An explicit null is decoded into a non-empty raw value holding
			// the four bytes "null", so a length check alone would pass it
			// through as a null argument instead of the empty object.
			if !json.Valid(body.Arguments) {
				writeError(w, http.StatusBadRequest, "invalid_request", "arguments must be valid JSON")
				return
			}
			if trimmed := bytes.TrimSpace(body.Arguments); len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
				args = body.Arguments
			}
		}
	}

	result, err := p.Tools.Invoke(r.Context(), presented, name, args)
	if err != nil {
		p.writeToolFailure(w, r, start, presented, name, args, err)
		return
	}

	encoded, err := json.Marshal(result.Result)
	if err != nil {
		rec := ToolRecord{
			StartedAt: start, KeyID: presented.ID, KeyName: presented.Name, Tool: name,
			DurationMS: p.Now().Sub(start).Milliseconds(), Status: http.StatusInternalServerError,
			Outcome: ToolOutcomeGatewayError, Error: "tool result is not encodable: " + err.Error(),
		}
		p.writeTool(rec)
		writeError(w, http.StatusInternalServerError, "tool_error", "tool result could not be encoded")
		return
	}

	rec := ToolRecord{
		StartedAt: start, KeyID: presented.ID, KeyName: presented.Name, Tool: name,
		Source: result.Source, Server: result.Server,
		Outcome: ToolOutcomeCompleted, Status: http.StatusOK,
		Result: string(encoded),
	}
	if result.IsError {
		rec.Outcome = ToolOutcomeToolError
		rec.Error = string(encoded)
	}
	rec.DurationMS = p.Now().Sub(start).Milliseconds()
	rec.Args, rec.Result, rec.Truncated = p.captureToolPayload(p.Capture(), string(args), rec.Result)
	p.writeTool(rec)

	var payload any
	if len(encoded) > 0 {
		// Round-trip through the encoder so the client sees the same shape the
		// log recorded, and so a bare string does not arrive as a JSON string
		// nested inside another one.
		if err := json.Unmarshal(encoded, &payload); err != nil {
			payload = string(encoded)
		}
	}
	writeJSON(w, http.StatusOK, invokeResponse{Object: "tool.result", Tool: name, IsError: result.IsError, Result: payload})
}

// writeToolFailure logs and answers an invocation that failed at the gateway
// level. A tool that ran and failed is not this path: it comes back from
// Invoke as a ToolResult with IsError set and is answered 200, so an agent
// loop never has to distinguish transport failure from tool failure.
func (p *Proxy) writeToolFailure(w http.ResponseWriter, r *http.Request, start time.Time, presented Key, name string, args []byte, err error) {
	rec := ToolRecord{
		StartedAt: start, KeyID: presented.ID, KeyName: presented.Name, Tool: name,
		Outcome: ToolOutcomeGatewayError, Status: http.StatusInternalServerError, Error: err.Error(),
	}

	status := http.StatusInternalServerError
	code := "tool_error"
	message := "tool invocation failed"
	switch {
	case errors.Is(err, ErrUnknownTool):
		rec.Outcome = ToolOutcomeRejected
		status, code = http.StatusNotFound, "unknown_tool"
		message = fmt.Sprintf("no callable tool named %q", name)
	case errors.Is(err, ErrForbidden):
		rec.Outcome = ToolOutcomeRejected
		status, code = http.StatusForbidden, "forbidden"
		message = "not allowed to use this tool"
	case r.Context().Err() != nil:
		rec.Outcome = ToolOutcomeClientDisconnected
		status = 0
	}

	rec.DurationMS = p.Now().Sub(start).Milliseconds()
	if status != 0 {
		rec.Status = status
	}
	rec.Args, rec.Result, rec.Truncated = p.captureToolPayload(p.Capture(), string(args), "")
	p.writeTool(rec)
	if rec.Outcome == ToolOutcomeClientDisconnected {
		return
	}
	writeError(w, status, code, message)
}

func (p *Proxy) captureToolPayload(enabled bool, args, result string) (string, string, bool) {
	if !enabled {
		return "", "", false
	}
	trimmedArgs, argsTruncated := truncateCapture(args)
	trimmedResult, resultTruncated := truncateCapture(result)
	return trimmedArgs, trimmedResult, argsTruncated || resultTruncated
}

func (p *Proxy) writeTool(rec ToolRecord) {
	if p.ToolLogs == nil {
		return
	}
	// Tool logs are bookkeeping, not chat history, so they write on a detached
	// context: a client that hangs up must still leave an audit trail.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.ToolLogs.WriteTool(ctx, rec); err != nil && p.Logger != nil {
		p.Logger.Error("failed to write tool log", "tool", rec.Tool, "error", err)
	}
}
