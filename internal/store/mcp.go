package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/gera2ld/prism/internal/gateway"
	"github.com/gera2ld/prism/internal/mcp"
)

const (
	mcpServersCollection = "mcp_servers"
	mcpGrantsCollection  = "mcp_tool_grants"
	toolsCollection      = "tools"
	toolLogsCollection   = "tool_logs"
)

// mcpNamePrefix and mcpNameSeparator build the name an agent calls an MCP tool
// by. Namespaceing keeps two servers that both publish read_file distinct.
// The composed name is deliberately never parsed back into its parts: neither
// server names nor tool names are constrained to exclude the separator, so the
// mapping is resolved from the discovered index instead, which cannot be
// ambiguous.
const (
	mcpNamePrefix    = "mcp__"
	mcpNameSeparator = "__"
)

// ErrApprovalStale reports that a tool's definition changed between the
// operator reviewing it and approving it, so the hash they signed off no longer
// matches what the server publishes.
var ErrApprovalStale = errors.New("tool definition changed since it was reviewed")

// mcpIndexEntry maps a callable name back to the server and tool behind it.
type mcpIndexEntry struct {
	server string
	tool   string
}

// MCPSource owns the MCP side of the catalog: server configuration read from
// the database, the approval ledger, and the connection pool that discovery
// and invocation go through.
type MCPSource struct {
	app     core.App
	pool    *mcp.Pool
	logger  *slog.Logger
	decrypt func(string) (string, error)
	// dial optionally overrides how a server is reached, set by WithMCPDial.
	dial func(string) func(context.Context) (mcpsdk.Transport, error)

	mu           sync.Mutex
	configs      []mcp.ServerConfig
	configsKnown bool
	// grants maps "server\x00tool" to the last approved definition hash. It is
	// returned to callers read-only.
	grants      map[string]string
	grantsKnown bool
	index       map[string]mcpIndexEntry
}

func newMCPSource(app core.App, pool *mcp.Pool, logger *slog.Logger, decrypt func(string) (string, error), dial func(string) func(context.Context) (mcpsdk.Transport, error)) *MCPSource {
	if logger == nil {
		logger = slog.Default()
	}
	source := &MCPSource{
		app:     app,
		pool:    pool,
		logger:  logger,
		decrypt: decrypt,
		dial:    dial,
		grants:  map[string]string{},
		index:   map[string]mcpIndexEntry{},
	}
	// A changed tool list invalidates the callable-name index: a name may now
	// point at a different tool, or at nothing.
	pool.OnChange(func() {
		source.mu.Lock()
		source.index = map[string]mcpIndexEntry{}
		source.mu.Unlock()
	})
	return source
}

// Invalidate drops cached configuration and grants, then reconciles live
// sessions. When reconnect is set, edited servers are also dropped so a
// changed transport or endpoint cannot keep serving from the old connection.
func (s *MCPSource) Invalidate(reconnect bool) error {
	s.mu.Lock()
	s.configs = nil
	s.configsKnown = false
	s.grants = map[string]string{}
	s.grantsKnown = false
	s.index = map[string]mcpIndexEntry{}
	s.mu.Unlock()

	configs, err := s.loadConfigs()
	if err != nil {
		return err
	}
	if reconnect {
		for _, cfg := range configs {
			if !cfg.Enabled {
				continue
			}
			if err := s.pool.Refresh(cfg.Name); err != nil && !errors.Is(err, mcp.ErrUnknownServer) {
				return err
			}
		}
	}
	s.pool.Apply(configs)
	return nil
}

// List returns approved MCP tools. Discovery re-lists on every call: the hash
// pin is only worth anything if definitions are actually re-read, and caching
// them would leave a silently changed tool callable until the next refresh.
// The cost is one tools/list per server on a path an agent hits once per
// conversation, which is far cheaper than the window it closes.
func (s *MCPSource) List(ctx context.Context) ([]gateway.Tool, error) {
	discovered, err := s.discover(ctx)
	if err != nil && len(discovered) == 0 {
		return nil, err
	}
	grants, err := s.loadGrants()
	if err != nil {
		return nil, err
	}

	out := make([]gateway.Tool, 0, len(discovered))
	for _, d := range discovered {
		// Withholding on a hash mismatch is the whole point: a server that
		// changed a tool after approval cannot keep it callable.
		if grants[grantKey(d.Server, d.Tool)] != d.Hash {
			continue
		}
		out = append(out, gateway.Tool{
			Name:        MCPToolName(d.Server, d.Tool),
			Description: d.Description,
			InputSchema: json.RawMessage(d.InputSchema),
			Source:      gateway.SourceMCP,
			Server:      d.Server,
		})
	}
	return out, nil
}

