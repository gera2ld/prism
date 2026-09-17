package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPrismAPISpec(t *testing.T) {
	mux := newPrismMux(nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/prism/docs/openapi.json", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("openapi.json status = %d", rec.Code)
	}
	var spec struct {
		OpenAPI string `json:"openapi"`
		Paths   map[string]struct {
			Get  *struct{} `json:"get"`
			Post *struct{} `json:"post"`
		} `json:"paths"`
		Servers []struct {
			URL string `json:"url"`
		} `json:"servers"`
		Components struct {
			SecuritySchemes map[string]struct {
				Scheme string `json:"scheme"`
			} `json:"securitySchemes"`
		} `json:"components"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.OpenAPI == "" {
		t.Fatal("missing openapi version")
	}
	want := map[string][]string{
		"/api/prism/keys":                    {"get", "post"},
		"/api/prism/keys/{name}/reveal":      {"get"},
		"/api/prism/providers":               {"get"},
		"/api/prism/providers/{name}/reveal": {"get"},
		"/api/prism/routes/export":           {"get"},
		"/api/prism/routes/import":           {"post"},
		"/v1/chat/completions":               {"post"},
		"/v1/models":                         {"get"},
	}
	for path, methods := range want {
		got, ok := spec.Paths[path]
		if !ok {
			t.Fatalf("spec missing path %s (have %v)", path, keys(spec.Paths))
		}
		for _, m := range methods {
			var present bool
			switch m {
			case "get":
				present = got.Get != nil
			case "post":
				present = got.Post != nil
			}
			if !present {
				t.Fatalf("spec path %s missing %s", path, m)
			}
		}
	}
	if len(spec.Servers) == 0 || spec.Servers[0].URL != "/" {
		t.Fatalf("unexpected servers: %+v", spec.Servers)
	}
	if spec.Components.SecuritySchemes["superuser"].Scheme != "bearer" {
		t.Fatalf("missing superuser security scheme: %+v", spec.Components.SecuritySchemes)
	}
	if spec.Components.SecuritySchemes["clientKey"].Scheme != "bearer" {
		t.Fatalf("missing clientKey security scheme: %+v", spec.Components.SecuritySchemes)
	}
	raw := rec.Body.String()
	if !strings.Contains(raw, "text/event-stream") {
		t.Fatal("chat completions missing text/event-stream response")
	}

	// Docs UI is served from the shared public prefix.
	req = httptest.NewRequest(http.MethodGet, "/api/prism/docs", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("docs status = %d", rec.Code)
	}

	// The spec no longer lives at the root of the API prefix.
	req = httptest.NewRequest(http.MethodGet, "/api/prism/openapi.json", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for /api/prism/openapi.json, got %d", rec.Code)
	}
}

func TestIsPublicDocsPath(t *testing.T) {
	for _, p := range []string{
		"/api/prism/docs",
		"/api/prism/docs/openapi.json",
		"/api/prism/docs/openapi.yaml",
		"/api/prism/docs/schemas/Foo.json",
	} {
		if !isPublicDocsPath(p) {
			t.Fatalf("expected public: %s", p)
		}
	}
	for _, p := range []string{
		"/api/prism/keys",
		"/api/prism/openapi.json",
		"/api/prism/schemas/Foo.json",
		"/api/prism/docs-evil",
		"/api/prism2/docs",
	} {
		if isPublicDocsPath(p) {
			t.Fatalf("expected gated: %s", p)
		}
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
