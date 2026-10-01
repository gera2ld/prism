// Package mcp supervises connections to Model Context Protocol servers so the
// gateway can offer their tools to agents. It knows nothing about PocketBase:
// callers supply plain ServerConfig values and receive plain results, which
// keeps it testable against in-memory transports with no subprocess and no
// network.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Transport names as stored in the mcp_servers collection.
const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"
)

// dialTimeout bounds one connection attempt. It is generous because stdio
// servers are often npx-style launchers that take a second or more to answer,
// but short enough that a dead server cannot hold a request open.
const dialTimeout = 20 * time.Second

// callTimeout bounds one tool invocation. Conduit steps use their own fixed 10s
// budget; this is deliberately longer so a slow tool is not cut short, while
// still bounding a request that would otherwise hang on a wedged server.
const callTimeout = 120 * time.Second

const (
	// baseBackoff is the delay after the first failure, doubled per
	// consecutive failure up to maxBackoff so a server that recovers is picked
	// up promptly while one that stays down is not hammered.
	baseBackoff = time.Second
	maxBackoff  = 30 * time.Second
)

// ServerConfig is everything needed to reach one server, with secrets already
// decrypted. The pool never reads records itself.
type ServerConfig struct {
	Name      string
	Transport string
	Command   string
	Args      []string
	Env       map[string]string
	URL       string
	Headers   map[string]string
	Enabled   bool

	// Dial overrides how the connection is made. It exists so a caller can
	// supply its own transport — an in-process pipe in tests, or a pooled
	// connection in a future embedder. When nil, the transport is built from
	// the fields above.
	Dial func(context.Context) (mcpsdk.Transport, error)
}

// Discovered is one tool a server currently publishes, together with the hash
// an approval is pinned to. Tools are unfiltered here: deciding which are
// approved belongs to the caller, which owns the ledger.
type Discovered struct {
	Server      string
	Tool        string
	Description string
	// InputSchema is the server's JSON Schema, marshalled for presentation.
	InputSchema []byte
	Hash        string
}

// Result is one invocation's output, flattened to the value an agent reads.
type Result struct {
	Value   any
	IsError bool
}

// ErrUnknownServer reports a server name with no usable configuration.
var ErrUnknownServer = errors.New("unknown MCP server")

// Pool supervises one session per server. Dialing is lazy: nothing connects
// until a tool is actually needed, so a broken or slow server cannot delay
// startup or block the chat proxy.
type Pool struct {
	logger *slog.Logger

	// mu guards entries. It is never held across a dial or an RPC, both of
	// which can take seconds.
	mu       sync.Mutex
	entries  map[string]*entry
	closed   bool
	onChange []func()
}

// entry is one server's mutable state. Its own mutex serializes dialing, so
// two concurrent first requests share one attempt instead of racing to spawn
// two subprocesses.
type entry struct {
	name string
	cfg  ServerConfig

	mu           sync.Mutex
	session      *mcpsdk.ClientSession
	tools        []Discovered
	failures     int
	backoffUntil time.Time
}

func NewPool(logger *slog.Logger) *Pool {
	if logger == nil {
		logger = slog.Default()
	}
	return &Pool{logger: logger, entries: map[string]*entry{}}
}

// Apply reconciles running sessions against the desired configuration, closing
// sessions for servers that were removed or disabled. Connections are
// established lazily, so Apply never dials. A live server adopts its new
// configuration in place; callers drop the session themselves when a change
// needs a reconnect.
func (p *Pool) Apply(configs []ServerConfig) {
	keep := make(map[string]ServerConfig, len(configs))
	for _, cfg := range configs {
		if cfg.Enabled {
			keep[cfg.Name] = cfg
		}
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	var stale []*entry
	for name, e := range p.entries {
		cfg, ok := keep[name]
		if !ok {
			stale = append(stale, e)
			delete(p.entries, name)
			continue
		}
		e.mu.Lock()
		e.cfg = cfg
		e.mu.Unlock()
		// Adopted: a live session keeps running against the new settings, and
		// reconnect=true in the caller's Invalidate already dropped it when the
		// change needed a fresh connection.
		delete(keep, name)
	}
	// New servers join the pool but stay undialed: connecting is lazy so a
	// broken server cannot delay startup.
	for name, cfg := range keep {
		p.entries[name] = &entry{name: name, cfg: cfg}
	}
	p.mu.Unlock()

	for _, e := range stale {
		e.reset(true)
	}
}

// Refresh disconnects a server so the next use redials and relists. It is the
// operator's escape hatch for a server whose tool list changed without a
// tools/list_changed notification, so it clears backoff rather than adding to
// it.
func (p *Pool) Refresh(name string) error {
	e := p.lookup(name)
	if e == nil {
		return ErrUnknownServer
	}
	e.reset(false)
	return nil
}

// List returns every tool published by every enabled server, sorted by server
// then tool so the catalog is stable between calls. Servers that fail to dial
// are logged and skipped rather than failing the whole listing: one broken
// server must not hide every other tool.
func (p *Pool) List(ctx context.Context) ([]Discovered, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, nil
	}
	names := make([]string, 0, len(p.entries))
	for name := range p.entries {
		names = append(names, name)
	}
	sort.Strings(names)
	entries := make([]*entry, len(names))
	for i, name := range names {
		entries[i] = p.entries[name]
	}
	p.mu.Unlock()

	var all []Discovered
	var errs []error
	for _, e := range entries {
		tools, err := p.tools(ctx, e)
		if err != nil {
			p.logger.Error("mcp server unavailable", "server", e.name, "error", err)
			errs = append(errs, fmt.Errorf("%s: %w", e.name, err))
			continue
		}
		all = append(all, tools...)
	}
	// A partial listing is more useful than none, so the tools that did load
	// are returned alongside the failures rather than discarded.
	return all, errors.Join(errs...)
}

