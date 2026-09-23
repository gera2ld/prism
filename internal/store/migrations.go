package store

import (
	"database/sql"
	"errors"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/migrations"
)

func init() {
	// Initial schema for pre-publication databases, plus additive follow-up
	// migrations. New columns always arrive via their own idempotent
	// migration so live data survives upgrades; the single-migration
	// wipe-and-reseed approach applies only to migrations older than the
	// follow-ups still registered here.
	migrations.Register(createAll, nil, "1800000000_gateway.go")
	migrations.Register(addRequestLogsStartedAt, nil, "1800000001_request_logs_started_at.go")
	migrations.Register(addTimestamps, nil, "1800000002_timestamps.go")
	migrations.Register(ensureUsageViews, nil, "1800000003_usage_views.go")
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
	return ensureUsageViews(app)
}

// addRequestLogsStartedAt persists the gateway-side request start time.
// Idempotent: existing databases gain the column, fresh ones get it from
// the chain, and rows logged before this migration keep started_at NULL.
func addRequestLogsStartedAt(app core.App) error {
	logs, err := app.FindCollectionByNameOrId("request_logs")
	if err != nil {
		return err
	}
	if logs.Fields.GetByName("started_at") != nil {
		return nil
	}
	logs.Fields.Add(&core.DateField{Name: "started_at"})
	return app.Save(logs)
}

// addTimestamps backfills the dashboard-convention created/updated fields on
// config collections. Idempotent; pre-existing rows keep empty values.
func addTimestamps(app core.App) error {
	for _, name := range []string{"providers", "api_keys", "routes", "transformers", settingsCollection} {
		collection, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			return err
		}
		changed := false
		if collection.Fields.GetByName("created") == nil {
			collection.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
			changed = true
		}
		if collection.Fields.GetByName("updated") == nil {
			collection.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
			changed = true
		}
		if changed {
			if err := app.Save(collection); err != nil {
				return err
			}
		}
	}
	return nil
}

// usageViews backs the read-only last-usage collections. last_used prefers
// the gateway-side started_at, falling back to the row-write created for
// rows logged before started_at existed (NULLIF: unset dates store as empty
// strings, which COALESCE alone would not skip). Unused rows stay NULL with
// a zero count thanks to the LEFT JOIN.
var usageViews = []struct {
	name  string
	query string
}{
	{
		name: "api_keys_usage",
		query: `SELECT k.id AS id, k.name AS name,` +
			` MAX(COALESCE(NULLIF(l.started_at, ''), l.created)) AS last_used,` +
			` COUNT(l.id) AS total_requests` +
			` FROM api_keys AS k LEFT JOIN request_logs AS l ON l.api_key = k.id` +
			` GROUP BY k.id, k.name`,
	},
	{
		name: "providers_usage",
		query: `SELECT p.id AS id, p.name AS name,` +
			` MAX(COALESCE(NULLIF(l.started_at, ''), l.created)) AS last_used,` +
			` COUNT(l.id) AS total_requests` +
			` FROM providers AS p LEFT JOIN request_logs AS l ON l.provider = p.id` +
			` GROUP BY p.id, p.name`,
	},
	{
		name: "routes_usage",
		query: `SELECT r.id AS id, r.alias AS alias, r.provider AS provider,` +
			` r.upstream_model AS upstream_model,` +
			` MAX(COALESCE(NULLIF(l.started_at, ''), l.created)) AS last_used,` +
			` COUNT(l.id) AS total_requests` +
			` FROM routes AS r LEFT JOIN request_logs AS l` +
			` ON l.alias = r.alias AND l.provider = r.provider AND l.upstream_model = r.upstream_model` +
			` GROUP BY r.id, r.alias, r.provider, r.upstream_model`,
	},
}

// ensureUsageViews creates or refreshes the usage view collections.
// Idempotent: existing views with identical queries are left untouched.
// Rules stay nil (superuser-only), mirroring the base collections.
func ensureUsageViews(app core.App) error {
	for _, v := range usageViews {
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
