// Package mcp supervises connections to Model Context Protocol servers so the
// gateway can offer their tools to agents. It knows nothing about PocketBase:
// callers supply plain ServerConfig values and receive plain results, which
// keeps it testable against in-memory transports with no subprocess and no
// network.
package mcp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefinitionHash returns the approval hash of a tool definition: the value
// pinned in the approval ledger.
//
// The hashed member set is exactly the tool's behaviour contract — what it is
// called, what it says about itself, and what it accepts and returns. Including
// description and annotations is the point of the hash: they are the fields a
// server can change to alter how an agent behaves without changing any code,
// and they are also untrusted hints, so a change to either must force a fresh
// review. Meta (_meta) is excluded because it carries per-session protocol
// bookkeeping that changes on ordinary calls and would re-lock every tool
// constantly; Icons is excluded as presentational and an outbound tracking
// channel.
//
// Number literals are preserved via json.Number rather than decoded to float64,
// so two definitions that differ only in a large integer schema bound cannot
// collide. encoding/json emits struct fields in declaration order and map keys
// in sorted order, so the encoding is stable no matter how the server ordered
// its own JSON.
func DefinitionHash(tool *mcpsdk.Tool) (string, error) {
	raw, err := json.Marshal(tool)
	if err != nil {
		return "", fmt.Errorf("marshal tool %q: %w", tool.Name, err)
	}
	return hashEncoded(raw)
}

// hashEncoded hashes an already-encoded MCP tool object.
func hashEncoded(raw []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var probe map[string]any
	if err := decoder.Decode(&probe); err != nil {
		return "", fmt.Errorf("decode tool: %w", err)
	}

	// Field tags fix the member set and its order. A member absent upstream
	// decodes to nil and encodes as null, which is stable, so a tool that
	// simply has no output schema hashes consistently.
	canonical := struct {
		Name         string `json:"name"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		InputSchema  any    `json:"input_schema"`
		OutputSchema any    `json:"output_schema"`
		Annotations  any    `json:"annotations"`
	}{
		Name:         stringMember(probe, "name"),
		Title:        stringMember(probe, "title"),
		Description:  stringMember(probe, "description"),
		InputSchema:  probe["inputSchema"],
		OutputSchema: probe["outputSchema"],
		Annotations:  probe["annotations"],
	}

	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("encode canonical tool: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// stringMember reads an optional string member, treating any non-string as
// absent. A malformed member must not panic the hash, and must not silently
// become a value the operator did not review.
func stringMember(probe map[string]any, key string) string {
	value, ok := probe[key].(string)
	if !ok {
		return ""
	}
	return value
}
