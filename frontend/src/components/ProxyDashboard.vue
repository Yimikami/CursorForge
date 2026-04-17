<script setup lang="ts">
import { ref, onMounted, onBeforeUnmount, computed, watch } from "vue";
import { ProxyService } from "../../bindings/cursorforge/internal/bridge";
import { Window, Events, Browser, System } from "@wailsio/runtime";
import OpenAIMark from "./logos/OpenAIMark.vue";
import AnthropicMark from "./logos/AnthropicMark.vue";

const APP_VERSION = "0.1.0";
const GITHUB_REPO = "Yimikami/cursorforge";
const GITHUB_URL = `https://github.com/${GITHUB_REPO}`;

// Label shown in the footer next to the version. System.IsWindows/IsMac/
// IsLinux are synchronous helpers injected by the Wails runtime at load
// time, so we can resolve the OS name without awaiting a round-trip to Go.
const OS_LABEL = (() => {
  try {
    if (System.IsWindows()) return "Windows build";
    if (System.IsMac()) return "macOS build";
    if (System.IsLinux()) return "Linux build";
  } catch {
    /* runtime not ready (dev tools preview) — fall through */
  }
  return "Desktop build";
})();

function winMinimise() {
  Window.Minimise();
}
function winHide() {
  Window.Hide();
}

type ProxyState = {
  running: boolean;
  listenAddr: string;
  baseURL: string;
  startedAt: number;
  caFingerprint: string;
  caPath: string;
  caInstalled: boolean;
  lastError?: string;
};

type ModelAdapter = {
  displayName: string;
  type: string;
  baseURL: string;
  apiKey: string;
  modelID: string;
  contextWindow?: string;
  reasoningEffort?: string;
  serviceTier?: string;
  maxOutputTokens?: string;
  thinkingBudget?: string;
  notes?: string;
  lastTestResult?: string;
  lastTestedAt?: number;
};

type UserConfig = {
  baseURL: string;
  modelAdapters: ModelAdapter[];
};

type CursorTweaks = {
  path: string;
  found: boolean;
  error?: string;
  proxySet: boolean;
  proxyValue?: string;
  strictSSLOff: boolean;
  proxySupportOn: boolean;
  systemCertsV2On: boolean;
  useHttp1: boolean;
  disableHttp2: boolean;
  proxyKerberos: boolean;
};

type View = "overview" | "models" | "stats" | "editor";
type Provider = "openai" | "anthropic";

type ModelUsageEntry = {
  model: string;
  provider: string;
  promptTokens: number;
  completionTokens: number;
  turnCount: number;
};
type DailyUsageEntry = {
  date: string;
  promptTokens: number;
  completionTokens: number;
};
type UsageStats = {
  totalPromptTokens: number;
  totalCompletionTokens: number;
  totalTokens: number;
  conversationCount: number;
  turnCount: number;
  perModel: ModelUsageEntry[];
  last7Days: DailyUsageEntry[];
};

const HELP = {
  cacheHit:
    "Fraction of requests served from the upstream cache instead of hitting the model.",
  turns:
    "Total assistant turns in this session. Invalid turns are those that errored or were cancelled.",
  tokens: "Combined prompt + completion tokens across this session.",
  listen:
    "Local address where CursorForge accepts proxied traffic from Cursor IDE.",
  uptime: "How long the local proxy has been running since last start.",
  caFp: "SHA-256 fingerprint of the local CA. Install this CA into the Windows trust store before enabling MITM.",
  displayName:
    "Label shown in the picker. Free-form, not sent to the provider.",
  modelID:
    "Canonical model identifier sent to the upstream provider (e.g. gpt-4o, claude-sonnet-4-5).",
  apiKey:
    "Provider API key. Stored locally in config.json, never transmitted outside BYOK calls.",
  baseURL:
    "Provider endpoint. Override for proxies / reverse proxies / Azure OpenAI.",
  contextWindow:
    "Max input tokens sent to the provider. Leave blank to use the provider default.",
  reasoningEffort:
    "Reasoning budget hint for OpenAI reasoning models (o1, o3, gpt-5 series).",
  fastMode:
    "Use priority service tier for faster responses (OpenAI). Costs more per token.",
  maxOutput:
    "Cap on output tokens per response. Leave blank to use the provider default.",
  thinkingBudget:
    "Anthropic extended-thinking token budget. Applies only to reasoning-capable models.",
  notes: "Private memo. Not sent anywhere.",
};

const state = ref<ProxyState>({
  running: false,
  listenAddr: "",
  baseURL: "",
  startedAt: 0,
  caFingerprint: "",
  caPath: "",
  caInstalled: false,
});
const cfg = ref<UserConfig>({ baseURL: "", modelAdapters: [] });
const tweaks = ref<CursorTweaks>({
  path: "",
  found: false,
  proxySet: false,
  strictSSLOff: false,
  proxySupportOn: false,
  systemCertsV2On: false,
  useHttp1: false,
  disableHttp2: false,
  proxyKerberos: false,
});
const busy = ref(false);

const currentView = ref<View>("overview");
const providerTab = ref<Provider>("openai");

const stats = ref<UsageStats>({
  totalPromptTokens: 0,
  totalCompletionTokens: 0,
  totalTokens: 0,
  conversationCount: 0,
  turnCount: 0,
  perModel: [],
  last7Days: [],
});
const statsLoading = ref(false);
async function loadStats() {
  statsLoading.value = true;
  try {
    stats.value = (await ProxyService.GetUsageStats()) as UsageStats;
  } finally {
    statsLoading.value = false;
  }
}
const maxDailyTotal = computed(() => {
  let m = 0;
  for (const d of stats.value.last7Days) {
    const t = d.promptTokens + d.completionTokens;
    if (t > m) m = t;
  }
  return m;
});
function formatNum(n: number): string {
  if (n < 1000) return String(n);
  if (n < 1_000_000) return (n / 1000).toFixed(n < 10_000 ? 1 : 0) + "k";
  return (n / 1_000_000).toFixed(n < 10_000_000 ? 2 : 1) + "M";
}
function shortDate(d: string): string {
  // YYYY-MM-DD → "Apr 12"
  const parts = d.split("-");
  if (parts.length !== 3) return d;
  const m = [
    "Jan",
    "Feb",
    "Mar",
    "Apr",
    "May",
    "Jun",
    "Jul",
    "Aug",
    "Sep",
    "Oct",
    "Nov",
    "Dec",
  ];
  const mi = Number(parts[1]) - 1;
  return (m[mi] ?? parts[1]) + " " + Number(parts[2]);
}

watch(currentView, (v) => {
  if (v === "stats") loadStats();
});

const editorIndex = ref<number>(-1);
const editorModel = ref<ModelAdapter | null>(null);
const editorProvider = ref<Provider>("openai");
const showApiKey = ref(false);

const shortFP = computed(() =>
  state.value.caFingerprint
    ? state.value.caFingerprint.slice(0, 10) +
      "…" +
      state.value.caFingerprint.slice(-6)
    : "—",
);

async function refresh() {
  state.value = (await ProxyService.GetState()) as ProxyState;
  const c = (await ProxyService.LoadUserConfig()) as UserConfig;
  if (!c.modelAdapters) c.modelAdapters = [];
  cfg.value = c;
  tweaks.value = (await ProxyService.GetCursorSettingsStatus()) as CursorTweaks;
}

