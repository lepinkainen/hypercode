# Protocol notes

## Codex

Verified locally on 2026-09-09 with `codex-cli 0.153.4`.

The adapter launches `codex app-server --listen stdio://`. It sends `initialize`, `initialized`, and either `thread/start` or `thread/resume`. Thread creation explicitly sets the working directory, sandbox, approval policy, and the user as approval reviewer. Each chat owns one captured process handle.

`model/list` (after `initialize`) returns the account's catalog with `id`, `displayName`, `description`, `hidden`, and `isDefault`; hidden entries are dropped. A chosen model is passed as `model` in the `thread/start` and `thread/resume` params. `turn/start` sends text input. `turn/started` supplies the active turn ID, which `turn/interrupt` uses. Thread and turn IDs are separate. The reader handles server requests while client RPC calls are waiting for responses.

Command and file approvals preserve the server's JSON-RPC ID, including whether it was numeric or a string. Supported simple decisions are `accept`, `acceptForSession`, `decline`, and `cancel`. When Codex provides `availableDecisions`, the UI limits itself to that list. Policy amendment objects are not exposed. The live command-approval capture offered `accept`, an exec-policy amendment, and `cancel`; this differs from the generated type's broader decision union.

`item/tool/requestUserInput` maps question IDs to answer arrays. Unknown client requests receive JSON-RPC method-not-found errors and a visible session notice. New protocol capabilities therefore require deliberate adapter support.

### Fixtures

`internal/adapter/codex/testdata` contains:

- `recorded-live.jsonl`: real start, streamed text, item completion, and turn completion.
- `recorded-approval.jsonl`: real tool activity, approval request, denied execution, and completion.
- `recorded-interrupt.jsonl`: real interruption after the first text delta.
- `notifications.jsonl`: a small synthetic fixture that covers the normalized event kinds.

Recorded files retain selected received envelopes. Native IDs were replaced with stable fixture IDs and `/private/tmp` was normalized to `/tmp`. Authentication/initialization metadata and unrelated event types were excluded. Raw originals remain in ignored `output/` files locally.

Unit tests replay these recordings directly through the decoder. A separate fake process tests bidirectional RPC, approval replies, structured questions, missing executables, process loss, and cold reattachment without invoking a live model.

## Claude Code

Verified locally on 2026-09-09 with Claude Code `2.1.263`.

The adapter launches `claude --print --verbose --output-format stream-json --input-format stream-json --include-partial-messages --permission-prompt-tool stdio --permission-mode <mode>`, plus `--session-id <uuid>` for a new chat or `--resume <uuid>` for a stored one. The adapter generates the UUID itself, so the native reference is known before the process starts and survives a cold resume unchanged. The working directory is the process directory. `CLAUDECODE` is removed from the environment so Hypercode can run from inside a Claude Code session; `CLAUDE_CODE_ENTRYPOINT=sdk-go` is set. Each chat owns one captured process handle.

Permission modes map as `read-only` → `default`, `workspace-write` → `acceptEdits`, `full-access` → `bypassPermissions` with `--allow-dangerously-skip-permissions`. Claude has no read-only sandbox; `default` prompts before every write and command that the user's own settings do not already allow. Allow rules from `~/.claude/settings.json` and project settings apply as usual, so an allow-listed command runs without a prompt.

After spawning, the adapter sends a `control_request` with subtype `initialize` and waits for the `control_response`. That response carries the account's `models` catalog (`value`, `displayName`, `description`); the `default` entry maps to the empty model id, any other `value` is passed as `--model` on open and resume. A failed `--resume` ("No conversation found with session ID") exits the process before that reply, which turns into an open error instead of a dead chat.

