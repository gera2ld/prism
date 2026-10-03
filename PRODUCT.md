# Prism

A single-binary, self-hosted proxy that puts one OpenAI-compatible endpoint in front of every
model provider I use, and records what each request cost in tokens and time.

Built on PocketBase used as a Go library: it supplies the config store, the admin UI, and the
log store, while the proxy itself is plain `net/http`.

## Goals

- **One endpoint, many providers.** Any OpenAI-compatible client points at this gateway and
  reaches whichever upstream is configured, without knowing which.
- **Stable names.** I refer to models by aliases I choose. Swapping the provider behind an alias
  is an edit in the admin UI, not a change in every client.
- **One place to reach for tools.** An agent asks this gateway what it may call and calls those
  tools here, and speaks MCP so agents that prefer it can ask for them there.
- **Know what I spent.** Every request records token usage, TTFT, and total duration.
- **Debuggable when I need it.** Full request/response capture exists, is off by default, and
  expires on its own.
- **Boring to operate.** One binary, one SQLite file, a web UI for config. No sidecars.

## Non-goals

- **Not the Responses API.** Stateful endpoints are out of scope. Chat Completions only.
- **Not the agent loop.** Prism lists the tools a client may call and executes one on request; it
  does not decide to call them. The agent drives its own loop, which keeps the gateway a
  pass-through for chat and keeps this feature from becoming an agent runtime.
- **Not a multi-tenant product.** Single operator, self-hosted. No org/team/quota model.
- **Not a translation layer.** Upstreams must already speak OpenAI-compatible HTTP. Providers
  with native-only APIs are handled by putting a compatible shim in front of them, not by
  teaching the gateway their dialect.
- **Not a cost optimizer.** No automatic cheapest-route selection, no semantic caching.
- **Not an MCP client.** Prism does not consume other people's MCP servers. Its tools are its
  own, defined in the database, so there is no third-party definition to review and approve. It
  does serve them over MCP, one direction only.

## Data model

Fourteen collections, declared in `internal/store/schema.json` and embedded in the binary:
nine are configuration and stores, four are read-only usage views.
The first two are configuration I edit by hand; `routes` and `transformers` shape requests;
`request_logs` and `request_bodies` are chat history; the rest belong to the tools surface.

### Schema ownership

The schema is data in the binary. `schema.json` holds a PocketBase collections snapshot,
applied in full on every open. It is the single source of truth in both
directions: a field it declares is created when absent and restored when deleted in the admin
UI, and a field or collection it no longer declares is removed along with its records. A fresh
database therefore needs nothing extra to have a schema, and changing one needs nothing extra
in either direction.

The admin UI is where a schema change is authored; exporting the collections document and
committing it is how the change comes back. `CanonicalSnapshot` accepts whatever shape the
export arrived in — a bare array or the list endpoint's `items` envelope, with system
collections mixed in — and normalizes it, so the file needs no hand-editing. Export is
deterministic, since PocketBase derives ids from names, so an unchanged database regenerates the
file byte for byte and a real change shows as a small diff. A test asserts that exact round-trip,
so the import cannot silently become lossy.

Because the snapshot is authoritative, a wrong one is an ordinary reviewable diff rather than a
silent boot-time effect — which is the reason it is a committed file and not something derived
at startup.

There are no migrations at all. The project registers none, and a test asserts that, because
the schema is the snapshot and there is no data to move: every row in the database is either
operator-authored configuration or something the gateway wrote and can recompute. A migration
appearing would mean something hand-written had crept in that the snapshot could describe.

With no migrations, the snapshot has to be able to carry a database forward from any earlier
state on its own. One case needed care. When a view selects a column that the new snapshot
drops, the import cannot satisfy it in either order: the table cannot lose the column while the
view selects it, and the view cannot be rewritten to select a column the table does not have
yet. Each half needs the other to have gone first. So a view whose query the snapshot rewrites is
dropped before the import and recreated by it. A view is a pure projection holding no rows, so
there is nothing to lose, and the snapshot rebuilds it with a query consistent with the tables
written alongside. Only the views whose query actually changed are dropped: an unchanged view
selects columns that still exist, so a boot where the schema did not change leaves every view
untouched. A view the snapshot does not declare is left for the import to delete, since deleting
one is never blocked by the column it selects.

