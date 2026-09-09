# Hypercode

A minimal single-user web UI for coding agent harnesses, run locally or on a persistent VM/VPS accessed through Tailscale. One Go binary, embedded frontend, SQLite for UI history. The installed harness executables are the integration boundary.

Planned harnesses: **Codex**, **Claude Code**, **OpenCode**, **Pi**.

Not a fork of T3 Code. Independent repository, Go module, and database. T3 Code's adapters are a reference for protocol behavior, not code to port.

## Single-user scope

The first release supports **Codex only**, behind an adapter contract that can accept the other harnesses later. Hypercode is for one person's private use on their own host. Design and review changes around that person's coding workflow, including long conversations, several active chats, large pasted messages, accidental repeated submissions, browser reconnection, and process restarts.

Prefer small fixes with regression tests for those situations. Do not add multi-tenant infrastructure, distributed coordination, or elaborate compatibility layers for hypothetical production workloads. A new native thread does not recover a missing thread's context; starting fresh must remain explicit. Shutdown must finish even if a subprocess retains a pipe, while process termination remains limited to the process handle captured at spawn.

## Stack

| Layer       | Choice                                                         |
| ----------- | -------------------------------------------------------------- |
| Backend     | Go standard library: `net/http`, `os/exec`, `encoding/json`    |
| HTML        | Go `html/template`, embedded with `go:embed`                   |
| Interaction | HTMX 4 (pinned, embedded) plus one small vanilla JS file       |
| Streaming   | Server-Sent Events from Go to browser; HTTP POST for actions   |
| Storage     | SQLite via `database/sql` and `modernc.org/sqlite` (CGO-free)  |
| Styling     | Plain CSS                                                      |
| Markdown    | Goldmark, raw HTML disabled                                    |

Result: one application binary with embedded assets, plus whichever harnesses the user installs.

## UI shape (v1)

```
┌──────────────────┬──────────────────────────────────────┐
│ Codex            │ my-project                           │
│ Claude           ├──────────────────────────────────────┤
│ OpenCode         │                                      │
│ Pi               │ Conversation                         │
│                  │                                      │
│ + New chat       │ Tool activity, expandable            │
│                  │                                      │
│ Recent chats     │ Approval / question when needed      │
│ • Fix tests      ├──────────────────────────────────────┤
│ • Review code    │ Message…                    Send/Stop│
└──────────────────┴──────────────────────────────────────┘
```

- Sidebar selects a harness. Selecting a harness shows its chats.
- New chat asks for a working directory and a permission mode. No model picker in v1; harness default is used.
- Each chat belongs to exactly one harness.
- One active turn per chat.
- Pending approvals and questions render inline in the conversation and survive refresh.

### HTMX / JS split

- HTMX handles navigation, new chat, message submit, approval and question forms.
- One browser `EventSource` per page. SSE payloads are HTML fragments swapped with `hx-swap-oob` wherever possible (sidebar status, tool activity rows, completed messages, approval prompts).
- Vanilla JS handles only the in-progress assistant message: append text deltas batched every 50–100 ms, manage scroll. Completed messages are re-rendered server-side as Markdown and swapped in.
- Never replace the whole conversation during streaming. Update the active message element only.

## Adapters

Each adapter exposes session creation, resumption, prompts, interruption, and normalized events. Transport, permissions, and user interaction remain specific to each harness. Codex, Claude, and Pi use stdio JSON; OpenCode uses HTTP and SSE.

### Contract

```go
type Adapter interface {
    // Open starts a fresh native session in dir with the given permission mode.
    Open(ctx context.Context, dir string, mode PermissionMode) (Session, error)
    // Resume reattaches to an existing native session by its stored reference.
    // The harness process may be gone; this must work cold after an app restart.
    Resume(ctx context.Context, dir string, ref string, mode PermissionMode) (Session, error)
}

type Session interface {
    Ref() string                                  // native session reference to persist
    Send(ctx context.Context, text string) error  // start a turn
    Stop(ctx context.Context) error               // interrupt the active turn
    Respond(ctx context.Context, requestID string, answer Answer) error // approval / question reply
    Events() <-chan Event                         // normalized stream
    Close() error
}
```

Normalized `Event` kinds: `text_delta`, `text_done`, `tool_started`, `tool_updated`, `tool_done`, `approval_requested`, `question_asked`, `status`, `turn_done`, `error`. Provider-specific detail stays inside the adapter as an opaque payload. Expose only controls the harness actually supports.

`Resume` is a distinct verb, not a flag on `Open`. Every adapter needs a cold-reattach path.

### Per-harness

| Harness  | Connection                                           | Notes                                                                                                                                                                                                                                                                                           |
| -------- | ---------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Codex    | `codex app-server`, JSON-RPC over stdio              | `initialize`, `thread/start` / `thread/resume`, `turn/start`, `turn/interrupt`. Events arrive as `thread/*` and `turn/*` notifications. Approvals arrive as server-to-client JSON-RPC requests; reply with the matching response id.                                                          |
| Claude   | `claude --print` with stream-json in and out         | Flags: `--print --input-format stream-json --output-format stream-json --include-partial-messages --permission-prompts host --permission-mode <mode>`. Pass `--session-id <uuid>` on open so the ref is known up front; `--resume <uuid>` on resume. Permission prompts arrive on stdout as `control_request` with subtype `can_use_tool`; answer on stdin with `control_response`. Interrupt and `set_permission_mode` are also control requests. |
| OpenCode | `opencode serve`, HTTP + SSE                         | One server process per app instance, not per chat. `/event` SSE stream is server-wide; filter by session id. Create/resume session, send prompt, abort. `permission.asked` and `question.asked` events have reply endpoints.                                                                     |
| Pi       | `pi --mode rpc`, JSON over stdio                     | Prompt, session selection, abort, events, extension dialogs. Render the dialogs Pi actually emits; do not map them onto a universal permission model. Not verified against a reference implementation yet, so fixture it first.                                                                  |

