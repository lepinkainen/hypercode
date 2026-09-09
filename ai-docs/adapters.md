# Adapters

Harness integration contract and per-harness protocol notes. Harness ids: `codex` and `claude` (shipped), `gemini`, `opencode`, `pi` (planned).

Each adapter exposes session creation, resumption, prompts, interruption, and normalized events. Transport, permissions, and user interaction remain specific to each harness. Codex, Claude, Gemini, and Pi speak JSON over stdio; OpenCode uses HTTP and SSE.

### Contract

```go
type Adapter interface {
    // Open starts a fresh native session in dir with the given permission mode.
    // An empty model keeps the harness default.
    Open(ctx context.Context, dir string, mode PermissionMode, model string) (Session, error)
    // Resume reattaches to an existing native session by its stored reference.
    // The harness process may be gone; this must work cold after an app restart.
    Resume(ctx context.Context, dir string, ref string, mode PermissionMode, model string) (Session, error)
    // Models lists the catalog the installed CLI offers this account.
    Models(ctx context.Context) ([]Model, error)
}

type Model struct {
    ID          string // native id passed back on Open; "" means the harness default
    Name        string
    Description string
    Default     bool
}

type Session interface {
    Ref() string                                  // native session reference to persist
    Model() string                                // effective native model id; the manager pins it on the chat
    Send(ctx context.Context, text string) error  // start a turn
    Stop(ctx context.Context) error               // interrupt the active turn
    Respond(ctx context.Context, requestID string, answer Answer) error // approval / question reply
    Events() <-chan Event                         // normalized stream
    Close() error
}
```

Normalized `Event` kinds: `text_delta`, `text_done`, `tool_started`, `tool_updated`, `tool_done`, `approval_requested`, `question_asked`, `status`, `turn_done`, `error`. Provider-specific detail stays inside the adapter as an opaque payload. Expose only controls the harness actually supports.

`Resume` is a distinct verb, not a flag on `Open`. Every adapter needs a cold-reattach path.

Model catalogs come from the CLI, never from a hard-coded list: Codex answers `model/list` on the app-server, Claude returns `models` in its `initialize` control response. The manager loads both once at startup in the background; a failure leaves that harness at its default model. A chat created with the default model stores the id the session actually resolved to (`Session.Model()`: Codex `thread/start` result `model`, Claude `get_settings` control request `effective.model` mapped back to a catalog value), so later changes to the CLI default never relabel or retarget an existing chat.

### Per-harness

| Harness  | Connection                                           | Notes                                                                                                                                                                                                                                                                                           |
| -------- | ---------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Codex    | `codex app-server`, JSON-RPC over stdio              | `initialize`, `thread/start` / `thread/resume`, `turn/start`, `turn/interrupt`. Events arrive as `thread/*` and `turn/*` notifications. Approvals arrive as server-to-client JSON-RPC requests; reply with the matching response id.                                                          |
| Claude   | `claude --print` with stream-json in and out         | Flags: `--print --verbose --input-format stream-json --output-format stream-json --include-partial-messages --permission-prompt-tool stdio --permission-mode <mode>`. Pass `--session-id <uuid>` on open so the ref is known up front; `--resume <uuid>` on resume. Permission prompts arrive on stdout as `control_request` with subtype `can_use_tool`; answer on stdin with `control_response`. `AskUserQuestion` is only offered to the model when `--permission-prompt-tool stdio` is set, and it arrives as the same `can_use_tool` request; answers go back in `updatedInput.answers` keyed by question text. Interrupt is a client control request. See `docs/protocol.md`. |
| Gemini   | `gemini --experimental-acp`, JSON-RPC over stdio     | Agent Client Protocol: initialize, new/load session, prompt, cancel, `session/update` notifications, `session/request_permission` requests. Not verified against a reference implementation yet, so spike and fixture it first.                                                                  |
| OpenCode | `opencode serve`, HTTP + SSE                         | One server process per app instance, not per chat. `/event` SSE stream is server-wide; filter by session id. Create/resume session, send prompt, abort. `permission.asked` and `question.asked` events have reply endpoints.                                                                     |
| Pi       | `pi --mode rpc`, JSON over stdio                     | Prompt, session selection, abort, events, extension dialogs. Render the dialogs Pi actually emits; do not map them onto a universal permission model. Not verified against a reference implementation yet, so fixture it first.                                                                  |

Claude caveat: the `control_request` / `control_response` protocol is what the official Agent SDK uses under the hood; the CLI reference documents only `--permission-prompt-tool` with an MCP tool. Verified against Claude Code 2.1.263 with recorded fixtures in `internal/adapter/claude/testdata`. Pin the CLI version and re-record when it changes. Same discipline as Codex app-server.

Permission and question shapes differ per harness. Normalize to `approval_requested` (yes/no/always) and `question_asked` (free-form or options), and let each adapter decide which it emits.