### `providers`

An upstream that speaks OpenAI-compatible HTTP.

| Field | Notes |
| --- | --- |
| `name` | Unique, human-facing (`openai`, `bedrock`, `local-vllm`) |
| `base_url` | Root the gateway appends `/chat/completions` to |
| `api_key` | Hidden from the API; encrypted at rest with a key from the environment |
| `enabled` | Soft off-switch |

### `routes`

Maps an alias to a concrete model on a concrete provider. This is the whole routing table.

| Field | Notes |
| --- | --- |
| `alias` | What clients send in `model` |
| `provider` | Relation to `providers` |
| `upstream_model` | The name the provider actually expects |
| `priority` | Lower wins; ties broken arbitrarily |
| `enabled` | Soft off-switch |

An alias may have several routes. That is the answer to "the same model lives on two providers":
it is not a conflict, it is a fallback chain.

### `request_logs`

One row per request, written after the response completes or fails.

Identity and routing: `api_key` (relation), `api_key_name` (API-key name snapshot), `alias`,
`provider` (relation), `provider_name` (provider-name snapshot), `upstream_model`, `transformer`
(name of the transformer that shaped
the request, empty when none), `stream`. Request history is disposable pre-release data.
Usage: `prompt_tokens`, `completion_tokens`, `total_tokens`, `cached_tokens` (prompt-cache
hits, a subset of `prompt_tokens`) — all nullable.
Timing: `started_at` (gateway-side request start, UTC; NULL on rows logged before the field
existed), `ttft_ms` (streaming only), `total_ms`. `created` is the row-write time, i.e. request end.
Outcome: `status`, `outcome`, `error`, and `finish_reason`. `outcome` is one of `completed`,
`client_disconnected`, `upstream_disconnected`, `upstream_error`, `gateway_error`, or `rejected`;
`error` carries the detailed cause. `finish_reason` remains provider-produced, nullable, first
choice only, and the last non-empty value wins on streams. A streamed response is completed
only after its `[DONE]` event is successfully relayed.

### `request_bodies`

Optional debugging payload, one row per logged request, deleted on a schedule.

`log` (relation), `request`, `response`, `truncated`.

### `api_keys`

Keys clients authenticate with. `key_hash`, `key_plain` (secret encrypted like provider
keys, revealed with `key reveal <name>`; display-only, auth verifies against the hash),
`name`, `enabled`.

Leave `key_plain` empty when creating a key and a secret is autogenerated (`sk-`-prefixed);
typing one in derives the hash and locks it automatically. Retyping rotates; clearing regenerates.

Optional RE2 whitelists `alias_pattern` (client model value), `provider_pattern` (resolved
provider name), `model_pattern` (upstream model); empty means unrestricted, and all three
must pass. Filtering happens across the priority-ordered route list before the winner is
picked, so a key denied on the top route still reaches allowed lower routes; an alias with
no allowed route is a 403, and `/v1/models` lists only usable aliases. Invalid patterns are
rejected at save time.

Client and provider secrets share the same at-rest pattern: ciphertext in the database,
plaintext only via explicit reveal. Backups still deserve care, but a bare database file
alone yields no usable credentials.

### `transformers`

Request shaping per provider and model. `name` (unique), `provider` (relation, exact match),
`model_pattern` (RE2 regexp on the upstream model name, unanchored: `^gpt-4` for prefix,
`^model$` for exact), `priority` (lower wins, first match applies), `enabled`, `expression`
(JSONata producing the new request body object).

### `gateway_settings`

Singleton settings row, edited in the admin UI. `capture_bodies` (global capture toggle,
off by default), `retention_hours` (how long `request_bodies` rows are kept, default 24),
`retention_cron` (cleanup job schedule, default `0 3 * * *`).
Cached in memory, invalidated by record-change hooks, so edits apply without a restart —
including the cron schedule, which is re-registered live (an invalid expression keeps the
previous job, so a typo never kills cleanup). The singleton row is created or backfilled with
defaults on every start; its columns, like every other collection's, come from the snapshot.

