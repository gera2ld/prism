package store

import (
	"database/sql"
	"errors"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/migrations"
)

func init() {
	// The name is the migration's primary key, not a label: renaming or
	// re-dating it makes every existing database re-run createAll, which then
	// fails on the duplicate collection names.
	migrations.Register(createAll, nil, "1800000000_gateway.go")
	// The tools surface arrived after the first release, so it is a separate
	// migration: folding it into createAll would leave every existing database
	// without these collections, since createAll never runs twice.
	migrations.Register(createTools, nil, "1800000001_tools.go")
}

// buildSettingsCollection assembles the collection from the canonical schema
// in settings.go.
func buildSettingsCollection() *core.Collection {
	settings := core.NewBaseCollection(settingsCollection)
	for _, makeField := range settingsFields() {
		settings.Fields.Add(makeField())
	}
	return settings
}

// ensureSettingsRow creates or backfills the singleton row via the typed
// Settings manager. Idempotent.
func ensureSettingsRow(app core.App) error {
	if _, err := ensureSettingsCollection(app); err != nil {
		return err
	}
	settings, dirty := readSettings(app)
	if !dirty {
		return nil
	}
	return writeSettings(app, settings)
}

func createAll(app core.App) error {
	providers := core.NewBaseCollection("providers")
	providers.Fields.Add(
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
		&core.TextField{Name: "name", Required: true, Pattern: `^[a-zA-Z0-9_-]+$`},
		&core.URLField{Name: "base_url", Required: true},
		&core.TextField{Name: "api_key", Hidden: true, Max: 16384},
		&core.BoolField{Name: "enabled"},
	)
	providers.AddIndex("idx_providers_name", true, "name", "")
	if err := app.Save(providers); err != nil {
		return err
	}

	keys := core.NewBaseCollection("api_keys")
	keys.Fields.Add(
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
		&core.TextField{Name: "name", Required: true},
		&core.TextField{Name: "key_hash", Hidden: true, Pattern: `^[a-f0-9]{64}$`},
		&core.TextField{Name: "key_plain", Hidden: true, Max: 256},
		&core.BoolField{Name: "enabled"},
		&core.TextField{Name: "alias_pattern", Max: 256, Help: "Optional RE2 whitelist on the client model value. Empty allows all."},
		&core.TextField{Name: "provider_pattern", Max: 256, Help: "Optional RE2 whitelist on the resolved provider name. Empty allows all."},
		&core.TextField{Name: "model_pattern", Max: 256, Help: "Optional RE2 whitelist on the upstream model. Empty allows all."},
	)
	keys.AddIndex("idx_api_keys_hash", true, "key_hash", "")
	if err := app.Save(keys); err != nil {
		return err
	}

	routes := core.NewBaseCollection("routes")
	routes.Fields.Add(
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
		&core.TextField{Name: "alias", Required: true, Pattern: `^[^\s]+$`},
		&core.RelationField{Name: "provider", CollectionId: providers.Id, Required: true, CascadeDelete: true},
		&core.TextField{Name: "upstream_model", Required: true},
		&core.NumberField{Name: "priority", OnlyInt: true},
		&core.BoolField{Name: "enabled"},
	)
	routes.AddIndex("idx_routes_alias_priority", false, "alias, priority", "")
	if err := app.Save(routes); err != nil {
		return err
	}

	logs := core.NewBaseCollection("request_logs")
	logs.Fields.Add(
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.DateField{Name: "started_at"},
		&core.RelationField{Name: "api_key", CollectionId: keys.Id},
		&core.TextField{Name: "api_key_name"},
		&core.TextField{Name: "alias"},
		&core.RelationField{Name: "provider", CollectionId: providers.Id, MaxSelect: 1},
		&core.TextField{Name: "provider_name"},
		&core.TextField{Name: "upstream_model"},
		&core.TextField{Name: "transformer", Max: 256},
		&core.BoolField{Name: "stream"},
		&core.JSONField{Name: "prompt_tokens", MaxSize: 32},
		&core.JSONField{Name: "completion_tokens", MaxSize: 32},
		&core.JSONField{Name: "total_tokens", MaxSize: 32},
		&core.JSONField{Name: "cached_tokens", MaxSize: 32},
		&core.JSONField{Name: "ttft_ms", MaxSize: 32},
		&core.NumberField{Name: "total_ms", OnlyInt: true},
		&core.NumberField{Name: "status", OnlyInt: true},
		&core.TextField{Name: "error"},
		&core.TextField{Name: "outcome", Max: 32},
		&core.TextField{Name: "finish_reason", Max: 64},
	)
	logs.AddIndex("idx_request_logs_created", false, "created", "")
	if err := app.Save(logs); err != nil {
		return err
	}

	bodies := core.NewBaseCollection("request_bodies")
	bodies.Fields.Add(
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.RelationField{Name: "log", CollectionId: logs.Id, Required: true, CascadeDelete: true},
		&core.TextField{Name: "request", Max: 262144},
		&core.TextField{Name: "response", Max: 262144},
		&core.BoolField{Name: "truncated"},
	)
	bodies.AddIndex("idx_request_bodies_log", true, "log", "")
	bodies.AddIndex("idx_request_bodies_created", false, "created", "")
	if err := app.Save(bodies); err != nil {
		return err
	}

	transformers := core.NewBaseCollection("transformers")
	transformers.Fields.Add(
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
		&core.TextField{Name: "name", Required: true},
		&core.RelationField{Name: "provider", CollectionId: providers.Id, Required: true, CascadeDelete: true},
		&core.TextField{Name: "model_pattern", Required: true, Max: 256, Help: "RE2 regexp on the upstream model (unanchored): ^gpt-4 for prefix, ^model$ for exact."},
		&core.NumberField{Name: "priority", OnlyInt: true},
		&core.BoolField{Name: "enabled"},
		&core.TextField{Name: "expression", Required: true, Max: 16384, Help: "JSONata producing the new request body object."},
	)
	transformers.AddIndex("idx_transformers_name", true, "name", "")
	transformers.AddIndex("idx_transformers_provider_priority", false, "provider, priority", "")
	if err := app.Save(transformers); err != nil {
		return err
	}

	if err := app.Save(buildSettingsCollection()); err != nil {
		return err
	}
	if err := ensureSettingsRow(app); err != nil {
		return err
	}
	return ensureUsageViews(app, usageViews...)
}

