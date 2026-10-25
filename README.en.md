# ChatGPT2API

<p align="center">
  <b>Go single binary · ChatGPT account pool proxy · OpenAI / Anthropic compatible API</b>
</p>

<p align="center">
  <a href="./README.md">中文</a> ·
  <a href="./docs/deployment.md">Deployment</a> ·
  <a href="./docs/adr/">Architecture decisions</a> ·
  <a href="./CHANGELOG.md">Changelog</a>
</p>

<p align="center">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.27-00ADD8?logo=go&logoColor=white">
  <img alt="Next.js" src="https://img.shields.io/badge/Next.js-16-000000?logo=nextdotjs&logoColor=white">
  <img alt="License" src="https://img.shields.io/badge/license-MIT-blue">
  <img alt="Tests" src="https://img.shields.io/badge/tests-576%20passing-brightgreen">
</p>

---

Expose ChatGPT's web capabilities as a standard API, backed by a managed
multi-account pool.

The backend is **a single static binary built from Go 1.27 + Gin**. The frontend
is a Next.js static export embedded via `go:embed` into that same binary — so
deployment is **one file**, with no runtime dependencies, no `node_modules`, and
no Python environment. All state lives locally, with four storage backends to
choose from.

> [!WARNING]
> **Disclaimer**
>
> This project is built by reverse-engineering ChatGPT's web endpoints. It is
> intended solely for personal learning, technical research, and non-commercial
> technical exchange.
>
> - Commercial use, profit-driven use, bulk operations, automated abuse, or
>   large-scale invocation are strictly prohibited.
> - Use to disrupt market order, engage in malicious competition, arbitrage,
>   resell services, or otherwise violate OpenAI's Terms of Service or applicable
>   law is strictly prohibited.
> - Use to generate, distribute, or assist in generating illegal, violent,
>   sexual, or minor-related content, or for fraud, deception, or harassment is
>   strictly prohibited.
> - You assume all risk, including account rate-limiting, temporary or permanent
>   bans, and any legal liability arising from misuse.
> - Using this project constitutes acceptance of this disclaimer. Do not test
>   with important, primary, or high-value accounts.

---

## Contents

