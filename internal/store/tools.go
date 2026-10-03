package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	conduitgo "github.com/gera2ld/conduit/packages/conduit-go"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/gera2ld/prism/internal/gateway"
)

// The collections behind the tools surface. Names are shared with the
// migration that creates them.
const toolsCollection = "tools"

const toolLogsCollection = "tool_logs"

// conduitRegistry serves the operator-authored tool source. A conduit
// definition is already a function-calling spec — it carries name,
// description and input_schema, and the engine validates incoming arguments
// against that schema — so a tool is exposed by publishing the definition, not
// by restating its fields somewhere else.
type conduitRegistry struct {
	app    core.App
	logger *slog.Logger

	// cache is shared across runs so a definition's cache_ttl persists between
	// invocations, which is the documented way to use it.
	cache *conduitgo.Cache
	// headers are sent on every request the engine makes. Injectable so a test
	// can observe them.
	headers map[string]string

	mu     sync.Mutex
	tools  []conduitTool
	loaded bool
}

// defaultConduitHeaders go on every outbound request a conduit tool makes.
//
// Go's HTTP client sends no User-Agent at all unless one is set, and plenty of
// public APIs refuse an unidentified client — Nominatim answers 403 with "Access
// denied" and no further detail. The value is a bare Name/version token on
// purpose: some of those APIs also reject the conventional "App/1.0 (contact)"
// form for containing parentheses or an @, so adding contact details here would
// get the request blocked instead. A step's own headers still win, so a tool
// that wants richer identification can say so where it is written.
var defaultConduitHeaders = map[string]string{"User-Agent": "Prism/" + gateway.Version}

type conduitTool struct {
	name   string
	def    *conduitgo.Conduit
	schema json.RawMessage
}

func newConduitRegistry(app core.App, logger *slog.Logger) *conduitRegistry {
	if logger == nil {
		logger = slog.Default()
	}
	return &conduitRegistry{
		app:     app,
		logger:  logger,
		cache:   conduitgo.NewCache(),
		headers: defaultConduitHeaders,
	}
}

func (r *conduitRegistry) Invalidate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools = nil
	r.loaded = false
}

// List returns the enabled conduit tools as the agent sees them.
func (r *conduitRegistry) List() ([]gateway.Tool, error) {
	tools, err := r.load()
	if err != nil {
		return nil, err
	}
	out := make([]gateway.Tool, 0, len(tools))
	for _, t := range tools {
		out = append(out, gateway.Tool{
			Name:        t.name,
			Description: t.def.Description,
			InputSchema: t.schema,
		})
	}
	return out, nil
}

// Call runs one tool. A definition that fails is a tool that reported failure,
// not a gateway fault, so it comes back as IsError with the engine's message:
// the agent needs the reason to correct its arguments or its plan.
func (r *conduitRegistry) Call(ctx context.Context, name string, args json.RawMessage) (gateway.ToolResult, error) {
	tools, err := r.load()
	if err != nil {
		return gateway.ToolResult{}, err
	}
	idx := slices.IndexFunc(tools, func(t conduitTool) bool { return t.name == name })
	if idx < 0 {
		return gateway.ToolResult{}, fmt.Errorf("%w: %q", gateway.ErrUnknownTool, name)
	}

	// conduitgo compares the input against input_schema, which needs a decoded
	// Go value. A malformed body is the caller's error and is surfaced as a
	// tool failure so the agent can see it.
	var input any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &input); err != nil {
			return failed("arguments are not valid JSON: " + err.Error()), nil
		}
	}

	out, err := conduitgo.Run(ctx, tools[idx].def, input, conduitgo.Options{
		Cache:   r.cache,
		Headers: r.headers,
	})
	if err != nil {
		return failed(err.Error()), nil
	}
	return gateway.ToolResult{Result: out}, nil
}

// failed builds the result of a tool that ran and reported failure.
func failed(reason string) gateway.ToolResult {
	return gateway.ToolResult{Result: reason, IsError: true}
}

// load returns enabled tools with their definitions parsed. Parsing happens
// once per invalidation rather than per call, and a definition that no longer
// compiles is logged and skipped so one broken tool cannot empty the catalog.
func (r *conduitRegistry) load() ([]conduitTool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loaded {
		return r.tools, nil
	}

	rows, err := r.app.FindAllRecords(toolsCollection, dbx.HashExp{"enabled": true})
	if err != nil {
		return nil, err
	}
	tools := make([]conduitTool, 0, len(rows))
	for _, row := range rows {
		name := row.GetString("name")
		raw := row.GetString("definition")
		def, err := conduitgo.Parse([]byte(raw))
		if err != nil {
			r.logger.Error("skipping unparseable tool", "name", name, "error", err)
			continue
		}
		tool := conduitTool{name: name, def: def}
		// An absent input_schema is legitimate (a tool with no arguments), so
		// it is left nil rather than marshalled as a misleading empty object.
		if def.InputSchema != nil {
			schema, err := json.Marshal(def.InputSchema)
			if err != nil {
				r.logger.Error("skipping tool with unusable input schema", "name", name, "error", err)
				continue
			}
			tool.schema = schema
		}
		tools = append(tools, tool)
	}
	slices.SortFunc(tools, func(a, b conduitTool) int { return strings.Compare(a.name, b.name) })

	r.tools = tools
	r.loaded = true
	return tools, nil
}

// ConduitToolInfo is the operator-facing view of one conduit tool, including
// fields the agent never sees.
type ConduitToolInfo struct {
	Name        string          `json:"name"`
	ConduitName string          `json:"conduit_name"`
	Description string          `json:"description"`
	Enabled     bool            `json:"enabled"`
	Invalid     bool            `json:"invalid"`
	Updated     string          `json:"updated"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// ListConduitTools returns every conduit tool, enabled or not, sorted by name.
func (r *conduitRegistry) Info() ([]ConduitToolInfo, error) {
	// The listing includes disabled tools, which load() skips, so the cache
	// cannot answer this.
	r.Invalidate()

	rows, err := r.app.FindAllRecords(toolsCollection)
	if err != nil {
		return nil, err
	}
	out := make([]ConduitToolInfo, 0, len(rows))
	for _, row := range rows {
		// A definition that stopped compiling still appears, just without
		// derived fields: the operator needs to see the broken row to fix it.
		info := ConduitToolInfo{
			Name:    row.GetString("name"),
			Enabled: row.GetBool("enabled"),
			Updated: row.GetString("updated"),
		}
		if def, err := conduitgo.Parse([]byte(row.GetString("definition"))); err == nil {
			info.ConduitName = def.Name
			info.Description = def.Description
			if def.InputSchema != nil {
				if schema, err := json.Marshal(def.InputSchema); err == nil {
					info.InputSchema = schema
				}
			}
		} else {
			info.Invalid = true
		}
		out = append(out, info)
	}
	slices.SortFunc(out, func(a, b ConduitToolInfo) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// ValidateConduitDefinition parses a candidate definition without saving it, so
// an operator can check one before committing it.
func ValidateConduitDefinition(definition []byte) error {
	if strings.TrimSpace(string(definition)) == "" {
		return errors.New("definition must not be empty")
	}
	_, err := conduitgo.Parse(definition)
	return err
}