async function applyTweaks() {
  try {
    tweaks.value = (await ProxyService.ApplyCursorTweaks()) as CursorTweaks;
  } catch (e: any) {
    alert(e?.message ?? String(e));
  }
}
async function revertTweaks() {
  try {
    tweaks.value = (await ProxyService.RevertCursorTweaks()) as CursorTweaks;
  } catch (e: any) {
    alert(e?.message ?? String(e));
  }
}

async function toggleService() {
  busy.value = true;
  try {
    state.value = state.value.running
      ? ((await ProxyService.StopProxy()) as ProxyState)
      : ((await ProxyService.StartProxy()) as ProxyState);
  } catch (e: any) {
    alert(e?.message ?? String(e));
  } finally {
    busy.value = false;
  }
}

async function persistConfig() {
  await ProxyService.SaveUserConfig(cfg.value);
}

async function openSettingsFolder() {
  try {
    await ProxyService.OpenSettingsFolder();
  } catch (e: any) {
    alert(e?.message ?? String(e));
  }
}

const caBusy = ref(false);

async function toggleCAInstall() {
  caBusy.value = true;
  try {
    state.value = state.value.caInstalled
      ? ((await ProxyService.UninstallCA()) as ProxyState)
      : ((await ProxyService.InstallCA()) as ProxyState);
  } catch (e: any) {
    alert(e?.message ?? String(e));
  } finally {
    caBusy.value = false;
  }
}

function openRepo() {
  Browser.OpenURL(GITHUB_URL).catch((e) => {
    alert(`Could not open browser: ${e?.message ?? String(e)}`);
  });
}

// Parse a semver-ish tag ("v0.1.0", "0.1.0", "0.1.0-rc1") into comparable
// numeric triple. Unknown parts default to 0 so pre-release suffixes just
// fall back to their numeric prefix, which is fine for a coarse check.
function parseVersion(tag: string): [number, number, number] {
  const cleaned = tag.trim().replace(/^v/i, "").split(/[-+]/)[0] ?? "";
  const parts = cleaned.split(".").map((p) => parseInt(p, 10));
  return [parts[0] || 0, parts[1] || 0, parts[2] || 0];
}

function isNewer(remote: string, local: string): boolean {
  const r = parseVersion(remote);
  const l = parseVersion(local);
  for (let i = 0; i < 3; i++) {
    if (r[i] > l[i]) return true;
    if (r[i] < l[i]) return false;
  }
  return false;
}

const updateBusy = ref(false);

async function checkForUpdates() {
  if (updateBusy.value) return;
  updateBusy.value = true;
  try {
    const resp = await fetch(
      `https://api.github.com/repos/${GITHUB_REPO}/releases/latest`,
      { headers: { Accept: "application/vnd.github+json" } },
    );
    if (resp.status === 404) {
      alert(`No releases published yet.\nYou are running v${APP_VERSION}.`);
      return;
    }
    if (!resp.ok) {
      throw new Error(`GitHub returned HTTP ${resp.status}`);
    }
    const data = (await resp.json()) as {
      tag_name?: string;
      name?: string;
      html_url?: string;
    };
    const tag = data.tag_name ?? "";
    const htmlURL = data.html_url ?? `${GITHUB_URL}/releases/latest`;
    if (!tag) {
      alert(`You are running v${APP_VERSION}. Could not read latest tag.`);
      return;
    }
    if (isNewer(tag, APP_VERSION)) {
      const open = confirm(
        `A new version is available.\n\nInstalled: v${APP_VERSION}\nLatest: ${tag}\n\nOpen the release page in your browser?`,
      );
      if (open) {
        Browser.OpenURL(htmlURL).catch((e) => {
          alert(`Could not open browser: ${e?.message ?? String(e)}`);
        });
      }
    } else {
      alert(`You are on the latest version (v${APP_VERSION}).`);
    }
  } catch (e: any) {
    alert(`Update check failed: ${e?.message ?? String(e)}`);
  } finally {
    updateBusy.value = false;
  }
}

const filteredAdapters = computed(() =>
  cfg.value.modelAdapters
    .map((a, i) => ({ a, i }))
    .filter((x) => x.a.type === providerTab.value),
);

const openAICount = computed(
  () => cfg.value.modelAdapters.filter((a) => a.type === "openai").length,
);
const anthropicCount = computed(
  () => cfg.value.modelAdapters.filter((a) => a.type === "anthropic").length,
);

function openEditor(i: number) {
  if (i === -1) {
    editorIndex.value = -1;
    editorProvider.value = providerTab.value;
    editorModel.value = {
      displayName: "",
      type: providerTab.value,
      baseURL: "",
      apiKey: "",
      modelID: "",
      contextWindow: "",
      reasoningEffort: "medium",
      serviceTier: "",
      maxOutputTokens: "",
      thinkingBudget: "",
      notes: "",
    };
  } else {
    editorIndex.value = i;
    const src = cfg.value.modelAdapters[i];
    editorProvider.value = src.type === "anthropic" ? "anthropic" : "openai";
    editorModel.value = JSON.parse(JSON.stringify(src));
  }
  showApiKey.value = false;
  currentView.value = "editor";
}

function cancelEditor() {
  currentView.value = "models";
}

async function saveEditor(runTest = false) {
  if (!editorModel.value) return;
  editorModel.value.type = editorProvider.value;
  if (editorIndex.value === -1) {
    cfg.value.modelAdapters.push(editorModel.value);
    editorIndex.value = cfg.value.modelAdapters.length - 1;
  } else {
    cfg.value.modelAdapters[editorIndex.value] = editorModel.value;
  }
  await persistConfig();
  if (runTest) {
    editorTestRunning.value = true;
    try {
      const updated = (await ProxyService.TestAdapter(
        editorIndex.value,
      )) as ModelAdapter;
      cfg.value.modelAdapters[editorIndex.value] = updated;
    } finally {
      editorTestRunning.value = false;
    }
    currentView.value = "models";
  } else {
    currentView.value = "models";
  }
}

const editorTestRunning = ref(false);

const testingIndex = ref(-1);
async function testAdapter(i: number) {
  testingIndex.value = i;
  try {
    const updated = (await ProxyService.TestAdapter(i)) as ModelAdapter;
    cfg.value.modelAdapters[i] = updated;
  } finally {
    testingIndex.value = -1;
  }
}

async function testAll() {
  for (let i = 0; i < cfg.value.modelAdapters.length; i++) {
    if (cfg.value.modelAdapters[i].type === providerTab.value) {
      await testAdapter(i);
    }
  }
}

async function duplicate(i: number) {
  const dup = JSON.parse(
    JSON.stringify(cfg.value.modelAdapters[i]),
  ) as ModelAdapter;
  dup.displayName += " (Copy)";
  cfg.value.modelAdapters.push(dup);
  await persistConfig();
}

async function removeAdapter(i: number) {
  cfg.value.modelAdapters.splice(i, 1);
  await persistConfig();
}

function shortHost(url: string) {
  if (!url) return "—";
  try {
    return new URL(url).hostname;
  } catch {
    return url;
  }
}

