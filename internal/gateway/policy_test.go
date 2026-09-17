package gateway_test

import (
	"errors"
	"testing"

	"github.com/gera2ld/prism/internal/gateway"
)

func mustPolicy(t *testing.T, alias, provider, model string) gateway.Key {
	t.Helper()
	a, p, m, err := gateway.CompilePolicy(alias, provider, model)
	if err != nil {
		t.Fatal(err)
	}
	return gateway.Key{ID: "k", AliasPattern: a, ProviderPattern: p, ModelPattern: m}
}

func TestCompilePolicy(t *testing.T) {
	a, p, m, err := gateway.CompilePolicy("", "", "")
	if err != nil || a != nil || p != nil || m != nil {
		t.Fatalf("empty must compile to nils, got %v %v %v %v", a, p, m, err)
	}
	for _, exprs := range [][3]string{{"([", "", ""}, {"", "([", ""}, {"", "", "(["}} {
		if _, _, _, err := gateway.CompilePolicy(exprs[0], exprs[1], exprs[2]); err == nil {
			t.Fatalf("expected error for %v", exprs)
		}
	}
}

func TestKeyAllows(t *testing.T) {
	target := gateway.Target{ProviderID: "p1", ProviderName: "prov", Model: "gpt-x"}
	cases := []struct {
		name  string
		key   gateway.Key
		alias string
		want  bool
	}{
		{"unrestricted", gateway.Key{ID: "k"}, "alias", true},
		{"alias pass", mustPolicy(t, `^qwen/`, "", ""), "qwen/qwen3:free", true},
		{"alias block", mustPolicy(t, `^qwen/`, "", ""), "gpt-4", false},
		{"provider pass", mustPolicy(t, "", `^prov`, ""), "alias", true},
		{"provider block", mustPolicy(t, "", `^other`, ""), "alias", false},
		{"model pass", mustPolicy(t, "", "", `gpt-`), "alias", true},
		{"model block", mustPolicy(t, "", "", `^o1$`), "alias", false},
		{"conjunction block", mustPolicy(t, `.*`, `^prov`, `^o1$`), "alias", false},
		{"conjunction pass", mustPolicy(t, `.*`, `^prov`, `gpt-`), "alias", true},
	}
	for _, c := range cases {
		if got := c.key.Allows(c.alias, target); got != c.want {
			t.Errorf("%s: Allows = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAuthorizePicksFirstAllowed(t *testing.T) {
	key := mustPolicy(t, "", "", `mirror`)
	targets := []gateway.Target{
		{ProviderID: "p1", ProviderName: "prov", Model: "gpt-x"},
		{ProviderID: "p2", ProviderName: "prov2", Model: "gpt-x-mirror"},
	}
	got, err := gateway.Authorize(key, "alias", targets)
	if err != nil || got.ProviderID != "p2" {
		t.Fatalf("got %+v err=%v", got, err)
	}
	if _, err := gateway.Authorize(mustPolicy(t, "", "", `^nope$`), "alias", targets); !errors.Is(err, gateway.ErrForbidden) {
		t.Fatalf("expected ErrForbidden, got %v", err)
	}
}
