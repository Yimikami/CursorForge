# CursorForge frontend

This is the Vue 3 + TypeScript + Vite dashboard embedded into the Wails 3
desktop binary. Under normal use you don't touch this directly — the root
`wails3 dev` / `wails3 build` commands drive Vite for you.

## Layout

```
frontend/
├── index.html                 # Vite entry
├── vite.config.ts             # minimal Vite config (Vue plugin only)
├── tsconfig.json              # TS config for vue-tsc
├── package.json
├── public/
│   └── puppertino/            # bundled Puppertino CSS theme
├── bindings/                  # AUTO-GENERATED — do not hand-edit
│   └── cursorforge/internal/bridge/
│       └── proxyservice.ts    # typed client for the Go ProxyService
└── src/
    ├── main.ts                # app bootstrap
    ├── App.vue                # root component
    ├── style.css
    └── components/
        ├── ProxyDashboard.vue # the whole dashboard (Overview / Models / Stats / Editor)
        └── logos/             # OpenAI / Anthropic mark SVGs
```

## Talking to the Go backend

All backend calls go through the generated bindings in `bindings/`, produced
by `wails3 generate bindings` (part of `wails3 build`). Example:

```ts
import { ProxyService } from "../../bindings/cursorforge/internal/bridge";

const state = await ProxyService.GetState();
await ProxyService.StartProxy();
```

Server-push events (e.g. `proxyState`) arrive via `@wailsio/runtime`:

```ts
import { Events } from "@wailsio/runtime";
Events.On("proxyState", (running: boolean) => {
  /* ... */
});
```

**Never edit anything under `bindings/`** — the whole directory is wiped and
regenerated on every build (see `generate:bindings` in `build/Taskfile.yml`).
If a binding is wrong, fix the Go side (`internal/bridge/proxy_service.go` or
`internal/bridge/types.go`) and rerun `wails3 build`.

## Scripts

| Command             | What it does                                                                                                                                |
| ------------------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| `npm run dev`       | Standalone Vite dev server (useful only if you want to iterate the UI against stubs). The real dev loop is `wails3 dev` from the repo root. |
| `npm run build`     | Production build → `frontend/dist/`, embedded into the Go binary via `//go:embed all:frontend/dist` in `main.go`.                           |
| `npm run build:dev` | Unminified dev build, same output directory.                                                                                                |
| `npm run preview`   | Preview the built bundle.                                                                                                                   |

## Styling

- Base tokens and layout in `src/style.css`.
- Puppertino CSS (Apple-inspired component classes like `p-btn p-prim-col`)
  is vendored under `public/puppertino/` and pulled in via `index.html`.
  The root `build/Taskfile.yml` has a `frontend:vendor:puppertino` task that
  will fetch / patch `index.html` if the file is missing.
- All component styles are `<style scoped>` inside the SFC.

## Adding a new service method to the UI

1. Add the method to `ProxyService` in `internal/bridge/proxy_service.go`.
   Types live in `internal/bridge/types.go`.
2. Run `wails3 build` once — this regenerates `frontend/bindings/`.
3. Import and call `ProxyService.YourMethod()` from the Vue side.

## Editor setup

- **VS Code / Cursor**: install the official Vue (Volar) extension.
  Disable the built-in TS extension in the workspace if you want Volar's
  Take Over Mode for faster type checking on `.vue` files.
- Type checking runs via `vue-tsc` as part of `npm run build`.