function obscure(key: string) {
  if (!key) return "—";
  if (key.length <= 8) return "••••";
  return key.slice(0, 4) + "••••" + key.slice(-4);
}

const allTweaksOn = computed(
  () =>
    tweaks.value.proxySet &&
    tweaks.value.strictSSLOff &&
    tweaks.value.proxySupportOn &&
    tweaks.value.systemCertsV2On &&
    tweaks.value.useHttp1 &&
    tweaks.value.disableHttp2 &&
    tweaks.value.proxyKerberos,
);

let offStateEvent: (() => void) | null = null;

onMounted(() => {
  refresh();
  offStateEvent = Events.On("proxyState", () => {
    refresh();
  });
});
onBeforeUnmount(() => {
  if (offStateEvent) offStateEvent();
});
</script>

<template>
  <div class="shell">
    <!-- ============ STICKY TOP BAR ============ -->
    <header class="topbar">
      <div class="brand">
        <div class="logo-mark">
          <svg viewBox="0 0 24 24" width="18" height="18" fill="none">
            <path
              d="M4 7l8-4 8 4-8 4-8-4z"
              stroke="#22c55e"
              stroke-width="1.5"
              stroke-linejoin="round"
            />
            <path
              d="M4 12l8 4 8-4M4 17l8 4 8-4"
              stroke="#52525b"
              stroke-width="1.5"
              stroke-linejoin="round"
            />
          </svg>
        </div>
        <div>
          <div class="brand-name">CursorForge</div>
          <div class="brand-sub">Local MITM · BYOK gateway</div>
        </div>
      </div>

      <div class="topbar-right">
        <div :class="['status-pill', state.running ? 'pill-on' : 'pill-off']">
          <span class="dot"></span>
          {{ state.running ? `Running · ${state.listenAddr}` : "Stopped" }}
        </div>
        <button
          :class="['btn', state.running ? 'btn-ghost' : 'btn-primary']"
          :disabled="busy"
          @click="toggleService"
        >
          {{ state.running ? "Stop" : "Start service" }}
        </button>

        <div class="win-controls">
          <button
            class="win-btn"
            @click="openRepo"
            title="Open GitHub repository"
            aria-label="Open GitHub repository"
          >
            <svg viewBox="0 0 16 16" width="13" height="13" aria-hidden="true">
              <path
                fill="currentColor"
                d="M8 0C3.58 0 0 3.58 0 8a8 8 0 0 0 5.47 7.59c.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82a7.4 7.4 0 0 1 2-.27c.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0 0 16 8c0-4.42-3.58-8-8-8Z"
              />
            </svg>
          </button>
          <button class="win-btn" @click="winMinimise" title="Minimise">
            <svg viewBox="0 0 12 12" width="10" height="10">
              <line
                x1="2"
                y1="6"
                x2="10"
                y2="6"
                stroke="currentColor"
                stroke-width="1.3"
                stroke-linecap="round"
              />
            </svg>
          </button>
          <button
            class="win-btn win-close"
            @click="winHide"
            title="Hide to tray"
          >
            <svg viewBox="0 0 12 12" width="10" height="10">
              <path
                d="M2 2 L10 10 M10 2 L2 10"
                stroke="currentColor"
                stroke-width="1.3"
                stroke-linecap="round"
              />
            </svg>
          </button>
        </div>
      </div>
    </header>

    <!-- ============ TABS ============ -->
    <nav class="tabs" v-if="currentView !== 'editor'">
      <button
        :class="['tab', currentView === 'overview' ? 'tab-active' : '']"
        @click="currentView = 'overview'"
      >
        Overview
      </button>
      <button
        :class="['tab', currentView === 'models' ? 'tab-active' : '']"
        @click="currentView = 'models'"
      >
        Models <span class="tab-count">{{ cfg.modelAdapters.length }}</span>
      </button>
      <button
        :class="['tab', currentView === 'stats' ? 'tab-active' : '']"
        @click="currentView = 'stats'"
      >
        Stats
      </button>
      <span class="tab-spacer"></span>
      <button
        class="link-btn"
        @click="currentView === 'stats' ? loadStats() : refresh()"
        title="Reload state from the backend"
      >
        <span class="icn">↻</span> Refresh
      </button>
    </nav>

    <!-- ============ OVERVIEW ============ -->
    <main v-if="currentView === 'overview'" class="page">
      <!-- Model list -->
      <div class="overview-section-head">
        <h3>Models</h3>
        <button class="btn btn-primary btn-sm" @click="openEditor(-1)">
          + Add model
        </button>
      </div>
      <div v-if="!cfg.modelAdapters.length" class="empty-mini">
        No models configured yet.
      </div>
      <div v-else class="overview-models">
        <article
          v-for="(a, i) in cfg.modelAdapters"
          :key="i"
          class="ov-model"
          @click="openEditor(i)"
        >
          <component
            :is="a.type === 'anthropic' ? AnthropicMark : OpenAIMark"
            class="ov-logo"
          />
          <div class="ov-info">
            <div class="ov-name">{{ a.displayName || "Untitled" }}</div>
            <div class="ov-id mono">{{ a.modelID || "—" }}</div>
          </div>
          <span
            :class="[
              'mc-status',
              a.lastTestResult === 'ok'
                ? 'ms-ok'
                : a.lastTestResult
                  ? 'ms-err'
                  : 'ms-none',
            ]"
          >
            <span class="mc-status-dot" />
            {{
              a.lastTestResult === "ok" ? "OK" : a.lastTestResult ? "Err" : "—"
            }}
          </span>
        </article>
      </div>

      <!-- Settings rows -->
      <div class="card" style="margin-top: 16px">
        <div class="row">
          <div class="row-text">
            <div class="row-title">
              Trust local CA
              <span
                :class="[
                  'row-chip',
                  state.caInstalled ? 'chip-ok' : 'chip-warn',
                ]"
              >
                {{ state.caInstalled ? "Installed" : "Not installed" }}
              </span>
            </div>
            <div class="row-desc">
              Adds CursorForge's CA to the current-user Trusted Root store so
              Cursor can verify TLS on intercepted connections.
            </div>
            <code class="row-path">SHA-256 {{ shortFP }}</code>
          </div>
          <div class="row-actions">
            <button
              :class="['btn', state.caInstalled ? 'btn-ghost' : 'btn-primary']"
              :disabled="caBusy"
              @click="toggleCAInstall"
            >
              {{ state.caInstalled ? "Uninstall CA" : "Install CA" }}
            </button>
          </div>
        </div>
        <div class="hr" />
        <div class="row">
          <div class="row-text">
            <div class="row-title">Settings folder</div>
            <div class="row-desc">Your config and CA live here.</div>
            <code class="row-path">{{
              state.caPath?.replace(/\\ca\\ca\.crt$/, "") || ""
            }}</code>
          </div>
          <div class="row-actions">
            <button class="btn btn-ghost" @click="openSettingsFolder">
              Open folder
            </button>
          </div>
        </div>
      </div>

      <div v-if="state.lastError" class="error-banner">
        {{ state.lastError }}
      </div>

      <div class="footer">
        <span>v{{ APP_VERSION }}</span>
        <span class="sep">·</span>
        <span>{{ OS_LABEL }}</span>
        <span class="footer-spacer" />
        <button class="link-btn" @click="openRepo">GitHub</button>
        <button
          class="link-btn"
          @click="checkForUpdates"
          :disabled="updateBusy"
        >
          {{ updateBusy ? "Checking…" : "Check for updates" }}
        </button>
      </div>
    </main>

    <!-- ============ MODELS ============ -->
    <main v-else-if="currentView === 'models'" class="page">
      <div class="provider-bar">
        <button
          :class="['prov', providerTab === 'openai' ? 'prov-openai' : '']"
          @click="providerTab = 'openai'"
        >
          <OpenAIMark class="prov-logo" />
          <span>OpenAI</span>
          <span class="prov-count">{{ openAICount }}</span>
        </button>
        <button
          :class="['prov', providerTab === 'anthropic' ? 'prov-anthropic' : '']"
          @click="providerTab = 'anthropic'"
        >
          <AnthropicMark class="prov-logo" />
          <span>Anthropic</span>
          <span class="prov-count">{{ anthropicCount }}</span>
        </button>

        <span class="tab-spacer" />

        <button
          class="btn btn-ghost"
          @click="testAll"
          :disabled="!filteredAdapters.length"
        >
          Test all
        </button>
        <button class="btn btn-primary" @click="openEditor(-1)">
          + Add model
        </button>
      </div>

      <div v-if="!filteredAdapters.length" class="empty">
        <div class="empty-icon">
          <OpenAIMark v-if="providerTab === 'openai'" />
          <AnthropicMark v-else />
        </div>
        <div class="empty-title">
          No {{ providerTab === "openai" ? "OpenAI" : "Anthropic" }} models yet
        </div>
        <div class="empty-desc">
          Add a model to route BYOK requests through your own API key.
        </div>
        <button class="btn btn-primary" @click="openEditor(-1)">
          + Add your first model
        </button>
      </div>

      <div class="model-grid">
        <article
          v-for="{ a, i } in filteredAdapters"
          :key="i"
          class="model-card"
        >
          <header class="mc-head">
            <div class="mc-title">
              <component
                :is="a.type === 'anthropic' ? AnthropicMark : OpenAIMark"
                class="mc-logo"
              />
              <div>
                <div class="mc-name">{{ a.displayName || "Untitled" }}</div>
                <div class="mc-id mono">{{ a.modelID || "—" }}</div>
              </div>
            </div>
            <span
              :class="[
                'mc-status',
                a.lastTestResult === 'ok'
                  ? 'ms-ok'
                  : a.lastTestResult
                    ? 'ms-err'
                    : 'ms-none',
              ]"
            >
              <span class="mc-status-dot" />
              {{
                a.lastTestResult === "ok"
                  ? "Healthy"
                  : a.lastTestResult
                    ? "Error"
                    : "Untested"
              }}
            </span>
          </header>

          <dl class="mc-grid">
            <div>
              <dt>Host</dt>
              <dd class="mono">{{ shortHost(a.baseURL) }}</dd>
            </div>
            <div>
              <dt>API key</dt>
              <dd class="mono">{{ obscure(a.apiKey) }}</dd>
            </div>
          </dl>

          <footer class="mc-actions">
            <button
              class="chip"
              @click="testAdapter(i)"
              :disabled="testingIndex === i"
            >
              {{ testingIndex === i ? "Testing..." : "Test" }}
            </button>
            <button class="chip" @click="openEditor(i)">Edit</button>
            <button class="chip" @click="duplicate(i)">Duplicate</button>
            <span class="tab-spacer" />
            <button class="chip chip-danger" @click="removeAdapter(i)">
              Delete
            </button>
          </footer>
        </article>
      </div>
    </main>

    <!-- ============ STATS ============ -->
    <main v-else-if="currentView === 'stats'" class="page">
      <div class="overview-section-head">
        <h3>Token usage</h3>
        <span class="row-desc" style="margin: 0"
          >Aggregated from on-disk conversation history.</span
        >
      </div>

      <div class="stat-cards">
        <div class="stat-card">
          <div class="stat-label">Total tokens</div>
          <div class="stat-value">{{ formatNum(stats.totalTokens) }}</div>
          <div class="stat-sub">{{ stats.totalTokens.toLocaleString() }}</div>
        </div>
        <div class="stat-card">
          <div class="stat-label">Prompt</div>
          <div class="stat-value">{{ formatNum(stats.totalPromptTokens) }}</div>
          <div class="stat-sub">input</div>
        </div>
        <div class="stat-card">
          <div class="stat-label">Completion</div>
          <div class="stat-value">
            {{ formatNum(stats.totalCompletionTokens) }}
          </div>
          <div class="stat-sub">output</div>
        </div>
        <div class="stat-card">
          <div class="stat-label">Conversations</div>
          <div class="stat-value">{{ stats.conversationCount }}</div>
          <div class="stat-sub">{{ stats.turnCount }} turns</div>
        </div>
      </div>

      <section class="card form-section" style="margin-top: 16px">
        <div class="section-head">
          <h3>Last 7 days</h3>
          <p>Prompt vs completion tokens per day (local time).</p>
        </div>
        <div
          v-if="maxDailyTotal === 0"
          class="empty-mini"
          style="margin: 8px 0"
        >
          No usage recorded in the last week.
        </div>
        <div v-else class="chart7">
          <div v-for="d in stats.last7Days" :key="d.date" class="chart-col">
            <div class="chart-bar-wrap">
              <div
                class="chart-bar chart-bar-prompt"
                :style="{
                  height:
                    (maxDailyTotal
                      ? (d.promptTokens / maxDailyTotal) * 100
                      : 0) + '%',
                }"
                :title="'Prompt ' + d.promptTokens.toLocaleString()"
              ></div>
              <div
                class="chart-bar chart-bar-completion"
                :style="{
                  height:
                    (maxDailyTotal
                      ? (d.completionTokens / maxDailyTotal) * 100
                      : 0) + '%',
                }"
                :title="'Completion ' + d.completionTokens.toLocaleString()"
              ></div>
            </div>
            <div class="chart-label">{{ shortDate(d.date) }}</div>
            <div class="chart-sub">
              {{ formatNum(d.promptTokens + d.completionTokens) }}
            </div>
          </div>
        </div>
        <div class="chart-legend">
          <span class="leg-dot leg-prompt"></span> Prompt
          <span class="leg-dot leg-completion" style="margin-left: 14px"></span>
          Completion
        </div>
      </section>

      <section class="card form-section" style="margin-top: 16px">
        <div class="section-head">
          <h3>Per model</h3>
          <p>Sorted by total tokens. One row per model+provider pair.</p>
        </div>
        <div
          v-if="!stats.perModel.length"
          class="empty-mini"
          style="margin: 8px 0"
        >
          No turns recorded yet.
        </div>
        <table v-else class="stats-table">
          <thead>
            <tr>
              <th>Model</th>
              <th>Provider</th>
              <th class="num">Prompt</th>
              <th class="num">Completion</th>
              <th class="num">Total</th>
              <th class="num">Turns</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(m, i) in stats.perModel" :key="i">
              <td class="mono">{{ m.model || "—" }}</td>
              <td>{{ m.provider || "—" }}</td>
              <td class="num mono">{{ m.promptTokens.toLocaleString() }}</td>
              <td class="num mono">
                {{ m.completionTokens.toLocaleString() }}
              </td>
              <td class="num mono">
                <b>{{
                  (m.promptTokens + m.completionTokens).toLocaleString()
                }}</b>
              </td>
              <td class="num mono">{{ m.turnCount }}</td>
            </tr>
          </tbody>
        </table>
      </section>
    </main>

    <!-- ============ EDITOR ============ -->
    <main v-else-if="currentView === 'editor' && editorModel" class="page">
      <div class="editor-head">
        <div>
          <button class="link-btn" @click="cancelEditor">← Models</button>
          <h2 class="editor-title">
            {{ editorIndex === -1 ? "New model" : "Edit model" }}
            <span class="editor-sub">{{
              editorModel.displayName ? "— " + editorModel.displayName : ""
            }}</span>
          </h2>
        </div>
        <div class="row-actions">
          <button class="btn btn-ghost" @click="cancelEditor">Cancel</button>
          <button class="btn btn-ghost" @click="saveEditor(true)">
            Save and test
          </button>
          <button class="btn btn-primary" @click="saveEditor(false)">
            Save
          </button>
        </div>
      </div>

      <div class="provider-bar slim">
        <button
          :class="['prov', editorProvider === 'openai' ? 'prov-openai' : '']"
          @click="editorProvider = 'openai'"
        >
          <OpenAIMark class="prov-logo" /> OpenAI
        </button>
        <button
          :class="[
            'prov',
            editorProvider === 'anthropic' ? 'prov-anthropic' : '',
          ]"
          @click="editorProvider = 'anthropic'"
        >
          <AnthropicMark class="prov-logo" /> Anthropic
        </button>
      </div>

      <section class="card form-section">
        <div class="section-head">
          <h3>Identity</h3>
          <p>Shown in the picker and mapped to the upstream provider.</p>
        </div>
        <div class="form-grid">
          <div class="field">
            <label
              >Display name
              <span class="info" :data-tip="HELP.displayName">i</span></label
            >
            <input
              v-model="editorModel.displayName"
              placeholder="GPT-4o (work)"
            />
          </div>
          <div class="field">
            <label
              >Model ID
              <span class="info" :data-tip="HELP.modelID">i</span></label
            >
            <input
              v-model="editorModel.modelID"
              :placeholder="
                editorProvider === 'openai' ? 'gpt-4o' : 'claude-sonnet-4-5'
              "
            />
          </div>
        </div>
      </section>

      <section class="card form-section">
        <div class="section-head">
          <h3>Endpoint & credentials</h3>
          <p>Never leaves this machine — stored in <code>config.json</code>.</p>
        </div>
        <div class="form-grid">
          <div class="field">
            <label
              >API key
              <span class="info" :data-tip="HELP.apiKey">i</span></label
            >
            <div class="input-with-action">
              <input
                :type="showApiKey ? 'text' : 'password'"
                v-model="editorModel.apiKey"
                placeholder="sk-…"
              />
              <button
                class="eye"
                @click="showApiKey = !showApiKey"
                type="button"
                :title="showApiKey ? 'Hide key' : 'Show key'"
              >
                <svg
                  v-if="showApiKey"
                  viewBox="0 0 24 24"
                  width="14"
                  height="14"
                  fill="none"
                  stroke="currentColor"
                  stroke-width="1.8"
                >
                  <path
                    d="M3 3l18 18M10.58 10.58a2 2 0 0 0 2.83 2.83M9.36 5.64A10.94 10.94 0 0 1 12 5c7 0 11 7 11 7a17.07 17.07 0 0 1-3.3 4.38M6.1 6.1C3.4 7.8 1 12 1 12s4 7 11 7a10.78 10.78 0 0 0 5-1.23"
                  />
                </svg>
                <svg
                  v-else
                  viewBox="0 0 24 24"
                  width="14"
                  height="14"
                  fill="none"
                  stroke="currentColor"
                  stroke-width="1.8"
                >
                  <path d="M1 12s4-7 11-7 11 7 11 7-4 7-11 7S1 12 1 12z" />
                  <circle cx="12" cy="12" r="3" />
                </svg>
              </button>
            </div>
          </div>
          <div class="field">
            <label
              >Base URL
              <span class="info" :data-tip="HELP.baseURL">i</span></label
            >
            <input
              v-model="editorModel.baseURL"
              :placeholder="
                editorProvider === 'openai'
                  ? 'https://api.openai.com/v1'
                  : 'https://api.anthropic.com'
              "
            />
          </div>
        </div>
      </section>

      <section class="card form-section">
        <div class="section-head">
          <h3>Advanced</h3>
          <p>All optional — leave blank to use provider defaults.</p>
        </div>
        <div class="form-grid">
          <div class="field">
            <label
              >Context window
              <span class="info" :data-tip="HELP.contextWindow">i</span></label
            >
            <input v-model="editorModel.contextWindow" placeholder="200000" />
          </div>
          <div v-if="editorProvider === 'openai'" class="field">
            <label
              >Reasoning effort
              <span class="info" :data-tip="HELP.reasoningEffort"
                >i</span
              ></label
            >
            <select v-model="editorModel.reasoningEffort">
              <option value="none">None</option>
              <option value="low">Low</option>
              <option value="medium">Medium</option>
              <option value="high">High</option>
              <option value="xhigh">XHigh</option>
            </select>
            <label class="fast-toggle">
              <input
                type="checkbox"
                :checked="editorModel.serviceTier === 'priority'"
                @change="
                  editorModel.serviceTier = ($event.target as HTMLInputElement)
                    .checked
                    ? 'priority'
                    : ''
                "
              />
              Fast mode <span class="info" :data-tip="HELP.fastMode">i</span>
            </label>
          </div>
          <div class="field">
            <label
              >Max output tokens
              <span class="info" :data-tip="HELP.maxOutput">i</span></label
            >
            <input v-model="editorModel.maxOutputTokens" placeholder="65536" />
          </div>
          <div v-if="editorProvider === 'anthropic'" class="field">
            <label
              >Thinking budget
              <span class="info" :data-tip="HELP.thinkingBudget">i</span></label
            >
            <input v-model="editorModel.thinkingBudget" placeholder="16000" />
          </div>
          <div class="field field-full">
            <label
              >Notes <span class="info" :data-tip="HELP.notes">i</span></label
            >
            <textarea
              v-model="editorModel.notes"
              rows="3"
              placeholder="Optional notes — only visible to you."
            />
          </div>
        </div>
      </section>

      <section class="card form-section test-section">
        <div class="section-head">
          <h3>Test result</h3>
          <p>Last probe against this adapter.</p>
        </div>
        <div v-if="!editorModel.lastTestResult" class="test-state test-none">
          <span class="mc-status-dot" /> No test run yet — use
          <b>Save and test</b>.
        </div>
        <div
          v-else-if="editorModel.lastTestResult === 'ok'"
          class="test-state test-ok"
        >
          <span class="mc-status-dot" /> Healthy — adapter responded
          successfully.
        </div>
        <div v-else class="test-state test-err">
          <span class="mc-status-dot" /> {{ editorModel.lastTestResult }}
        </div>
      </section>
    </main>
  </div>
