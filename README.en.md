# marvis2api-panel

[中文](README.md) · [Deployment guide (中文)](docs/DEPLOYMENT.md) · [Contributing](CONTRIBUTING.md)

[![CI](https://github.com/qianjindexiaozu/marvis2api-panel/actions/workflows/ci.yml/badge.svg)](https://github.com/qianjindexiaozu/marvis2api-panel/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

A self-hosted gateway exposing Tencent Marvis accounts as an OpenAI-compatible API, with a web panel. Supports multiple WeChat / QQ accounts, QR login, API keys, quota queries, token refresh, and streaming chat.

**Not an official Tencent product or endorsed by Tencent.** The protocol was observed from the desktop client and has no public specification. Upstream changes may break functionality or restrict accounts. Use only accounts and client files you are authorized to use, and comply with upstream terms and local law.

## Requirements

- macOS, Docker Desktop, and Docker Compose v2 or newer.
- Bash and Python 3 on the Mac; preparation uses only the Python standard library.
- Official Marvis for macOS installed and run to complete device registration.

Source, binaries, and images do not embed the upstream signing key or account credentials. **Only macOS initialization and deployment are currently supported; standalone Linux deployment is not supported.** Initial preparation needs local official client data. Once prepared, Docker runtime does not require the App.

## Quick start

### macOS + Docker

Install and run official Marvis first to register the device:

```bash
git clone https://github.com/qianjindexiaozu/marvis2api-panel.git
cd marvis2api-panel
bash scripts/prepare.sh
docker compose up -d --build
```

The script creates `.prepare/marvis.json`, mounted read-only in the container. The script does not print signing or device values, or read account tokens, Cookies, or the keychain. See the [deployment guide](docs/DEPLOYMENT.md) for sources, permissions, and updates.

Open <http://127.0.0.1:18620/panel/>. Default password: `marvis`.

1. Change the panel password immediately.
2. Add a WeChat / QQ account by scanning a QR code, or enter your own credentials manually.
3. Generate an API key. The panel password does not authorize `/v1` requests.

The published port binds to host `127.0.0.1` by default. Never expose a default-password panel publicly. Neither `.env` nor a host `config.json` is required.

## API usage

Query models using your API key, then choose a returned id. `marvis` is not a valid upstream model id:

```bash
curl http://127.0.0.1:18620/v1/models \
  -H 'Authorization: Bearer <API_KEY>'

curl -N http://127.0.0.1:18620/v1/chat/completions \
  -H 'Authorization: Bearer <API_KEY>' \
  -H 'Content-Type: application/json' \
  -d '{"model":"<MODEL_ID>","stream":true,"messages":[{"role":"user","content":"Hello"}]}'
```

Optional headers:

| Header | Purpose |
|---|---|
| `X-Marvis-Account` | Select an account id |
| `X-Marvis-Session` | Use a stable conversation id |

Without an account header, a new conversation goes to an enabled account with the most remaining quota and stays on it. Clients sending only their latest message should supply a stable session id; otherwise requests may be treated as new conversations.

`GET /healthz` reports business readiness. A `503` before adding an account is expected. Docker liveness uses the panel page instead.

## Configuration and data

| Data | Location |
|---|---|
| Upstream signing key and device id | Host `.prepare/marvis.json`, mounted at `/run/secrets/marvis.json` |
| Runtime configuration | `/app/data/config.json` in the volume, initially copied from `config.example.json` |
| Accounts, panel password, API keys, state, usage | Compose project volume `marvis2api-data` |

The prepare file and data volume are separate private data stores. Do not commit or publish either. The legacy `api_key` config field no longer authorizes the gateway and is removed on startup.

Optionally copy `.env.example` to `.env`:

| Variable | Default / purpose |
|---|---|
| `MV2A_BIND` | `127.0.0.1`, host bind address |
| `MV2A_PORT` | `18620`, host port |
| `MV2A_PREPARE_FILE` | `./.prepare/marvis.json`, host prepare file |
| `MV2A_IMAGE` | `marvis2api-panel:local`, or a published GHCR image |

Do not use `chmod 777` on credential files or data directories. See the [deployment guide](docs/DEPLOYMENT.md) for permissions, remote access, backups, prebuilt images, and troubleshooting.

## Supported modes

- Docker Desktop on macOS: internally uses Linux `amd64` / `arm64` images and upstream HTTP, not official kernels. This does not imply standalone initialization support on a Linux host.
- Native binaries: macOS `amd64` / `arm64`. Source requires Go 1.22+; use the current stable toolchain when possible.
- Official macOS kernel: optional experimental mode requiring official Host / Agent components and Xcode Command Line Tools. Official components are macOS arm64 and cannot run in Linux containers.
- No native Linux / Windows binaries or standalone initialization flows yet.
- WeChat display names are not guaranteed; QQ login sometimes provides one.

## Development and releases

```bash
go test -race ./...
go vet ./...
```

The frontend embeds `internal/panel/index.html` and `app.js`. No separate frontend build or external Go module dependency is required.

- [Contributing](CONTRIBUTING.md): layout, tests, and builds.
- [Security](SECURITY.md): private data and vulnerability reporting.
- [Publishing checklist (中文)](docs/PUBLISHING.md): GitHub, GHCR, and versioned releases.

## License

[MIT](LICENSE), covering repository code only. It does not grant rights to third-party clients, accounts, services, or signing material.
