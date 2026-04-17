package cursor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// Cursor (and VSCode) settings.json keys we set so that Cursor honours the
// local proxy and tolerates our self-signed CA. Stored on disk so we can
// restore the user's previous values on Stop.
type tweakBackup struct {
	HasHTTPProxy           bool `json:"hasHttpProxy"`
	HTTPProxy              any  `json:"httpProxy,omitempty"`
	HasHTTPProxyStrictSSL  bool `json:"hasHttpProxyStrictSsl"`
	HTTPProxyStrictSSL     any  `json:"httpProxyStrictSsl,omitempty"`
	HasHTTPProxySupport    bool `json:"hasHttpProxySupport"`
	HTTPProxySupport       any  `json:"httpProxySupport,omitempty"`
	HasHTTPProxyAuthHelper bool `json:"hasHttpProxyAuthHelper"`
	HTTPProxyAuthHelper    any  `json:"httpProxyAuthHelper,omitempty"`
	HasHTTPCompatHTTP1     bool `json:"hasHttpCompatHttp1"`
	HTTPCompatHTTP1        any  `json:"httpCompatHttp1,omitempty"`
	HasDisableHTTP2        bool `json:"hasDisableHttp2"`
	DisableHTTP2           any  `json:"disableHttp2,omitempty"`
	HasProxyKerberos       bool `json:"hasProxyKerberos"`
	ProxyKerberos          any  `json:"proxyKerberos,omitempty"`
}

var settingsMu sync.Mutex

// Status reports what we found in Cursor's settings.json so the UI can show
// the user exactly which keys are wired up.
type Status struct {
	Path            string `json:"path"`
	Found           bool   `json:"found"`
	Error           string `json:"error,omitempty"`
	ProxySet        bool   `json:"proxySet"`
	ProxyValue      string `json:"proxyValue,omitempty"`
	StrictSSLOff    bool   `json:"strictSSLOff"`
	ProxySupportOn  bool   `json:"proxySupportOn"`
	SystemCertsV2On bool   `json:"systemCertsV2On"`
	UseHTTP1        bool   `json:"useHttp1"`
	DisableHTTP2    bool   `json:"disableHttp2"`
	ProxyKerberos   bool   `json:"proxyKerberos"`
}

// GetStatus inspects settings.json and reports which tweaks are currently
// applied.
func GetStatus() Status {
	st := Status{Path: settingsPath()}
	if st.Path == "" {
		st.Error = "could not resolve Cursor settings path"
		return st
	}
	if _, err := os.Stat(st.Path); err != nil {
		if !os.IsNotExist(err) {
			st.Error = err.Error()
		}
		return st
	}
	st.Found = true
	settingsMu.Lock()
	defer settingsMu.Unlock()
	m, err := readSettings()
	if err != nil {
		st.Error = err.Error()
		return st
	}
	if v, ok := m["http.proxy"].(string); ok && v != "" {
		st.ProxySet = true
		st.ProxyValue = v
	}
	if v, ok := m["http.proxyStrictSSL"].(bool); ok && !v {
		st.StrictSSLOff = true
	}
	if v, ok := m["http.proxySupport"].(string); ok && v == "on" {
		st.ProxySupportOn = true
	}
	if v, ok := m["http.experimental.systemCertificatesV2"].(bool); ok && v {
		st.SystemCertsV2On = true
	}
	if v, ok := m["cursor.general.useHttp1"].(bool); ok && v {
		st.UseHTTP1 = true
	}
	if v, ok := m["cursor.general.disableHttp2"].(bool); ok && v {
		st.DisableHTTP2 = true
	}
	if v, ok := m["http.proxyKerberosServicePrincipal"].(string); ok && v != "" {
		st.ProxyKerberos = true
	}
	return st
}

// settingsPath returns the path to Cursor's user settings.json on the current
// platform. Returns "" when the directory doesn't exist (e.g. Cursor not
// installed for this user).
func settingsPath() string {
	switch runtime.GOOS {
	case "windows":
		root := os.Getenv("APPDATA")
		if root == "" {
			return ""
		}
		return filepath.Join(root, "Cursor", "User", "settings.json")
	case "darwin":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Application Support", "Cursor", "User", "settings.json")
	default:
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".config", "Cursor", "User", "settings.json")
	}
}

