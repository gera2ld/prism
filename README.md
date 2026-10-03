# Prism

One OpenAI-compatible endpoint in front of every model provider you use — with stable
model names, per-key access control, and per-request usage history. Single binary,
single SQLite file, web UI for configuration. Self-hosted, single operator.

Built on [PocketBase](https://pocketbase.io), which supplies the config store, admin
UI, and log store.

## Features

- **Stable aliases** — clients send a name you choose; swapping providers is a UI edit.
  One alias can map to several providers as a fallback chain.
- **Per-key access control** — optional allowlists over alias, provider, and model.
- **Tools for agents** — hand a client the tools it may call and run one on request.
  Your own tools are declarative HTTP recipes; the same catalog is also served over
  MCP for agents that speak it.
- **Usage history** — tokens (incl. cache hits), TTFT, duration, finish reason per request.
- **Request shaping** — optional JSONata transformers per provider, fail-closed and audited.
- **Debuggable when needed** — body capture off by default, capped, auto-expiring.
- **Encrypted secrets** — tokens encrypted at rest, shown only via reveal commands.

## Quick start (Docker)

Prebuilt images are published to `ghcr.io/gera2ld/prism` as `latest`, for both
`linux/amd64` and `linux/arm64`:

```bash
docker pull ghcr.io/gera2ld/prism:latest
```

Prism needs one secret at runtime, used to encrypt stored credentials:

```bash
docker run -d --name prism \
  -e GATEWAY_ENCRYPTION_KEY="$(openssl rand -base64 32)" \
  -v prism-data:/app/pb_data \
  -p 8090:8090 \
  ghcr.io/gera2ld/prism:latest
```

Generate the key once and keep it somewhere safe — losing it means re-entering all
stored provider tokens and client secrets. Without it the container refuses to start.

Then open the admin UI at `http://localhost:8090/_/` and create an account on first visit.

## First setup

1. **Add a provider** (`providers` collection): a `name` (e.g. `openai`), its `base_url`
   (the root Prism appends `/chat/completions` to), the upstream API key, `enabled`.
2. **Add a route** (`routes` collection): an `alias` (what your clients will send as
   `model`), the provider, the `upstream_model` name the provider expects, and a
   `priority` (lower wins).
3. **Create a client key**: `docker exec prism /app/prism key generate <name>` prints a
   `sk-…` secret once. Use it as the bearer token in your clients.

Point your client at `http://localhost:8090/v1`:

```bash
curl http://localhost:8090/v1/chat/completions \
  -H "Authorization: Bearer sk-…" \
  -H "Content-Type: application/json" \
  -d '{"model": "<alias>", "messages": [{"role": "user", "content": "hello"}]}'
```

Streaming works as usual (`"stream": true`); `GET /v1/models` lists usable aliases in
OpenAI's shape.

## Tools

Prism can hand an agent the tools it is allowed to call, and run one on request. It does
not decide to call them — the agent asks what is available, and runs its own loop.

```bash
curl http://localhost:8090/v1/tools -H "Authorization: Bearer sk-…"

curl http://localhost:8090/v1/tools/<name>/invoke \
  -H "Authorization: Bearer sk-…" -H "Content-Type: application/json" \
  -d '{"arguments": {"city": "Oslo"}}'
```

`GET /v1/tools` returns OpenAI's tool shape, so the array can be handed straight to a
chat request. A tool that ran and failed still answers `200` with `is_error: true` and
the reason, because an agent feeds that back as a tool message like any other result.

A tool is a [conduit](https://github.com/gera2ld/conduit) definition: a declarative
YAML/JSON document describing HTTP calls. The document supplies the name, description and
argument schema, and the engine validates arguments before running. Add one to the `tools`
collection in the admin UI, which documents every field; `enabled` is the only switch.
Definitions are checked when you save them, so a broken one is rejected there rather than
at call time.

Requests carry a `Prism/<version>` User-Agent, since Go sends none by default and many public
APIs reject an unidentified client. A step's own `headers` override it.

### Over MCP

The same tools are served over the Model Context Protocol at `http://localhost:8090/mcp`,
for agents that speak MCP instead of REST. Authenticate with a client API key, the same
bearer token the REST endpoints take:

```json
{ "mcpServers": { "prism": { "url": "http://localhost:8090/mcp",
                             "headers": { "Authorization": "Bearer sk-…" } } } }
```

Prism serves MCP; it does not connect to other people's MCP servers. It publishes tool
schemas exactly as your definitions write them, and an admin-UI change takes effect on the
next call with no restart.



## Reference

Both references document themselves, so this file does not duplicate them:

- **Commands** — `prism --help`, and `prism <group> --help` for `key`, `provider`,
  `route`, `tool`, and `schema`. Inside the container:
  `docker exec prism /app/prism --help`.
- **HTTP API** — interactive docs at `/api/prism/docs` on a running instance (raw
  spec at `/api/prism/docs/openapi.json`). Every command is also a superuser HTTP
  endpoint, alongside the OpenAI-compatible client endpoints. Authenticate with a
  PocketBase `_superusers` token.

Rotating a client secret: type a new value into `key_plain` in the admin UI, or clear
the field to have a new one generated. Either way the old secret stops working
immediately.

## Configuration overview

Everything lives in the admin UI; edits apply without a restart.

| Area | What it does |
| --- | --- |
| `providers` | Upstreams: name, base URL, token, on/off switch |
| `routes` | Alias → provider + upstream model, with priority |
| `api_keys` | Client keys, on/off switch, optional RE2 allowlists (`alias_pattern`, `provider_pattern`, `model_pattern`; empty means unrestricted) |
| `transformers` | JSONata reshaping per provider + model pattern, first match wins |
| `gateway_settings` | Body-capture toggle, capture retention, cleanup schedule |
| `request_logs` | Append-only usage history (tokens, timing, finish reason, outcome) |
| `tools` | Your own tools, as conduit definitions, with an on/off switch |
| `tool_logs` | Append-only tool invocation history (tool, transport, outcome, duration) |

Out of scope by design: stateful APIs (chat completions only), multi-user/quota models,
automatic cheapest-route selection, non-OpenAI provider dialects, running the agent's
loop — Prism lists the tools a client may call and executes one on request, but it never
decides to call them — and consuming other people's MCP servers.