</template>

<style scoped>
.shell {
  min-height: 100vh;
  display: flex;
  flex-direction: column;
  color: #e4e4e7;
}

/* ============ TOPBAR ============ */
.topbar {
  position: sticky;
  top: 0;
  z-index: 10;
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 10px 12px 10px 18px;
  background: rgba(9, 9, 11, 0.85);
  backdrop-filter: blur(10px);
  border-bottom: 1px solid #1f1f22;
  --wails-draggable: drag;
}
.topbar button,
.topbar .status-pill,
.topbar input,
.topbar .win-controls {
  --wails-draggable: no-drag;
}

/* -------- Window controls -------- */
.win-controls {
  display: inline-flex;
  margin-left: 4px;
}
.win-btn {
  background: transparent;
  border: 0;
  color: #71717a;
  width: 34px;
  height: 28px;
  border-radius: 6px;
  display: grid;
  place-items: center;
  cursor: pointer;
  font-family: inherit;
  transition:
    background 0.12s,
    color 0.12s;
}
.win-btn:hover {
  background: #18181b;
  color: #fafafa;
}
.win-close:hover {
  background: #7f1d1d;
  color: #fff;
}
.brand {
  display: flex;
  align-items: center;
  gap: 10px;
}
.logo-mark {
  width: 30px;
  height: 30px;
  display: grid;
  place-items: center;
  background: #0b0b0d;
  border: 1px solid #1f1f22;
  border-radius: 8px;
}
.brand-name {
  font-size: 14px;
  font-weight: 600;
  letter-spacing: -0.01em;
  color: #fafafa;
}
.brand-sub {
  font-size: 11px;
  color: #71717a;
  margin-top: 1px;
}