// Call invokes one approved MCP tool by its callable name.
func (s *MCPSource) Call(ctx context.Context, name string, args json.RawMessage) (gateway.ToolResult, error) {
	entry, err := s.resolve(ctx, name)
	if err != nil {
		return gateway.ToolResult{}, err
	}
	grants, err := s.loadGrants()
	if err != nil {
		return gateway.ToolResult{}, err
	}
	// Approval is re-checked against the connection's current tool list rather
	// than the index that resolved the name, so a tool withdrawn or redefined
	// since the index was built cannot be called. This costs no extra round
	// trip: the pool already holds what the session last published.
	live := ""
	for _, d := range s.pool.Cached(entry.server) {
		if d.Tool == entry.tool {
			live = d.Hash
			break
		}
	}
	if live == "" || grants[grantKey(entry.server, entry.tool)] != live {
		return gateway.ToolResult{}, fmt.Errorf("%w: %q", gateway.ErrUnknownTool, name)
	}

	var arguments any
	if len(args) > 0 && string(args) != "null" {
		if err := json.Unmarshal(args, &arguments); err != nil {
			return mcpFailed(entry.server, "arguments are not valid JSON: "+err.Error()), nil
		}
	}
	result, err := s.pool.Call(ctx, entry.server, entry.tool, arguments)
	if err != nil {
		if errors.Is(err, mcp.ErrUnknownServer) {
			return gateway.ToolResult{}, fmt.Errorf("%w: %q", gateway.ErrUnknownTool, name)
		}
		// A tool the server rejected is a tool that reported failure: the
		// agent needs the reason to correct its arguments.
		return mcpFailed(entry.server, err.Error()), nil
	}
	return gateway.ToolResult{
		Result:  result.Value,
		IsError: result.IsError,
		Source:  gateway.SourceMCP,
		Server:  entry.server,
	}, nil
}

// mcpFailed builds the result of an MCP tool that ran and reported failure.
func mcpFailed(server, reason string) gateway.ToolResult {
	return gateway.ToolResult{Result: reason, IsError: true, Source: gateway.SourceMCP, Server: server}
}

// resolve maps a callable name to its server and tool, discovering once if the
// name is not already indexed.
func (s *MCPSource) resolve(ctx context.Context, name string) (mcpIndexEntry, error) {
	if entry, ok := s.lookupIndex(name); ok {
		return entry, nil
	}
	_, err := s.discover(ctx)
	if entry, ok := s.lookupIndex(name); ok {
		return entry, nil
	}
	if err != nil {
		return mcpIndexEntry{}, err
	}
	return mcpIndexEntry{}, fmt.Errorf("%w: %q", gateway.ErrUnknownTool, name)
}

// discover re-lists every server and rebuilds the callable-name index.
func (s *MCPSource) discover(ctx context.Context) ([]mcp.Discovered, error) {
	found, err := s.pool.List(ctx)
	index := make(map[string]mcpIndexEntry, len(found))
	for _, d := range found {
		index[MCPToolName(d.Server, d.Tool)] = mcpIndexEntry{server: d.Server, tool: d.Tool}
	}
	s.mu.Lock()
	s.index = index
	s.mu.Unlock()
	return found, err
}

func (s *MCPSource) lookupIndex(name string) (mcpIndexEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.index[name]
	return entry, ok
}

// MCPToolName is the name an agent calls a server's tool by.
func MCPToolName(server, tool string) string {
	return mcpNamePrefix + server + mcpNameSeparator + tool
}

func grantKey(server, tool string) string { return server + "\x00" + tool }

