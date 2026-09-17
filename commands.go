package main

import (
	"bytes"
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
