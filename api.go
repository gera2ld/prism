package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/pocketbase/pocketbase/core"

	"github.com/gera2ld/prism/internal/store"
)

// prismAPIPrefix carries the gated data endpoints. Documentation (UI, spec,
// schemas) nests under prismDocsPrefix so the public rule is one shared
// prefix, never a list of matched filenames.
const prismAPIPrefix = "/api/prism"

const prismDocsPrefix = prismAPIPrefix + "/docs"

var superuserSecurity = []map[string][]string{{"superuser": {}}}

// newPrismMux builds the huma sub-mux. Every operation delegates to the
// shared Commands handlers, so API and CLI behavior cannot drift.
func newPrismMux(app core.App, s *store.Store) *http.ServeMux {
	cmds := &Commands{App: app, Store: s}
	mux := http.NewServeMux()
	config := huma.DefaultConfig("Prism", "1.0.0")
	// Docs, spec and schemas nest under the shared public prefix; the data
	// operations use absolute paths under the API prefix.
	config.DocsPath = prismDocsPrefix
	config.OpenAPIPath = prismDocsPrefix + "/openapi"
	config.SchemasPath = prismDocsPrefix + "/schemas"
	config.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"superuser": {
			Type:        "http",
			Scheme:      "bearer",
			Description: "PocketBase superuser token from /api/collections/_superusers/auth-with-password.",
		},
		"clientKey": {
			Type:        "http",
			Scheme:      "bearer",
			Description: "Client API key (sk-…) for the LLM endpoints.",
		},
	}
	// Absolute paths with servers at the root so try-it-out resolves
	// correctly for both the operational and the LLM paths.
	config.Servers = []*huma.Server{{URL: "/"}}
	api := humago.New(mux, config)

	huma.Register(api, huma.Operation{
		OperationID: "list-keys",
		Method:      http.MethodGet,
		Path:        prismAPIPrefix + "/keys",
		Summary:     "List client API keys",
		Description: "Names and status only; secrets are never included.",
		Tags:        []string{"keys"},
		Security:    superuserSecurity,
	}, func(ctx context.Context, _ *struct{}) (*struct {
		Body []store.APIKeyInfo
	}, error) {
		keys, err := cmds.ListKeys()
		if err != nil {
			return nil, huma.Error500InternalServerError("list keys failed", err)
		}
		return &struct {
			Body []store.APIKeyInfo
		}{Body: keys}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "generate-key",
		Method:      http.MethodPost,
		Path:        prismAPIPrefix + "/keys",
		Summary:     "Create a client API key",
		Description: "The secret is returned once and cannot be retrieved again except via reveal.",
		Tags:        []string{"keys"},
		Security:    superuserSecurity,
	}, func(ctx context.Context, input *struct {
		Body struct {
			Name string `json:"name" minLength:"1" doc:"Unique key name"`
		}
	}) (*struct {
		Body struct {
			Secret string `json:"secret" doc:"The new client secret, shown once"`
		}
	}, error) {
		secret, err := cmds.GenerateKey(input.Body.Name)
		if err != nil {
			return nil, huma.Error500InternalServerError("create key failed", err)
		}
		out := &struct {
			Body struct {
				Secret string `json:"secret" doc:"The new client secret, shown once"`
			}
		}{}
		out.Body.Secret = secret
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "reveal-key",
		Method:      http.MethodGet,
		Path:        prismAPIPrefix + "/keys/{name}/reveal",
		Summary:     "Reveal a client API key",
		Description: "Decrypts the stored secret for copying. Auth never uses this path.",
		Tags:        []string{"keys"},
		Security:    superuserSecurity,
	}, func(ctx context.Context, input *struct {
		Name string `path:"name" doc:"Key name"`
	}) (*struct {
		Body struct {
			Secret string `json:"secret" doc:"The client secret"`
		}
	}, error) {
		secret, err := cmds.RevealKey(input.Name)
		if err != nil {
			return nil, storeError(err)
		}
		out := &struct {
			Body struct {
				Secret string `json:"secret" doc:"The client secret"`
			}
		}{}
		out.Body.Secret = secret
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-providers",
		Method:      http.MethodGet,
		Path:        prismAPIPrefix + "/providers",
		Summary:     "List providers",
		Description: "Names, URLs and status only; tokens are never included.",
		Tags:        []string{"providers"},
		Security:    superuserSecurity,
	}, func(ctx context.Context, _ *struct{}) (*struct {
		Body []store.ProviderInfo
	}, error) {
		providers, err := cmds.ListProviders()
		if err != nil {
			return nil, huma.Error500InternalServerError("list providers failed", err)
		}
		return &struct {
			Body []store.ProviderInfo
		}{Body: providers}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "reveal-provider-token",
		Method:      http.MethodGet,
		Path:        prismAPIPrefix + "/providers/{name}/reveal",
		Summary:     "Reveal a provider token",
		Description: "Decrypts the stored upstream token for copying.",
		Tags:        []string{"providers"},
		Security:    superuserSecurity,
	}, func(ctx context.Context, input *struct {
		Name string `path:"name" doc:"Provider name"`
	}) (*struct {
		Body struct {
			Token string `json:"token" doc:"The upstream token"`
		}
	}, error) {
		token, err := cmds.RevealProviderToken(input.Name)
		if err != nil {
			return nil, storeError(err)
		}
		out := &struct {
			Body struct {
				Token string `json:"token" doc:"The upstream token"`
			}
		}{}
		out.Body.Token = token
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "export-routes",
		Method:      http.MethodGet,
		Path:        prismAPIPrefix + "/routes/export",
		Summary:     "Export routes as CSV",
		Description: "Same bytes as the route export CLI command.",
		Tags:        []string{"routes"},
		Security:    superuserSecurity,
		Responses: map[string]*huma.Response{
			"200": {
				Description: "Routes CSV",
				Content: map[string]*huma.MediaType{
					"text/csv": {},
				},
			},
		},
	}, func(ctx context.Context, _ *struct{}) (*struct {
		ContentType string `header:"Content-Type"`
		Body        []byte
	}, error) {
		data, err := cmds.ExportRoutes()
		if err != nil {
			return nil, huma.Error500InternalServerError("export routes failed", err)
		}
		return &struct {
			ContentType string `header:"Content-Type"`
			Body        []byte
		}{ContentType: "text/csv", Body: data}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "import-routes",
		Method:      http.MethodPost,
		Path:        prismAPIPrefix + "/routes/import",
		Summary:     "Import routes from CSV",
		Description: "Merges the uploaded routing table; same validation as the route import CLI command.",
		Tags:        []string{"routes"},
		Security:    superuserSecurity,
	}, func(ctx context.Context, input *struct {
		Prune   bool `query:"prune" doc:"Delete routes absent from the CSV"`
		RawBody huma.MultipartFormFiles[struct {
			File huma.FormFile `form:"file" required:"true" doc:"Routes CSV file"`
		}]
	}) (*struct{}, error) {
		formData := input.RawBody.Data()
		data, err := io.ReadAll(formData.File)
		if err != nil {
			return nil, huma.Error400BadRequest("read upload failed", err)
		}
		if err := cmds.ImportRoutes(data, input.Prune); err != nil {
			return nil, huma.Error400BadRequest("import routes failed", err)
		}
		return &struct{}{}, nil
	})

	registerLLMDocs(api)

	return mux
}