.topbar-right {
  display: flex;
  align-items: center;
  gap: 10px;
}
.status-pill {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  padding: 5px 10px;
  border-radius: 999px;
  font-size: 12px;
  font-weight: 500;
  border: 1px solid transparent;
}
.status-pill .dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
}
.pill-on {
  color: #4ade80;
  background: rgba(34, 197, 94, 0.08);
  border-color: rgba(34, 197, 94, 0.25);
}
.pill-on .dot {
  background: #22c55e;
  box-shadow: 0 0 0 3px rgba(34, 197, 94, 0.25);
  animation: pulse 1.8s ease-in-out infinite;
}
.pill-off {
  color: #a1a1aa;
  background: #18181b;
  border-color: #27272a;
}
.pill-off .dot {
  background: #52525b;
}
@keyframes pulse {
  50% {
    box-shadow: 0 0 0 6px rgba(34, 197, 94, 0);
  }
}

/* ============ TABS ============ */
.tabs {
  display: flex;
  align-items: center;
  gap: 4px;
  padding: 10px 24px 0;
  border-bottom: 1px solid #1f1f22;
}
.tab {
  position: relative;
  background: transparent;
  border: 0;
  color: #a1a1aa;
  font-family: inherit;
  font-size: 13px;
  font-weight: 500;
  padding: 10px 14px;
  cursor: pointer;
  border-bottom: 2px solid transparent;
  margin-bottom: -1px;
}
.tab:hover {
  color: #fafafa;
}
.tab-active {
  color: #fafafa;
  border-bottom-color: #22c55e;
}
.tab-count {
  background: #27272a;
  color: #a1a1aa;
  font-size: 10px;
  padding: 1px 6px;
  border-radius: 999px;
  margin-left: 6px;
}
.tab-spacer {
  flex: 1;
}

