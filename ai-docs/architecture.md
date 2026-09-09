# Architecture

Stack, UI shape, and the HTMX/JS split. See `PROJECT.md` for scope and `ai-docs/adapters.md` for the harness contract.

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
┌──────────────────┬──────────────────────────────────────────────┐
│ hypercode        │ my-project / Fix tests   ~/src/my-project    │
│ + New chat     N ├──────────────────────────────────────────────┤
│                  │ You      Fix the failing tests.              │
│ Chats            │ 14:02                                        │
│ ● Fix tests    ◇ │                                              │
│ ● Review code  ✳ │ Codex ◇  I looked at …                       │
│ ● Add lint     ◇ │ 14:02    (Markdown, code blocks)             │
│                  │                                              │
│                  │ Tool     ▸ Tool activity          completed  │
│                  │ 14:03                                        │
│                  │ Approval ┌ Permission request ─────────────┐ │
│                  │ 14:03    │ Allow once · Allow for session … │ │
│                  ├──────────┴──────────────────────────────────┤
│ ● 3 agents ready │ [◇ Codex · workspace-write]     hint   Send │
└──────────────────┴──────────────────────────────────────────────┘
```

- One flat chat list in the sidebar. Each row shows a status dot, the title, and a trailing harness glyph (the t3code pattern). The glyph is the only per-row harness indicator; a tooltip carries the full name.
- New chat asks for the harness, a working directory, and a permission mode. Only configured harnesses are offered. No model picker in v1; harness default is used.
- Each chat belongs to exactly one harness. The harness name and glyph repeat in the topbar chip, the composer chip, and the assistant label in the gutter.
- Conversation is a dense two-column grid: a fixed left gutter with speaker, harness glyph, and time; content on the right. No bubbles. Tool activity is a collapsed row; approvals and questions are cards.
- Dark mode only. System font stack. All sizes in `rem` with the root at 110% so the browser's default font-size setting scales the whole UI. Nothing renders below 0.75rem except the keyboard-shortcut hint in the sidebar (0.6875rem).
- One active turn per chat.
- Pending approvals and questions render inline in the conversation and survive refresh.

### HTMX / JS split

- HTMX handles navigation, new chat, message submit, approval and question forms.
- One browser `EventSource` per page. Each SSE message is a JSON envelope: `snapshot` flag, `html` (out-of-band fragments for the chat list, breadcrumb, status, and controls), `conversation` (full list, snapshot only), and `items` (one entry per changed item, carrying either a rendered HTML fragment or a raw text delta for the streaming message). The JS hands each part to `htmx.swap`.
- Vanilla JS owns the in-progress assistant message (text deltas batched every 75 ms, follow-scroll) plus the browser-side conveniences HTMX cannot: one `EventSource` per page with reconnect on page lifecycle events, per-chat draft memory, the error banner, the `N` shortcut, radio versus free-text exclusivity in question forms, and dropping empty answers before POST. Completed messages are re-rendered server-side as Markdown and swapped in.
- Never replace the whole conversation during streaming. Update the active message element only.
