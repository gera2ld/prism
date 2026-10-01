package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pocketbase/pocketbase/core"

	"github.com/gera2ld/prism/internal/gateway"
	"github.com/gera2ld/prism/internal/mcp"
)

// toolCatalog is the gateway's view of every callable tool, composed from the
// two sources. Conduit tools are named by their record; MCP tools are
// namespaced by server. Collisions between them are resolved here, which is
// the only place the two namespaces meet.
type toolCatalog struct {
	conduit *conduitRegistry
	mcp     *MCPSource
	logger  *slog.Logger
}

func newToolCatalog(app core.App, pool *mcp.Pool, logger *slog.Logger, decrypt func(string) (string, error), dial func(string) func(context.Context) (mcpsdk.Transport, error)) *toolCatalog {
	if logger == nil {
		logger = slog.Default()
	}
	return &toolCatalog{
		conduit: newConduitRegistry(app, logger),
		mcp:     newMCPSource(app, pool, logger, decrypt, dial),
		logger:  logger,
	}
}

// List returns the tools an agent may call, sorted by name so the catalog does
// not shuffle between calls.
func (c *toolCatalog) List(ctx context.Context, _ gateway.Key) ([]gateway.Tool, error) {
	conduit, conduitErr := c.conduit.List()
	mcpTools, mcpErr := c.mcp.List(ctx)

	// Either source failing must not hide the other, so the errors are logged
	// and joined rather than returned alone; only a total failure reports.
	switch {
	case conduitErr != nil && mcpErr != nil:
		return nil, fmt.Errorf("conduit tools: %w; mcp tools: %w", conduitErr, mcpErr)
	case conduitErr != nil:
		c.logger.Error("failed to load conduit tools", "error", conduitErr)
	case mcpErr != nil:
		c.logger.Error("failed to load mcp tools", "error", mcpErr)
	}

	merged := make([]gateway.Tool, 0, len(conduit)+len(mcpTools))
	merged = append(merged, conduit...)
	merged = append(merged, mcpTools...)

	// Namespacing prevents two MCP servers colliding, and the patterns keep a
	// conduit name from colliding with the mcp__ prefix, so a duplicate here
	// means one name was assigned twice. Withholding the later entry keeps the
	// catalog callable rather than failing outright.
	seen := make(map[string]bool, len(merged))
	out := make([]gateway.Tool, 0, len(merged))
	for _, tool := range merged {
		if seen[tool.Name] {
			c.logger.Error("skipping duplicate tool name", "name", tool.Name)
			continue
		}
		seen[tool.Name] = true
		out = append(out, tool)
	}
	slices.SortFunc(out, func(a, b gateway.Tool) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Invoke runs one tool by name. UnknownTool is returned for anything the agent
// may not call, so a withheld tool is indistinguishable from one that does not
// exist.
func (c *toolCatalog) Invoke(ctx context.Context, key gateway.Key, name string, args json.RawMessage) (gateway.ToolResult, error) {
	if strings.HasPrefix(name, mcpNamePrefix) {
		return c.mcp.Call(ctx, name, args)
	}
	return c.conduit.Call(ctx, name, args)
}