func (s *MCPSource) loadConfigs() ([]mcp.ServerConfig, error) {
	s.mu.Lock()
	if s.configsKnown {
		configs := s.configs
		s.mu.Unlock()
		return configs, nil
	}
	s.mu.Unlock()

	rows, err := s.app.FindAllRecords(mcpServersCollection)
	if err != nil {
		return nil, err
	}
	configs := make([]mcp.ServerConfig, 0, len(rows))
	for _, row := range rows {
		cfg, err := s.serverConfig(row)
		if err != nil {
			// A row whose secrets will not decrypt is skipped rather than
			// breaking the whole list; it stays visible in the operator API so
			// the problem can be fixed.
			s.logger.Error("skipping unreadable MCP server", "name", row.GetString("name"), "error", err)
			continue
		}
		configs = append(configs, cfg)
	}
	sort.Slice(configs, func(i, j int) bool { return configs[i].Name < configs[j].Name })

	s.mu.Lock()
	s.configs = configs
	s.configsKnown = true
	s.mu.Unlock()
	return configs, nil
}

func (s *MCPSource) serverConfig(row *core.Record) (mcp.ServerConfig, error) {
	cfg := mcp.ServerConfig{
		Name:      row.GetString("name"),
		Transport: row.GetString("transport"),
		Command:   row.GetString("command"),
		URL:       row.GetString("url"),
		Enabled:   row.GetBool("enabled"),
	}
	if cfg.Transport == mcp.TransportStdio && cfg.Command == "" {
		return cfg, fmt.Errorf("server %q has no command", cfg.Name)
	}
	if cfg.Transport == mcp.TransportHTTP && cfg.URL == "" {
		return cfg, fmt.Errorf("server %q has no url", cfg.Name)
	}

	args, err := stringList(row.Get("args"))
	if err != nil {
		return cfg, fmt.Errorf("server %q args: %w", cfg.Name, err)
	}
	cfg.Args = args

	env, err := s.secretMap(row.GetString("env"))
	if err != nil {
		return cfg, fmt.Errorf("server %q env: %w", cfg.Name, err)
	}
	cfg.Env = env

	headers, err := s.secretMap(row.GetString("headers"))
	if err != nil {
		return cfg, fmt.Errorf("server %q headers: %w", cfg.Name, err)
	}
	cfg.Headers = headers
	if s.dial != nil {
		cfg.Dial = s.dial(cfg.Name)
	}
	return cfg, nil
}

// secretMap reads a JSON object whose values are stored encrypted, tolerating
// the plaintext form so a row written before encryption still reads.
func (s *MCPSource) secretMap(stored string) (map[string]string, error) {
	if strings.TrimSpace(stored) == "" {
		return nil, nil
	}
	var raw map[string]string
	if err := json.Unmarshal([]byte(stored), &raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(raw))
	for key, value := range raw {
		if value == "" {
			out[key] = value
			continue
		}
		plain, err := s.decrypt(value)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", key, err)
		}
		out[key] = plain
	}
	return out, nil
}

func stringList(value any) ([]string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if string(raw) == "null" {
		return nil, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (s *MCPSource) loadGrants() (map[string]string, error) {
	s.mu.Lock()
	if s.grantsKnown {
		grants := s.grants
		s.mu.Unlock()
		return grants, nil
	}
	s.mu.Unlock()

	rows, err := s.app.FindAllRecords(mcpGrantsCollection)
	if err != nil {
		return nil, err
	}
	servers, err := s.app.FindAllRecords(mcpServersCollection)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]string, len(servers))
	for _, row := range servers {
		byID[row.Id] = row.GetString("name")
	}

	grants := make(map[string]string, len(rows))
	for _, row := range rows {
		name, ok := byID[row.GetString("server")]
		if !ok {
			// The server was deleted between the two reads, so the grant is
			// unreachable either way.
			continue
		}
		grants[grantKey(name, row.GetString("tool"))] = row.GetString("definition_hash")
	}

	s.mu.Lock()
	s.grants = grants
	s.grantsKnown = true
	s.mu.Unlock()
	return grants, nil
}

