# Codex protocol notes

Verified locally on 2026-09-09 with `codex-cli 0.153.4`.

The adapter launches `codex app-server --listen stdio://`. It sends `initialize`, `initialized`, and either `thread/start` or `thread/resume`. Thread creation explicitly sets the working directory, sandbox, approval policy, and the user as approval reviewer. Each chat owns one captured process handle.

`turn/start` sends text input and leaves model selection to Codex. `turn/started` supplies the active turn ID, which `turn/interrupt` uses. Thread and turn IDs are separate. The reader handles server requests while client RPC calls are waiting for responses.

Command and file approvals preserve the server's JSON-RPC ID, including whether it was numeric or a string. Supported simple decisions are `accept`, `acceptForSession`, `decline`, and `cancel`. When Codex provides `availableDecisions`, the UI limits itself to that list. Policy amendment objects are not exposed. The live command-approval capture offered `accept`, an exec-policy amendment, and `cancel`; this differs from the generated type's broader decision union.

`item/tool/requestUserInput` maps question IDs to answer arrays. Unknown client requests receive JSON-RPC method-not-found errors and a visible session notice. New protocol capabilities therefore require deliberate adapter support.

## Fixtures

`internal/adapter/codex/testdata` contains:

- `recorded-live.jsonl`: real start, streamed text, item completion, and turn completion.
- `recorded-approval.jsonl`: real tool activity, approval request, denied execution, and completion.
- `recorded-interrupt.jsonl`: real interruption after the first text delta.
- `notifications.jsonl`: a small synthetic fixture that covers the normalized event kinds.

Recorded files retain selected received envelopes. Native IDs were replaced with stable fixture IDs and `/private/tmp` was normalized to `/tmp`. Authentication/initialization metadata and unrelated event types were excluded. Raw originals remain in ignored `output/` files locally.

Unit tests replay these recordings directly through the decoder. A separate fake process tests bidirectional RPC, approval replies, structured questions, missing executables, process loss, and cold reattachment without invoking a live model.

## Stream recovery

The manager owns live chat state independently of HTTP requests. Partial text flushes to SQLite every 250 ms and on non-delta events. Completed messages are rendered as Markdown on the server.

A process-wide runtime generation and monotonically increasing sequence identify SSE boundaries. The bounded replay buffer retains 256 updates. Subscription, snapshot capture, and publication use the same mutex. Slow browsers receive coalesced wakeups and cannot block an agent. A generation mismatch or expired sequence causes a fresh snapshot; otherwise the client receives missed item updates. Partial text updates carry the accumulated text, making replay idempotent.

Ordinary live events update individual items, controls, and the sidebar. The browser replaces the conversation only on initial connection or snapshot recovery. Assistant text updates are batched every 75 ms and use `textContent`.

The first pass retains loaded UI history in memory and flushes changed chats transactionally. It does not yet paginate very long histories.
