# Contributing

Repository: <https://github.com/qianjindexiaozu/marvis2api-panel>

Use Go 1.22+ (prefer the current stable release), Bash, and Python 3. The frontend is embedded, and the project currently has no external Go module dependencies.

## Layout

| Path | Responsibility |
|---|---|
| `cmd/server/` | Configuration and service startup |
| `internal/prepare/`, `internal/ual/` | Private runtime data validation and upstream signing |
| `internal/upstream/`, `internal/oauth/` | Upstream HTTP and QR login |
| `internal/gateway/`, `internal/scheduler/` | OpenAI-compatible routes, account selection, background jobs |
| `internal/account/`, `internal/state/`, `internal/apikey/`, `internal/usage/` | Persistent accounts, state, keys, usage |
| `internal/panel/` | Embedded web panel |
| `internal/kernel/` | Optional macOS official-kernel integration |
| `scripts/`, `docker/` | Host preparation and container startup |

## Checks

```bash
gofmt -w cmd internal
go vet ./...
go test -race ./...
for script in scripts/*.sh; do bash -n "$script"; done
sh -n docker/entrypoint.sh
```

Default tests use synthetic credentials and local mock services. They do not need a real prepare file, account, or App. The optional QQ network test is opt-in:

```bash
MV2A_LIVE_TESTS=1 go test ./internal/oauth -run TestQQStartReturnsWaitingQR -count=1
```

That test contacts the real QQ login service and may fail or skip depending on upstream availability. It does not prove successful account login or chat. Do not enable it in ordinary CI.

## Builds

```bash
go build -o marvis2api ./cmd/server
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -o /tmp/marvis2api-darwin-amd64 ./cmd/server
docker build -t marvis2api-panel:local .
```

Release builds inject the version using `-ldflags="-X main.appVersion=<version>"`; local builds report `dev`. Native releases target macOS `amd64` and `arm64` only. Linux / Windows native deployment and standalone initialization are not currently supported. Docker Desktop on macOS uses Linux `amd64` and `arm64` images internally; these images do not remove the macOS client requirement for initial preparation.

## Pull requests

- Keep changes focused and add regression tests for changed behavior.
- Keep Chinese and English READMEs consistent; put detailed setup instructions in `docs/DEPLOYMENT.md`.
- Never add real keys, tokens, device ids, client files, account exports, or screenshots containing private data. Do not use real credentials as test fixtures.
- Verify `.gitignore` before committing. Custom prepare outputs should live outside the repository.
- Report security-sensitive problems privately as described in [SECURITY.md](SECURITY.md), not in a public issue.
- Respect third-party licenses and upstream service terms. Repository licensing does not authorize redistribution of official client materials.

CI checks formatting, shell syntax, vet, race tests, secret scanning, native cross-builds, and Docker builds. Publishing behavior is described in [docs/PUBLISHING.md](docs/PUBLISHING.md).
