# Kestrel

Kestrel is a small, self-hosted autonomous agent written in Go. Telegram is
the primary interface, but the agent loop is independent of Telegram and is
also available through the local terminal REPL. It uses OpenAI-compatible
providers, pure-Go SQLite, bounded concurrency, and a single static binary.

## Quick Start

### Docker Compose

1. Create a bot with [@BotFather](https://t.me/BotFather) and obtain the
   numeric Telegram user ID that will be allowed to use it.
2. Provide the required environment variables and run the compose example:

```sh
export AGENT_TELEGRAM_TOKEN='123456:replace-me'
export AGENT_TELEGRAM_ALLOWED_USERS='123456789'
export AGENT_PROVIDER_BASE_URL='https://api.openai.com/v1'
export AGENT_PROVIDER_API_KEY='replace-me'
export AGENT_PROVIDER_MODEL='gpt-4o-mini'
docker compose -f deploy/docker-compose.yml up --build -d
```

The image stores the database under `/data` and agent files under
`/workspace`. It runs as the unprivileged `agent` user, uses `tini` as PID 1,
and includes the command-line tools listed below.

Telegram can reveal your numeric user ID through an ID lookup bot, or through
the Bot API: send your new bot a message, then call
`https://api.telegram.org/bot<TOKEN>/getUpdates` and read `message.from.id`.
Do not share the token or leave the lookup response in a public log.

For a safer deployment, keep the root filesystem read-only, retain the
capability drop and `no-new-privileges` settings, and adjust the memory/CPU
limits to the host. The compose example intentionally exposes no public port;
long polling works behind NAT.

### Bare Binary

```sh
make dist
mkdir -p data workspace
./dist/agent-linux-amd64 init \
  -telegram-token "$AGENT_TELEGRAM_TOKEN" \
  -allowed-users "$AGENT_TELEGRAM_ALLOWED_USERS" \
  -base-url 'https://api.openai.com/v1' \
  -api-key "$AGENT_PROVIDER_API_KEY" \
  -model 'gpt-4o-mini'
./dist/agent-linux-amd64 -config ./data/config.yaml gateway
```

`agent init` also runs interactively when the required flags are omitted. Use
`agent chat` for a local REPL with the same provider, tool, store, and agent
core. `agent config show` masks credentials before printing them.

## Configuration

The default configuration is `./data/config.yaml`. Set `AGENT_CONFIG` or pass
`-config FILE` to select another file. The container defaults are `/data` and
`/workspace`.

See [`deploy/config.example.yaml`](deploy/config.example.yaml) for a complete
example with a cloud provider, a local Ollama provider, provider-specific
proxies, Telegram proxying, and an allowlist. Provider API keys can be literal
values or `${ENV_VAR}` references. Environment variables override YAML values;
important overrides include:

```text
AGENT_DATA_DIR, AGENT_WORKSPACE, AGENT_PROXY
AGENT_TELEGRAM_TOKEN, AGENT_TELEGRAM_ALLOWED_USERS, AGENT_TELEGRAM_PROXY
AGENT_TELEGRAM_LOG_ACTIONS, AGENT_TELEGRAM_SHOW_REASONING
AGENT_TELEGRAM_DRAFT_STREAMING, AGENT_TELEGRAM_PINNED_STATUS
AGENT_SKILLS_GITHUB_TOKEN
AGENT_PROVIDER_BASE_URL, AGENT_PROVIDER_API_KEY, AGENT_PROVIDER_MODEL
AGENT_PROVIDER_<NAME>_BASE_URL, AGENT_PROVIDER_<NAME>_API_KEY
AGENT_PROVIDER_<NAME>_MODEL, AGENT_PROVIDER_<NAME>_PROXY
```

A minimal file-based configuration is:

```yaml
telegram:
  token: ${AGENT_TELEGRAM_TOKEN}
  allowed_users: [123456789]
providers:
  - name: cloud
    base_url: https://api.openai.com/v1
    api_key: ${AGENT_PROVIDER_API_KEY}
    default_model: gpt-4o-mini
```

To add another OpenAI-compatible endpoint, add a provider entry with its
`name`, `base_url`, `api_key`, and `default_model`, then restart the gateway.
Use `/model name:model` in Telegram to select it. A local Ollama deployment
typically uses `http://localhost:11434/v1` on the host (or a reachable service
name inside Compose).

The active selection is `provider:model`. Telegram `/model provider:model`
changes it at runtime and persists the selection in SQLite.

## Proxies

Kestrel supports HTTP, HTTPS, SOCKS5, and SOCKS5H URLs, including URL
credentials. Resolution is most-specific-first:

```text
provider proxy -> component proxy -> global proxy -> HTTP_PROXY/HTTPS_PROXY/ALL_PROXY
```

Use `none` to disable proxying at an explicit level. `NO_PROXY` is respected.
The Telegram client, provider requests, web tools, audio transcription, and
child processes use the resolved settings. Credentials are redacted from
structured logs and user-facing errors.

## Telegram

The allowlist is mandatory for gateway operation. Unauthorized messages and
callbacks are ignored without a Telegram response. Pairing is optional: set
`telegram.pairing_code` and an unauthorized user may send `/pair CODE`; a
successful pairing is persisted in SQLite.

Available commands include `/help`, `/new`, `/stop`, `/status`, `/skills`,
`/model`, `/models`, `/home`, `/cd`, `/whoami`, `/pair`, and `/version`.
Polling is the default. Webhooks can be enabled with `telegram.mode: webhook`,
`telegram.webhook_url`, `telegram.webhook_addr`, and a persistent
`telegram.webhook_secret`.

Responses stream with animated Bot API drafts in private chats and fall back to
coalesced, rate-limited message edits in groups or when drafts are unavailable.
The final answer is always posted as a normal message and split at the Bot API
message limit. A pinned per-chat status board shows the current phase, elapsed
time, model, active tool, and task plan; completed tasks are checked off. It
settles on Done, Error, or Stopped, and the stop button on a streaming draft
ends the run. Disable drafts with `telegram.draft_streaming: false` or
`AGENT_TELEGRAM_DRAFT_STREAMING=false`; disable the board with
`telegram.pinned_status: false` or `AGENT_TELEGRAM_PINNED_STATUS=false`.

Documents, photos, and optional voice transcription are supported. Dangerous
shell commands require inline approval by default and timeout to denial.

Every tool call is posted as its own `⚙️ name: detail` message before it runs,
so the chat keeps a complete action log. Commands, file paths, URLs, and search
queries are shown; file and memory contents are never included. Disable the
log with `telegram.log_actions: false` or `AGENT_TELEGRAM_LOG_ACTIONS=false`.

The model's reasoning is posted as a `💭 Thinking` block before each answer and
before tool execution, and its intermediate narration is kept as its own
message instead of being overwritten by the final answer. Provider failures,
tool errors, panics, and timeouts are posted as `❌` messages regardless of the
action-log setting. Disable reasoning blocks with
`telegram.show_reasoning: false` or `AGENT_TELEGRAM_SHOW_REASONING=false`.

Runs have no iteration cap and no wall-clock limit by default
(`agent.max_iterations: 0`, `agent.run_timeout: 0`). A run ends when the model
returns a final answer, the provider fails or stops responding (per-request
and per-tool timeouts still apply), or you send `/stop`. Set a positive
`agent.max_iterations` or `agent.run_timeout` to restore a limit.

## Skills

Skills are markdown procedures stored under `skills.dir` (default
`<data_dir>/skills`). Installed skill names and descriptions are included in the
agent's system prompt, and the agent loads a matching one with the `skills`
tool before following it. `/skills` lists what is installed.

Install a skill by dropping a folder archive, a `SKILL.md`, or another markdown
skill into the chat and asking the agent to install it. The agent can also
search public marketplaces by name and install a repository directly. Supported
sources include a local directory, a single markdown file, `.zip`, `.tar.gz`,
`.tgz`, or `.tar` archives, `owner/repo` or GitHub URLs, GitHub tree/blob URLs,
`skills.sh` URLs, and direct archive URLs. Extraction rejects path traversal,
symlinks, oversized entries, and non-regular files. When a package contains
multiple skills the agent asks for a `skill=<name>` selector, and an existing
skill is only replaced with `force`.

Marketplace search uses GitHub's public API. Set `skills.github_token` or
`AGENT_SKILLS_GITHUB_TOKEN` to raise API rate limits; it is not required for
public repositories.

## Image Toolset

The full Alpine image installs Bash, GNU core utilities, `findutils`, `grep`,
`sed`, `gawk`, `diffutils`, `patch`, `util-linux`, `less`, `file`, `tree`,
`procps-ng`, `htop`, `tmux`, `curl`, `wget`, OpenSSH client, `rsync`, DNS and
network diagnostics, `openssl`, certificates, timezone data, archive tools,
`git`, `jq`, `yq`, `ripgrep`, `fd`, `sqlite`, `make`, `nano`, `vim`, Python 3
with pip, Node.js with npm, `tini`, and `doas`.

Alpine names the OpenSSH client package `openssh-client-default` and the Go
YAML query tool `yq-go`; those package names are used because they exist in
the main/community repositories for amd64, arm64, and riscv64. `FULL=0` omits
the optional `build-base` package. The default image uses `FULL=1`.

## Build, Test, and Release

```sh
make build                       # local static binary
make test                        # race tests; requires CGO and a C compiler
make bench                       # provider/parser and package benchmarks
make lint                        # go vet and golangci-lint when installed
make dist                        # linux/amd64, linux/arm64, linux/riscv64
docker buildx build --platform linux/amd64,linux/arm64,linux/riscv64 \
  -f deploy/Dockerfile -t your-registry/kestrel:latest --push .
```

The release build uses `CGO_ENABLED=0` and `-trimpath -ldflags='-s -w'`.
Verified local artifacts are statically linked ELF binaries:

```text
linux/amd64    14 MB
linux/arm64    13 MB
linux/riscv64  12 MB
```

`alpine:3.23` has official amd64, arm64, and riscv64 variants. The GitHub
Actions workflow is configured to run amd64 race tests, emulated arm64 and
riscv64 tests, cross-compilation, a multi-platform image build, and
per-architecture image smoke tests. These CI jobs have not yet been run.
Locally the static binaries and amd64 slim/full images build and the image
tools run as the non-root `agent` user. This Docker host has no QEMU/binfmt
registration, so cross-architecture image builds and execution stop at the
runtime `RUN` step with `exec format error`.
The local amd64 images measure about 98 MB (slim) and 194 MB (full) as
reported by `docker image inspect`; this is image storage size, not idle RSS.
`go test ./...`, `go vet ./...`, and containerized `go test -race ./...` pass.
The mock Bot API/provider test covers an environment-only polling reply and
silent rejection of an unauthorized user; it does not replace a live Telegram
and provider integration test.

Focused 100-iteration benchmark samples on a Windows amd64 Ryzen 7 5825U
(Go 1.27.1, local filesystem, `-benchtime=100x`) measured about 5.3 µs for an
SSE event, 1.1 µs for a prompt tool-call parse, 2.2 µs for tool-output
trimming, and 5.8 ms for a synchronous SQLite message append. These are
diagnostic samples, not throughput or ARM/RISC-V performance claims.

## Operations

The health endpoint defaults to `127.0.0.1:8081/healthz`. It checks the SQLite
read pool and returns `503` during shutdown. Optional pprof is restricted to a
loopback address. Logs use `slog` and can be emitted as JSON with
`log_format: json`.

`SIGINT` and `SIGTERM` stop polling, cancel active agent runs, terminate shell
process groups and background processes, wait for actor persistence, flush the
SQLite writer, and close HTTP transports. `shutdown_timeout` bounds the
coordinated wait; a misbehaving external operation can still exceed it.

## Architecture

See [`docs/architecture.md`](docs/architecture.md) for the component diagram,
goroutine ownership rules, persistence model, and data flow.

## Dependencies

Kestrel uses the standard library for HTTP, Telegram API calls, concurrency,
process execution, and logging. The non-standard dependencies are:

| Dependency | Purpose | Portability reason |
| --- | --- | --- |
| `modernc.org/sqlite` | Pure-Go SQLite with FTS5 | No CGO or target-side C toolchain |
| `gopkg.in/yaml.v3` | YAML configuration | Small, pure Go |
| `golang.org/x/net` | HTTP proxy matching and HTML parsing | Pure Go, standard networking extensions |
| `golang.org/x/sys` | Portable OS support used by dependencies | Generic fallbacks available |
