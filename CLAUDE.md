# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Read `PROJECT.md` first for scope: single-user tool, Tailscale is the access control, no accounts or multi-tenant infrastructure. Design notes live in `ai-docs/` (architecture, adapter contract, storage and sessions, deployment constraints, roadmap). Operator docs and protocol notes live in `docs/`.

## Commands

Requires Go 1.26+, [Task](https://taskfile.dev/), Node.js (for one JS test), and Codex CLI for live use.

```sh
task build                    # bin/hypercode, single binary with embedded assets
task dev                      # go run ./cmd/hypercode
task test                     # go test -race ./... + node --test internal/web/browser_test.cjs
task lint                     # gofmt check (internal/checkformat) + go vet
task dev-fixture -- -addr 127.0.0.1:8091   # offline Codex peer, isolated history in .cache/fixture

go test -race ./internal/session/ -run TestProcessLossAndColdResume   # one test
go run ./cmd/codex-spike -dir /tmp                                    # live protocol check against real Codex
```

`task lint` fails on any unformatted Go file, so run `gofmt -w` before finishing.

The fixture server (`dev-fixture`) is how UI work is checked without model calls. Messages containing `[approval]`, `[question]`, `[wait]`, `[reject]`, or `[crash]` trigger those flows; anything else returns a Markdown sample. With Playwright CLI attached to it, `internal/web/browser-regressions.js` is the manual browser regression script. Delete `.cache/fixture` afterwards if you created chats.

## Architecture

One Go binary. No frontend build step. HTMX 4.0.0 is vendored in `internal/web/assets`; `app.js` is the only hand-written JS.

**Data flow**: browser POST → `internal/web` handler → `session.Manager` → `adapter.Session` (a `codex app-server` subprocess over stdio JSON-RPC). Events come back through `Session.Events()`, the manager turns them into `store.Item` updates, persists them to SQLite, appends them to a bounded replay ring, and wakes SSE subscribers. `internal/web` renders items to HTML fragments and streams a JSON envelope over `/events`.

**Manager owns processes, not the browser.** Closing the tab never stops a turn. Each live chat has a `runtime` holding the native session. Updates carry a generation id plus a monotonic sequence; the SSE handler uses `Last-Event-ID` to replay from the ring or falls back to a full snapshot when the ring no longer covers it or the generation changed. Partial assistant output is persisted on a timer, so a snapshot after reconnect includes text the browser missed.

**Restart semantics.** `store.Open` marks every `running` session and every `streaming`/`pending` item `interrupted` at startup. Nothing auto-resumes. `Resume` on the adapter is a separate verb from `Open` and must work cold, with the old process gone. A chat that never sent a message opens a fresh native thread on resume instead. Replies to prompts whose process died are rejected ("expired or already answered").

**Harnesses.** `store.Chat.Harness` is the key into the adapter map built in `cmd/hypercode/main.go`. Only `codex` is registered today. Adding one means: implement `adapter.Adapter` + `adapter.Session` in `internal/adapter/<name>`, register it in `main.go`, add its display name to `harnessNames` in `internal/web/server.go`, and add a matching `<symbol id="h-<name>">` to the `icons` template in `app.html`. `Manager.Harnesses()` drives the new-chat select, so no other UI change is needed. Native wire formats never leave the adapter package.

**Template contract with `app.js`.** The JS depends on these ids and attributes staying stable: `#workspace[data-chat]`, `#conversation`, `#empty-conversation`, `#item-<id> .message-body`, `#message-form`, `#message`, `#turn-controls[data-ready]`, `#connection` (text set to "Connected"/"Reconnecting…", class `online`), `#app-error`, `#chat-list`, `#breadcrumb-title`, `#chat-status`, `.new-chat` (the `N` shortcut), `[data-free-answer]`, `.item[data-status]`. `Server.live()` emits `hx-swap-oob` fragments for the chat list, breadcrumb, status, and controls. Items are rendered through `itemView` (item + chat harness), not bare `store.Item`.

**Streaming rule.** Only the in-progress assistant message is patched by JS (text deltas batched every 75 ms). Completed messages are re-rendered server-side as Markdown (Goldmark, raw HTML off) and swapped whole. Never replace the conversation container mid-stream except on a snapshot.

**Security boundary.** `Server.boundary` rejects cross-origin POSTs via `Origin`/`Sec-Fetch-Site`, sets CSP, and caps form size. `main.go` refuses to bind anything but loopback or Tailscale (100.64/10, fd7a:115c:a1e0::/48) addresses. Processes are killed only through the `exec.Cmd` handle captured at spawn.

## Testing conventions

- Adapter tests replay recorded fixtures in `internal/adapter/codex/testdata` through the decoder, and drive a fake process for bidirectional RPC. No live model in unit tests. Recordings are redacted; raw captures go to the ignored `output/` directory.
- `internal/testcodex` is the deterministic app-server peer used by `dev-fixture` and integration tests.
- Wait on events, never on sleeps. A test that needs a timeout to pass is wrong.
- The protocol was verified against codex-cli 0.153.4; it can change across CLI releases. When it does, update `docs/protocol.md` and re-record fixtures via `cmd/codex-spike -record`.

## UI conventions

Dark only, system font stack, plain CSS in `internal/web/assets/app.css`. All sizes are `rem` with `html { font-size: 110% }`; keep 1px/2px hairlines in px, everything else in rem. Nothing below 0.75rem except the sidebar kbd hint. Dense two-column layout: speaker/harness glyph/time in a fixed left gutter, content on the right, no bubbles, no marketing copy. Harness glyphs are simple monochrome strokes on `currentColor`; they are considered sufficient, do not replace with brand logos.
