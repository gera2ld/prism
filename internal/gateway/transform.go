package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/recolabs/gnata"
)

// transformTimeout bounds a single JSONata evaluation so a runaway
// expression cannot hang the proxy.
const transformTimeout = 5 * time.Second

// Transformer reshapes the upstream-bound request body for one provider and
// the upstream models its pattern matches. Compiled once at load; safe for
// concurrent use.
type Transformer struct {
	Name       string
	ProviderID string
	Pattern    *regexp.Regexp
	Priority   float64
	expr       *gnata.Expression
}

// CompileTransformer validates and compiles a transformer definition.
// pattern is an unanchored RE2 regexp matched against the upstream model
// name (^…$ for exact, ^… for prefix). expression must be JSONata.
func CompileTransformer(name, providerID, pattern string, priority float64, expression string) (Transformer, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return Transformer{}, fmt.Errorf("bad model pattern: %w", err)
	}
	expr, err := gnata.Compile(expression)
	if err != nil {
		return Transformer{}, fmt.Errorf("bad expression: %w", err)
	}
	return Transformer{Name: name, ProviderID: providerID, Pattern: re, Priority: priority, expr: expr}, nil
}

// Matches reports whether the transformer applies to this resolved target.
// Provider matches exactly; the model pattern is an unanchored regexp.
func (t Transformer) Matches(providerID, model string) bool {
	return t.ProviderID == providerID && t.Pattern.MatchString(model)
}

// Apply evaluates the expression against the request body. The result must
// be a JSON object; anything else is a fail-closed error.
func (t Transformer) Apply(ctx context.Context, body []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, transformTimeout)
	defer cancel()
	result, err := t.expr.EvalBytes(ctx, body)
	if err != nil {
		return nil, fmt.Errorf("transformer %q: %w", t.Name, err)
	}
	out, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("transformer %q: %w", t.Name, err)
	}
	if !isJSONObject(out) {
		return nil, fmt.Errorf("transformer %q must produce a JSON object", t.Name)
	}
	return out, nil
}

func isJSONObject(b []byte) bool {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var obj map[string]json.RawMessage
	return json.Unmarshal(trimmed, &obj) == nil
}
