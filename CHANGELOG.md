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

## [0.2.0] - 2026-04-17

### Added

- **macOS native system proxy support** — `networksetup` CLI integration for HTTP/HTTPS proxy configuration per network service, with crash-safe backup/restore to `sysproxy-backup.json`. Mirrors the Windows registry implementation.
- **macOS CA trust install / uninstall / detection** — the dashboard can now add CursorForge's CA to the current user's login keychain, remove it again, and detect whether the CA is already trusted.
- **Linux NSS trust support** — when `certutil` is available, the dashboard can import the CA into the user's NSS DB (`~/.pki/nssdb`) so Chromium/Electron-style consumers can trust the local MITM certificate.
- **"Quit or minimize to tray?" dialog** — First close shows a modal asking whether to fully quit (stop proxy, revert settings) or minimize to system tray (keep proxy running). Includes "Remember my choice" checkbox persisted to `config.json`.
- **Dynamic OS label in footer** — Footer now shows the actual platform ("Windows build", "macOS build", "Linux build") via `System.IsWindows/IsMac/IsLinux` instead of a hardcoded string.

### Changed

- **Linux system proxy is now a no-op** — Cursor's `settings.json` already routes through our MITM listener via `ApplyCursorTweaks`. Forcing system-wide proxy on Linux is fragile (GNOME/KDE/Sway each wire it differently) and can strand users; leaving it as a no-op means BYOK works reliably with zero risk of clobbering network config.
- **README now documents cross-platform trust behaviour** — platform-specific data locations, CA installation flow, Linux manual trust fallback, and platform caveats are now spelled out in the setup and safety sections.
- **Close-behaviour preference persisted** — `UserConfig.CloseAction` field (`"" | "quit" | "tray"`) remembers the user's choice across sessions. When set, the close button immediately performs the chosen action without showing the modal.
- **Backend callbacks for frontend dialog** — `ProxyService` now exposes `SetQuitCallback`, `SetHideCallback`, `RequestQuit`, `RequestHide`, `GetCloseAction`, and `SetCloseAction` methods so the Vue modal can drive the Wails window/app lifecycle cleanly.

### Fixed

- **WinINET proxy restore after crash** — `sysproxy-backup.json` sidecar now stores the pre-override state of `ProxyEnable`, `ProxyServer`, and `ProxyOverride`. `EnableSystemProxy` only snapshots when no backup exists (crash-safe), and `DisableSystemProxy` restores the exact original state including deletion of values that didn't exist before.
- **Cursor settings.json backup is idempotent** — `ApplyCursorTweaks` now checks whether `settings-backup.json` exists before overwriting it, preventing crash-restart cycles from permanently corrupting the user's pristine Cursor settings.
- **CA state is surfaced in the dashboard** — the overview now shows CA install mode and platform warnings so Linux manual trust gaps and macOS keychain behaviour are visible instead of hidden behind generic errors.
- **Multi-chat session isolation** — `sessionStore` now tracks `droppedIDConv` (request_id → conversation_id) so retry/reconnect RunSSE requests rejoin their original conversation instead of cross-wiring to the most recent chat. The `lastConvSafeFallback` guard refuses the fallback when more than one conversation is active to avoid silent data corruption.
- **Unique tool-call sequence numbers** — Tool execution now uses a process-wide `atomic.Uint32` counter instead of the previous `(round*10 + len(result.ToolCalls) + 1)` formula, which gave the same sequence number to every tool call in a single round and caused `seqAlias` / `shellAccum` collisions.
- **Partial-startup warnings preserved** — `ProxyService.StartProxy` now accumulates non-fatal startup errors (e.g., failed Cursor settings.json tweak, SQLite auth inject) into a single "Partial start:" message instead of clearing `LastError` after a successful MITM start, so the UI can surface the partial failures.

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
