package store

import (
	"log/slog"
	"strings"

	"github.com/pocketbase/pocketbase/core"
)

// Single source of truth for gateway_settings. Everything else (Store cache,
// migrations, retention job) goes through Settings, readSettings and
// writeSettings below. Nothing outside this file touches settings records.

const settingsCollection = "gateway_settings"

const (
	fieldCaptureBodies  = "capture_bodies"
	fieldRetentionHours = "retention_hours"
	fieldRetentionCron  = "retention_cron"
)

const (
	defaultCaptureBodies = false
	defaultRetentionHrs  = 24
	defaultRetentionCron = "0 3 * * *"
)

// Settings is the typed gateway_settings singleton.
type Settings struct {
	CaptureBodies  bool
	RetentionHours int
	RetentionCron  string
}

func defaultSettings() Settings {
	return Settings{
		CaptureBodies:  defaultCaptureBodies,
		RetentionHours: defaultRetentionHrs,
		RetentionCron:  defaultRetentionCron,
	}
}

// normalized repairs invalid values back to defaults.
func (s Settings) normalized() Settings {
	if s.RetentionHours <= 0 {
		s.RetentionHours = defaultRetentionHrs
	}
	if strings.TrimSpace(s.RetentionCron) == "" {
		s.RetentionCron = defaultRetentionCron
	} else {
		s.RetentionCron = strings.TrimSpace(s.RetentionCron)
	}
	return s
}

// settingsFields is the collection schema, in creation order.
func settingsFields() []func() core.Field {
	return []func() core.Field{
		func() core.Field { return &core.BoolField{Name: fieldCaptureBodies} },
		func() core.Field {
			return &core.NumberField{Name: fieldRetentionHours, OnlyInt: true, Required: true}
		},
		func() core.Field {
			return &core.TextField{Name: fieldRetentionCron, Required: true, Max: 64}
		},
	}
}

// ensureSettingsCollection finds the collection, creating it when missing.
func ensureSettingsCollection(app core.App) (*core.Collection, error) {
	if collection, err := app.FindCollectionByNameOrId(settingsCollection); err == nil {
		return collection, nil
	}
	collection := core.NewBaseCollection(settingsCollection)
	for _, makeField := range settingsFields() {
		collection.Fields.Add(makeField())
	}
	if err := app.Save(collection); err != nil {
		return nil, err
	}
	return app.FindCollectionByNameOrId(settingsCollection)
}

// reconcileSettingsFields adds missing field definitions and removes unknown
// non-system ones left over from older versions. Returns whether the
// collection was saved. log must be non-nil.
func reconcileSettingsFields(app core.App, collection *core.Collection, log *slog.Logger) (bool, error) {
	known := make(map[string]bool, len(settingsFields()))
	changed := false
	for _, makeField := range settingsFields() {
		field := makeField()
		known[field.GetName()] = true
		if collection.Fields.GetByName(field.GetName()) == nil {
			collection.Fields.Add(field)
			changed = true
			log.Info("settings field added", "field", field.GetName())
		}
	}
	for _, name := range collection.Fields.FieldNames() {
		field := collection.Fields.GetByName(name)
		if field.GetSystem() || known[name] {
			continue
		}
		collection.Fields.RemoveByName(name)
		changed = true
		log.Info("redundant settings field removed", "field", name)
	}
	if changed {
		if err := app.Save(collection); err != nil {
			return false, err
		}
	}
	return changed, nil
}

// readSettings maps the singleton row to Settings, applying defaults for a
// missing row or blank values. dirty is true when persisting would change
// the stored row.
func readSettings(app core.App) (Settings, bool) {
	settings := defaultSettings()
	records, err := app.FindAllRecords(settingsCollection)
	if err != nil || len(records) == 0 {
		return settings, len(records) == 0 && err == nil
	}
	row := records[0]
	dirty := false
	settings.CaptureBodies = row.GetBool(fieldCaptureBodies)
	if hours := row.GetInt(fieldRetentionHours); hours > 0 {
		settings.RetentionHours = hours
	} else {
		dirty = true
	}
	if expr := strings.TrimSpace(row.GetString(fieldRetentionCron)); expr != "" {
		settings.RetentionCron = expr
	} else {
		dirty = true
	}
	return settings, dirty
}

// writeSettings upserts the singleton row from typed Settings.
func writeSettings(app core.App, settings Settings) error {
	settings = settings.normalized()
	records, err := app.FindAllRecords(settingsCollection)
	if err != nil {
		return err
	}
	var row *core.Record
	if len(records) == 0 {
		collection, err := app.FindCollectionByNameOrId(settingsCollection)
		if err != nil {
			return err
		}
		row = core.NewRecord(collection)
	} else {
		row = records[0]
	}
	row.Set(fieldCaptureBodies, settings.CaptureBodies)
	row.Set(fieldRetentionHours, settings.RetentionHours)
	row.Set(fieldRetentionCron, settings.RetentionCron)
	return app.Save(row)
}

// reconcileSettings runs on start (Store.Open) and makes gateway_settings
// match the schema: missing fields are added, unknown non-system fields
// left over from older versions are removed, and the singleton row is
// created or backfilled with defaults. Idempotent.
func (s *Store) reconcileSettings() error {
	collection, err := ensureSettingsCollection(s.app)
	if err != nil {
		return err
	}
	if _, err := reconcileSettingsFields(s.app, collection, s.log()); err != nil {
		return err
	}
	settings, dirty := readSettings(s.app)
	if !dirty {
		return nil
	}
	return writeSettings(s.app, settings)
}

func (s *Store) log() *slog.Logger {
	if s.logger == nil {
		return slog.Default()
	}
	return s.logger
}