// toolsUsageView backs the read-only invocation view. Like the others it
// prefers the gateway-side started_at, falling back to the row-write created
// for rows logged before started_at existed. It is registered by createTools
// rather than alongside the other views because it reads tool_logs, which
// createAll has not created yet on a fresh database.
var toolsUsageView = usageView{
	name: "tools_usage",
	// The tool name is the id: the catalog enforces name uniqueness across
	// both sources, so it is a stable key for a grouped aggregate where there
	// is no single underlying row to borrow an id from. It also matches how
	// the other views key on the measured entity's own id.
	query: `SELECT l.tool AS id, l.tool AS tool, l.source AS source, l.server AS server,` +
		` MAX(COALESCE(NULLIF(l.started_at, ''), l.created)) AS last_used,` +
		` COUNT(CASE WHEN l.outcome = 'completed' THEN 1 END) AS success_requests,` +
		` COUNT(CASE WHEN l.outcome <> 'completed' THEN 1 END) AS fail_requests` +
		` FROM tool_logs AS l GROUP BY l.tool, l.source, l.server`,
}

// createTools builds everything behind the tools surface: configured MCP
// servers, the per-tool approval ledger, operator-authored conduit tools, and
// the invocation log. Collections are created in dependency order because each
// relation field needs the target collection id, and api_keys is resolved by
// name because it was created by an earlier migration.
func createTools(app core.App) error {
	keys, err := app.FindCollectionByNameOrId("api_keys")
	if err != nil {
		return err
	}

	servers := core.NewBaseCollection("mcp_servers")
	servers.Fields.Add(
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
		&core.TextField{Name: "name", Required: true, Pattern: `^[a-zA-Z0-9_-]+$`},
		&core.SelectField{Name: "transport", Required: true, MaxSelect: 1, Values: []string{"stdio", "http"}, Help: "How the gateway reaches the server."},
		&core.TextField{Name: "command", Max: 1024, Help: "stdio only: executable to spawn."},
		&core.JSONField{Name: "args", MaxSize: 16384, Help: "stdio only: array of arguments."},
		&core.JSONField{Name: "env", MaxSize: 16384, Help: "stdio only: object of environment variables, encrypted at rest."},
		&core.TextField{Name: "url", Max: 2048, Help: "http only: endpoint URL."},
		&core.JSONField{Name: "headers", MaxSize: 16384, Help: "http only: request headers, encrypted at rest."},
		&core.BoolField{Name: "enabled", Help: "Master switch. Tools also require per-tool approval."},
	)
	servers.AddIndex("idx_mcp_servers_name", true, "name", "")
	if err := app.Save(servers); err != nil {
		return err
	}

	// The approval ledger. There is deliberately no enabled flag: the row's
	// existence is the toggle and definition_hash is the pin, so a tool
	// redefined upstream stops matching and drops out of the catalog.
	grants := core.NewBaseCollection("mcp_tool_grants")
	grants.Fields.Add(
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "approved", OnCreate: true, OnUpdate: true},
		&core.RelationField{Name: "server", CollectionId: servers.Id, Required: true, CascadeDelete: true},
		&core.TextField{Name: "tool", Required: true, Max: 256},
		&core.TextField{Name: "definition_hash", Required: true, Pattern: `^[a-f0-9]{64}$`, Help: "Hash of the last approved tool definition."},
	)
	grants.AddIndex("idx_mcp_tool_grants_tool", true, "server, tool", "")
	if err := app.Save(grants); err != nil {
		return err
	}

	// Conduit tools. No description column: it is derived from the parsed
	// definition, so the description cannot drift from the schema the agent
	// is shown. The definition is text, not JSON, because YAML is not JSON —
	// a JSONField would re-encode the document as a quoted string.
	tools := core.NewBaseCollection("tools")
	tools.Fields.Add(
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
		&core.TextField{Name: "name", Required: true, Pattern: `^[a-zA-Z0-9_-]{1,64}$`, Help: "The name the agent calls."},
		&core.TextField{Name: "definition", Required: true, Max: 131072, Help: "Conduit definition (YAML or JSON)."},
		&core.BoolField{Name: "enabled"},
	)
	tools.AddIndex("idx_tools_name", true, "name", "")
	if err := app.Save(tools); err != nil {
		return err
	}

	logs := core.NewBaseCollection("tool_logs")
	logs.Fields.Add(
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.DateField{Name: "started_at"},
		&core.RelationField{Name: "api_key", CollectionId: keys.Id, MaxSelect: 1},
		&core.TextField{Name: "api_key_name"},
		&core.TextField{Name: "tool", Max: 128},
		&core.TextField{Name: "source", Max: 32},
		&core.TextField{Name: "server", Max: 128},
		&core.TextField{Name: "arguments", Max: 262144},
		&core.TextField{Name: "result", Max: 262144},
		&core.BoolField{Name: "truncated"},
		&core.NumberField{Name: "duration_ms", OnlyInt: true},
		&core.NumberField{Name: "status", OnlyInt: true},
		&core.TextField{Name: "outcome", Max: 32},
		&core.TextField{Name: "error"},
	)
	logs.AddIndex("idx_tool_logs_created", false, "created", "")
	logs.AddIndex("idx_tool_logs_tool", false, "tool", "")
	if err := app.Save(logs); err != nil {
		return err
	}
	return ensureUsageViews(app, toolsUsageView)
}

