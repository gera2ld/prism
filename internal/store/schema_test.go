package store

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

func TestProjectHasNoMigrations(t *testing.T) {
	app := newTestApp(t)
	var files []struct {
		File string `db:"file"`
	}
	if err := app.DB().NewQuery(
		"SELECT file FROM _migrations WHERE file LIKE '1800%' ORDER BY file").
		All(&files); err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("expected no project migrations, got %+v", files)
	}
}

// Hardcoded so dropping a collection from schema.json fails the test.
var declaredCollections = []string{
	"api_keys", "gateway_settings", "providers", "request_bodies", "request_logs",
	"request_images", "routes", "tool_logs", "tools", "transformers", "users",
	"api_keys_usage", "providers_usage", "routes_usage", "tools_usage",
}

func TestEmbeddedSchemaIsWellFormed(t *testing.T) {
	var entries []map[string]any
	if err := json.Unmarshal(SchemaJSON(), &entries); err != nil {
		t.Fatalf("schema.json is not a JSON array of collections: %v", err)
	}
	if len(entries) != len(declaredCollections) {
		t.Fatalf("snapshot has %d collections, want %d", len(entries), len(declaredCollections))
	}
	for _, entry := range entries {
		name, _ := entry["name"].(string)
		if name == "" {
			t.Fatal("a collection in the snapshot has no name")
		}
		if _, ok := entry["system"]; ok {
			t.Fatalf("%s declares a system flag", name)
		}
	}
}

func TestExportRoundTripsTheSnapshot(t *testing.T) {
	app := newTestApp(t)
	if _, err := Open(app, nil); err != nil {
		t.Fatal(err)
	}
	exported, err := ExportSchema(app)
	if err != nil {
		t.Fatal(err)
	}
	if string(exported) != string(SchemaJSON()) {
		t.Fatal("exporting a database built from the snapshot did not reproduce it; " +
			"the import must be lossless or every export will show a diff")
	}
}

func TestExportIsDeterministicAcrossDatabases(t *testing.T) {
	first := newTestApp(t)
	second := newTestApp(t)
	for _, app := range []*core.BaseApp{first, second} {
		if _, err := Open(app, nil); err != nil {
			t.Fatal(err)
		}
	}
	a, err := ExportSchema(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ExportSchema(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatal("two databases with the same schema exported differently")
	}
}

func TestCanonicalSnapshotAcceptsEitherExportedShape(t *testing.T) {
	raw, err := ExportSchema(newTestApp(t))
	if err != nil {
		t.Fatal(err)
	}
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}

	enveloped, err := json.Marshal(map[string]any{
		"page": 1, "totalPages": 1,
		"items": append([]map[string]any{{
			"name": "_superusers", "type": "auth", "system": true,
		}}, entries...),
	})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := CanonicalSnapshot(enveloped)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != string(raw) {
		t.Fatal("the envelope form did not normalize to the bare-array form")
	}

	var indented strings.Builder
	indented.WriteByte('[')
	for i, entry := range entries {
		encoded, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			indented.WriteByte(',')
		}
		indented.Write(encoded)
	}
	indented.WriteByte(']')
	canonical, err = CanonicalSnapshot([]byte(indented.String()))
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != string(raw) {
		t.Fatal("a bare array did not normalize to the canonical form")
	}
}