Only `GATEWAY_ENCRYPTION_KEY` deliberately stays environment-only: a secret that must
not sit next to the ciphertext it protects, and is needed before the database opens.

### `tools` — conduit tools

Operator-authored tools, `name` (unique, `^[a-zA-Z0-9_-]{1,64}$`, the callable identity),
`definition` (a conduit document in YAML or JSON), `enabled` (on by default).

There is deliberately no description or schema column: both are derived from the parsed
definition, so they cannot drift from the behavior the agent is shown.

### `tool_logs`

One row per invocation, for the same reason `request_logs` exists: to see what was called and
how it went. `started_at`, `api_key` (relation), `api_key_name`, `tool`, `source` (`conduit`
or `mcp`), `arguments`/`result`/`truncated` (captured payload), `duration_ms`,
`status`, `outcome`, `error`.

A tool call has no provider, model or token usage, which is why this is separate from
`request_logs` rather than folded into it. Rows are permanent history; only the captured
payload ages out under the retention cron, matching how `request_bodies` works.

## Behavior

### Routing

- **Clients only ever name an alias.** Every `model` value must match a `routes.alias`
  (slashes allowed, e.g. `qwen/qwen3:free`); anything unaliased is an unknown-model error.
  There is no provider/model bypass — reaching a new model means adding a route.
- **Lowest enabled priority wins.** Disabled routes and disabled providers are skipped.
- **One alias, many providers.** Several routes may share an alias with different priorities,
  forming an ordered fallback chain; version one takes the winner and returns its error.
  Walking down the list on 429/5xx is a natural follow-up, not a launch requirement.

### Streaming

- **Relayed, not buffered.** Chunks are flushed as they arrive; the gateway adds no latency of
  its own beyond a tee into the usage parser.
- **TTFT is the first body byte**, measured from the moment the upstream request is sent.
- **`stream_options.include_usage` is injected** on streaming requests, so that providers which
  honor it report real numbers.

### Usage accounting

- **Recorded when the provider reports it, blank when it does not.** No local tokenizer, no
  estimates. An empty usage field means "unknown", and that is an acceptable answer.

### Transform

- **Shapes the upstream-bound request**, after the gateway's own mutation (model rewrite,
  `stream_options` injection) and before sending.
- **First match wins** by priority, among enabled transformers whose provider matches exactly
  and whose pattern matches the upstream model.
- **Fail-closed.** An eval error or non-object result rejects the request with a 500 and logs
  the cause; the upstream is never called. Bad expressions and patterns are rejected at save
  time, and each evaluation is bounded by a 5s timeout.
- **Auditable.** The winning transformer's name lands in `request_logs.transformer`; what got
  captured in `request_bodies` is the transformed body.

This is request shaping (drop unsupported params, inject defaults, rename fields), not a
translation layer: upstreams must still speak OpenAI-compatible HTTP.

### Tools

One source, one catalog. A tool is a conduit definition I wrote; the document supplies the
name, description and argument schema, and the engine validates arguments against that schema
before running. So publishing one is the whole job — the record name is the callable identity,
and the description and schema are derived rather than stored, so they cannot drift from the
behavior the agent is shown. Definitions are parsed at save time and an uncompilable one is
rejected, so bad YAML, unknown methods and duplicate step ids never persist.

Every outbound request carries a default `Prism/<version>` User-Agent. Go's HTTP client sends
none unless one is set, and public APIs answer an unidentified client with a bare 403 and no
explanation — Nominatim is the example that prompted this. The value is deliberately a bare
`Name/version` token: the conventional `App/1.0 (contact)` form gets *rejected* by the same
APIs, for containing parentheses or an `@`, so adding contact details at the gateway level would
introduce a failure. A step's own headers still win, so a definition that needs richer
identification sets it where it is written.

A tool that ran and failed is still HTTP 200 with `is_error: true` and the reason in `result`,
because an agent feeds both cases back as a tool message and cannot special-case a transport
failure. Only a gateway fault, or a name that is not callable, is a non-200.

### Exposing tools over MCP

The same catalog is served over the Model Context Protocol at `POST /mcp`, for agents that
speak it rather than REST. It is one direction only: Prism does not consume other people's MCP
servers.