// usageViews backs the read-only last-usage collections. last_used prefers
// the gateway-side started_at, falling back to the row-write created for
// rows logged before started_at existed (NULLIF: unset dates store as empty
// strings, which COALESCE alone would not skip). Unused rows stay NULL with
// zero counts thanks to the LEFT JOIN.
type usageView struct {
	name  string
	query string
}

var usageViews = []usageView{
	{
		name: "api_keys_usage",
		query: `SELECT k.id AS id, k.name AS name,` +
			` MAX(COALESCE(NULLIF(l.started_at, ''), l.created)) AS last_used,` +
			` COUNT(CASE WHEN l.outcome = 'completed' THEN 1 END) AS success_requests,` +
			` COUNT(CASE WHEN l.outcome <> 'completed' THEN 1 END) AS fail_requests` +
			` FROM api_keys AS k LEFT JOIN request_logs AS l ON l.api_key = k.id` +
			` GROUP BY k.id, k.name`,
	},
	{
		name: "providers_usage",
		query: `SELECT p.id AS id, p.name AS name,` +
			` MAX(COALESCE(NULLIF(l.started_at, ''), l.created)) AS last_used,` +
			` COUNT(CASE WHEN l.outcome = 'completed' THEN 1 END) AS success_requests,` +
			` COUNT(CASE WHEN l.outcome <> 'completed' THEN 1 END) AS fail_requests` +
			` FROM providers AS p LEFT JOIN request_logs AS l ON l.provider = p.id` +
			` GROUP BY p.id, p.name`,
	},
	{
		name: "routes_usage",
		query: `SELECT r.id AS id, r.alias AS alias, r.provider AS provider,` +
			` r.upstream_model AS upstream_model,` +
			` MAX(COALESCE(NULLIF(l.started_at, ''), l.created)) AS last_used,` +
			` COUNT(CASE WHEN l.outcome = 'completed' THEN 1 END) AS success_requests,` +
			` COUNT(CASE WHEN l.outcome <> 'completed' THEN 1 END) AS fail_requests` +
			` FROM routes AS r LEFT JOIN request_logs AS l` +
			` ON l.alias = r.alias AND l.provider = r.provider AND l.upstream_model = r.upstream_model` +
			` GROUP BY r.id, r.alias, r.provider, r.upstream_model`,
	},
}

// ensureUsageViews creates or refreshes the usage view collections.
// Idempotent: existing views with identical queries are left untouched.
// Rules stay nil (superuser-only), mirroring the base collections.
func ensureUsageViews(app core.App, views ...usageView) error {
	for _, v := range views {
		existing, err := app.FindCollectionByNameOrId(v.name)
		if err == nil {
			if existing.ViewQuery == v.query {
				continue
			}
			existing.ViewQuery = v.query
			if err := app.Save(existing); err != nil {
				return err
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		view := core.NewViewCollection(v.name)
		view.ViewQuery = v.query
		if err := app.Save(view); err != nil {
			return err
		}
	}
	return nil
}