/* ============ PAGE ============ */
.page {
  flex: 1;
  padding: 20px 24px 40px;
  max-width: 1040px;
  width: 100%;
  margin: 0 auto;
  box-sizing: border-box;
}

/* ============ STATS ============ */
/* ============ OVERVIEW MODELS ============ */
.overview-section-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 10px;
}
.overview-section-head h3 {
  font-size: 14px;
  font-weight: 600;
  color: #e4e4e7;
  margin: 0;
}
.btn-sm {
  padding: 5px 12px;
  font-size: 12px;
}
.empty-mini {
  color: #71717a;
  font-size: 13px;
  padding: 20px 0;
  text-align: center;
}
.overview-models {
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.ov-model {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px 14px;
  background: #101012;
  border: 1px solid #1f1f22;
  border-radius: 10px;
  cursor: pointer;
  transition: border-color 0.15s;
}
.ov-model:hover {
  border-color: #3f3f46;
}
.ov-logo {
  width: 20px;
  height: 20px;
  flex-shrink: 0;
  opacity: 0.7;
}
.ov-info {
  flex: 1;
  min-width: 0;
}
.ov-name {
  font-size: 13px;
  font-weight: 500;
  color: #fafafa;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.ov-id {
  font-size: 11px;
  color: #71717a;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.sep {
  color: #3f3f46;
  margin: 0 2px;
}
.mono {
  font-family: "JetBrains Mono", Consolas, monospace;
}

.chip-btn {
  background: #18181b;
  border: 1px solid #27272a;
  color: #e4e4e7;
  padding: 6px 12px;
  border-radius: 8px;
  font-size: 12px;
  cursor: pointer;
  font-family: inherit;
}
.chip-btn:hover {
  background: #222225;
}

/* ============ CARD / ROW ============ */
.card {
  background: #101012;
  border: 1px solid #1f1f22;
  border-radius: 12px;
  padding: 18px 20px;
  margin-bottom: 14px;
}
.row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 18px;
}
.row-text {
  flex: 1;
  min-width: 0;
}
.row-title {
  color: #fafafa;
  font-size: 14px;
  font-weight: 500;
}
.row-desc {
  color: #a1a1aa;
  font-size: 12.5px;
  margin-top: 3px;
  line-height: 1.5;
}
.row-tag {
  display: inline-block;
  margin-top: 6px;
  padding: 2px 8px;
  font-size: 11px;
  border-radius: 999px;
}
.tag-info {
  color: #60a5fa;
  background: rgba(96, 165, 250, 0.1);
  border: 1px solid rgba(96, 165, 250, 0.25);
}
.tag-warn {
  color: #f59e0b;
  background: rgba(245, 158, 11, 0.1);
  border: 1px solid rgba(245, 158, 11, 0.25);
}
.row-chip {
  display: inline-block;
  margin-left: 8px;
  padding: 2px 8px;
  font-size: 10px;
  font-weight: 500;
  letter-spacing: 0.03em;
  border-radius: 999px;
  vertical-align: middle;
  text-transform: uppercase;
}
.chip-ok {
  color: #4ade80;
  background: rgba(34, 197, 94, 0.1);
  border: 1px solid rgba(34, 197, 94, 0.3);
}
.chip-warn {
  color: #f59e0b;
  background: rgba(245, 158, 11, 0.1);
  border: 1px solid rgba(245, 158, 11, 0.3);
}
.tweak-keys {
  display: flex;
  flex-wrap: wrap;
  gap: 6px 12px;
  margin-top: 8px;
  font-size: 11px;
  font-family: "JetBrains Mono", Consolas, monospace;
}
.k-on {
  color: #4ade80;
}
.k-off {
  color: #71717a;
}
.row-path {
  display: block;
  color: #52525b;
  font-size: 11px;
  margin-top: 4px;
  font-family: "JetBrains Mono", Consolas, monospace;
}
.row-actions {
  display: flex;
  gap: 8px;
  flex-shrink: 0;
}
.hr {
  height: 1px;
  background: #1f1f22;
  margin: 16px 0;
}

