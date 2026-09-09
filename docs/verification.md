# Initial verification

Local verification on 2026-09-09.

## Automated checks

`task test`, `task lint`, and `task build` passed. Tests run with Go's race detector.

The tests cover the Codex process/RPC lifecycle, recorded native notifications, approval and question replies, resolved request expiry, process loss, SQLite recovery, project deduplication, one-turn enforcement, canceled HTTP requests, partial snapshots, bounded replay fallback, cold resume, same-origin rejection, embedded assets, safe Markdown, and SSE initialization.

## Native Codex checks

Using installed `codex-cli 0.153.4` and an authenticated CLI session:

- Opened a read-only thread and received the requested streamed reply.
- Closed the process, then resumed the stored thread in a new process and received another reply.
- Triggered an explicit command approval and sent a denial. Codex confirmed that it did not execute or retry the command.
- Interrupted a response after its first text delta. Codex reported an interrupted turn.

Selected received protocol envelopes are checked into the adapter fixture directory. Structured questions and the browser scenarios use the offline protocol peer; they do not require model calls.

## Playwright checks

The headed Chromium browser exercised the actual embedded UI against the offline peer:

- Created chats and sent messages using the composer.
- Rendered incremental text, Markdown, tool activity, and inline approvals.
- Refreshed with an approval pending, then answered it successfully.
- Submitted a selected option and a free-text answer in one question form.
- Switched chats while a turn remained active, then returned to its preserved output.
- Stopped a turn and resumed the chat.
- Restarted Hypercode during a pending approval. The browser reconnected, the prompt became inactive, and explicit resume appended new history.
- Took the browser offline, answered an approval through HTTP, and reconnected. The UI recovered without duplicate items or pending controls.
- Recovered from an offline peer process exit.
- Checked a 390 × 844 mobile viewport for horizontal overflow and duplicate sidebar/composer elements.

The refreshed UI reported no console errors. Expected connection failures occurred while the service was deliberately stopped. Screenshots and local browser snapshots are in ignored `output/playwright/` and `.playwright-cli/` directories.

A remote Tailscale host, VM reboot, and systemd deployment were not exercised locally. The deployment document includes the remaining host checks.

## Native Claude Code checks

Verified on 2026-09-09 with Claude Code 2.1.263 through `cmd/claude-spike`:

- Opened a fresh session with a generated `--session-id`, streamed a text reply, and received a completed result.
- Requested a Bash command in read-only mode. The command appeared as a `can_use_tool` control request with permission suggestions; a denial produced an `is_error` tool result and the model reported the denial. An allow-listed command from the user's Claude settings ran without a prompt.
- Triggered `AskUserQuestion`; the answer keyed by question text was accepted and echoed back. A question with fewer than two options was rejected by the CLI itself.
- Interrupted a response after its first text delta. The result carried `terminal_reason: aborted_streaming`.
- Resumed the approval session cold with `--resume`; the model recalled the denied command.
- Resumed an unknown session id; the process exited before answering `initialize` and the open failed with the CLI's message.