func TestCanonicalSnapshotRejectsRubbish(t *testing.T) {
	for name, input := range map[string]string{
		"not json":     `nope`,
		"a scalar":     `"hello"`,
		"object error": `{"detail":"unauthorized"}`,
		"bad entry":    `[1,2,3]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := CanonicalSnapshot([]byte(input)); err == nil {
				t.Fatalf("expected %s to be rejected", name)
			}
		})
	}
}

func TestSnapshotRestoresADeclaredField(t *testing.T) {
	app := newTestApp(t)
	if _, err := Open(app, nil); err != nil {
		t.Fatal(err)
	}
	tools, err := app.FindCollectionByNameOrId(toolsCollection)
	if err != nil {
		t.Fatal(err)
	}
	tools.Fields.RemoveByName("definition")
	if err := app.Save(tools); err != nil {
		t.Fatal(err)
	}

	if err := ApplySchema(app); err != nil {
		t.Fatal(err)
	}
	tools, err = app.FindCollectionByNameOrId(toolsCollection)
	if err != nil {
		t.Fatal(err)
	}
	if tools.Fields.GetByName("definition") == nil {
		t.Fatal("the snapshot did not restore a declared field that was deleted in the UI")
	}
}

func TestUnchangedViewsAreLeftAlone(t *testing.T) {
	app := newTestApp(t)
	if _, err := Open(app, nil); err != nil {
		t.Fatal(err)
	}

	dropped, err := dropStaleViews(app)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 0 {
		t.Fatalf("a database matching the snapshot should drop nothing, dropped %v", dropped)
	}

	if err := ApplySchema(app); err != nil {
		t.Fatal(err)
	}
	views, err := app.FindAllCollections(string(core.CollectionTypeView))
	if err != nil {
		t.Fatal(err)
	}
	if len(views) == 0 {
		t.Fatal("views went missing")
	}
}

func TestOnlyViewsTheSnapshotRewritesAreDropped(t *testing.T) {
	app := newTestApp(t)
	if _, err := Open(app, nil); err != nil {
		t.Fatal(err)
	}

	stale, err := app.FindCollectionByNameOrId("tools_usage")
	if err != nil {
		t.Fatal(err)
	}
	stale.ViewQuery = "SELECT l.tool AS id, MAX(l.created) AS last_used " +
		"FROM tool_logs AS l GROUP BY l.tool"
	if err := app.Save(stale); err != nil {
		t.Fatal(err)
	}

	extra := core.NewViewCollection("stray_view")
	extra.ViewQuery = "SELECT id, tool FROM tool_logs"
	if err := app.Save(extra); err != nil {
		t.Fatal(err)
	}

	dropped, err := dropStaleViews(app)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 || dropped[0] != "tools_usage" {
		t.Fatalf("expected only tools_usage dropped, got %v", dropped)
	}
	if _, err := app.FindCollectionByNameOrId("stray_view"); err != nil {
		t.Fatalf("an undeclared view is the import's business, not ours: %v", err)
	}
}

func TestApplySchemaRecoversWhenAStaleViewBlocksAColumnDrop(t *testing.T) {
	app := newTestApp(t)
	if _, err := Open(app, nil); err != nil {
		t.Fatal(err)
	}

	logs, err := app.FindCollectionByNameOrId(toolLogsCollection)
	if err != nil {
		t.Fatal(err)
	}
	logs.Fields.Add(&core.TextField{Name: "source", Max: 64})
	logs.Fields.Add(&core.TextField{Name: "server", Max: 128})
	if err := app.Save(logs); err != nil {
		t.Fatal(err)
	}
	stale, err := app.FindCollectionByNameOrId("tools_usage")
	if err != nil {
		t.Fatal(err)
	}
	stale.ViewQuery = "SELECT l.tool AS id, l.tool AS tool, l.source AS source, " +
		"l.server AS server, MAX(l.created) AS last_used, COUNT(*) AS calls " +
		"FROM tool_logs AS l GROUP BY l.tool, l.source, l.server"
	if err := app.Save(stale); err != nil {
		t.Fatal(err)
	}

	if err := ApplySchema(app); err != nil {
		t.Fatalf("snapshot could not replace a view that blocked a column drop: %v", err)
	}

	logs, err = app.FindCollectionByNameOrId(toolLogsCollection)
	if err != nil {
		t.Fatal(err)
	}
	names := logs.Fields.FieldNames()
	for _, gone := range []string{"source", "server"} {
		if slices.Contains(names, gone) {
			t.Fatalf("%q should have been dropped, still have %v", gone, names)
		}
	}
	if !slices.Contains(names, "transport") {
		t.Fatalf("transport should exist, have %v", names)
	}

	view, err := app.FindCollectionByNameOrId("tools_usage")
	if err != nil {
		t.Fatal(err)
	}
	if view.Type != core.CollectionTypeView {
		t.Fatalf("tools_usage should be a view again, got %q", view.Type)
	}
	if !strings.Contains(view.ViewQuery, "transport") {
		t.Fatalf("restored view should select transport, got %q", view.ViewQuery)
	}
	if err := app.DB().NewQuery("SELECT * FROM tools_usage").All(&[]struct{}{}); err != nil {
		t.Fatalf("restored view does not execute: %v", err)
	}
}

func TestSnapshotRemovesWhatItNoLongerDeclares(t *testing.T) {
	app := newTestApp(t)
	if _, err := Open(app, nil); err != nil {
		t.Fatal(err)
	}

	tools, err := app.FindCollectionByNameOrId(toolsCollection)
	if err != nil {
		t.Fatal(err)
	}
	tools.Fields.Add(&core.TextField{Name: "note", Max: 64})
	if err := app.Save(tools); err != nil {
		t.Fatal(err)
	}
	scratch := core.NewBaseCollection("scratchpad")
	scratch.Fields.Add(&core.TextField{Name: "note", Max: 128})
	if err := app.Save(scratch); err != nil {
		t.Fatal(err)
	}

	exported, err := ExportSchema(app)
	if err != nil {
		t.Fatal(err)
	}
	trimmed, err := dropFromSnapshot(t, exported, toolsCollection, "note")
	if err != nil {
		t.Fatal(err)
	}
	trimmed, err = dropFromSnapshot(t, trimmed, "scratchpad")
	if err != nil {
		t.Fatal(err)
	}

	restored := SchemaJSON()
	defer func() { schemaJSON = restored }()
	schemaJSON = trimmed

	if err := ApplySchema(app); err != nil {
		t.Fatal(err)
	}
	tools, err = app.FindCollectionByNameOrId(toolsCollection)
	if err != nil {
		t.Fatal(err)
	}
	if tools.Fields.GetByName("note") != nil {
		t.Fatal("a field the snapshot omits was not removed")
	}
	if _, err := app.FindCollectionByNameOrId("scratchpad"); err == nil {
		t.Fatal("a collection the snapshot omits was not removed")
	}
	for _, name := range declaredCollections {
		if _, err := app.FindCollectionByNameOrId(name); err != nil {
			t.Fatalf("%s was damaged by the apply: %v", name, err)
		}
	}
	for _, name := range []string{"_superusers", "_otps", "_mfas", "users"} {
		if _, err := app.FindCollectionByNameOrId(name); err != nil {
			t.Fatalf("PocketBase collection %s was removed: %v", name, err)
		}
	}
}

func dropFromSnapshot(t *testing.T, raw []byte, collection string, field ...string) ([]byte, error) {
	t.Helper()
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	out := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		if name, _ := entry["name"].(string); name == collection {
			if len(field) == 0 {
				continue // drop the collection entirely
			}
			fields, _ := entry["fields"].([]any)
			kept := make([]any, 0, len(fields))
			for _, item := range fields {
				f, _ := item.(map[string]any)
				if f == nil {
					continue
				}
				if f["name"] == field[0] {
					continue
				}
				kept = append(kept, f)
			}
			entry["fields"] = kept
		}
		out = append(out, entry)
	}
	return json.Marshal(out)
}

func TestApplySchemaIsIdempotent(t *testing.T) {
	app := newTestApp(t)
	for range 3 {
		if err := ApplySchema(app); err != nil {
			t.Fatalf("ApplySchema: %v", err)
		}
	}
	tools, err := app.FindCollectionByNameOrId(toolsCollection)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, name := range tools.Fields.FieldNames() {
		seen[name]++
	}
	for name, count := range seen {
		if count != 1 {
			t.Fatalf("field %s appears %d times", name, count)
		}
	}
}

func TestUsageViewsExposeSplitCounts(t *testing.T) {
	app := newTestApp(t)
	if _, err := Open(app, nil); err != nil {
		t.Fatal(err)
	}
	views := []string{"api_keys_usage", "providers_usage", "routes_usage", "tools_usage"}
	for _, view := range views {
		for _, column := range []string{"success_requests", "fail_requests"} {
			if _, err := app.DB().NewQuery(
				"SELECT " + column + " FROM {{" + view + "}} LIMIT 1").Execute(); err != nil {
				t.Fatalf("%s: expected column %s: %v", view, column, err)
			}
		}
		if _, err := app.DB().NewQuery(
			"SELECT total_requests FROM {{" + view + "}} LIMIT 1").Execute(); err == nil {
			t.Fatalf("%s: total_requests should have been replaced by the split", view)
		}
	}
}
