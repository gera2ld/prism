package mcp

import (
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"slices"
	"strings"
	"testing"
)

func envMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		out[name] = value
	}
	return out
}

func TestChildEnvInheritsWhitelist(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOME", "/home/operator")

	got := envMap(childEnv(nil))
	if got["PATH"] != "/usr/bin:/bin" {
		t.Fatalf("PATH = %q; a stdio server cannot launch without it", got["PATH"])
	}
	if got["HOME"] != "/home/operator" {
		t.Fatalf("HOME = %q", got["HOME"])
	}
}

func TestChildEnvWithholdsUnlistedVariables(t *testing.T) {
	// The whitelist is the point: a server must not be able to read the
	// gateway's own secrets. GATEWAY_ENCRYPTION_KEY is the sharp case, since it
	// decrypts every ciphertext in the database.
	t.Setenv("GATEWAY_ENCRYPTION_KEY", "super-secret-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "another-secret")
	t.Setenv("PATH", "/usr/bin")

	got := envMap(childEnv(nil))
	for _, name := range []string{"GATEWAY_ENCRYPTION_KEY", "AWS_SECRET_ACCESS_KEY"} {
		if value, ok := got[name]; ok {
			t.Fatalf("%s leaked into the server environment as %q", name, value)
		}
	}
}

func TestChildEnvConfiguredValuesWin(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("GITHUB_TOKEN", "ambient-token")

	got := envMap(childEnv(map[string]string{
		"GITHUB_TOKEN": "configured-token",
		"EXTRA":        "added",
	}))
	if got["GITHUB_TOKEN"] != "configured-token" {
		t.Fatalf("GITHUB_TOKEN = %q, want the configured value to win", got["GITHUB_TOKEN"])
	}
	if got["EXTRA"] != "added" {
		t.Fatalf("EXTRA = %q", got["EXTRA"])
	}
	// Inheriting an ambient credential that the record then overrides is fine,
	// but nothing unlisted may appear.
	if _, ok := got["PATH"]; !ok {
		t.Fatal("PATH should still be inherited alongside configured values")
	}
}

func TestChildEnvIsDeterministic(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	configured := map[string]string{"B": "2", "A": "1", "C": "3"}
	first := childEnv(configured)
	if !slices.IsSorted(first) {
		t.Fatalf("environment is not sorted: %v", first)
	}
	// Map iteration order is random, so an unsorted result would show up here.
	for range 20 {
		if !slices.Equal(first, childEnv(configured)) {
			t.Fatal("childEnv is not deterministic for one configuration")
		}
	}
}

func TestChildEnvAlwaysSetsEnvSoParentIsNotInherited(t *testing.T) {
	// cmd.Env == nil tells os/exec to inherit everything, which is exactly what
	// the whitelist exists to avoid, so the slice must always be non-nil.
	if childEnv(nil) == nil {
		t.Fatal("childEnv returned nil, which os/exec reads as 'inherit everything'")
	}
}

func TestStdioTransportIsWiredToTheWhitelist(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("GATEWAY_ENCRYPTION_KEY", "super-secret-key")

	e := &entry{name: "s", cfg: ServerConfig{
		Name:      "s",
		Transport: TransportStdio,
		Command:   "true",
		Args:      []string{"--flag"},
		Env:       map[string]string{"TOKEN": "t"},
	}}
	transport, err := e.transport(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	command, ok := transport.(*mcpsdk.CommandTransport)
	if !ok {
		t.Fatalf("transport = %T, want *mcp.CommandTransport", transport)
	}
	if command.Command.Env == nil {
		t.Fatal("command has no environment, so os/exec would inherit the gateway's own")
	}
	got := envMap(command.Command.Env)
	if got["PATH"] != "/usr/bin:/bin" {
		t.Fatalf("PATH = %q; the executable could not be found", got["PATH"])
	}
	if got["TOKEN"] != "t" {
		t.Fatalf("TOKEN = %q", got["TOKEN"])
	}
	if _, leaked := got["GATEWAY_ENCRYPTION_KEY"]; leaked {
		t.Fatal("the encryption key reached the spawned server")
	}
}
