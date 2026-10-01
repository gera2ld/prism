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

	"github.com/gera2ld/prism/internal/gateway"
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

	registerToolAPI(api, cmds)
	registerLLMDocs(api)

	return mux
}

// registerToolAPI exposes the operator half of the tools surface: the MCP
// server registry, the per-tool approval ledger, and the conduit tool list.
// The agent-facing half lives on /v1 and is documented in llm_docs.go.
func registerToolAPI(api huma.API, cmds *Commands) {
	huma.Register(api, huma.Operation{
		OperationID: "list-mcp-servers",
		Method:      http.MethodGet,
		Path:        prismAPIPrefix + "/mcp-servers",
		Summary:     "List MCP servers",
		Description: "Names, transports and endpoints only; env and header values are never included.",
		Tags:        []string{"mcp"},
		Security:    superuserSecurity,
	}, func(ctx context.Context, _ *struct{}) (*struct {
		Body []store.MCPServerInfo
	}, error) {
		servers, err := cmds.ListMCPServers()
		if err != nil {
			return nil, huma.Error500InternalServerError("list mcp servers failed", err)
		}
		return &struct {
			Body []store.MCPServerInfo
		}{Body: servers}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "reveal-mcp-secrets",
		Method:      http.MethodGet,
		Path:        prismAPIPrefix + "/mcp-servers/{name}/reveal",
		Summary:     "Reveal an MCP server's secrets",
		Description: "Decrypts the stored env and header values. Neither is ever used for authentication; both are outbound credentials the gateway presents to the server.",
		Tags:        []string{"mcp"},
		Security:    superuserSecurity,
	}, func(ctx context.Context, input *struct {
		Name string `path:"name" doc:"MCP server name"`
	}) (*struct {
		Body struct {
			Secrets map[string]string `json:"secrets" doc:"Keys are prefixed with env. or headers."`
		}
	}, error) {
		secrets, err := cmds.RevealMCPSecrets(input.Name)
		if err != nil {
			return nil, storeError(err)
		}
		out := &struct {
			Body struct {
				Secrets map[string]string `json:"secrets" doc:"Keys are prefixed with env. or headers."`
			}
		}{}
		out.Body.Secrets = secrets
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-mcp-tools",
		Method:      http.MethodGet,
		Path:        prismAPIPrefix + "/mcp-servers/{name}/tools",
		Summary:     "List an MCP server's tools and their approval state",
		Description: "Connects to the server and returns every tool it publishes, each with the definition hash an approval would be pinned to. Status is unapproved, approved, changed (approved under an older hash, so withheld from agents) or orphaned (approved but no longer published). Nothing is exposed to an agent until approved.",
		Tags:        []string{"mcp"},
		Security:    superuserSecurity,
	}, func(ctx context.Context, input *struct {
		Name string `path:"name" doc:"MCP server name"`
	}) (*struct {
		Body []store.MCPToolView
	}, error) {
		tools, err := cmds.ListMCPTools(ctx, input.Name)
		if err != nil {
			return nil, storeError(err)
		}
		return &struct {
			Body []store.MCPToolView
		}{Body: tools}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "approve-mcp-tool",
		Method:      http.MethodPost,
		Path:        prismAPIPrefix + "/mcp-servers/{name}/tools/{tool}/approve",
		Summary:     "Approve an MCP tool",
		Description: "Pins the tool's current definition hash, which is what makes it callable. The reviewed hash is required and must still match what the server publishes, so a definition that changed after review is refused with 409 instead of being approved by accident.",
		Tags:        []string{"mcp"},
		Security:    superuserSecurity,
	}, func(ctx context.Context, input *struct {
		Name string `path:"name" doc:"MCP server name"`
		Tool string `path:"tool" doc:"Tool name as the server publishes it"`
		Body struct {
			DefinitionHash string `json:"definition_hash" minLength:"64" maxLength:"64" doc:"The hash read from the tool list, being approved"`
		}
	}) (*struct{}, error) {
		if err := cmds.ApproveMCPTool(ctx, input.Name, input.Tool, input.Body.DefinitionHash); err != nil {
			return nil, toolApprovalError(err)
		}
		return &struct{}{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "revoke-mcp-tool",
		Method:      http.MethodDelete,
		Path:        prismAPIPrefix + "/mcp-servers/{name}/tools/{tool}/approve",
		Summary:     "Revoke an MCP tool approval",
		Description: "Removes the approval, hiding the tool from agents again. The definition hash is forgotten, so re-approving later requires a fresh review.",
		Tags:        []string{"mcp"},
		Security:    superuserSecurity,
	}, func(ctx context.Context, input *struct {
		Name string `path:"name" doc:"MCP server name"`
		Tool string `path:"tool" doc:"Tool name as the server publishes it"`
	}) (*struct{}, error) {
		if err := cmds.RevokeMCPTool(input.Name, input.Tool); err != nil {
			return nil, storeError(err)
		}
		return &struct{}{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "refresh-mcp-server",
		Method:      http.MethodPost,
		Path:        prismAPIPrefix + "/mcp-servers/{name}/refresh",
		Summary:     "Reconnect and relist an MCP server",
		Description: "Drops the live session so the next use redials and re-reads the tool list. The escape hatch for a server that changed its tools without sending a tools/list_changed notification.",
		Tags:        []string{"mcp"},
		Security:    superuserSecurity,
	}, func(ctx context.Context, input *struct {
		Name string `path:"name" doc:"MCP server name"`
	}) (*struct{}, error) {
		if err := cmds.RefreshMCPServer(input.Name); err != nil {
			return nil, storeError(err)
		}
		return &struct{}{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-tools",
		Method:      http.MethodGet,
		Path:        prismAPIPrefix + "/tools",
		Summary:     "List conduit tools",
		Description: "Operator-authored tools built from conduit definitions, enabled or not. Description and input schema are derived from the definition, so they cannot disagree with it.",
		Tags:        []string{"tools"},
		Security:    superuserSecurity,
	}, func(ctx context.Context, _ *struct{}) (*struct {
		Body []store.ConduitToolInfo
	}, error) {
		tools, err := cmds.ListTools()
		if err != nil {
			return nil, huma.Error500InternalServerError("list tools failed", err)
		}
		return &struct {
			Body []store.ConduitToolInfo
		}{Body: tools}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "validate-tool",
		Method:      http.MethodPost,
		Path:        prismAPIPrefix + "/tools/validate",
		Summary:     "Validate a conduit definition",
		Description: "Parses a definition and reports what the agent would be shown, without saving anything. The same check runs when a tool is saved.",
		Tags:        []string{"tools"},
		Security:    superuserSecurity,
	}, func(ctx context.Context, input *struct {
		Body struct {
			Definition string `json:"definition" minLength:"1" doc:"Conduit definition as YAML or JSON"`
		}
	}) (*struct {
		Body struct {
			Valid bool `json:"valid"`
		}
	}, error) {
		if err := cmds.ValidateTool([]byte(input.Body.Definition)); err != nil {
			return nil, huma.Error400BadRequest("invalid conduit definition", err)
		}
		out := &struct {
			Body struct {
				Valid bool `json:"valid"`
			}
		}{}
		out.Body.Valid = true
		return out, nil
	})
}

// toolApprovalError maps approval failures to statuses. A stale hash is a
// conflict, not a bad request: the request was well formed, but the definition
// it referred to is no longer what the server publishes.
func toolApprovalError(err error) error {
	if errors.Is(err, store.ErrApprovalStale) {
		return huma.Error409Conflict("tool definition changed since it was reviewed", err)
	}
	if errors.Is(err, gateway.ErrUnknownTool) {
		return huma.Error404NotFound("no such tool on that server", err)
	}
	return huma.Error500InternalServerError("approve tool failed", err)
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