// MCPServerInfo is the non-secret view of a server. Command arguments and
// endpoints are configuration and are shown; env and header values are
// credentials and never are.
type MCPServerInfo struct {
	Name       string   `json:"name"`
	Transport  string   `json:"transport"`
	Command    string   `json:"command,omitempty"`
	Args       []string `json:"args,omitempty"`
	URL        string   `json:"url,omitempty"`
	Enabled    bool     `json:"enabled"`
	HasSecrets bool     `json:"has_secrets"`
}

// Servers returns every configured server, sorted by name.
func (s *MCPSource) Servers() ([]MCPServerInfo, error) {
	rows, err := s.app.FindAllRecords(mcpServersCollection)
	if err != nil {
		return nil, err
	}
	out := make([]MCPServerInfo, 0, len(rows))
	for _, row := range rows {
		info := MCPServerInfo{
			Name:      row.GetString("name"),
			Transport: row.GetString("transport"),
			Command:   row.GetString("command"),
			URL:       row.GetString("url"),
			Enabled:   row.GetBool("enabled"),
		}
		if args, err := stringList(row.Get("args")); err == nil {
			info.Args = args
		}
		info.HasSecrets = hasSecretValues(row.GetString("env")) || hasSecretValues(row.GetString("headers"))
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// hasSecretValues reports whether a stored JSON object holds anything, without
// decrypting it.
func hasSecretValues(stored string) bool {
	if strings.TrimSpace(stored) == "" {
		return false
	}
	var raw map[string]string
	if err := json.Unmarshal([]byte(stored), &raw); err != nil {
		return false
	}
	return len(raw) > 0
}

// RevealSecrets returns a server's decrypted env and headers for copying.
// Authentication never uses this path, mirroring provider reveal.
func (s *MCPSource) RevealSecrets(name string) (map[string]string, error) {
	row, err := s.app.FindFirstRecordByFilter(mcpServersCollection, "name = {:name}", dbx.Params{"name": name})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	env, err := s.secretMap(row.GetString("env"))
	if err != nil {
		return nil, err
	}
	for key, value := range env {
		out["env."+key] = value
	}
	headers, err := s.secretMap(row.GetString("headers"))
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		out["headers."+key] = value
	}
	return out, nil
}

// MCPToolStatus is an MCP tool's approval state relative to what its server
// currently publishes.
type MCPToolStatus string

const (
	// MCPToolUnapproved is published but never approved: hidden from agents.
	MCPToolUnapproved MCPToolStatus = "unapproved"
	// MCPToolApproved is published and matches the approved hash.
	MCPToolApproved MCPToolStatus = "approved"
	// MCPToolChanged means an approval exists but the definition has since
	// changed, so the tool is withheld pending review.
	MCPToolChanged MCPToolStatus = "changed"
	// MCPToolOrphaned means an approval exists but the server no longer
	// publishes the tool.
	MCPToolOrphaned MCPToolStatus = "orphaned"
)

// MCPToolView is the operator-facing view of one MCP tool: what the agent can
// see about it, plus the hashes an approval is pinned to.
type MCPToolView struct {
	Tool         string          `json:"tool"`
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	Hash         string          `json:"definition_hash,omitempty"`
	ApprovedHash string          `json:"approved_hash,omitempty"`
	Status       MCPToolStatus   `json:"status"`
	Offered      bool            `json:"offered"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
}

// Tools returns one server's tools with their approval status, including
// approvals for tools the server has stopped publishing. Those are kept
// deliberately: a server flapping on a flaky network should not lose every
// approval it had.
func (s *MCPSource) Tools(ctx context.Context, name string) ([]MCPToolView, error) {
	if _, err := s.app.FindFirstRecordByFilter(mcpServersCollection, "name = {:name}", dbx.Params{"name": name}); err != nil {
		return nil, err
	}
	grants, err := s.loadGrants()
	if err != nil {
		return nil, err
	}

	discovered, _ := s.discover(ctx)
	live := make(map[string]mcp.Discovered, len(discovered))
	for _, d := range discovered {
		if d.Server == name {
			live[d.Tool] = d
		}
	}

	names := make([]string, 0, len(live))
	for tool := range live {
		names = append(names, tool)
	}
	sort.Strings(names)

	views := make([]MCPToolView, 0, len(names))
	for _, tool := range names {
		d := live[tool]
		approved := grants[grantKey(name, tool)]
		status := MCPToolUnapproved
		switch {
		case approved == d.Hash:
			status = MCPToolApproved
		case approved != "":
			status = MCPToolChanged
		}
		views = append(views, MCPToolView{
			Tool:         tool,
			Name:         MCPToolName(name, tool),
			Description:  d.Description,
			Hash:         d.Hash,
			ApprovedHash: approved,
			Status:       status,
			Offered:      true,
			InputSchema:  json.RawMessage(d.InputSchema),
		})
	}

	// Approvals the server no longer offers, so the operator can see them and
	// revoke them rather than wondering where a grant went.
	keys := make([]string, 0, len(grants))
	for key := range grants {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		server, tool := splitGrantKey(key)
		if server != name || live[tool].Tool != "" {
			continue
		}
		views = append(views, MCPToolView{
			Tool:         tool,
			Name:         MCPToolName(name, tool),
			ApprovedHash: grants[key],
			Status:       MCPToolOrphaned,
			Offered:      false,
		})
	}
	return views, nil
}

// Approve pins a tool's current definition hash. The caller passes the hash it
// reviewed; if the server has since redefined the tool the approval is refused
// rather than silently extending consent to a definition nobody saw.
func (s *MCPSource) Approve(ctx context.Context, server, tool, reviewedHash string) error {
	discovered, err := s.discover(ctx)
	var live *mcp.Discovered
	for i := range discovered {
		if discovered[i].Server == server && discovered[i].Tool == tool {
			live = &discovered[i]
			break
		}
	}
	if live == nil {
		if err != nil {
			return err
		}
		return fmt.Errorf("%w: %s/%s is not published by that server", gateway.ErrUnknownTool, server, tool)
	}
	if reviewedHash != live.Hash {
		return fmt.Errorf("%w: %s/%s now hashes to %s", ErrApprovalStale, server, tool, live.Hash)
	}

	if err := s.writeGrant(server, tool, live.Hash); err != nil {
		return err
	}
	return s.Invalidate(false)
}

func (s *MCPSource) writeGrant(server, tool, hash string) error {
	serverID := s.serverID(server)
	if serverID == "" {
		return fmt.Errorf("%w: %s", gateway.ErrUnknownTool, server)
	}
	row, err := s.app.FindFirstRecordByFilter(mcpGrantsCollection,
		"server = {:server} && tool = {:tool}",
		dbx.Params{"server": serverID, "tool": tool})
	switch {
	case err == nil:
		row.Set("definition_hash", hash)
		return s.app.Save(row)
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	collection, err := s.app.FindCollectionByNameOrId(mcpGrantsCollection)
	if err != nil {
		return err
	}
	row = core.NewRecord(collection)
	row.Set("server", serverID)
	row.Set("tool", tool)
	row.Set("definition_hash", hash)
	return s.app.Save(row)
}

// Revoke removes an approval, which hides the tool from agents again.
func (s *MCPSource) Revoke(server, tool string) error {
	row, err := s.app.FindFirstRecordByFilter(mcpGrantsCollection,
		"server = {:server} && tool = {:tool}",
		dbx.Params{"server": s.serverID(server), "tool": tool})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no approval for %s/%s", server, tool)
		}
		return err
	}
	if err := s.app.Delete(row); err != nil {
		return err
	}
	return s.Invalidate(false)
}

func (s *MCPSource) serverID(name string) string {
	row, err := s.app.FindFirstRecordByFilter(mcpServersCollection, "name = {:name}", dbx.Params{"name": name})
	if err != nil {
		return ""
	}
	return row.Id
}

// Refresh disconnects a server so the next use redials and relists it.
func (s *MCPSource) Refresh(name string) error {
	if err := s.pool.Refresh(name); err != nil {
		return err
	}
	s.mu.Lock()
	s.index = map[string]mcpIndexEntry{}
	s.mu.Unlock()
	return nil
}

func splitGrantKey(key string) (string, string) {
	server, tool, _ := strings.Cut(key, "\x00")
	return server, tool
}
