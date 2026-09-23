package store

import (
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
	return ensureSettingsRow(app)
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