// tools returns a server's published tools, dialing or redialing as needed.
// Listing is a read, so one transparent redial is safe; a tool call is not and
// never retries (see Call).
func (p *Pool) tools(ctx context.Context, e *entry) ([]Discovered, error) {
	tools, err := p.loadTools(ctx, e)
	if err == nil {
		return tools, nil
	}
	if !p.recoverable(err) || e.backingOff() {
		return nil, err
	}
	e.reset(false)
	return p.loadTools(ctx, e)
}

func (p *Pool) loadTools(ctx context.Context, e *entry) ([]Discovered, error) {
	session, err := p.session(ctx, e)
	if err != nil {
		return nil, err
	}
	listCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	listed, err := session.ListTools(listCtx, nil)
	if err != nil {
		return nil, fmt.Errorf("list tools: %w", err)
	}
	tools, err := toDiscovered(e.name, listed.Tools)
	if err != nil {
		return nil, err
	}
	changed := e.setTools(tools)
	if changed {
		p.notifyChange()
	}
	return tools, nil
}

// Cached returns what a connected server last published, without issuing an
// RPC. It is how an invocation re-checks a tool's approval hash for free: the
// session already holds the definitions, so verifying consent costs nothing.
// The result is empty for a server that is not connected.
func (p *Pool) Cached(server string) []Discovered {
	e := p.lookup(server)
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.session == nil {
		return nil
	}
	return slices.Clone(e.tools)
}

// Call invokes one tool. It never retries: a tool call has side effects, so a
// redial mid-flight could repeat an action the server already performed. The
// caller sees the error and the next request reconnects.
func (p *Pool) Call(ctx context.Context, server, tool string, arguments any) (Result, error) {
	e := p.lookup(server)
	if e == nil {
		return Result{}, ErrUnknownServer
	}
	session, err := p.session(ctx, e)
	if err != nil {
		return Result{}, err
	}

	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	out, err := session.CallTool(callCtx, &mcpsdk.CallToolParams{Name: tool, Arguments: arguments})
	if err != nil {
		// A closed connection is the session's own failure, not the call's:
		// drop it so the next request redials instead of reusing a corpse.
		if p.recoverable(err) {
			e.reset(true)
		}
		return Result{}, fmt.Errorf("call %s.%s: %w", server, tool, err)
	}
	return toResult(out), nil
}

// session returns the live session for a server, dialing under the entry lock
// so concurrent first requests share one attempt.
func (p *Pool) session(ctx context.Context, e *entry) (*mcpsdk.ClientSession, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.session != nil {
		return e.session, nil
	}
	if time.Now().Before(e.backoffUntil) {
		return nil, fmt.Errorf("server %q is in reconnect backoff", e.name)
	}

	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	transport, err := e.transport(dialCtx)
	if err != nil {
		return nil, err
	}
	client := mcpsdk.NewClient(
		&mcpsdk.Implementation{Name: "prism", Version: "1.0.0"},
		&mcpsdk.ClientOptions{
			// An explicit empty capability set: the zero value would advertise
			// roots/listChanged, which Prism does not implement.
			Capabilities: &mcpsdk.ClientCapabilities{},
			ToolListChangedHandler: func(ctx context.Context, _ *mcpsdk.ToolListChangedRequest) {
				p.logger.Info("mcp tool list changed", "server", e.name)
				// Best effort: a notification handler has nowhere to report an
				// error, and any later List re-reads the list anyway.
				if _, err := p.loadTools(ctx, e); err != nil {
					p.logger.Error("failed to refresh mcp tool list", "server", e.name, "error", err)
				}
			},
		},
	)

	session, err := client.Connect(dialCtx, transport, nil)
	if err != nil {
		e.noteFailureLocked()
		return nil, fmt.Errorf("connect to %q: %w", e.name, err)
	}
	e.session = session
	e.failures = 0
	e.backoffUntil = time.Time{}
	p.logger.Info("mcp server connected", "server", e.name, "transport", e.cfg.Transport)
	return session, nil
}

