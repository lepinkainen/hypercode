# Roadmap and testing

## Out of scope for v1

Model selection per chat, git tooling, diff view, terminal emulation, native session import, public or non-Tailscale remote access, multi-user support, multiple concurrent turns per chat.

## Order of work

1. **Spikes first** (Codex done, `cmd/codex-spike`; Claude and Gemini pending). A ~100-line Go CLI per harness that opens a session, sends a prompt, streams output, triggers and answers one permission prompt, interrupts, and resumes. Codex and Claude first; then Gemini, OpenCode, Pi. Record the raw protocol traffic from each as test fixtures.
2. **Full UI flow with Codex and SQLite** (done). Sidebar, new chat, send, stream, stop, approvals, refresh survival, cold resume.
3. **Claude** through the same contract.
4. **Gemini CLI, OpenCode, Pi** through the same contract.
5. **Hardening pass:** refresh during a turn, switching chats mid-turn, browser and Tailscale disconnection, SSE replay and snapshot fallback, harness process failure, service restart and cold resume, approval replies after refresh, rejection of stale replies after process loss.

## Testing

- Adapter tests replay recorded protocol fixtures against a fake process; no live harness in unit tests.
- Verify VM/VPS deployment through Tailscale, including browser reconnection while a turn continues and explicit session resumption after a service restart.
- A test that needs a sleep or timeout to pass is wrong. Wait on events.
