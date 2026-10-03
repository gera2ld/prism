package store

import (
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"
)

// registerToolHooks wires the tools surface into PocketBase: fail-closed
// definition validation, and the cache invalidation that lets an admin UI edit
// apply without a restart.
//
// Validation runs on the way in, so a definition that cannot compile never
// reaches the catalog and an uncompilable one never has to be skipped at load
// time.
//
// Every Bind gets its own handler value. TaggedHook.Bind wraps handler.Func in
// place with that call's tag filter, and Hook.Bind reuses the id already set on
// the handler, so sharing one value across collections would nest the tag checks
// and skip the body for every collection but the last one bound.
func (s *Store) registerToolHooks(catalog *toolCatalog) {
	app := s.app

	validateDefinition := &hook.Handler[*core.RecordEvent]{
		Func: func(e *core.RecordEvent) error {
			if err := validateConduitRecord(e.Record); err != nil {
				return err
			}
			return e.Next()
		},
	}
	app.OnRecordCreate(toolsCollection).Bind(validateDefinition)
	app.OnRecordUpdate(toolsCollection).Bind(&hook.Handler[*core.RecordEvent]{
		Func: validateDefinition.Func,
	})

	invalidateTools := &hook.Handler[*core.RecordEvent]{
		Func: func(e *core.RecordEvent) error {
			catalog.conduit.Invalidate()
			return e.Next()
		},
	}
	app.OnRecordAfterCreateSuccess(toolsCollection).Bind(invalidateTools)
	app.OnRecordAfterUpdateSuccess(toolsCollection).Bind(invalidateTools)
	app.OnRecordAfterDeleteSuccess(toolsCollection).Bind(invalidateTools)
}

// validateConduitRecord rejects a definition that cannot compile at save time,
// so bad YAML, unknown methods, duplicate step ids and unknown top-level keys
// never persist.
func validateConduitRecord(record *core.Record) error {
	return ValidateConduitDefinition([]byte(record.GetString("definition")))
}
