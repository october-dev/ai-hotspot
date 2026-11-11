# AI Hotspot

**Share your AI model access with everyone on your WiFi, without sharing your keys.**

AI Hotspot is a self-hosted, OpenAI-compatible model router for your local network. One binary, standard library only, with a live usage dashboard.

You (the operator) hold the real provider API keys. Everyone on the same
WiFi/LAN uses models through your keys with **no accounts** — you just hand them
a base URL and a client key. AI Hotspot tracks per-client usage and budgets and
shows a live web dashboard.

It speaks the OpenAI API, so any OpenAI-compatible client, SDK, or tool works
against it — just point the client at your hotspot's base URL and use a client
key as the API key.

---

## How it works

```
  Phone / laptop on WiFi                 AI Hotspot (this server)            Upstream providers
  ┌──────────────────┐   Bearer hs_...   ┌────────────────────────┐  real   ┌──────────────────┐
  │ OpenAI client     │ ───────────────▶ │ auth + budget check     │  key    │ orcarouter        │
  │ base=hotspot URL  │                   │ route by model prefix   │ ──────▶ │ openrouter        │
  │ model=            │ ◀─────────────── │ inject provider key     │         │ featherless       │
  │  openrouter/...   │   SSE / JSON      │ record usage            │         │ concurred         │
  └──────────────────┘                   └────────────────────────┘         └──────────────────┘
```

- **Client keys** (`hs_...`) are issued by the operator from the dashboard or the
  admin API. Clients never see the real provider keys.
- **Model prefix routing**: a model like `openrouter/anthropic/claude-3.5-sonnet`
  is routed to the `openrouter` upstream with model `anthropic/claude-3.5-sonnet`.
  A model with no known prefix goes to the configured `defaultUpstream` unchanged.
- **Usage tracking**: prompt/completion/total tokens are read from the upstream's
  `usage` object (including the final streaming chunk) and rolled up per client,
  with an optional per-client token **budget** enforced before proxying.

---

## Quickstart

Requires Go 1.21+.

```bash
# 1. Configure
cp config.example.json config.json
#   then edit config.json: set adminToken and your real provider apiKeys

# 2. Run
go run ./cmd/ai-hotspot
#   (or: go build -o ai-hotspot ./cmd/ai-hotspot && ./ai-hotspot)
```

On startup it prints every LAN URL, e.g.:

```
AI Hotspot on http://192.168.1.42:8760  (dashboard: http://192.168.1.42:8760/)
```

Open the dashboard, paste your admin token, and create a client key.

Flags:

- `--config <path>` — config file (default `config.json`)
- `--port <n>` — override the config port

---

## Configuration

`config.json` (see `config.example.json`):

| Field             | Meaning                                                        |
|-------------------|----------------------------------------------------------------|
| `port`            | Port to bind on `0.0.0.0` (default `8760`).                    |
| `adminToken`      | Bearer token for the admin API and dashboard. **Required.**    |
| `defaultUpstream` | Upstream name used when a model has no known prefix.           |
| `dataFile`        | Where clients + usage are persisted (default `./data/hotspot.json`). |
| `upstreams[]`     | List of OpenAI-compatible providers.                           |

Each upstream:

| Field          | Meaning                                                     |
|----------------|-------------------------------------------------------------|
| `name`         | Prefix used for routing (`<name>/<model>`).                 |
| `baseURL`      | Provider base URL, e.g. `https://openrouter.ai/api/v1`.     |
| `apiKey`       | The operator's real provider key.                           |
| `extraHeaders` | Optional headers to inject (e.g. OpenRouter's `HTTP-Referer`, `X-Title`). |

Example upstream bases: `orcarouter` → `https://api.orcarouter.ai/v1`,
`openrouter` → `https://openrouter.ai/api/v1`,
`featherless` → `https://api.featherless.ai/v1`,
`concurred` → `https://concurred.ai/api/v1`.

State is written **atomically** and reloaded on boot, so restarts keep clients
and usage.

---

## Connecting a LAN client

Give someone on your WiFi two things: the base URL (from the startup banner) and
a client key. Then they use it exactly like the OpenAI API.

**curl (non-streaming):**

```bash
curl http://192.168.1.42:8760/v1/chat/completions \
  -H "Authorization: Bearer hs_your_client_key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "openrouter/anthropic/claude-3.5-sonnet",
    "messages": [{"role": "user", "content": "Hello from the hotspot!"}]
  }'
```

**curl (streaming, SSE):**

```bash
curl -N http://192.168.1.42:8760/v1/chat/completions \
  -H "Authorization: Bearer hs_your_client_key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "openrouter/anthropic/claude-3.5-sonnet",
    "stream": true,
    "messages": [{"role": "user", "content": "Stream a haiku."}]
  }'
```

**OpenAI Python SDK:**

```python
from openai import OpenAI
client = OpenAI(base_url="http://192.168.1.42:8760/v1", api_key="hs_your_client_key")
resp = client.chat.completions.create(
    model="openrouter/anthropic/claude-3.5-sonnet",
    messages=[{"role": "user", "content": "Hi"}],
)
```

If you use a model with no known upstream prefix, it is sent to `defaultUpstream`
unchanged (e.g. `model: "gpt-4o-mini"`).

---

## The dashboard

Visit `http://<lan-ip>:<port>/`. It is a single self-contained page (no external
CDNs). It prompts for the admin token (stored in `localStorage`, sent as the
Bearer), then polls `/admin/usage` live and shows:

- **Totals**: clients, requests, total tokens, prompt/completion split.
- **Clients table**: name, masked key (with copy), requests, tokens used vs.
  budget with a progress bar.
- **Create client** form (name + optional token budget).
- **Recent requests** feed: time, client, upstream, model, tokens, status.

---

## API reference

### Client endpoints (auth: `Authorization: Bearer <clientKey>`)

- `POST /v1/chat/completions` — routes by model prefix, injects the upstream key,
  forwards the body. Supports streaming (SSE pass-through) and non-streaming. For
  streaming it adds `"stream_options":{"include_usage":true}` if absent so usage
  arrives in the final chunk. Returns **HTTP 402** if the client is over budget.
- `GET /v1/models` — proxied to the default upstream's `/models`.

### Admin endpoints (auth: `Authorization: Bearer <adminToken>`)

- `POST /admin/clients` — body `{ "name": "...", "budgetTokens": 0 }` → creates and
  returns a new client (`{ "key": "hs_...", ... }`). `budgetTokens: 0` = unlimited.
- `GET /admin/clients` — list clients with usage + budget.
- `DELETE /admin/clients/{key}` — revoke a client key.
- `GET /admin/usage` — aggregate totals, per-client usage, and recent requests.

### Open

- `GET /health` — `{ "ok": true, ... }`, no auth.
- `GET /` — the dashboard.

---

## A note on costs

AI Hotspot does **not** compute real billed dollar amounts. All usage figures are
**approximations derived from token counts** reported by the upstream provider's
`usage` object. Actual provider pricing varies by model and may include costs
(images, tool calls, cache reads, minimums) not captured by token counts. Use
budgets as a soft guardrail, not an exact billing meter — reconcile against your
provider's own dashboard for real spend.

---

## Project layout

```
cmd/ai-hotspot/main.go     entrypoint: config, banner, LAN IPs, graceful shutdown
internal/config/           config loading + validation
internal/store/            atomic JSON-file store (clients, usage, recent) + mutex
internal/proxy/            model-prefix routing + streaming/non-streaming forwarding
internal/server/           HTTP handlers, auth, admin API
web/dashboard.html         embedded (go:embed) self-contained dashboard
config.example.json        sample config
```

No third-party dependencies, standard library only.

---

## Security

- **Keep it on your LAN.** AI Hotspot binds to `0.0.0.0` so devices on your network can reach it. Do not port-forward it or put it on a public server; anyone with a client key spends your provider credit.
- **Use a long, random `adminToken`.** It controls key creation and the dashboard. `openssl rand -hex 32` works.
- **`config.json` and `data/` hold secrets.** The first has your provider keys, the second your client keys. Both are in `.gitignore`; keep them out of backups you share.
- **Revoke keys you no longer need** from the dashboard or with `DELETE /admin/clients/{key}`.

To report a vulnerability, see [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE)
