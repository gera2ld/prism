package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"

	"github.com/pocketbase/pocketbase/core"

	"github.com/gera2ld/prism/internal/store"
)

// Commands implements every operational command exactly once. The CLI verbs
// and the HTTP operations are thin surfaces over these handlers: they parse
// transport-specific input, call the handler, and format transport-specific
// output. All behavior — including validation — lives here, so it cannot
// drift between surfaces.
type Commands struct {
	App   core.App
	Store *store.Store
}

func requireName(kind, name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New(kind + " name is required")
	}
	return nil
}

// GenerateKey creates a client API key and returns the secret (shown once).
func (c *Commands) GenerateKey(name string) (string, error) {
	if err := requireName("key", name); err != nil {
		return "", err
	}
	return c.Store.CreateAPIKey(c.App, name)
}

// RevealKey decrypts a client API key for copying.
func (c *Commands) RevealKey(name string) (string, error) {
	if err := requireName("key", name); err != nil {
		return "", err
	}
	return c.Store.RevealAPIKey(c.App, name)
}

// ListKeys returns all client keys (names and status, never secrets).
func (c *Commands) ListKeys() ([]store.APIKeyInfo, error) {
	return c.Store.ListAPIKeys(c.App)
}

// ListProviders returns all providers (names, URLs and status, never tokens).
func (c *Commands) ListProviders() ([]store.ProviderInfo, error) {
	return c.Store.ListProviders(c.App)
}

// RevealProviderToken decrypts a provider token for copying.
func (c *Commands) RevealProviderToken(name string) (string, error) {
	if err := requireName("provider", name); err != nil {
		return "", err
	}
	return c.Store.RevealProviderToken(c.App, name)
}

// ExportRoutes renders the whole routing table as CSV bytes.
func (c *Commands) ExportRoutes() ([]byte, error) {
	return c.Store.ExportRoutesCSV()
}

// ExportRoutesToFile renders the routing table and writes it to path.
func (c *Commands) ExportRoutesToFile(path string) error {
	return c.Store.ExportRoutes(path)
}

// ImportRoutes merges routes from CSV data. With prune, the data becomes
// the full desired state: existing routes absent from it are deleted.
func (c *Commands) ImportRoutes(data []byte, prune bool) error {
	if len(data) == 0 {
		return errors.New("empty CSV")
	}
	return c.Store.ImportRoutesReader(bytes.NewReader(data), prune)
}

// ImportRoutesFromFile merges routes from the CSV file at path.
func (c *Commands) ImportRoutesFromFile(path string, prune bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return c.ImportRoutes(data, prune)
}

// ListMCPServers returns all configured MCP servers (names, transports and
// endpoints, never env or header values).
func (c *Commands) ListMCPServers() ([]store.MCPServerInfo, error) {
	return c.Store.MCPServers()
}

// RevealMCPSecrets decrypts a server's env and headers for copying.
func (c *Commands) RevealMCPSecrets(name string) (map[string]string, error) {
	if err := requireName("mcp server", name); err != nil {
		return nil, err
	}
	return c.Store.RevealMCPSecrets(name)
}

// ListMCPTools returns one server's tools with their approval status,
// including the live definition hash an approval would be pinned to.
func (c *Commands) ListMCPTools(ctx context.Context, name string) ([]store.MCPToolView, error) {
	if err := requireName("mcp server", name); err != nil {
		return nil, err
	}
	return c.Store.MCPTools(ctx, name)
}

// ApproveMCPTool pins a tool's current definition hash. The reviewed hash is
// required and must match what the server publishes now, so consent cannot be
// extended to a definition that appeared after review.
func (c *Commands) ApproveMCPTool(ctx context.Context, server, tool, reviewedHash string) error {
	if err := requireName("mcp server", server); err != nil {
		return err
	}
	if strings.TrimSpace(tool) == "" {
		return errors.New("tool is required")
	}
	if strings.TrimSpace(reviewedHash) == "" {
		return errors.New("definition_hash is required; read it from the server's tool list")
	}
	return c.Store.ApproveMCPTool(ctx, server, tool, reviewedHash)
}

// RevokeMCPTool removes an approval, hiding the tool from agents again.
func (c *Commands) RevokeMCPTool(server, tool string) error {
	if err := requireName("mcp server", server); err != nil {
		return err
	}
	if strings.TrimSpace(tool) == "" {
		return errors.New("tool is required")
	}
	return c.Store.RevokeMCPTool(server, tool)
}

// RefreshMCPServer disconnects a server so the next use redials and relists it.
func (c *Commands) RefreshMCPServer(name string) error {
	if err := requireName("mcp server", name); err != nil {
		return err
	}
	return c.Store.RefreshMCPServer(name)
}

// ListTools returns every conduit tool, enabled or not.
func (c *Commands) ListTools() ([]store.ConduitToolInfo, error) {
	return c.Store.ConduitTools()
}

// ValidateTool checks a conduit definition without saving it.
func (c *Commands) ValidateTool(definition []byte) error {
	return store.ValidateConduitDefinition(definition)
}