// OnChange registers a callback fired when a server's published tool set
// actually differs from the previous one, whether that came from an explicit
// refresh or a tools/list_changed notification. Callers use it to invalidate
// whatever they derived from discovery. A refresh that returns the same tools
// does not fire, so repeated polling cannot cause churn.
func (p *Pool) OnChange(fn func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onChange = append(p.onChange, fn)
}

// notifyChange runs the change callbacks outside the pool lock, since a
// callback is free to call back into the pool.
func (p *Pool) notifyChange() {
	p.mu.Lock()
	callbacks := make([]func(), len(p.onChange))
	copy(callbacks, p.onChange)
	p.mu.Unlock()
	for _, fn := range callbacks {
		fn()
	}
}

func (p *Pool) lookup(name string) *entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	return p.entries[name]
}

// recoverable reports whether an error means the session is gone rather than
// the request being bad. Only these justify discarding a connection; a tool
// that rejects its arguments is the caller's problem, not the session's.
func (p *Pool) recoverable(err error) bool {
	return errors.Is(err, mcpsdk.ErrConnectionClosed) || errors.Is(err, mcpsdk.ErrSessionMissing)
}

// Close disconnects every session, which for stdio terminates the child
// processes. Called on shutdown so no server is orphaned.
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	entries := make([]*entry, 0, len(p.entries))
	for _, e := range p.entries {
		entries = append(entries, e)
	}
	p.entries = map[string]*entry{}
	p.mu.Unlock()

	for _, e := range entries {
		e.reset(true)
	}
}

func (e *entry) transport(ctx context.Context) (mcpsdk.Transport, error) {
	if e.cfg.Dial != nil {
		return e.cfg.Dial(ctx)
	}
	switch e.cfg.Transport {
	case TransportStdio:
		if e.cfg.Command == "" {
			return nil, fmt.Errorf("server %q has no command for stdio transport", e.name)
		}
		cmd := exec.Command(e.cfg.Command, e.cfg.Args...)
		cmd.Env = childEnv(e.cfg.Env)
		return &mcpsdk.CommandTransport{Command: cmd}, nil
	case TransportHTTP:
		if e.cfg.URL == "" {
			return nil, fmt.Errorf("server %q has no url for http transport", e.name)
		}
		transport := &mcpsdk.StreamableClientTransport{
			Endpoint: e.cfg.URL,
			// Retries belong to the supervisor, which can also re-spawn the
			// process; letting the SDK retry independently risks two reconnect
			// policies fighting over one session.
			MaxRetries: -1,
		}
		if len(e.cfg.Headers) > 0 {
			// Headers are per-server credentials, so they go on a transport
			// scoped to this server rather than a shared client.
			transport.HTTPClient = &http.Client{
				Transport: headerTransport{base: http.DefaultTransport, headers: e.cfg.Headers},
			}
		}
		return transport, nil
	default:
		return nil, fmt.Errorf("server %q has unsupported transport %q", e.name, e.cfg.Transport)
	}
}

// reset closes the session and discards what it published, since a tool list
// must never outlive the connection that produced it. Teardown happens outside
// the entry lock so a notification arriving during it cannot deadlock.
// backoff records the failure; an operator-forced reset clears it instead.
func (e *entry) reset(backoff bool) {
	e.mu.Lock()
	session := e.session
	e.session = nil
	e.tools = nil
	if backoff {
		e.noteFailureLocked()
	} else {
		e.failures = 0
		e.backoffUntil = time.Time{}
	}
	e.mu.Unlock()

	if session != nil {
		if err := session.Close(); err != nil {
			// Teardown is best effort; the process is signalled regardless and
			// there is no caller left to report to.
			_ = err
		}
	}
}

func (e *entry) backingOff() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return time.Now().Before(e.backoffUntil)
}

