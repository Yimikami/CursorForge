package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"cursorforge/internal/agent"
	"cursorforge/internal/certs"
	"cursorforge/internal/cursor"
	"cursorforge/internal/mitm"
	"cursorforge/internal/relay"
)

const (
	defaultListenAddr = "127.0.0.1:18080"
	defaultUpstream   = "https://api2.cursor.sh"
)

type ProxyService struct {
	mu              sync.RWMutex
	state           ProxyState
	proxy           *mitm.Server
	ca              *certs.CA
	gateway         *relay.Gateway
	cfgDir          string
	onStateChange   func(running bool)
	gatewayAdapters func() []relay.AdapterInfo
}

// adapterListFor reads the user config and returns the BYOK adapter list
// suitable for relay rewrites and SQLite pinning.
func adapterListFor(cfgDir string) []relay.AdapterInfo {
	c, err := readConfig(cfgDir)
	if err != nil {
		return nil
	}
	out := make([]relay.AdapterInfo, 0, len(c.ModelAdapters))
	for _, a := range c.ModelAdapters {
		if a.ModelID == "" {
			continue
		}
		out = append(out, relay.AdapterInfo{
			DisplayName:     a.DisplayName,
			Type:            a.Type,
			ModelID:         a.ModelID,
			BaseURL:         a.BaseURL,
			APIKey:          a.APIKey,
			ReasoningEffort: a.ReasoningEffort,
			ServiceTier:     a.ServiceTier,
			MaxOutputTokens: parseIntSafe(a.MaxOutputTokens),
			ThinkingBudget:  parseIntSafe(a.ThinkingBudget),
		})
	}
	return out
}

func (s *ProxyService) SetStateCallback(cb func(running bool)) {
	s.mu.Lock()
	s.onStateChange = cb
	s.mu.Unlock()
}

func (s *ProxyService) fireState() {
	cb := s.onStateChange
	running := s.state.Running
	if cb != nil {
		cb(running)
	}
}

func NewProxyService() (*ProxyService, error) {
	dir, err := cfgDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	ca, err := certs.LoadOrCreate(filepath.Join(dir, "ca"))
	if err != nil {
		return nil, err
	}
	gw := relay.NewGateway()
	// Persist chat turns under %APPDATA%/CursorForge/history so conversations
	// survive restart. The agent package walks this on first use and loads
	// every <conv_id>.jsonl back into its in-memory session history.
	agent.InitHistoryDir(filepath.Join(dir, "history"))
	cfg, _ := readConfig(dir)
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultUpstream
	}
	svc := &ProxyService{
		cfgDir:  dir,
		ca:      ca,
		gateway: gw,
		state: ProxyState{
			ListenAddr:    defaultListenAddr,
			BaseURL:       baseURL,
			CAFingerprint: ca.Fingerprint(),
			CAPath:        filepath.Join(ca.Dir(), "ca.crt"),
			CAInstalled:   certs.IsInstalledUserRoot(),
		},
	}
	svc.gatewayAdapters = func() []relay.AdapterInfo { return adapterListFor(svc.cfgDir) }
	gw.SetAdapterProvider(func() []relay.AdapterInfo {
		c, err := readConfig(svc.cfgDir)
		if err != nil {
			return nil
		}
		out := make([]relay.AdapterInfo, 0, len(c.ModelAdapters))
		for _, a := range c.ModelAdapters {
			if a.ModelID == "" {
				continue
			}
			out = append(out, relay.AdapterInfo{
				DisplayName:     a.DisplayName,
				Type:            a.Type,
				ModelID:         a.ModelID,
				BaseURL:         a.BaseURL,
				APIKey:          a.APIKey,
				ReasoningEffort: a.ReasoningEffort,
				MaxOutputTokens: parseIntSafe(a.MaxOutputTokens),
				ThinkingBudget:  parseIntSafe(a.ThinkingBudget),
			})
		}
		return out
	})
	return svc, nil
}

func (s *ProxyService) GetState() ProxyState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

func (s *ProxyService) InstallCA() (ProxyState, error) {
	if err := certs.InstallUserRoot(filepath.Join(s.ca.Dir(), "ca.crt")); err != nil {
		s.mu.Lock()
		s.state.LastError = err.Error()
		s.mu.Unlock()
		return s.GetState(), err
	}
	s.mu.Lock()
	s.state.CAInstalled = true
	s.state.LastError = ""
	s.mu.Unlock()
	return s.GetState(), nil
}

func (s *ProxyService) UninstallCA() (ProxyState, error) {
	if err := certs.UninstallUserRoot(); err != nil {
		s.mu.Lock()
		s.state.LastError = err.Error()
		s.mu.Unlock()
		return s.GetState(), err
	}
	s.mu.Lock()
	s.state.CAInstalled = false
	s.state.LastError = ""
	s.mu.Unlock()
	return s.GetState(), nil
}

