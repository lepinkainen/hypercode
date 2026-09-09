# Storage and session manager

What SQLite owns, what the harness owns, and how Go keeps work alive independent of the browser.

## Storage

Three tables. The harness owns conversation context; SQLite owns UI history and the reference needed to resume.

- `projects`: id, name, working directory.
- `sessions`: id, project_id, harness, native_ref, title, permission_mode, status (`idle`, `running`, `interrupted`, `error`), created_at, updated_at.
- `items`: id, session_id, ordinal, kind (`user`, `assistant`, `tool`, `approval`, `question`), status (`streaming`, `done`, `pending`, `answered`, `interrupted`, `rejected`, `error`), body (text or JSON payload), created_at.

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
