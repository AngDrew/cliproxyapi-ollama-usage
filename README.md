# CLIProxyAPI Ollama Usage

A native, read-only **quota-provider plugin** for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI).
Ollama appears in **Quota Management → Plugin**, using the existing management UI—not a replacement dashboard.

- Included credit balance, period allowance, purchased credits and billing reset.
- Legacy plans: session (5-hour) and weekly quota left, with reset timestamps.
- Last 24 hours: requests, plus spend and input/cached-input/output tokens when the plan reports them.
- Server-side Bearer authentication; keys never appear in the plugin quota response.
- A 60-second per-key cache, ten-second deadline per upstream request, and `Retry-After` backoff on HTTP 429.
- No inference hooks, model registration, quota resets, database or background polling.

Uses the documented [balance](https://docs.ollama.com/api/balance) and [cloud usage](https://docs.ollama.com/api/cloud-usage) endpoints:

```text
GET https://ollama.com/api/balance
GET https://ollama.com/api/usage?range=24h
```

Balance percentages come from included credits/allowance or legacy remaining percentages—not 24-hour spend divided by a monthly budget. Purchased credits are shown separately, not folded into the included-credit gauge. A zero allowance shows money without inventing a percentage.

## Requirements

- A current CLIProxyAPI **v8** build with dynamic plugins and quota-provider callbacks enabled.
- A current [CPAMC management UI](https://github.com/router-for-me/Cli-Proxy-API-Management-Center) with Plugin quota cards and summary metrics.
- A valid Ollama Cloud API key. This does not monitor a local Ollama daemon.
- Matching OS, CPU architecture **and C runtime** for the shared library.

Integration-tested against official CLIProxyAPI containers reporting **v8.0.52** and **v8.0.69** (glibc/Debian Bookworm). Release binaries target Linux glibc on amd64 and arm64. They are **not** Alpine/musl or native macOS binaries. Build locally with CGO for other hosts; do not copy a Mac library into a Linux container.

## Install

### 1. Get the library

Download the appropriate archive from [Releases](https://github.com/AngDrew/cliproxyapi-ollama-usage/releases) and extract `ollama-usage.so`. Verify it against the matching `SHA256SUMS-linux-*` file. Keep that exact filename; its basename is the plugin ID.

Or build for your Docker host (Docker only; no local Go installation needed):

```sh
git clone https://github.com/AngDrew/cliproxyapi-ollama-usage.git
cd cliproxyapi-ollama-usage
docker build --output type=local,dest=dist .
```

The library is in `dist/ollama-usage.so`. This runs the unit/race checks and vet as part of the build.

### 2. Mount and enable it

For Docker, merge these entries into your **existing** service; preserve your other volumes, environment and settings:

```yaml
services:
  cliproxyapi:
    environment:
      OLLAMA_API_KEY: ${OLLAMA_API_KEY:?Set the Ollama Cloud key}
    volumes:
      - ./plugins:/CLIProxyAPI/plugins:ro
```

Place the library at `./plugins/ollama-usage.so` on the Docker host. A Compose `.env` file alone does **not** inject the variable into the container; the `environment` entry above does.

Merge into the CLIProxyAPI `config.yaml` (preserve existing plugin settings):

```yaml
plugins:
  enabled: true
  dir: /CLIProxyAPI/plugins
  configs:
    ollama-usage:
      enabled: true
```

Use a persistent mounted plugins directory; otherwise container recreation loses the library.

### 3. Add a quota-only monitor credential

**Keep your existing OpenAI-compatible Ollama provider unchanged.** It continues to serve inference.

CLIProxyAPI does not list config-backed OpenAI-compatible keys as physical auth files in Quota Management. This plugin therefore uses a separate **monitor auth file**, not another inference provider.

Copy `ollama-monitor.example.json` into the existing, mounted **auth directory** (the directory configured by `auth-dir`, or `auth.dir` in v8 configuration), named `ollama-monitor.json`:

```json
{
  "type": "ollama-usage",
  "label": "Ollama Cloud",
  "api_key_env": "OLLAMA_API_KEY"
}
```

Use the actual directory on the Docker host, not an unmounted container folder. Restrict file permissions to the service user.

Alternatively replace `api_key_env` with `"api_key": "YOUR_OLLAMA_KEY"` in that private auth file. Do not commit or upload it publicly. The env form avoids keeping a second plaintext copy of the key. The key is resolved inside CLIProxyAPI, not in the browser.

For multiple accounts, add one uniquely named monitor file per account, with its own environment variable/key. Do not create multiple monitors for the same account unless you want duplicate cards; team balances can also be shared.

### 4. Restart and refresh

Recreate **only** the CLIProxyAPI service if adding an environment variable or mount:

```sh
docker compose up -d --no-deps cliproxyapi
```

Include your existing Compose overrides and env-file flags if applicable. The example is not an instruction to replace your deployment configuration.

Open **Quota Management → Plugin**, then refresh the `ollama-monitor.json` card. Loading the plugin alone is insufficient: the monitor auth file is what creates the quota card.

No CLIProxyAPI or management-UI fork is required. This repository does not automatically install itself into the official plugin store.

## Management API / herdr

The host exposes this plugin through its authenticated quota API:

```text
POST /v0/management/quota/fetch
Authorization: Bearer <CLIProxyAPI management key>
Content-Type: application/json

{"auth_index":"<monitor auth_index>","plugin_id":"ollama-usage"}
```

The v8 management UI uses `POST /v8/management/plugins/ollama-usage/quota` with the same `auth_index`. Both routes return standard `subscription`, `groups[].buckets[]` and numeric `summary[]` data. These requests take the **management key**, not the Ollama key.

[herdr-core-cliproxy](https://github.com/AngDrew/herdr-core-cliproxy) reads this same normalized response and shows Ollama alongside Claude and Codex. It needs only its existing hub connection, not an Ollama key.

## Scope, freshness and errors

- Some legacy plans report only request counts. Missing spend/token fields are omitted, not presented as zero.
- Usage is Ollama's `self` scope over the last 24h window, across clients—not a CLIProxyAPI-only traffic ledger. Included/purchased balances may be shared across a team.
- Ollama usage can lag. The plugin does not equate usage totals with remaining quota.
- Ollama documents a balance limit of 10 requests/minute per user, shared across keys/devices. The plugin caches per key for one minute; avoid unnecessary duplicate monitors/pollers.
- 401 means the Ollama key is invalid; 403 may mean suspension; 429 respects backoff. HTTP status errors are reported without upstream bodies or credentials.
- If either endpoint fails or the schema is unrecognized, refresh fails rather than substituting zero usage or 100% quota. The plugin does not silently return stale data.
- Refreshes across accounts are serialized. Use per-key locking if operating enough accounts for this to become noticeable.
- Plugin files are trusted native code inside the proxy process. Protect your management key and review third-party plugins before installing.

## Development checks

Native Go with a C compiler:

```sh
go test -race ./...
go vet ./...
go build -buildmode=c-shared -o dist/ollama-usage.so .
```

For native macOS builds use `dist/ollama-usage.dylib` instead. Linux release builds use the Bookworm Dockerfile to match the official host's glibc ABI.

After the Docker build, run:

```sh
python3 tests/integration.py
```

Requires Docker, Python 3 and `openssl`. The integration check loads the actual library in a separate real CLIProxyAPI container. A fake HTTPS Ollama service, private test CA and Docker DNS alias exercise credit/legacy balances, usage, plugin discovery, both quota routes, management auth and sanitized 401/429 responses. **No real Ollama credentials or inference are used.** Temporary containers/network/files are removed afterward. CLIProxyAPI's own version checks may access the internet.

CI builds/tests both Linux architectures and publishes archives/checksums on `v*` tags. License: MIT.