func (s *ProxyService) StartProxy() (ProxyState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Running {
		return s.state, nil
	}
	_, _ = readConfig(s.cfgDir)
	s.state.ListenAddr = defaultListenAddr
	listen := defaultListenAddr
	resolver := func() (providerType, baseURL, apiKey, model string, opts agent.AdapterOpts, ok bool) {
		ads := s.gatewayAdapters()
		if len(ads) == 0 {
			return "", "", "", "", agent.AdapterOpts{}, false
		}
		a := ads[0]
		return a.Type, a.BaseURL, a.APIKey, a.ModelID, agent.AdapterOpts{
			ReasoningEffort: a.ReasoningEffort,
			ServiceTier:     a.ServiceTier,
			MaxOutputTokens: a.MaxOutputTokens,
			ThinkingBudget:  a.ThinkingBudget,
		}, true
	}
	srv, err := mitm.New(listen, s.ca, s.gateway, resolver)
	if err != nil {
		s.state.LastError = err.Error()
		return s.state, err
	}
	if err := srv.Start(); err != nil {
		s.state.LastError = err.Error()
		return s.state, err
	}
	if err := cursor.EnableSystemProxy(s.state.ListenAddr); err != nil {
		_ = srv.Stop(context.Background())
		s.state.LastError = "proxy started but couldn't update system settings: " + err.Error()
		return s.state, err
	}
	if err := cursor.ApplyCursorTweaks(s.state.ListenAddr); err != nil {
		// Non-fatal — system proxy alone might be enough; just record.
		s.state.LastError = "Cursor settings.json tweak failed: " + err.Error()
	}
	// Inject the synthetic Pro session into Cursor's SQLite. Cursor reads
	// these auth keys at startup and renders "logged-in Pro user" UI when
	// they look valid; combined with the auth-endpoint MITM mocks this
	// unlocks BYOK on the chat picker without a real cursor.com account.
	// The previous values are saved to backupPath so Stop can restore the
	// user's real Cursor account — without this the user gets stuck with
	// our fake JWT after disabling the proxy and Cursor refuses to log in
	// against the real api2.cursor.sh.
	if err := cursor.InjectFakeProUser(s.authBackupPath()); err != nil {
		s.state.LastError = "Cursor SQLite Pro inject failed: " + err.Error()
	}
	if adapters := s.gatewayAdapters(); len(adapters) > 0 {
		// Pin every Cursor feature (composer / cmd-K / agent / etc.) at the
		// first BYOK adapter. Without this Cursor's picker reads a stale
		// selection from SQLite and renders nothing because the cached id
		// no longer matches our AvailableModels response.
		if err := cursor.ForceModelSelection(adapters[0].StableID()); err != nil {
			s.state.LastError = "Cursor SQLite model pin failed: " + err.Error()
		}
	}
	s.proxy = srv
	s.state.Running = true
	s.state.StartedAt = time.Now().Unix()
	s.state.LastError = ""
	s.fireState()
	return s.state, nil
}

func (s *ProxyService) StopProxy() (ProxyState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.state.Running {
		return s.state, nil
	}
	_ = cursor.DisableSystemProxy()
	_ = cursor.RevertCursorTweaks()
	// Put the user's original Cursor auth back so Cursor can log in against
	// the real api2.cursor.sh once it's no longer routed through us. Failure
	// here is non-fatal; we'd rather report stopped than block the UI.
	if err := cursor.RestoreFakeProUser(s.authBackupPath()); err != nil {
		s.state.LastError = "Cursor SQLite auth restore failed: " + err.Error()
	}
	if s.proxy != nil {
		_ = s.proxy.Stop(context.Background())
		s.proxy = nil
	}
	s.state.Running = false
	s.state.StartedAt = 0
	s.fireState()
	return s.state, nil
}

// authBackupPath returns the JSON sidecar where we stash the user's
// original cursorAuth/* values before injecting our fake Pro session.
func (s *ProxyService) authBackupPath() string {
	return filepath.Join(s.cfgDir, "cursor-auth-backup.json")
}

func (s *ProxyService) SetBaseURL(url string) (ProxyState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if url == "" {
		return s.state, errors.New("base URL cannot be empty")
	}
	s.state.BaseURL = url
	return s.state, nil
}

func (s *ProxyService) LoadUserConfig() (UserConfig, error) {
	return readConfig(s.cfgDir)
}

func (s *ProxyService) SaveUserConfig(cfg UserConfig) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.cfgDir, "config.json"), b, 0o600)
}

func (s *ProxyService) ExportCAPEM() string {
	return string(s.ca.CertPEM())
}

func (s *ProxyService) ConfigDir() string {
	return s.cfgDir
}