func backupPath() string {
	dir, err := configDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "cursor-settings-backup.json")
}

func configDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "CursorForge"), nil
}

func readSettings() (map[string]any, error) {
	p := settingsPath()
	if p == "" {
		return nil, fmt.Errorf("could not resolve Cursor settings path")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("settings.json is not valid JSON: %w", err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func writeSettings(m map[string]any) error {
	p := settingsPath()
	if p == "" {
		return fmt.Errorf("could not resolve Cursor settings path")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

// ApplyCursorTweaks writes the proxy + cert tweaks Cursor needs. Original
// values are saved so RevertCursorTweaks can put them back.
func ApplyCursorTweaks(proxyAddr string) error {
	settingsMu.Lock()
	defer settingsMu.Unlock()

	m, err := readSettings()
	if err != nil {
		return err
	}

	var bk tweakBackup
	bk.HTTPProxy, bk.HasHTTPProxy = m["http.proxy"]
	bk.HTTPProxyStrictSSL, bk.HasHTTPProxyStrictSSL = m["http.proxyStrictSSL"]
	bk.HTTPProxySupport, bk.HasHTTPProxySupport = m["http.proxySupport"]
	bk.HTTPProxyAuthHelper, bk.HasHTTPProxyAuthHelper = m["http.experimental.systemCertificatesV2"]
	bk.HTTPCompatHTTP1, bk.HasHTTPCompatHTTP1 = m["cursor.general.useHttp1"]
	bk.DisableHTTP2, bk.HasDisableHTTP2 = m["cursor.general.disableHttp2"]
	bk.ProxyKerberos, bk.HasProxyKerberos = m["http.proxyKerberosServicePrincipal"]

	if err := saveBackup(bk); err != nil {
		return err
	}

	m["http.proxy"] = "http://" + proxyAddr
	m["http.proxyStrictSSL"] = false
	m["http.proxySupport"] = "on"
	m["http.experimental.systemCertificatesV2"] = true
	m["cursor.general.useHttp1"] = true
	m["cursor.general.disableHttp2"] = true
	m["http.proxyKerberosServicePrincipal"] = "http://" + proxyAddr

	return writeSettings(m)
}

// RevertCursorTweaks restores whatever was in settings.json before
// ApplyCursorTweaks ran.
func RevertCursorTweaks() error {
	settingsMu.Lock()
	defer settingsMu.Unlock()

	bk, err := loadBackup()
	if err != nil {
		// No backup, nothing to revert.
		return nil
	}
	m, err := readSettings()
	if err != nil {
		return err
	}

	restore := func(key string, has bool, val any) {
		if has {
			m[key] = val
		} else {
			delete(m, key)
		}
	}
	restore("http.proxy", bk.HasHTTPProxy, bk.HTTPProxy)
	restore("http.proxyStrictSSL", bk.HasHTTPProxyStrictSSL, bk.HTTPProxyStrictSSL)
	restore("http.proxySupport", bk.HasHTTPProxySupport, bk.HTTPProxySupport)
	restore("http.experimental.systemCertificatesV2", bk.HasHTTPProxyAuthHelper, bk.HTTPProxyAuthHelper)
	restore("cursor.general.useHttp1", bk.HasHTTPCompatHTTP1, bk.HTTPCompatHTTP1)
	restore("cursor.general.disableHttp2", bk.HasDisableHTTP2, bk.DisableHTTP2)
	restore("http.proxyKerberosServicePrincipal", bk.HasProxyKerberos, bk.ProxyKerberos)

	if err := writeSettings(m); err != nil {
		return err
	}
	_ = os.Remove(backupPath())
	return nil
}

func saveBackup(bk tweakBackup) error {
	p := backupPath()
	if p == "" {
		return fmt.Errorf("could not resolve backup path")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(bk, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

func loadBackup() (tweakBackup, error) {
	var bk tweakBackup
	b, err := os.ReadFile(backupPath())
	if err != nil {
		return bk, err
	}
	return bk, json.Unmarshal(b, &bk)
}