- **Authenticated by a client API key**, checked in the router before the SDK sees the request,
  so `/mcp` cannot become an unauthenticated tool executor on a gateway that listens on a port.
- **The session is stateless.** The SDK builds a server per request and closes it when the
  request ends, so the catalog is rebuilt every time and an admin-UI edit applies with no
  restart. That is also what lets an invocation be attributed to the key that made it, since the
  tool handler runs inside the caller's request.
- **Schemas are published verbatim.** The SDK does not compile them, so a definition using any
  JSON Schema draft works as written.
- **Two unusable schemas are refused rather than published**, because the SDK panics on them: a
  schema that is not an object, and one carrying properties with no `type`. A definition with no
  schema at all is published as an empty object schema rather than hidden — it is still callable,
  and hiding it would be surprising. Refusal logs and skips, so an admin-UI edit cannot take the
  gateway down.
- **A failed tool is a tool-level error**, returned as `isError` with the reason as content,
  which is how an MCP client expects it. Only a gateway fault becomes a protocol error.

Invocations from either surface land in `tool_logs` with `transport` distinguishing them.

### Capture

- **Off by default**, enabled globally in settings.
- **Expires on a schedule.** Default retention 24h, enforced by a PocketBase cron.
- **Capped per body** at 256KB, with a `truncated` flag, so one runaway context cannot bloat
  the database file.

## Interfaces

Three seams keep PocketBase, the MCP SDK and conduit out of the proxy. Everything else about the
gateway is ordinary Go.

- **Config source** — resolves an alias to an ordered list of targets, and a presented key to an
  API key record. Read-heavy, cached in memory, invalidated by record-change hooks.
- **Log sink** — accepts a completed request record.
- **Tool registry** — lists callable tools and invokes one.

PocketBase implements all three. SQLite is the right store for a single-operator gateway, and the
realistic pressure point is log volume rather than concurrency, so retention is the answer
before a different database is. If that ever stops being true, these interfaces are the only
things a replacement has to satisfy.

The MCP server sits behind that seam too: it knows nothing about PocketBase, taking a tool registry
and a log sink, which is why it can be tested against in-process transports with no network.

## Surface

- `POST /v1/chat/completions` — streaming and non-streaming.
- `GET /v1/models` — the enabled aliases, in OpenAI's shape.
- `GET /v1/tools` — the tools this key may call, in OpenAI's tool shape so an agent can hand the
  array straight to a chat request.
- `POST /v1/tools/{name}/invoke` — run one tool. A failed tool is still 200 with `is_error` true.
- `POST /mcp` — the same tools over MCP, for agents that speak it.
- `/_/` — PocketBase admin UI: configuration, approvals, and usage history as a queryable table.

Authentication is a bearer token matched against `api_keys`. Key policy is model-scoped, so every
valid key sees the same approved tool catalog.

Operational CLI commands are exposed directly as `key generate`, `key reveal`, `key list`,
`provider list`, `provider reveal`, `route export`, `route import`, `tool list`, and
`tool validate`;
`just` is reserved for build, test, and server conveniences. Every CLI command is
also a superuser HTTP endpoint under `/api/prism/*` (same store methods, documented
via huma OpenAPI at `/api/prism/docs`, which additionally documents the
pass-through `POST /v1/chat/completions` (JSON and SSE), `GET /v1/models`,
`GET /v1/tools`, and `POST /v1/tools/{name}/invoke`
endpoints for client-key holders.

## Open questions

- **Whether `/v1/embeddings` is worth adding.** Same passthrough shape, so cheap, but unused so far.
- **Whether failover deserves to exist at all**, or whether a failed request I can see in the log
  is the more honest outcome for a personal gateway.
- **Whether tool policy should be per-key.** Today every valid key sees the whole catalog,
  because key policy is model-scoped only. A per-key allowlist would fit the existing
  `alias_pattern` style, but I have not yet wanted it.
- **Whether serving MCP is earning its keep.** It is one dependency the binary would not have
  without it, and one more surface to document and keep working. It is cheap now that the
  catalogue build is shared with REST, but it is not free.
