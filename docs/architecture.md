# Architecture

Kestrel is split into gateway, agent, provider, tools, persistence, and
deployment layers. The agent package has no Telegram dependency, so another
frontend can implement the `agent.Chat` and `agent.Manager` boundaries.

## Data Flow

```text
Telegram Bot API
       |
       v
  Telegram gateway -- allowlist/commands/attachments/webhook or polling
       |
       v
  bounded update workers -- one queue shard per worker
       |
       v
  agent.Manager -- one actor goroutine per active chat
       |
       v
  agent.Engine -- context, prompt, provider chain, tool loop
       |                 |
       |                 +--> bounded parallel tool calls
       |                              |
       |                              +--> shell/file/web/memory/schedule tools
       |
       +--> provider.Client -- OpenAI-compatible JSON or incremental SSE
       |
       +--> Telegram Chat -- coalesced edits and outbound rate limiters
       |
       +--> store.Store -- bounded write channel -> one SQLite writer
                                      |
                                      +--> read-only SQLite pool + FTS5
```

## Goroutine Ownership

### Gateway

Long polling or webhook serving is tied to the gateway context. Telegram
updates are dispatched to bounded queues using the chat ID as a stable shard
key. Each worker has a request timeout and a panic boundary. The gateway owns
per-chat and global outbound rate limiters. Streaming updates are accumulated
in a per-chat flusher and sent as one message edit at a configurable interval.

Unauthorized messages are rejected before any response or callback answer is
sent. Approval callbacks are checked against both the allowlist and the chat
that created the pending request.

### Session Actors

`agent.Manager` creates at most one long-lived `sessionActor` for each active
chat. The actor owns its history, active session ID, working directory, queue,
run cancellation function, and reset state. Incoming messages are serialized
in that actor; separate chats run independently. Queue capacity is bounded and
the frontend receives a busy response when it is full.

The engine receives a copy of history. Tool calls from one provider response
run concurrently behind a per-run semaphore, and results are assembled in
original provider order before the next model request.

`/stop` cancels the run context. `/new` cancels the old run, waits for its
result and persistence, then creates the new active SQLite session. This keeps
old assistant/tool messages out of the new conversation and prevents late
stream edits from the previous run.

### Providers

Provider clients use a shared proxy-aware `http.Transport` for each proxy
specification. Non-streaming JSON and incremental SSE responses share the same
request/retry path. Retries are limited to retryable HTTP/network failures and
are disabled after visible streamed output. Prompt-based tool calls are parsed
when a provider does not advertise native function calling.

### Tools and Processes

Every tool receives the run context. Shell commands run through `bash -lc`
with bounded head/tail output, a timeout, proxy environment propagation, and a
process group. Cancellation kills the group. Background processes are owned by
one shared `ProcessManager`, bounded by a maximum count, and killed during
shutdown.

File tools resolve paths against configured roots. The default dangerous-command
policy requires an approval request; `off` and `allowlist` are explicit
operator choices.

### SQLite

Writes enter a bounded channel and are batched by a dedicated writer goroutine
inside transactions. Savepoints isolate individual write operations in a batch
without allowing one failed operation to discard unrelated writes. The read
pool uses read-only connections. WAL mode, `synchronous=NORMAL`, foreign keys,
and FTS5 triggers are enabled during schema setup.

Shutdown closes the input channel, waits for the writer to drain, checkpoints
the WAL, and closes both pools. A timed-out caller can retry `Close`; the
underlying close operation remains supervised and idempotent.

## Lifecycle

```text
start
  -> load/validate config
  -> create runtime directories
  -> open SQLite and schema
  -> configure shared HTTP transports/providers/tools
  -> attach gateway + session manager
  -> serve polling/webhook and health endpoint

SIGTERM/SIGINT
  -> cancel root context
  -> stop accepting Telegram work
  -> cancel actors and shell process groups
  -> wait for actor persistence
  -> close tools and transports
  -> drain/checkpoint SQLite
  -> exit
```

## Resource Boundaries

All externally fed queues are bounded: Telegram update queues, session inboxes,
background processes, provider retries, response bodies, attachment sizes,
tool output, and database write batches. Context deadlines are applied at the
gateway request, model attempt, tool, transcription, and shutdown boundaries.

The container uses a read-only root filesystem with writable `/data`,
`/workspace`, and a bounded `/tmp`. The process runs as `agent` under `tini`.
