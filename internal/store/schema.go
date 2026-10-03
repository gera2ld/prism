package store

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/pocketbase/pocketbase/core"
)

// Regenerate with:
//
//	prism schema export > internal/store/schema.json
//
//go:embed schema.json
var schemaJSON []byte

// SchemaJSON returns the embedded collections snapshot.
func SchemaJSON() []byte { return schemaJSON }

// ApplySchema makes the database match the snapshot exactly, on every open.
// Authoritative in both directions: a declared field is created if absent and
// restored if deleted, and an undeclared field or collection is removed with its
// records.
func ApplySchema(app core.App) error {
	if _, err := dropStaleViews(app); err != nil {
		return err
	}
	if err := app.ImportCollectionsByMarshaledJSON(schemaJSON, true); err != nil {
		return fmt.Errorf("apply schema snapshot: %w", err)
	}
	return nil
}

// dropStaleViews removes the view collections the snapshot is about to rewrite,
// and returns their names.
//
// This breaks a deadlock. A view selecting a column the snapshot drops cannot be
// fixed in either order: the table cannot lose the column while the view selects
// it, and the view cannot be rewritten to select a column the table does not have
// yet. Each half needs the other gone first. A view holds no rows, so dropping it
// costs nothing and the same import recreates it.
//
// Views the snapshot declares unchanged are left alone, so a boot where the schema
// did not change touches nothing. A view the snapshot omits is left for the import
// to delete, since deleting one is never blocked.
//
// It returns the dropped names so tests can observe the decision.
func dropStaleViews(app core.App) ([]string, error) {
	declared, err := declaredViewQueries()
	if err != nil {
		return nil, err
	}
	views, err := app.FindAllCollections(string(core.CollectionTypeView))
	if err != nil {
		return nil, fmt.Errorf("list view collections: %w", err)
	}
	var dropped []string
	for _, view := range views {
		if view.System {
			continue
		}
		query, kept := declared[view.Id]
		if !kept {
			// Absent from the snapshot, so the import deletes it — and deleting a
			// view is never blocked by the column it selects.
			continue
		}
		if query == view.ViewQuery {
			continue
		}
		if err := app.Delete(view); err != nil {
			return dropped, fmt.Errorf("drop stale view collection %q: %w", view.Name, err)
		}
		dropped = append(dropped, view.Name)
	}
	return dropped, nil
}

// declaredViewQueries maps collection id to the view query the snapshot declares.
func declaredViewQueries() (map[string]string, error) {
	document, err := decodeCollectionsDocument(schemaJSON)
	if err != nil {
		return nil, fmt.Errorf("embedded schema snapshot: %w", err)
	}
	queries := make(map[string]string, len(document))
	for _, item := range document {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := entry["type"].(string); kind != string(core.CollectionTypeView) {
			continue
		}
		id, _ := entry["id"].(string)
		query, _ := entry["viewQuery"].(string)
		queries[id] = query
	}
	return queries, nil
}

// CanonicalSnapshot normalizes an exported collections document into exactly the
// bytes schema.json holds. Accepts a bare array or the list endpoint's items
// envelope, drops system collections and per-instance noise, and sorts, so the
// same schema always produces the same file.
func CanonicalSnapshot(raw []byte) ([]byte, error) {
	document, err := decodeCollectionsDocument(raw)
	if err != nil {
		return nil, err
	}
	entries := make([]map[string]any, 0, len(document))
	for i, item := range document {
		entry, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("entry %d is not an object", i)
		}
		// System collections belong to PocketBase and an authoritative import
		// must never claim them.
		if system, _ := entry["system"].(bool); system {
			continue
		}
		entries = append(entries, normalizeEntry(entry))
	}
	slices.SortFunc(entries, func(a, b map[string]any) int {
		return compareStrings(a["name"], b["name"])
	})
	encoded, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return nil, err
	}
	// Trailing newline included so the bytes are exactly what the file on disk
	// holds, which makes a round-trip an exact comparison.
	return append(encoded, '\n'), nil
}

// decodeCollectionsDocument accepts either a bare array or the list endpoint's
// {"items": [...]} envelope.
func decodeCollectionsDocument(raw []byte) ([]any, error) {
	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("collections export is not JSON: %w", err)
	}
	switch typed := probe.(type) {
	case []any:
		return typed, nil
	case map[string]any:
		items, ok := typed["items"].([]any)
		if !ok {
			return nil, fmt.Errorf("object has no items array; expected a collections list")
		}
		return items, nil
	default:
		return nil, fmt.Errorf("expected an array of collections or an object with items")
	}
}

// normalizeEntry drops everything that is per-instance noise from one exported
// collection.
func normalizeEntry(entry map[string]any) map[string]any {
	out := make(map[string]any, len(entry))
	for key, value := range entry {
		out[key] = value
	}
	// Per-instance timestamps and the system flag are not part of the schema.
	delete(out, "created")
	delete(out, "updated")
	delete(out, "system")

	// Views project their query rather than declaring fields or indexes.
	if kind, _ := out["type"].(string); kind == string(core.CollectionTypeView) {
		delete(out, "fields")
		delete(out, "indexes")
	} else {
		delete(out, "viewQuery")
	}

	// Unset rules and options are indistinguishable from unset ones, and a wall
	// of nulls would hide the settings that do matter.
	for key, value := range out {
		if value == nil {
			delete(out, key)
		}
	}
	return out
}

// ExportSchema renders the database's collections in the shape schema.json holds,
// so re-exporting an unchanged database is a byte-identical file.
func ExportSchema(app core.App) ([]byte, error) {
	collections, err := app.FindAllCollections()
	if err != nil {
		return nil, err
	}
	document := make([]any, 0, len(collections))
	for _, collection := range collections {
		// Every non-system collection belongs in the snapshot. An authoritative
		// import removes what it omits, so leaving one out here would delete it.
		if collection.System {
			continue
		}
		encoded, err := json.Marshal(collection)
		if err != nil {
			return nil, err
		}
		var entry map[string]any
		if err := json.Unmarshal(encoded, &entry); err != nil {
			return nil, err
		}
		document = append(document, entry)
	}
	return CanonicalSnapshot(mustMarshal(document))
}

func mustMarshal(document []any) []byte {
	encoded, err := json.Marshal(document)
	if err != nil {
		// Marshalling a []any of maps that came from json.Unmarshal cannot
		// fail; anything else is a programming error.
		panic(err)
	}
	return encoded
}

func compareStrings(a, b any) int {
	left, _ := a.(string)
	right, _ := b.(string)
	return strings.Compare(left, right)
}
