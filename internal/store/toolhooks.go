package store

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"

	"github.com/gera2ld/prism/internal/mcp"
)

// secretFields lists the mcp_servers columns whose values are credentials.
// They are stored as a JSON object with every value encrypted, following the
// same enc: scheme as provider and client-key secrets.
var secretFields = []string{"env", "headers"}

// registerToolHooks wires the tools surface into PocketBase: secret
// encryption, fail-closed definition validation, and the cache invalidation
// that lets an admin UI edit apply without a restart.
//
// Secrets are encrypted on write rather than on read, and validation runs on
// the way in, so a definition that cannot compile never reaches the catalog and
// an uncompilable one never has to be skipped at load time.
//
// Every Bind below gets its own handler value. TaggedHook.Bind wraps handler.Func
// in place with that call's tag filter, and Hook.Bind reuses the id already set
// on the handler, so sharing one value across collections would nest the tag
// checks and skip the body for every collection but the last one bound.
func (s *Store) registerToolHooks(catalog *toolCatalog) {
	app := s.app

	encryptSecrets := &hook.Handler[*core.RecordEvent]{
		Func: func(e *core.RecordEvent) error {
			for _, field := range secretFields {
				if err := s.encryptJSONSecret(e.Record, field); err != nil {
					return err
				}
			}
			return e.Next()
		},
	}
	app.OnRecordCreate(mcpServersCollection).Bind(encryptSecrets)
	app.OnRecordUpdate(mcpServersCollection).Bind(&hook.Handler[*core.RecordEvent]{
		Func: encryptSecrets.Func,
	})

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

	validateServer := &hook.Handler[*core.RecordEvent]{
		Func: func(e *core.RecordEvent) error {
			if err := validateMCPServer(e.Record); err != nil {
				return err
			}
			return e.Next()
		},
	}
	app.OnRecordCreate(mcpServersCollection).Bind(validateServer)
	app.OnRecordUpdate(mcpServersCollection).Bind(&hook.Handler[*core.RecordEvent]{
		Func: validateServer.Func,
	})

	// A grant edit must take effect on the next request, or a revoked tool
	// would stay callable from cache.
	invalidateGrants := &hook.Handler[*core.RecordEvent]{
		Func: func(e *core.RecordEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			if err := catalog.mcp.Invalidate(false); err != nil && s.logger != nil {
				s.logger.Error("failed to invalidate mcp grants", "error", err)
			}
			return nil
		},
	}
	app.OnRecordAfterCreateSuccess(mcpGrantsCollection).Bind(invalidateGrants)
	app.OnRecordAfterUpdateSuccess(mcpGrantsCollection).Bind(invalidateGrants)
	app.OnRecordAfterDeleteSuccess(mcpGrantsCollection).Bind(invalidateGrants)

	// A server edit can change the transport, endpoint or credentials, so the
	// live session is dropped rather than reused against new settings.
	invalidateServers := &hook.Handler[*core.RecordEvent]{
		Func: func(e *core.RecordEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			if err := catalog.mcp.Invalidate(true); err != nil && s.logger != nil {
				s.logger.Error("failed to reconcile mcp servers", "error", err)
			}
			return nil
		},
	}
	app.OnRecordAfterCreateSuccess(mcpServersCollection).Bind(invalidateServers)
	app.OnRecordAfterUpdateSuccess(mcpServersCollection).Bind(invalidateServers)
	app.OnRecordAfterDeleteSuccess(mcpServersCollection).Bind(invalidateServers)

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

// encryptJSONSecret rewrites one JSON-object field with every value encrypted,
// leaving an already-encrypted or empty field untouched so a name-only edit
// does not double-encrypt.
func (s *Store) encryptJSONSecret(record *core.Record, field string) error {
	stored := record.GetString(field)
	if stored == "" {
		return nil
	}
	var values map[string]string
	if err := json.Unmarshal([]byte(stored), &values); err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	if len(values) == 0 {
		return nil
	}
	changed := false
	for key, value := range values {
		if value == "" || strings.HasPrefix(value, "enc:") {
			continue
		}
		blob, err := s.encrypt(value)
		if err != nil {
			return fmt.Errorf("%s.%s: %w", field, key, err)
		}
		values[key] = "enc:" + blob
		changed = true
	}
	if !changed {
		return nil
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	record.Set(field, string(encoded))
	return nil
}

// validateConduitRecord rejects a definition that cannot compile at save time,
// so bad YAML, unknown methods, duplicate step ids and unknown top-level keys
// never persist.
func validateConduitRecord(record *core.Record) error {
	return ValidateConduitDefinition([]byte(record.GetString("definition")))
}

// validateMCPServer rejects a server whose transport and fields disagree, which
// would otherwise only surface as a connection failure at first use.
func validateMCPServer(record *core.Record) error {
	switch record.GetString("transport") {
	case mcp.TransportStdio:
		if strings.TrimSpace(record.GetString("command")) == "" {
			return fmt.Errorf("stdio servers require a command")
		}
	case mcp.TransportHTTP:
		if strings.TrimSpace(record.GetString("url")) == "" {
			return fmt.Errorf("http servers require a url")
		}
	default:
		return fmt.Errorf("unsupported transport %q", record.GetString("transport"))
	}
	if _, err := stringList(record.Get("args")); err != nil {
		return fmt.Errorf("args must be an array of strings: %w", err)
	}
	for _, field := range secretFields {
		stored := record.GetString(field)
		if stored == "" {
			continue
		}
		var values map[string]string
		if err := json.Unmarshal([]byte(stored), &values); err != nil {
			return fmt.Errorf("%s must be an object of strings: %w", field, err)
		}
	}
	return nil
}
