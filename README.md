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
- **Usage history** — tokens (incl. cache hits), TTFT, duration, finish reason per request.
- **Request shaping** — optional JSONata transformers per provider, fail-closed and audited.
- **Debuggable when needed** — body capture off by default, capped, auto-expiring.
- **Encrypted secrets** — tokens encrypted at rest, shown only via reveal commands.

## Quick start (Docker)

Images are published to `ghcr.io/gera2ld/prism` as `latest` on every push to `main`
and each release, for both `linux/amd64` and `linux/arm64`.
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

## CLI reference

```bash
prism key generate <name>   # create a client key, printed once
prism key reveal <name>     # show a client secret again for copying
prism key list              # list keys (names and status, never secrets)

prism provider list         # list providers (names, URLs, status, never tokens)
prism provider reveal <name> # show a provider token for copying

prism route export <file>   # dump the routing table to CSV
prism route import <file>   # merge routes from CSV (--prune to delete absent rows)
```

## Superuser API

Every CLI command above is also an HTTP endpoint for superusers (PocketBase
`_superusers` token). See the interactive OpenAPI docs at `/api/prism/docs`
(raw spec: `/api/prism/docs/openapi.json`), which also document the
OpenAI-compatible `POST /v1/chat/completions` and `GET /v1/models` endpoints.

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

Out of scope by design: stateful APIs (chat completions only), multi-user/quota models,
automatic cheapest-route selection, and non-OpenAI provider dialects.
