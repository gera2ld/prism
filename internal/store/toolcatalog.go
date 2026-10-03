package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/pocketbase/pocketbase/core"

	"github.com/gera2ld/prism/internal/gateway"
)

// toolCatalog is the gateway's view of every callable tool. It is the single
// source both surfaces read: the REST endpoints and the MCP server, so the two
// can never disagree about what exists.
type toolCatalog struct {
	conduit *conduitRegistry
	logger  *slog.Logger
}

func newToolCatalog(app core.App, logger *slog.Logger) *toolCatalog {
	if logger == nil {
		logger = slog.Default()
	}
	return &toolCatalog{
		conduit: newConduitRegistry(app, logger),
		logger:  logger,
	}
}

// List returns the tools an agent may call, sorted by name so the catalog does
// not shuffle between calls.
func (c *toolCatalog) List(ctx context.Context, _ gateway.Key) ([]gateway.Tool, error) {
	tools, err := c.conduit.List()
	if err != nil {
		return nil, fmt.Errorf("conduit tools: %w", err)
	}
	// Names are unique by a collection index, so there is nothing to merge and
	// no collision to resolve: one source, one namespace.
	slices.SortFunc(tools, func(a, b gateway.Tool) int { return strings.Compare(a.Name, b.Name) })
	return tools, nil
}

// Invoke runs one tool by name. A name that is not callable is reported as
// ErrUnknownTool so callers cannot distinguish absent from withheld.
func (c *toolCatalog) Invoke(ctx context.Context, key gateway.Key, name string, args json.RawMessage) (gateway.ToolResult, error) {
	return c.conduit.Call(ctx, name, args)
}