.error-banner {
  background: rgba(239, 68, 68, 0.08);
  border: 1px solid rgba(239, 68, 68, 0.3);
  color: #fca5a5;
  padding: 10px 14px;
  border-radius: 8px;
  font-size: 13px;
  margin-bottom: 14px;
}

.footer {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 16px 4px 0;
  color: #52525b;
  font-size: 11px;
}
.footer-spacer {
  flex: 1;
}

/* ============ BUTTONS ============ */
.btn {
  border: 1px solid #27272a;
  background: #18181b;
  color: #fafafa;
  padding: 7px 14px;
  border-radius: 8px;
  font-size: 13px;
  font-weight: 500;
  cursor: pointer;
  font-family: inherit;
  transition:
    background 0.12s,
    border-color 0.12s;
}
.btn:disabled {
  opacity: 0.45;
  cursor: not-allowed;
}
.btn:hover:not(:disabled) {
  background: #222225;
}
.btn-primary {
  background: #22c55e;
  border-color: #22c55e;
  color: #052e16;
}
.btn-primary:hover:not(:disabled) {
  background: #16a34a;
  border-color: #16a34a;
}
.btn-ghost {
  background: transparent;
}
.btn-ghost:hover:not(:disabled) {
  background: #18181b;
}

.link-btn {
  background: transparent;
  border: 0;
  color: #a1a1aa;
  font-size: 13px;
  cursor: pointer;
  padding: 4px 8px;
  border-radius: 6px;
  font-family: inherit;
}
.link-btn:hover {
  color: #fafafa;
  background: #18181b;
}
.link-btn:disabled {
  opacity: 0.55;
  cursor: default;
  background: transparent;
  color: #71717a;
}
.icn {
  display: inline-block;
  margin-right: 2px;
}

/* ============ SWITCH ============ */
.switch {
  position: relative;
  display: inline-block;
  width: 40px;
  height: 22px;
  flex-shrink: 0;
}
.switch input {
  opacity: 0;
  width: 0;
  height: 0;
}
.slider {
  position: absolute;
  cursor: pointer;
  inset: 0;
  background: #27272a;
  border-radius: 999px;
  transition: 0.2s;
}
.slider::before {
  content: "";
  position: absolute;
  height: 16px;
  width: 16px;
  left: 3px;
  top: 3px;
  background: #fafafa;
  border-radius: 50%;
  transition: 0.2s;
}
.switch input:checked + .slider {
  background: #22c55e;
}
.switch input:checked + .slider::before {
  transform: translateX(18px);
}

/* ============ INFO TOOLTIP DOT ============ */
.info {
  position: relative;
  display: inline-grid;
  place-items: center;
  width: 14px;
  height: 14px;
  border-radius: 50%;
  background: #27272a;
  color: #a1a1aa;
  font-size: 9px;
  font-weight: 600;
  font-style: italic;
  font-family: "Georgia", serif;
  cursor: help;
  user-select: none;
  border: 1px solid transparent;
}
.info:hover {
  background: #3f3f46;
  color: #fafafa;
  border-color: #52525b;
}

.info::after {
  content: attr(data-tip);
  position: absolute;
  bottom: calc(100% + 8px);
  left: 50%;
  transform: translateX(-50%) translateY(4px);
  width: max-content;
  max-width: 250px;
  padding: 8px 11px;
  background: #0a0a0c;
  color: #e4e4e7;
  border: 1px solid #27272a;
  border-radius: 7px;
  font-family:
    "Inter",
    -apple-system,
    BlinkMacSystemFont,
    "Segoe UI",
    system-ui,
    sans-serif;
  font-size: 12px;
  font-style: normal;
  font-weight: 400;
  line-height: 1.5;
  text-align: left;
  white-space: normal;
  letter-spacing: 0.005em;
  opacity: 0;
  pointer-events: none;
  box-shadow:
    0 12px 32px rgba(0, 0, 0, 0.7),
    0 0 0 1px rgba(255, 255, 255, 0.03) inset;
  transition:
    opacity 0.14s ease,
    transform 0.14s ease;
  z-index: 200;
}
.info:hover::after {
  opacity: 1;
  transform: translateX(-50%) translateY(0);
}

/* ============ PROVIDER BAR ============ */
.provider-bar {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 16px;
}
.provider-bar.slim {
  margin-bottom: 18px;
}
.prov {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  background: #101012;
  border: 1px solid #1f1f22;
  color: #a1a1aa;
  padding: 7px 14px;
  border-radius: 8px;
  font-size: 13px;
  font-weight: 500;
  cursor: pointer;
  font-family: inherit;
}
.prov:hover {
  color: #fafafa;
}
.prov .prov-logo {
  width: 14px;
  height: 14px;
  color: currentColor;
}
.prov-count {
  font-size: 10px;
  padding: 1px 6px;
  border-radius: 999px;
  background: #27272a;
  color: #a1a1aa;
}
.prov-openai {
  color: #4ade80;
  background: rgba(34, 197, 94, 0.08);
  border-color: rgba(34, 197, 94, 0.35);
}
.prov-openai .prov-count {
  background: rgba(34, 197, 94, 0.18);
  color: #86efac;
}
.prov-anthropic {
  color: #d97757;
  background: rgba(217, 119, 87, 0.08);
  border-color: rgba(217, 119, 87, 0.35);
}
.prov-anthropic .prov-count {
  background: rgba(217, 119, 87, 0.18);
  color: #fdba74;
}

/* ============ MODEL CARDS ============ */
.empty {
  display: flex;
  flex-direction: column;
  align-items: center;
  padding: 48px 24px;
  background: #101012;
  border: 1px dashed #27272a;
  border-radius: 12px;
  text-align: center;
}
.empty-icon {
  width: 42px;
  height: 42px;
  display: grid;
  place-items: center;
  background: #18181b;
  border-radius: 10px;
  color: #52525b;
  margin-bottom: 12px;
}
.empty-icon :deep(svg) {
  width: 22px;
  height: 22px;
}
.empty-title {
  color: #fafafa;
  font-size: 15px;
  font-weight: 500;
}
.empty-desc {
  color: #71717a;
  font-size: 13px;
  margin: 4px 0 18px;
}

.model-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(320px, 1fr));
  gap: 12px;
}
.model-card {
  background: #101012;
  border: 1px solid #1f1f22;
  border-radius: 12px;
  padding: 14px 16px 12px;
}
.mc-head {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  margin-bottom: 12px;
  gap: 10px;
}
.mc-title {
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 0;
}
.mc-logo {
  width: 22px;
  height: 22px;
  flex-shrink: 0;
  color: #a1a1aa;
}
.mc-name {
  color: #fafafa;
  font-size: 14px;
  font-weight: 500;
}
.mc-id {
  color: #71717a;
  font-size: 11px;
  margin-top: 2px;
}
.mc-status {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: 11px;
  padding: 3px 8px;
  border-radius: 999px;
  flex-shrink: 0;
}
.mc-status-dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: currentColor;
}
.ms-ok {
  color: #4ade80;
  background: rgba(34, 197, 94, 0.1);
}
.ms-err {
  color: #f87171;
  background: rgba(239, 68, 68, 0.1);
}
.ms-none {
  color: #71717a;
  background: #18181b;
}