// storeError maps store failures to HTTP status: unknown names are 404,
// everything else (e.g. undecryptable ciphertext) is a 500.
func storeError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return huma.Error404NotFound("not found", err)
	}
	return huma.Error500InternalServerError("internal error", err)
}

// isPublicDocsPath reports whether a request path falls under the shared
// public docs prefix. The auth split is this one prefix check, never a list
// of matched filenames.
func isPublicDocsPath(path string) bool {
	return path == prismDocsPrefix || strings.HasPrefix(path, prismDocsPrefix+"/")
}

// registerPrismAPI mounts the huma sub-mux: everything under /api/prism/docs
// is public documentation, everything else under /api/prism requires
// superuser auth (e.Auth is already loaded by PocketBase's global middleware).
func registerPrismAPI(e *core.ServeEvent, mux *http.ServeMux) {
	e.Router.Any(prismAPIPrefix+"/{path...}", func(e *core.RequestEvent) error {
		if !isPublicDocsPath(e.Request.URL.Path) {
			if e.Auth == nil {
				return e.UnauthorizedError("Superuser authorization is required.", nil)
			}
			if !e.Auth.IsSuperuser() {
				return e.ForbiddenError("Superuser authorization is required.", nil)
			}
		}
		mux.ServeHTTP(e.Response, e.Request)
		return nil
	})
}
