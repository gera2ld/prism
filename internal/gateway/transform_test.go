package gateway_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gera2ld/prism/internal/gateway"
)

func mustCompile(t *testing.T, pattern, expr string) gateway.Transformer {
	t.Helper()
	tr, err := gateway.CompileTransformer("t", "p1", pattern, 10, expr)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestCompileTransformerRejectsBadInputs(t *testing.T) {
	if _, err := gateway.CompileTransformer("t", "p1", "([bad", 0, `$`); err == nil {
		t.Fatal("expected pattern error")
	}
	if _, err := gateway.CompileTransformer("t", "p1", "^x$", 0, `{{{`); err == nil {
		t.Fatal("expected expression error")
	}
}

func TestTransformerMatches(t *testing.T) {
	tr := mustCompile(t, `^gpt-4`, `$`)
	cases := []struct {
		provider, model string
		want            bool
	}{
		{"p1", "gpt-4", true},
		{"p1", "gpt-4o-mini", true},
		{"p1", "o1", false},
		{"p1", "my-gpt-4", false}, // ^ anchor: no prefix match
		{"p2", "gpt-4", false},    // provider is exact
	}
	for _, c := range cases {
		if got := tr.Matches(c.provider, c.model); got != c.want {
			t.Errorf("Matches(%q, %q) = %v, want %v", c.provider, c.model, got, c.want)
		}
	}
}

func TestTransformerApply(t *testing.T) {
	tr := mustCompile(t, `.*`, `{"model": model, "shaped": true}`)
	out, err := tr.Apply(context.Background(), []byte(`{"model":"gpt-4","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"model":"gpt-4","shaped":true}` {
		t.Fatalf("unexpected output %s", out)
	}
}

func TestTransformerApplyRejectsNonObject(t *testing.T) {
	for _, expr := range []string{`messages`, `42`, `"str"`, `null`} {
		tr := mustCompile(t, `.*`, expr)
		if _, err := tr.Apply(context.Background(), []byte(`{"model":"m","messages":[]}`)); err == nil {
			t.Fatalf("expected error for expression %s", expr)
		}
	}
}

func TestTransformerApplyEvalError(t *testing.T) {
	bad, err := gateway.CompileTransformer("t", "p1", `.*`, 0, `$unknownFunc(model)`)
	if err != nil {
		t.Skip("expression rejected at compile time")
	}
	if _, err := bad.Apply(context.Background(), []byte(`{"model":"m"}`)); err == nil {
		t.Fatal("expected eval error")
	}
}

func TestTransformerApplyCancelledContext(t *testing.T) {
	tr := mustCompile(t, `.*`, `$`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tr.Apply(ctx, []byte(`{"model":"m"}`)); err == nil {
		t.Fatal("expected context error")
	} else if !strings.Contains(err.Error(), `"t"`) {
		t.Fatalf("error should name the transformer, got %v", err)
	}
}
