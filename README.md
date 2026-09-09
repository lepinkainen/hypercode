# Hypercode

A private web workspace for the Codex CLI. One Go binary embeds the UI and stores conversation history in SQLite. Codex runs on the host, so closing the browser does not stop a turn.

This first version supports **Codex only**. The adapter contract and agent registry allow other agents to be added without changing storage or the session manager.

## Run

Install Go 1.26 or newer, [Task](https://taskfile.dev/), and Codex. Authenticate with `codex login` on the host, then:

```sh
task build
./bin/hypercode
```

Open <http://127.0.0.1:8090>. Create a chat with an absolute project directory and a permission mode. The model comes from your Codex configuration.

```sh
./bin/hypercode \
  -addr 127.0.0.1:8090 \
  -data-dir /absolute/path/to/hypercode-data \
  -codex /absolute/path/to/codex
```

The default data directory is `hypercode` under the OS user configuration directory. Run under the same OS user as your projects and Codex login. No OpenAI API key is required by Hypercode itself.

## Included

- Recent chats, project directories, and per-chat permissions.
- Streamed assistant text, safe Markdown rendering, and expandable tool activity.
- Inline command/file approvals and structured Codex questions.
- Stop and explicit resume. One active turn per chat.
- SQLite history, partial output persistence, browser refresh survival, and SSE reconnection.
- Recovery after Codex exits or Hypercode restarts. Stale prompts become interrupted and reject replies.
- Same-origin checks for POST actions. The listener accepts loopback or Tailscale address ranges.

Workspace write permits changes inside the project and asks Codex to request approval for escalation. Read only starts a read-only sandbox. Full access disables sandboxing and approval prompts. Only decisions supported by the current approval request are offered; permanent policy amendments are not implemented.

A restart interrupts active work. Use **Resume chat** to reattach to the stored Codex thread, then send the next message. Resume never automatically repeats the previous prompt. An empty chat that has never sent a message opens a new native thread because Codex may not have persisted the original yet.

## Development

```sh
task dev
task test       # Go race tests and JavaScript lifecycle tests (requires Node.js)
task lint       # Go formatting check and go vet
task build
```

No frontend bundler or network-served assets are required. HTMX 4.0.0 is pinned in `internal/web/assets` with its license. Goldmark disables raw HTML and unsafe links.

For browser work without model calls:

```sh
task dev-fixture -- -addr 127.0.0.1:8091
```

This uses separate history in `.cache/fixture` and an offline protocol peer. Messages containing `[approval]`, `[question]`, `[wait]`, `[reject]`, or `[crash]` exercise the corresponding flow. Other messages return a Markdown example. The peer never executes commands.

With Playwright CLI open on that fixture server, run `playwright-cli run-code --filename internal/web/browser-regressions.js` to check duplicate submissions, rejection recovery, large messages, and reconnecting after cached-page lifecycle events. The lifecycle unit test also runs with `task test`.

## Protocol checks

The adapter was verified with **codex-cli 0.153.4**, using the installed CLI's generated protocol types and the [official app-server documentation](https://learn.chatgpt.com/docs/app-server). The app-server protocol can change across CLI releases.

```sh
go run ./cmd/codex-spike -dir /tmp
# Save the printed thread ID, then cold-resume it:
go run ./cmd/codex-spike -dir /tmp -resume THREAD_ID
# Record a dedicated diagnostic session:
go run ./cmd/codex-spike -dir /tmp -record /tmp/codex-traffic.jsonl
```

The spike also accepts `-prompt`, `-mode`, `-approve`, and `-interrupt`. Raw traffic can include local paths and conversation content; recordings are opt-in. The checked-in fixtures contain only selected, redacted protocol traffic from dedicated tests. See [protocol notes](docs/protocol.md).

## Layout

| Directory | Responsibility |
| --- | --- |
| `cmd/hypercode` | Listener, configuration, shutdown |
| `cmd/codex-spike` | Standalone native protocol verification |
| `internal/adapter` | Common session/event contract |
| `internal/adapter/codex` | Process ownership and Codex stdio JSON-RPC |
| `internal/session` | Turn lifecycle, live state, event replay |
| `internal/store` | SQLite UI history |
| `internal/web` | HTTP actions, SSE, embedded templates/assets |
| `internal/testcodex` | Offline protocol peer for tests |

To add another agent, implement `adapter.Adapter`, register it in the entry point, and add a UI selection. Native wire formats stay inside that adapter. The database already records which agent owns each chat.

Remote access and service setup are described in [deployment](docs/deployment.md). Scope lives in `PROJECT.md`; design notes (architecture, adapter contract, storage, roadmap) live in `ai-docs/`. The first pass does not include a model picker, terminal, diff viewer, native session import, or public hosting.