// setTools records a freshly discovered tool set and reports whether it
// differs from the one already held.
func (e *entry) setTools(tools []Discovered) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if slices.EqualFunc(e.tools, tools, func(a, b Discovered) bool {
		return a.Tool == b.Tool && a.Hash == b.Hash && a.Description == b.Description &&
			bytes.Equal(a.InputSchema, b.InputSchema)
	}) {
		return false
	}
	e.tools = tools
	return true
}

// noteFailureLocked doubles the reconnect delay. The caller holds e.mu.
func (e *entry) noteFailureLocked() {
	e.failures++
	delay := baseBackoff << min(e.failures-1, 16)
	if delay > maxBackoff {
		delay = maxBackoff
	}
	e.backoffUntil = time.Now().Add(delay)
}

// toDiscovered converts SDK tools into the pool's representation, computing
// each approval hash once here rather than on every listing.
func toDiscovered(server string, tools []*mcpsdk.Tool) ([]Discovered, error) {
	out := make([]Discovered, 0, len(tools))
	for _, tool := range tools {
		hash, err := DefinitionHash(tool)
		if err != nil {
			return nil, fmt.Errorf("server %q tool %q: %w", server, tool.Name, err)
		}
		d := Discovered{
			Server:      server,
			Tool:        tool.Name,
			Description: tool.Description,
			Hash:        hash,
		}
		// A schema that will not marshal is reported as absent rather than
		// failing the listing: the tool is still callable, it just cannot
		// advertise its arguments.
		if schema, err := json.Marshal(tool.InputSchema); err == nil {
			d.InputSchema = schema
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tool < out[j].Tool })
	return out, nil
}

// toResult flattens an MCP result into a single value. StructuredContent wins
// because it is the machine-readable form the server committed to; text blocks
// are the fallback. Image, audio and embedded-resource blocks have no text
// rendering and are dropped, so is_error is then the only signal that anything
// came back at all.
func toResult(out *mcpsdk.CallToolResult) Result {
	if out == nil {
		return Result{}
	}
	if out.StructuredContent != nil {
		return Result{Value: out.StructuredContent, IsError: out.IsError}
	}
	var text []string
	for _, content := range out.Content {
		if block, ok := content.(*mcpsdk.TextContent); ok {
			text = append(text, block.Text)
		}
	}
	switch len(text) {
	case 0:
		return Result{IsError: out.IsError}
	case 1:
		return Result{Value: text[0], IsError: out.IsError}
	default:
		return Result{Value: strings.Join(text, "\n"), IsError: out.IsError}
	}
}

// inheritedEnv is the ambient whitelist a spawned stdio server receives in
// addition to whatever its record configures. It is a whitelist rather than the
// whole parent environment so a server is a poor-man's sandbox: it can find and
// run its executable, but it cannot read credentials that happen to be lying
// around in the gateway's own environment. GATEWAY_ENCRYPTION_KEY is the sharp
// example — handing it to a child would hand over the key to every ciphertext in
// the database.
//
// Anything else a server needs is set explicitly in its record, where it is
// encrypted at rest and therefore not accidental.
var inheritedEnv = []string{
	// Enough to find and run the executable at all.
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR",
	// Locale and timezone, so a child does not format output differently
	// from the operator reading it.
	"LANG", "LC_ALL", "LC_CTYPE", "TZ",
	// Outbound proxy configuration, for a server that fetches.
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
	// Windows equivalents, so one record works on either platform.
	"SystemRoot", "COMSPEC", "PATHEXT",
	"USERPROFILE", "APPDATA", "LOCALAPPDATA", "ProgramData",
	// Node toolchains are usually installed under a version manager that
	// locates itself through these, and npx is the common MCP server launcher.
	"NVM_DIR", "NVM_BIN", "NODE_PATH",
}

// childEnv builds a stdio server's environment: the inherited whitelist, then
// the record's own values layered on top so a configured variable always wins.
// The result is sorted so a given configuration always produces the same
// environment, which keeps behaviour reproducible and diffable.
func childEnv(configured map[string]string) []string {
	merged := make(map[string]string, len(inheritedEnv)+len(configured))
	for _, name := range inheritedEnv {
		if value, ok := os.LookupEnv(name); ok {
			merged[name] = value
		}
	}
	for name, value := range configured {
		merged[name] = value
	}
	out := make([]string, 0, len(merged))
	for name, value := range merged {
		out = append(out, name+"="+value)
	}
	slices.Sort(out)
	return out
}

// headerTransport injects configured headers into every request.
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// A RoundTripper must not modify the request it is given.
	clone := req.Clone(req.Context())
	for key, value := range t.headers {
		clone.Header.Set(key, value)
	}
	return t.base.RoundTrip(clone)
}
