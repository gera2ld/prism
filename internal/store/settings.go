package store

import (
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

// seedSettings creates or backfills the gateway_settings singleton row on start.
// Only the row is data; the collection's shape comes from the embedded snapshot
// like every other, so this must not touch the schema. Idempotent.
func (s *Store) seedSettings() error {
	settings, dirty := readSettings(s.app)
	if !dirty {
		return nil
	}
	return writeSettings(s.app, settings)
}