- [What it is](#what-it-is)
- [Quick start](#quick-start)
- [Configuration](#configuration)
- [Features](#features)
- [API](#api)
- [Development](#development)
- [Architecture](#architecture)
- [Known limitations](#known-limitations)
- [Documentation](#documentation)

---

## What it is

**In one line**: a self-hosted ChatGPT-to-API gateway plus an account pool manager.

It provides three things:

| Capability | Description |
|:--|:--|
| **Protocol translation** | Wraps ChatGPT's conversation and image-generation flows into OpenAI- and Anthropic-compatible HTTP APIs |
| **Pool scheduling** | Multi-account rotation, per-account concurrency slots, automatic invalid-token eviction, scheduled rate-limit refresh, password re-login recovery |
| **Web console** | Online image workbench, pool management, image management, logs, settings — all embedded in the same binary |

**Shape**:

```
┌─────────────────────────────────────────────┐
│  Single static binary (CGO_ENABLED=0)       │
│  ├── Gin HTTP server                        │
│  ├── Next.js static export (go:embed)       │
│  └── 8 internal packages, strictly one-way  │
└─────────────────────────────────────────────┘
              │                    │
        ┌─────┴─────┐        ┌─────┴──────┐
        │ Local state│        │ Upstream    │
        │ json/sqlite│        │ direct/proxy│
        │ pg/git     │        │ WARP/CF     │
        └───────────┘        └────────────┘
```

**Scale**: 31,350 lines of non-test Go, 20,079 lines of tests, 576 tests passing.

---

## Quick start

### Docker (recommended)

```bash
git clone https://github.com/IFuZYI/chatgpt2api.git
cd chatgpt2api

# 1. Create config.json. Compose binds it with create_host_path: false, so a
#    missing file fails loudly. That is deliberate: the short syntax would
#    silently create a same-named *directory*, the service would fail to read
#    config, and the error would read "config is a directory" — hard to diagnose.
cp config.example.json config.json

# 2. Set the auth key. Generate one with `openssl rand -hex 32`;
#    do not use the project name or "admin".
$EDITOR config.json     # fill in "auth-key"

# 3. Start
docker compose up -d
```

> **Prefer not to use config.json?** Environment variables alone are enough —
> see [Configuration](#configuration). The compose file mount still requires the
> file to exist, so the lightweight path under Docker is to
> `cp config.example.json config.json` and then **override only via environment**,
> without editing the file.

| | |
|:--|:--|
| Web console | `http://localhost:3000` |
| API base | `http://localhost:3000/v1` |
| Data directory | `./data` |

> The container listens on **8080** internally: distroless `nonroot` (uid 65532)
> lacks `CAP_NET_BIND_SERVICE` and cannot bind ports below 1024. The published
> port is controlled by `CHATGPT2API_PORT` (default `3000`).

> Ownership of `data/` is fixed automatically by the bundled `init-data`
> one-shot container (chowns to uid 65532). Without it the service reports
> `/health` 200 yet fails every write with `permission denied` — see
> [troubleshooting](./docs/deployment.md#容器起来了但写数据报-permission-denied).

Updating:

```bash
docker compose pull && docker compose up -d
```

### Prebuilt binary

Download from [Releases](https://github.com/IFuZYI/chatgpt2api/releases):

| Platform | File |
|:--|:--|
| Linux x86_64 | `chatgpt2api_*_linux_amd64` |
| Linux ARM64 | `chatgpt2api_*_linux_arm64` |
| Linux ARMv7 | `chatgpt2api_*_linux_armv7` |
| macOS Intel / Apple Silicon | `chatgpt2api_*_darwin_amd64` / `_darwin_arm64` |
| Windows x64 | `chatgpt2api_*_windows_amd64.exe` |

```bash
sha256sum -c SHA256SUMS.txt          # verify
chmod +x chatgpt2api_*_linux_amd64

mkdir -p /opt/chatgpt2api && cd /opt/chatgpt2api
CHATGPT2API_AUTH_KEY=$(openssl rand -hex 32) \
  /path/to/chatgpt2api_*_linux_amd64 --addr :8080
```

First run creates `data/` in the current directory. The binary **already embeds
the frontend** — no separate static files needed.

`config.json` is **not** created automatically — without it the service starts
from an empty config plus environment variables. To change settings (image
retention, proxy, backup, moderation), fetch the template:

```bash
curl -fsSLO https://raw.githubusercontent.com/IFuZYI/chatgpt2api/main/config.example.json
mv config.example.json config.json && $EDITOR config.json
```

Once the service is running, the remaining settings can also be changed in the
web console, which writes them back to `config.json`.

### From source

Requires **Go 1.27+** and **Node 22+**.

```bash
git clone https://github.com/IFuZYI/chatgpt2api.git
cd chatgpt2api

# web_dist contains a .gitkeep placeholder, so this compiles without building
# the frontend first.
go build -o chatgpt2api ./cmd/server
CHATGPT2API_AUTH_KEY=dev-secret ./chatgpt2api --addr :8000
```

> Use **`:8000`** for source runs: the frontend dev server hardcodes
> `http://127.0.0.1:8000` (see `web/src/constants/common-env.ts`). The Docker
> setup (8080 inside, 3000 published) is a separate arrangement.

Frontend with hot reload:

```bash
cd web && npm install && npm run dev
```

Point the backend at an un-embedded frontend build:

```bash
cd web && npm run build                # output in web/out
CHATGPT2API_WEB_DIST=$PWD/web/out CHATGPT2API_AUTH_KEY=dev ./chatgpt2api --addr :8000
```

### WARP / FlareSolverr (optional)

When the image pipeline keeps hitting Cloudflare challenges:

```bash
cp .env.example .env
docker compose -f docker-compose.warp.yml up -d --build
```

This additionally starts `warp` (egress), `privoxy` (SOCKS5→HTTP),
`flaresolverr` (challenge solving), and a one-shot `init-config` that writes
proxy_runtime defaults into `config.json`.

> ⚠️ `warp` needs `NET_ADMIN` / `SYS_MODULE`; `init-config` mounts the project
> directory to write `config.json`. Check host directory permissions, and note
> that `.env` credentials are visible to other containers.

### Deployment comparison

| Method | Best for | Frontend | Data | Upgrade |
|:--|:--|:--|:--|:--|
| Docker Compose | Production self-hosting | Embedded in image | `./data` volume | `docker compose pull` |
| Single binary | No Docker, edge devices | Embedded at build | `data/` in CWD | Replace file, restart |
| From source | Development | `CHATGPT2API_WEB_DIST` | `data/` in CWD | Rebuild |

---

## Configuration

Precedence: **environment > `config.json` > code defaults**.

Mind the **scope**, though: only the settings below accept environment
variables. Everything else (image retention, proxy, backup, moderation) must go
in `config.json` or be set from the web console.

| Environment-configurable | Notes |
|:--|:--|
| `CHATGPT2API_AUTH_KEY` | Auth key. **No default** — the service refuses to start if unset in both places |
| `CHATGPT2API_BASE_URL` | Public base URL |
| `CHATGPT2API_BASE_DIR` | Data root |
| `CHATGPT2API_ADDR` | Listen address |
| `CHATGPT2API_DEBUG` | Diagnostic logging |
| `CHATGPT2API_WEB_DIST` | Frontend static dir |
| `STORAGE_BACKEND` / `DATABASE_URL` | Storage backend |
| `GIT_REPO_URL` / `GIT_TOKEN` / `GIT_BRANCH` / `GIT_FILE_PATH` / `GIT_AUTH_KEYS_FILE_PATH` | git backend |

`config.json` is **optional**: when absent the service starts from an empty
config (`readJSONObject` returns an empty map rather than failing). So a
**minimal, environment-only deployment works**:

```bash
CHATGPT2API_AUTH_KEY=$(openssl rand -hex 32) ./chatgpt2api --addr :8080
```

As long as `auth-key` has a value — from the environment or the file — the
service starts.

### Environment variables

| Variable | Description | Default |
|:--|:--|:--|
| `CHATGPT2API_AUTH_KEY` | Auth key. **Required**; empty refuses to start | none |
| `CHATGPT2API_BASE_URL` | Public base URL for generated image links | empty, falls back to request `Host` |
| `CHATGPT2API_ADDR` | Listen address | `:8080` (in compose) |
| `CHATGPT2API_BASE_DIR` | Data root | binary's directory |
| `CHATGPT2API_DEBUG` | `1` emits `[image]` diagnostics | off |
| `CHATGPT2API_WEB_DIST` | Frontend static dir (dev, overrides embed) | empty |
| `STORAGE_BACKEND` | `json` / `sqlite` / `postgres` / `git` | `json` |
| `DATABASE_URL` | File path for sqlite, DSN for postgres | empty |

See [`.env.example`](./.env.example) for the full list (git backend, proxy runtime).

### CLI flags

| Flag | Description |
|:--|:--|
| `--addr` | Listen address, e.g. `:8080` |
| `--base-dir` | Data root |
| `--healthcheck` | Probe and exit (used by container healthcheck; distroless has no curl) |

### Storage backends

| Backend | Description | When to pick it |
|:--|:--|:--|
| `json` | Local JSON files (default) | Few accounts, single instance |
| `sqlite` | Local SQLite (CGO-free) | More accounts; lower memory |
| `postgres` | External PostgreSQL | External backups or a dedicated DB |
| `git` | Private Git repository | Versioned backups via git |

```yaml
# PostgreSQL
environment:
  STORAGE_BACKEND: postgres
  DATABASE_URL: postgresql://user:password@host:5432/dbname
```

```yaml
# SQLite — note the four slashes.
# sqlite:/// is treated as a relative path and later fails with
# "unable to open database file", which is hard to trace back to the slash count.
environment:
  STORAGE_BACKEND: sqlite
  DATABASE_URL: sqlite:////app/data/accounts.db
```

> **Single-instance constraint**: local state plus in-process task queues means
> **horizontal scaling is not supported**. See
> [ADR-0004](./docs/adr/0004-single-instance-local-state.md).

---

## Features

### API compatibility

| Endpoint | Status |
|:--|:--|
| `POST /v1/images/generations` | ✅ OpenAI-compatible, `n=1–4`, supports `stream` |
| `POST /v1/images/edits` | ✅ multipart and JSON (data URL) |
| `POST /v1/chat/completions` | ✅ text / image / web-search modes, SSE streaming |
| `POST /v1/responses` | ✅ with `image_generation` / `web_search` tools |
| `POST /v1/messages` | ✅ Anthropic-compatible (separate error envelope) |
| `POST /v1/search` | ✅ Web search |
| `POST /v1/ppt/generations` | ✅ PPT export (async task) |
| `POST /v1/psd/generations` | ✅ PSD export (async task) |
| `GET /v1/models` | ✅ Text models fetched at runtime + built-in image models |
| `http(s)://` refs in `/v1/images/edits` | ❌ Returns 400 (no server-side remote fetch) |

### Models

`GET /v1/models` returns 15 models, varying with pool capability:

- **Text** (11): `auto`, `gpt-5-3-mini`, `gpt-5-6`, `gpt-5-6-mini`,
  `gpt-5-6-t-mini`, `gpt-5-6-t-mini-mini`, `gpt-6`, `gpt-6-mini`,
  `gpt-6-t-mini`, `gpt-6-t-mini-mini`, `research`
- **Image** (4): `gpt-image-2`, `gpt-image-2.5`, `gpt-image-2.5-flare`,
  `gpt-image-2.5-sunburst`

Text models are fetched at runtime from upstream `/backend-api/models`
(TTL 300s, invalidated on pool change), so new text models need no code change.
Image models are a built-in table.

> **Codex image generation** (`codex-gpt-image-2`) goes through
> `/backend-api/codex/responses` and requires a `Plus` / `Team` / `Pro` account
> with `source_type=codex`. The request side is byte-aligned, but the **response
> event format has not been verified against the real upstream**; a shape
> mismatch reports `No image result found in response` rather than silently
> returning an empty image. See
> [ADR-0010](./docs/adr/0010-codex-responses-response-shape-unverified.md).

### Image workbench

- Generation, single-image editing, multi-image composition
- Reference image upload, multi-image interaction
- Local session history (review / delete / clear)
- Server-side image URL caching, progress tracking, resume-wait after timeout
- Lazy loading and scroll-position memory

### Pool management

- Automatic refresh of email, plan type, quota, and recovery time (async progress)
- Slot-based rotation for generation/editing (per-account concurrency configurable, default 3)
- Invalid tokens (401/403) evicted automatically; **429 rate limits never evicted**
- Scheduled rate-limit refresh
- **Password re-login** with TOTP two-factor recovery
- **`refresh_token` → `access_token`** keepalive to prevent rt expiry from idling
- **`auto_relogin_after_refresh`**: automatic re-login after a refresh detects an
  invalid account (429 does not trigger it)
- Global HTTP / HTTPS / SOCKS5 / SOCKS5H proxy
- WARP / FlareSolverr proxy runtime
- Search, filter, bulk refresh, export, manual edit, cleanup
- Four import paths: local CPA JSON, remote CPA server, `sub2api` server, `access_token`
- Three image storage modes: `local` / `webdav` / `both`

### Image storage

| Mode | Behavior | Use when |
|:--|:--|:--|
| `local` (default) | Local disk only | Single host, few images |
| `webdav` | WebDAV only; failure fails the whole save | Want zero local disk usage |
| `both` | Local + WebDAV | Balance of reliability and offloading |

In `both` mode a WebDAV failure **degrades to local and still returns success**;
the index honestly records `storage=local` / `webdav=false`, and the remote
upload happens asynchronously without blocking the generation response.
`webdav` mode does the opposite — fail means fail — so users never believe an
image is remote while local disk silently fills up. See
[ADR-0011](./docs/adr/0011-both-mode-degrades-on-webdav-failure.md) /
[ADR-0012](./docs/adr/0012-both-mode-async-upload.md).

---

## API

All `/v1/*` and `/api/*` endpoints require:

```http
Authorization: Bearer <auth-key>
```

Three error envelopes (one per client ecosystem — see
[ADR-0005](./docs/adr/0005-three-error-envelopes.md)):

| Route group | Error shape |
|:--|:--|
| `/api/*` (admin) | `{"detail": ...}` |
| OpenAI-compatible | `{"error": {"message","type","param","code"}}` |
| Anthropic | `{"type":"error","error":{"type","message"}}` |

### Image generation

```bash
curl http://localhost:3000/v1/images/generations \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <auth-key>" \
  -d '{
    "model": "gpt-image-2",
    "prompt": "a cat floating in space",
    "n": 1,
    "response_format": "b64_json"
  }'
```

| Field | Notes |
|:--|:--|
| `model` | Per `/v1/models`; `gpt-image-2` recommended |
| `n` | `1`–`4`. **Out of range returns 422 — never silently clamped** |
| `response_format` | `b64_json` (default) or `url` |
| `stream` | `true` uses SSE; failures emit an `error` event before `[DONE]` |

### Image editing

```bash
curl http://localhost:3000/v1/images/edits \
  -H "Authorization: Bearer <auth-key>" \
  -F "model=gpt-image-2" \
  -F "prompt=replace the background with a beach" \
  -F "image=@input.png"
```

`n` out of range returns 400. `http(s)://` references return 400 — the server
does not fetch remote images.

### Async tasks (PPT / PSD)

```bash
# Submit
curl -X POST http://localhost:3000/v1/ppt/generations \
  -H "Authorization: Bearer <auth-key>" \
  -H "Content-Type: application/json" \
  -d '{"prompt": "quarterly review", "client_task_id": "my-task-1"}'

# Query
curl "http://localhost:3000/v1/editable-file-tasks?ids=my-task-1" \
  -H "Authorization: Bearer <auth-key>"
```

Artifacts land in `data/files/ppt/<task_id>/` and are served at
`/files/ppt/<task_id>/<filename>`. Omitting `client_task_id` makes the server
generate a UUID; it acts as an **idempotency key**, so resubmitting the same id
from the same owner reuses the task.

### Admin surface

**77 routes** total (GET 30 / POST 42 / DELETE 5). The authoritative list is the
`*_routes.go` files under [`internal/api/`](./internal/api/). Common groups:

| Group | Prefix |
|:--|:--|
| Pool | `/api/accounts` (CRUD, refresh, re-login, import, export) |
| Images | `/api/images` (list, tags, delete, download, compress, cleanup-to-target) |
| Image tasks | `/api/image-tasks` (submit, query, resume-poll) |
| Settings | `/api/settings`, `/api/proxy/*`, `/api/image-storage/*` |
| Backup | `/api/backups` (R2 upload / list / download) |
| Integrations | `/api/cpa/pools/*`, `/api/sub2api/servers/*` |
| User keys | `/api/auth/users` (admin only) |
| Logs | `/api/logs` (admin only) |

---

## Development

### Tests

```bash
go test ./...                                 # 576 tests
go test -race ./...                           # race detector
go test -tags live ./internal/upstream/ -v    # real upstream handshake (needs network)
```

`-tags live` cases only record upstream state and assert nothing — upstream drift
should be *observed*, not turn CI red.

Coverage (statements):

| Package | Coverage | Package | Coverage |
|:--|--:|:--|--:|
| `web` | 91.5% | `config` | 65.1% |
| `health` | 88.5% | `upstream` | 55.8% |
| `protocol` | 78.2% | `service` | 56.1% |
| `api` | 44.1% | `storage` | 44.1% |

PostgreSQL backend tests require `TEST_POSTGRES_URL`; they are skipped otherwise.

### Release

Pushing a tag triggers two independent pipelines:

```bash
git tag v2.0.1 && git push origin v2.0.1
```

| Workflow | Purpose |
|:--|:--|
| [`release.yml`](./.github/workflows/release.yml) | Cross-platform binaries, `SHA256SUMS.txt`, GitHub Release |
| [`docker-publish.yml`](./.github/workflows/docker-publish.yml) | amd64/arm64 images to GHCR |

`release.yml` also supports `workflow_dispatch` for build-only runs.

---

## Architecture

8 packages with **strictly one-way** dependencies (verified with `go list -deps`,
test files included):

```
protocol   storage   web   config      ← no internal deps
      ↑
   upstream  →  protocol
      ↑
    service  →  config, protocol, storage, upstream
      ↑
      api    →  config, protocol, service, storage, web
```

**Why this split**: `service` imports neither `upstream` nor `api` — upstream
access is injected through function-typed seams (`ImageUpstreamFactory`,
`TaskRunner`, `EditableFileTaskRunner`). That lets business logic be tested
entirely against fake upstreams, and means swapping the upstream implementation
never touches the business layer.

**Error envelopes** live in one place (`internal/api/errors.go`) rather than being
scattered across handlers.

Key decisions are recorded in [`docs/adr/`](./docs/adr/) — 12 of them, covering
why the rewrite happened, how TLS fingerprinting is replicated, where each error
envelope applies, why data formats are preserved byte-for-byte, the goroutine
slot model, the single-instance constraint, why `both` mode degrades, and why
uploads became async.

---

## Known limitations

Listed honestly rather than papered over:

| Limitation | Detail |
|:--|:--|
| Single instance | Local state plus in-process queues; no horizontal scaling |
| Background tasks are not cancellable | A running image task cannot be interrupted; `Close` can only wait out the timeout |
| Streaming is not incremental | Images and text are written after completion; clients receive nothing during long requests, so proxies must disable buffering |
| Progress objects never evicted | Each admin refresh/re-login retains a pool copy until restart |
| No retention for editable tasks | `data/files/` and task records are never auto-cleaned |
| No inbound body size limit | Large request bodies can inflate process memory |
| Backups unencrypted by default | Archives contain every pool credential; `backup.encrypt` must be enabled explicitly |
| Codex response format unverified | Request side aligned; response event shape unconfirmed against real upstream (ADR-0010) |
| Anonymous conversation unavailable | Upstream restriction, not an implementation gap (ADR-0008) |

Full list with rationale in
[ADR-0009](./docs/adr/0009-go-rewrite-scope-and-gaps.md).

---

## Documentation

| Document | Contents |
|:--|:--|
| [Deployment](./docs/deployment.md) | Deployment modes, reverse proxy, storage, backup, upgrade/rollback, hardening, troubleshooting |
| [Feature status](./docs/feature-status.en.md) | Per-feature status |
| [ADR](./docs/adr/) | 12 architecture decision records |
| [Upstream SSE protocol](./docs/upstream-sse-conversation.md) | Conversation streaming protocol notes |
| [Python backend archive](./docs/archive-python-backend.md) | Where the 1.x Python implementation lives and why it was removed |
| [Glossary](./GLOSSARY.md) | Project terminology |
| [Changelog](./CHANGELOG.md) | Version history |

---

## Community

[LinuxDO](https://linux.do)

## License

[MIT](./LICENSE)

## Star History

[![Star History Chart](https://api.star-history.com/chart?repos=IFuZYI/chatgpt2api&type=date&legend=top-left)](https://www.star-history.com/?repos=IFuZYI%2Fchatgpt2api&type=date&legend=top-left)