Claude caveat: the `control_request` / `control_response` protocol is what the official Agent SDK uses under the hood, but only `--permission-prompt-tool` is formally documented. Pin the CLI version and record protocol fixtures. Same discipline as Codex app-server.

Pi's extension dialogs and Codex approvals differ. Normalize to `approval_requested` (yes/no/always) and `question_asked` (free-form or options), and let each adapter decide which it emits.

## Storage

Three tables. The harness owns conversation context; SQLite owns UI history and the reference needed to resume.

- `projects`: id, name, working directory.
- `sessions`: id, project_id, harness, native_ref, title, permission_mode, status (`idle`, `running`, `interrupted`, `error`), created_at, updated_at.
- `items`: id, session_id, ordinal, kind (`user`, `assistant`, `tool`, `approval`, `question`), status (`streaming`, `done`, `pending`, `answered`, `interrupted`), body (text or JSON payload), created_at.

Rules:

- Pending approvals and questions are `items` rows with status `pending`. The UI renders them from the database, so a refresh mid-approval still shows the prompt. Answering routes through the adapter and flips the row to `answered`.
- Persist partial assistant output periodically and at completion.
- On startup, mark any `running` session, `streaming` item, and `pending` approval or question as `interrupted`. Offer explicit resume in the UI.
- Pending prompts survive a browser refresh only while their owning harness session remains live. If that process dies, mark its pending prompts interrupted and reject replies to their stale request IDs. Resumption displays any new prompts emitted by the harness.

## Session manager

Go owns running processes independently of browser connections. Refreshing, switching chats, closing the browser, or losing the Tailscale connection never cancels work.

- One in-memory `SessionRuntime` per active chat, holding the adapter session and a ring buffer of recent events.
- Give SSE updates monotonic sequence numbers within a runtime generation. Subscribers receive a current snapshot and its sequence boundary, followed by ordered live events with no gap or duplicate application. Include buffered partial output in the snapshot so periodic database writes do not lose visible text on reconnect.
- Use the last event ID to replay missed updates from the ring buffer. If the buffer no longer covers that ID or the runtime generation changed, send a fresh snapshot. A slow or disconnected subscriber must not block the harness.
- Adapter processes are killed only by the PID captured at spawn.
- On app crash or restart: unfinished turns are marked interrupted; nothing auto-resumes.

## Deployment and access (v1)

- Hypercode is a single-user application. It can run on the user's computer or on a persistent VM/VPS. Remote access is exclusively through Tailscale, which provides network authentication and encrypted access. No application accounts, login screen, or multi-user authorization system.
- Default to a loopback listener. For VM/VPS deployment, allow binding to the host's Tailscale address. Do not expose the application on a public interface.
- Run Hypercode as a system service on the VM/VPS, under the same OS user that owns the projects and harness logins. Start the service at boot with an explicit data directory and executable paths.
- Keep project directories, SQLite, and harness home directories on persistent storage. The browser is a client; all agent execution, credentials, and project files remain on the host.
- A browser or network disconnect leaves work running. A service or VM restart preserves files and history but interrupts active work. Restarting the service does not automatically resume agent turns.
- Serve assets, HTTP actions, and SSE from the same origin. Use relative browser URLs so both localhost and the Tailscale address work.

## Security (v1)

- Trust access granted through Tailscale for this single user. Public hosting and sharing access with other users are outside v1.
- Keep the browser's same-origin boundary for mutation endpoints: check `Origin` / `Sec-Fetch-Site` on every POST. This prevents another website from issuing actions through the user's browser; it does not add an application login.
- Login and harness configuration stay in the harness CLIs. Surface installation and auth errors in the app.

## Out of scope for v1

Model selection per chat, git tooling, diff view, terminal emulation, native session import, public or non-Tailscale remote access, multi-user support, multiple concurrent turns per chat.

## Order of work

1. **Spikes first.** A ~100-line Go CLI per harness that opens a session, sends a prompt, streams output, triggers and answers one permission prompt, interrupts, and resumes. Codex and Claude first; both are stdio JSON and cheap to prove. Then OpenCode, then Pi. Record the raw protocol traffic from each as test fixtures.
2. **Full UI flow with Codex and SQLite.** Sidebar, new chat, send, stream, stop, approvals, refresh survival, cold resume.
3. **Claude** through the same contract.
4. **Pi and OpenCode** through the same contract.
5. **Hardening pass:** refresh during a turn, switching chats mid-turn, browser and Tailscale disconnection, SSE replay and snapshot fallback, harness process failure, service restart and cold resume, approval replies after refresh, rejection of stale replies after process loss.

## Testing

- Adapter tests replay recorded protocol fixtures against a fake process; no live harness in unit tests.
- Playwright for the integrated browser flows in step 5.
- Verify VM/VPS deployment through Tailscale, including browser reconnection while a turn continues and explicit session resumption after a service restart.
- A test that needs a sleep or timeout to pass is wrong. Wait on events.
