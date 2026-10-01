package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// rawHash hashes a literal tool object the way a server would have sent it, so
// these cases can express key ordering and number formatting directly.
func rawHash(t *testing.T, body string) string {
	t.Helper()
	hash, err := hashEncoded([]byte(body))
	if err != nil {
		t.Fatalf("hashEncoded(%s): %v", body, err)
	}
	return hash
}

func TestDefinitionHashIsStableAcrossKeyOrder(t *testing.T) {
	// Two encodings of the same tool, keys in a different order. A server that
	// reorders its JSON must not re-lock an approved tool.
	a := rawHash(t, `{"name":"read","description":"Reads a file.","inputSchema":{"type":"object","properties":{"path":{"type":"string"}}}}`)
	b := rawHash(t, `{"description":"Reads a file.","inputSchema":{"properties":{"path":{"type":"string"}},"type":"object"},"name":"read"}`)
	if a != b {
		t.Fatalf("key order changed the hash:\n%s\n%s", a, b)
	}
}

func TestDefinitionHashIgnoresExcludedMembers(t *testing.T) {
	base := rawHash(t, `{"name":"read","description":"Reads.","inputSchema":{"type":"object"}}`)
	// _meta carries per-session protocol bookkeeping and icons are presentational
	// and an outbound tracking channel, so neither may re-lock a tool. Note
	// that title is *not* here: it is in the member set, since a changed tool
	// means a changed tool whether or not the change is cosmetic.
	for _, extra := range []string{
		`,"_meta":{"progressToken":"abc"}`,
		`,"icons":[{"src":"https://tracker.example/pixel.png"}]`,
	} {
		if got := rawHash(t, `{"name":"read","description":"Reads.","inputSchema":{"type":"object"}`+extra+`}`); got != base {
			t.Fatalf("member %s changed the hash: %s != %s", extra, got, base)
		}
	}
}

func TestDefinitionHashCoversBehaviorContract(t *testing.T) {
	base := rawHash(t, `{"name":"read","description":"Reads.","inputSchema":{"type":"object"}}`)
	// Each of these is a change an agent's behavior could depend on, so each
	// must produce a different hash. Description is the prompt-injection vector;
	// annotations is how a server flips destructiveHint.
	changed := map[string]string{
		"description":   `{"name":"read","description":"Reads everything.","inputSchema":{"type":"object"}}`,
		"name":          `{"name":"write","description":"Reads.","inputSchema":{"type":"object"}}`,
		"title":         `{"name":"read","title":"Read","description":"Reads.","inputSchema":{"type":"object"}}`,
		"input_schema":  `{"name":"read","description":"Reads.","inputSchema":{"type":"object","required":["path"]}}`,
		"output_schema": `{"name":"read","description":"Reads.","inputSchema":{"type":"object"},"outputSchema":{"type":"object"}}`,
		"annotations":   `{"name":"read","description":"Reads.","inputSchema":{"type":"object"},"annotations":{"destructiveHint":true}}`,
	}
	for member, body := range changed {
		if got := rawHash(t, body); got == base {
			t.Fatalf("changing %s did not change the hash (%s)", member, got)
		}
	}
}

func TestDefinitionHashPreservesNumberLiterals(t *testing.T) {
	// The reason numbers are decoded with UseNumber: a float64 round trip would
	// make these two hash alike even though the bounds differ, and a large
	// integer would silently lose precision.
	a := rawHash(t, `{"name":"t","inputSchema":{"type":"object","properties":{"n":{"maximum":9007199254740993}}}}`)
	b := rawHash(t, `{"name":"t","inputSchema":{"type":"object","properties":{"n":{"maximum":9007199254740992}}}}`)
	if a == b {
		t.Fatalf("differing large integers collided on %s", a)
	}
	// The same literal twice must of course agree.
	c := rawHash(t, `{"name":"t","inputSchema":{"type":"object","properties":{"n":{"maximum":9007199254740993}}}}`)
	if a != c {
		t.Fatalf("identical definitions hashed differently: %s != %s", a, c)
	}
}

func TestDefinitionHashHandlesMissingMembers(t *testing.T) {
	// A tool with no description, schema or annotations is legal, and must hash
	// stably rather than panic.
	hash := rawHash(t, `{"name":"ping"}`)
	if len(hash) != 64 {
		t.Fatalf("hash = %q, want 64 hex chars", hash)
	}
	if hash != rawHash(t, `{"name":"ping"}`) {
		t.Fatal("hash is not deterministic for a minimal tool")
	}
}

func TestDefinitionHashOfSDKTool(t *testing.T) {
	// The exported entry point must agree with hashing the wire encoding of the
	// same tool, since that is what a server actually produces.
	tool := &mcpsdk.Tool{
		Name:        "echo",
		Description: "Echoes its input.",
		InputSchema: map[string]any{"type": "object"},
	}
	fromSDK, err := DefinitionHash(tool)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(tool)
	if err != nil {
		t.Fatal(err)
	}
	fromWire := rawHash(t, string(encoded))
	if fromSDK != fromWire {
		t.Fatalf("DefinitionHash = %s, wire hash = %s", fromSDK, fromWire)
	}
	if strings.TrimSpace(fromSDK) == "" {
		t.Fatal("empty hash")
	}
}

func TestHashEncodedRejectsGarbage(t *testing.T) {
	if _, err := hashEncoded([]byte(`not json`)); err == nil {
		t.Fatal("expected an error for a non-JSON tool")
	}
}