func (s *ProxyService) OpenSettingsFolder() error {
	path := s.cfgDir
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", path)
	case "darwin":
		cmd = exec.Command("open", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	return cmd.Start()
}

func (s *ProxyService) GetCursorSettingsStatus() CursorSettingsStatus {
	st := cursor.GetStatus()
	return CursorSettingsStatus{
		Path:            st.Path,
		Found:           st.Found,
		Error:           st.Error,
		ProxySet:        st.ProxySet,
		ProxyValue:      st.ProxyValue,
		StrictSSLOff:    st.StrictSSLOff,
		ProxySupportOn:  st.ProxySupportOn,
		SystemCertsV2On: st.SystemCertsV2On,
		UseHTTP1:        st.UseHTTP1,
		DisableHTTP2:    st.DisableHTTP2,
		ProxyKerberos:   st.ProxyKerberos,
	}
}

func (s *ProxyService) ApplyCursorTweaks() (CursorSettingsStatus, error) {
	if err := cursor.ApplyCursorTweaks(s.state.ListenAddr); err != nil {
		return s.GetCursorSettingsStatus(), err
	}
	return s.GetCursorSettingsStatus(), nil
}

func (s *ProxyService) RevertCursorTweaks() (CursorSettingsStatus, error) {
	if err := cursor.RevertCursorTweaks(); err != nil {
		return s.GetCursorSettingsStatus(), err
	}
	return s.GetCursorSettingsStatus(), nil
}

func (s *ProxyService) TestAdapter(index int) (ModelAdapterConfig, error) {
	cfg, err := readConfig(s.cfgDir)
	if err != nil {
		return ModelAdapterConfig{}, err
	}
	if index < 0 || index >= len(cfg.ModelAdapters) {
		return ModelAdapterConfig{}, errors.New("adapter index out of range")
	}
	a := cfg.ModelAdapters[index]

	if a.APIKey == "" {
		a.LastTestResult = "missing API key"
	} else if a.BaseURL == "" {
		a.LastTestResult = "missing base URL"
	} else {
		a.LastTestResult = runAdapterPing(a)
	}

	a.LastTestedAt = time.Now().Unix()
	cfg.ModelAdapters[index] = a
	_ = s.SaveUserConfig(cfg)
	return a, nil
}

func runAdapterPing(a ModelAdapterConfig) string {
	base := strings.TrimRight(a.BaseURL, "/")
	var url string
	req := (*http.Request)(nil)
	var err error
	switch strings.ToLower(a.Type) {
	case "anthropic":
		url = base + "/v1/models"
		req, err = http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return "bad URL: " + err.Error()
		}
		req.Header.Set("x-api-key", a.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	default:
		url = base + "/models"
		req, err = http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return "bad URL: " + err.Error()
		}
		req.Header.Set("Authorization", "Bearer "+a.APIKey)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "network error: " + trimErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return "ok"
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 140 {
		snippet = snippet[:140] + "…"
	}
	return fmt.Sprintf("http %d — %s", resp.StatusCode, snippet)
}

func trimErr(err error) string {
	msg := err.Error()
	if len(msg) > 160 {
		return msg[:160] + "…"
	}
	return msg
}

// GetUsageStats returns the aggregate token-use snapshot the Stats tab in the
// Wails UI renders. Computed on demand from disk; safe to call any time
// (even while the proxy is stopped — history persists across restarts).
func (s *ProxyService) GetUsageStats() UsageStats {
	snap := agent.ComputeUsageStats()
	out := UsageStats{
		TotalPromptTokens:     snap.TotalPromptTokens,
		TotalCompletionTokens: snap.TotalCompletionTokens,
		TotalTokens:           snap.TotalTokens,
		ConversationCount:     snap.ConversationCount,
		TurnCount:             snap.TurnCount,
		PerModel:              make([]ModelUsageEntry, 0, len(snap.PerModel)),
		Last7Days:             make([]DailyUsageEntry, 0, len(snap.Last7Days)),
	}
	for _, m := range snap.PerModel {
		out.PerModel = append(out.PerModel, ModelUsageEntry{
			Model:            m.Model,
			Provider:         m.Provider,
			PromptTokens:     m.PromptTokens,
			CompletionTokens: m.CompletionTokens,
			TurnCount:        m.TurnCount,
		})
	}
	for _, d := range snap.Last7Days {
		out.Last7Days = append(out.Last7Days, DailyUsageEntry{
			Date:             d.Date,
			PromptTokens:     d.PromptTokens,
			CompletionTokens: d.CompletionTokens,
		})
	}
	return out
}

func (s *ProxyService) Shutdown() {
	_, _ = s.StopProxy()
}

func readConfig(dir string) (UserConfig, error) {
	cfg := UserConfig{BaseURL: defaultUpstream}
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	_ = json.Unmarshal(b, &cfg)
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultUpstream
	}
	return cfg, nil
}

func parseIntSafe(s string) int {
	if s == "" {
		return 0
	}
	var n int
	for _, ch := range s {
		if ch >= '0' && ch <= '9' {
			n = n*10 + int(ch-'0')
		}
	}
	return n
}

func cfgDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "CursorForge"), nil
}