.mc-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 8px;
  margin: 0 0 12px;
}
.mc-grid > div {
  background: #0a0a0c;
  border: 1px solid #1f1f22;
  border-radius: 8px;
  padding: 6px 10px;
  min-width: 0;
}
.mc-grid dt {
  font-size: 10px;
  color: #71717a;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  margin-bottom: 2px;
}
.mc-grid dd {
  margin: 0;
  color: #fafafa;
  font-size: 12.5px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.mc-actions {
  display: flex;
  gap: 4px;
  align-items: center;
  padding-top: 4px;
  border-top: 1px solid #1f1f22;
  margin-top: 4px;
}
.chip {
  background: transparent;
  border: 0;
  color: #a1a1aa;
  font-size: 12px;
  cursor: pointer;
  padding: 6px 10px;
  border-radius: 6px;
  font-family: inherit;
}
.chip:hover {
  color: #fafafa;
  background: #18181b;
}
.chip-danger {
  color: #f87171;
}
.chip-danger:hover {
  color: #fca5a5;
  background: rgba(239, 68, 68, 0.08);
}

/* ============ EDITOR ============ */
.editor-head {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  gap: 16px;
  margin-bottom: 16px;
}
.editor-title {
  margin: 4px 0 0;
  font-size: 20px;
  font-weight: 500;
  color: #fafafa;
  letter-spacing: -0.01em;
}
.editor-sub {
  color: #71717a;
  font-size: 14px;
  font-weight: 400;
  margin-left: 4px;
}

.form-section {
  padding: 18px 20px;
}
.section-head {
  margin-bottom: 14px;
}
.section-head h3 {
  margin: 0;
  font-size: 12px;
  color: #a1a1aa;
  text-transform: uppercase;
  letter-spacing: 0.08em;
  font-weight: 600;
}
.section-head p {
  margin: 4px 0 0;
  font-size: 12px;
  color: #71717a;
}
.section-head code {
  font-family: "JetBrains Mono", Consolas, monospace;
  color: #a1a1aa;
  font-size: 11px;
}

.form-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 14px 18px;
}
.field {
  display: flex;
  flex-direction: column;
  gap: 6px;
  min-width: 0;
}
.field-full {
  grid-column: span 2;
}
.fast-toggle {
  display: flex;
  align-items: center;
  gap: 6px;
  margin-top: 4px;
  cursor: pointer;
  font-size: 13px;
  color: var(--fg2);
}
.fast-toggle input[type="checkbox"] {
  width: 16px;
  height: 16px;
  accent-color: var(--brand);
  margin: 0;
}
.field label {
  font-size: 12px;
  color: #d4d4d8;
  display: flex;
  align-items: center;
  gap: 6px;
}
input,
select,
textarea {
  background: #0a0a0c;
  border: 1px solid #27272a;
  color: #fafafa;
  border-radius: 8px;
  padding: 8px 11px;
  font-size: 13px;
  font-family: inherit;
  width: 100%;
  box-sizing: border-box;
}
textarea {
  resize: vertical;
  font-family: inherit;
}
input:focus,
select:focus,
textarea:focus {
  outline: none;
  border-color: #3f3f46;
  background: #0f0f11;
}
.input-with-action {
  position: relative;
}
.input-with-action input {
  padding-right: 36px;
}
.eye {
  position: absolute;
  right: 4px;
  top: 50%;
  transform: translateY(-50%);
  background: transparent;
  border: 0;
  color: #a1a1aa;
  cursor: pointer;
  padding: 6px;
  display: grid;
  place-items: center;
}
.eye:hover {
  color: #fafafa;
}

.test-section .test-state {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 10px 12px;
  border-radius: 8px;
  font-size: 13px;
}
.test-none {
  background: #0a0a0c;
  color: #71717a;
  border: 1px solid #1f1f22;
}
.test-ok {
  background: rgba(34, 197, 94, 0.08);
  color: #4ade80;
  border: 1px solid rgba(34, 197, 94, 0.25);
}
.test-err {
  background: rgba(239, 68, 68, 0.08);
  color: #f87171;
  border: 1px solid rgba(239, 68, 68, 0.25);
}
.test-section .mc-status-dot {
  width: 8px;
  height: 8px;
}

/* ============ STATS PANEL ============ */
.stat-cards {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: 12px;
}
.stat-card {
  background: #0d0d0f;
  border: 1px solid #1f1f22;
  border-radius: 10px;
  padding: 14px 16px;
}
.stat-label {
  font-size: 11px;
  text-transform: uppercase;
  letter-spacing: 0.08em;
  color: #71717a;
  margin-bottom: 8px;
}
.stat-value {
  font-size: 24px;
  font-weight: 600;
  color: #fafafa;
  line-height: 1;
}
.stat-sub {
  margin-top: 6px;
  font-size: 12px;
  color: #a1a1aa;
}

.chart7 {
  display: grid;
  grid-template-columns: repeat(7, 1fr);
  gap: 10px;
  align-items: end;
  height: 180px;
  margin-top: 4px;
}
.chart-col {
  display: flex;
  flex-direction: column;
  align-items: center;
  height: 100%;
}
.chart-bar-wrap {
  flex: 1;
  width: 100%;
  display: flex;
  align-items: flex-end;
  justify-content: center;
  gap: 4px;
  min-height: 2px;
}
.chart-bar {
  width: 14px;
  min-height: 2px;
  border-radius: 3px 3px 0 0;
  transition: height 0.2s;
}
.chart-bar-prompt {
  background: #22c55e;
}
.chart-bar-completion {
  background: #60a5fa;
}
.chart-label {
  margin-top: 8px;
  font-size: 11px;
  color: #a1a1aa;
}
.chart-sub {
  font-size: 10px;
  color: #52525b;
  margin-top: 2px;
}
.chart-legend {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 12px;
  color: #a1a1aa;
  margin-top: 12px;
  padding-top: 12px;
  border-top: 1px solid #1f1f22;
}
.leg-dot {
  display: inline-block;
  width: 10px;
  height: 10px;
  border-radius: 2px;
  margin-right: 4px;
}
.leg-prompt {
  background: #22c55e;
}
.leg-completion {
  background: #60a5fa;
}

.stats-table {
  width: 100%;
  border-collapse: collapse;
  font-size: 13px;
}
.stats-table th {
  text-align: left;
  font-size: 11px;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: #71717a;
  padding: 8px 12px;
  border-bottom: 1px solid #1f1f22;
  font-weight: 500;
}
.stats-table td {
  padding: 10px 12px;
  border-bottom: 1px solid #141416;
  color: #d4d4d8;
}
.stats-table tr:last-child td {
  border-bottom: 0;
}
.stats-table .num {
  text-align: right;
}
.stats-table th.num {
  text-align: right;
}
</style>
