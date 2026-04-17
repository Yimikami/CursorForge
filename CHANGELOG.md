# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

When the release workflow builds a tag `vX.Y.Z`, the section below titled
`## [X.Y.Z]` (if present) is prepended to the auto-generated GitHub release
notes. Leave it empty and the release will contain only the auto-generated
PR / commit summary.

## [Unreleased]

-

## [0.1.0] - 2026-04-17

Initial public release.

### BYOK gateway

- Local MITM proxy that intercepts the four Cursor IDE RPC paths needed to
  drive the agent (`BidiAppend`, `RunSSE`, `AvailableModels`,
  `GetDefaultModelNudgeData`) and 404s everything else so Cursor's BYOK
  picker stays happy.
- Synthetic "Pro" session injected into Cursor's SQLite auth store so the
  chat picker and agent UI unlock without a real cursor.com account.
  Originals are backed up to `cursor-auth-backup.json` and restored on Stop.
- System proxy + Cursor `settings.json` tweaks applied on Start, rolled
  back on Stop.

### Providers

- **OpenAI-compatible** transport (`/chat/completions`, Bearer auth) covering
  OpenAI, Groq, OpenRouter, Together, Azure OpenAI, local vLLM / llama.cpp,
  and anything else speaking the chat-completions wire. Supports
  `reasoning_effort`, `service_tier`, `max_tokens`, and streamed
  `reasoning_content` thinking for reasoning-capable models.
- **Native Anthropic Messages API** transport (`/v1/messages`, `x-api-key`)
  with full tool_use / tool_result content-block round-tripping,
  `thinking_delta` streamed through Cursor's Thinking UI,
  `stop_reason` → `finish_reason` mapping, and usage accounting.

### Agent loop

- Full tool-call loop (20 rounds / 5-minute wallclock cap) with the 7
  built-in Cursor tools (Shell, Read, Write, Glob, Grep, Delete, StrReplace)
  plus every MCP tool exposed as its own OpenAI-style function.
- Plan mode with live `.plan.md` panel updates; mode switching between
  Agent / Ask / Plan / Debug with an Approve / Reject dialog for the
  SwitchMode handshake.
- Shell tool supports both foreground (wait until finished) and background
  (`block_until_ms=0`) modes with a sentinel contract for tailing long
  commands via the terminals folder.
- Per-turn persistence under `%APPDATA%/CursorForge/history/<conv-id>/` —
  request body, raw SSE, summary JSON, and a replay message list that
  faithfully reproduces tool_calls + tool results on the next turn.

### UI

- Wails 3 desktop app (Go backend + Vue 3 + TypeScript frontend) with a
  tray icon; windowed dashboard has Overview, Models, Stats, and an Editor
  view for adding / testing adapters.
- Stats tab aggregates prompt / completion tokens per model plus a 7-day
  chart from the persisted per-turn artifacts.
- Apple-inspired Puppertino CSS theme bundled under `frontend/public/`.

### Ops

- CA installation / fingerprint display from the dashboard.
- `TestAdapter` ping with provider-aware endpoint (`/v1/models` for
  Anthropic, `/models` for OpenAI-compatible).
- CI + release GitHub Actions workflows producing signed-path Windows /
  macOS / Linux artifacts on every `vX.Y.Z` tag.

### Known limitations

- Cursor IDE updates may break protocol fidelity until a matching
  CursorForge release.
- No prompt-caching support for Anthropic yet — planned for a later cut.
- Token counters are accurate for OpenAI / Anthropic; third-party
  OpenAI-compatible providers that omit `usage` on the stream fall back to
  a heuristic (`len/4`) that will under- or over-count.

<!--
Template for future releases — copy this block, replace [X.Y.Z] / YYYY-MM-DD
with the real version + date, and fill in when cutting a tag. The leading
spaces on the heading are intentional: they keep the extractor (which
matches `^## `) from ever picking up this example.

    ## [X.Y.Z] - YYYY-MM-DD

    ### Added
    - …

    ### Changed
    - …

    ### Fixed
    - …
-->