Prompts are `{"type":"user","message":{"role":"user","content":[{"type":"text","text":…}]}}` lines on stdin. The CLI never acknowledges a prompt; the `result` frame ends the turn. Text arrives as `stream_event` frames wrapping Messages API stream events: `text_delta` becomes `text_delta`, `content_block_stop` becomes `text_done` with the accumulated block. The item id is `<message id>-<block index>`. Tool calls become `tool_started` from the `tool_use` block of the `assistant` frame (complete input) and `tool_done` from the matching `tool_result` block of a `user` frame; `is_error` marks the item failed. Frames with a non-null `parent_tool_use_id` belong to subagents and are dropped. Thinking deltas are not rendered.

Permission prompts arrive as `{"type":"control_request","request_id":…,"request":{"subtype":"can_use_tool","tool_name":…,"input":…,"permission_suggestions":…}}`. The reply is `{"type":"control_response","response":{"subtype":"success","request_id":…,"response":{…}}}` with `{"behavior":"allow","updatedInput":<input>}`, or `{"behavior":"deny","message":…}`. The UI decisions reuse the Codex vocabulary: `accept` allows once, `acceptForSession` allows and adds the CLI's suggested rules rescoped to `destination: "session"` (a whole-tool session rule when no suggestion is offered), `decline` denies, `cancel` denies with `interrupt: true`. Nothing is written to settings files.

`AskUserQuestion` is only available to the model when a stdio permission host is declared; it arrives as a `can_use_tool` request for that tool and is normalized to `question_asked`. The CLI validates that every question has 2 to 4 options. The reply is `{"behavior":"allow","updatedInput":{"questions":<original>,"answers":{"<question text>":"<answer>"}}}`; answers are looked up by question text, so the question text is also the question id. Free-text answers are accepted as the answer string.

`control_cancel_request` withdraws a pending prompt (for example after an interrupt) and expires it in the UI. Unknown control request subtypes receive an error `control_response` and a visible notice.

Stop sends a `control_request` with subtype `interrupt` and waits for its response. The interrupted turn's `result` carries `terminal_reason: "aborted_streaming"` or `"aborted_tools"` (with `is_error: true` and an `[ede_diagnostic]` entry in `errors`), which the adapter maps to an interrupted turn. Other `result` frames with `is_error` or an `error_*` subtype fail the turn with the first non-diagnostic error string.

### Fixtures

`internal/adapter/claude/testdata` contains:

- `recorded-live.jsonl`: real init, streamed text, assistant frame, and result.
- `recorded-approval.jsonl`: real Bash tool use, `can_use_tool` request with permission suggestions, denied tool result, and completion.
- `recorded-question.jsonl`: real `AskUserQuestion` round trip, including the CLI rejecting a question with too few options.
- `recorded-interrupt.jsonl`: real interruption after the first text delta.
- `notifications.jsonl`: a small synthetic fixture that covers the normalized event kinds, a subagent frame, a withdrawn request, and an unsupported control request.

Recorded files retain received envelopes only. Session, message, tool, and request ids were replaced with stable fixture ids, `/private/tmp` was normalized to `/tmp`, and usage, cost, plugin, and machine-specific init metadata were removed. Raw originals remain in ignored `output/` files locally.

Unit tests replay these recordings through the decoder. `internal/testclaude` is the fake process for bidirectional tests: approvals, questions, interrupts, withdrawn requests, missing executables, process loss, stdout noise, and cold reattachment.

## Stream recovery (both harnesses)

The manager owns live chat state independently of HTTP requests. Partial text flushes to SQLite every 250 ms and on non-delta events. Completed messages are rendered as Markdown on the server.

A process-wide runtime generation and monotonically increasing sequence identify SSE boundaries. The bounded replay buffer retains 256 updates. Subscription, snapshot capture, and publication use the same mutex. Slow browsers receive coalesced wakeups and cannot block an agent. A generation mismatch or expired sequence causes a fresh snapshot; otherwise the client receives missed item updates. Partial text updates carry the accumulated text, making replay idempotent.

Ordinary live events update individual items, controls, and the sidebar. The browser replaces the conversation only on initial connection or snapshot recovery. Assistant text updates are batched every 75 ms and use `textContent`.

The first pass retains loaded UI history in memory and flushes changed chats transactionally. It does not yet paginate very long histories.
