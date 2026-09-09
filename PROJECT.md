# Hypercode — Project Purpose

This document says why the project exists and how to keep work in scope.
`README.md` has the run, build, and layout details. `ai-docs/` holds the
design notes: architecture, adapters, storage and sessions, deployment and
security, roadmap. This document does not repeat them.

## What it is

Hypercode is a minimal web UI for coding agent harnesses. It runs on the
user's computer or on a persistent VM reached through Tailscale. One Go
binary with an embedded frontend and SQLite for UI history. The installed
harness CLIs are the integration boundary; Hypercode drives them and shows
their output, approvals, and questions in a browser.

Harnesses: **Codex** (`codex`) today. Planned: **Claude Code** (`claude`),
**Gemini CLI** (`gemini`), **OpenCode** (`opencode`), **Pi** (`pi`). All sit
behind one adapter contract; the key in parentheses is the harness id used
in the database, the adapter map, and the UI icon sprite.

Not a fork of T3 Code. T3 Code's adapters are a reference for protocol
behavior, not code to port.

## Scope — a single-user personal tool

One person uses this on their own host. Design and review changes around
that person's coding workflow: long conversations, several active chats,
large pasted messages, accidental repeated submissions, browser reconnection,
process restarts. Prefer small fixes with regression tests for those cases.

Do not add:

- Accounts, login, multi-tenancy, or per-user settings. Tailscale is the
  access control.
- Distributed coordination, job queues, or compatibility layers for
  hypothetical production load.
- Public or non-Tailscale remote access.
- Model selection per chat, git tooling, diff view, terminal emulation,
  native session import, concurrent turns in one chat (v1).

Two invariants: a new native thread never recovers a missing thread's
context, so starting fresh stays explicit; and shutdown finishes even if a
subprocess holds a pipe, while killing is limited to the process handle
captured at spawn.

## Direction

A new harness must stay cheap to add: one adapter package, a registration
in the entry point, a name and glyph in the UI. Native wire formats stay
inside the adapter. Put effort into the parts the user feels every day:
streaming that survives reconnects, approvals that survive refresh, and a
plain, legible UI.
