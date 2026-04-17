//go:build windows

package cursor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

const inetSettingsKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

const (
	internetOptionSettingsChanged = 39
	internetOptionRefresh         = 37
)

// sysProxyBackup is the pre-override snapshot of the three WinINET proxy
// keys we touch. "Has*" reflects whether the registry value existed at
// snapshot time — a missing value must be deleted on restore, not written
// back as an empty string (that would look like "proxy set to empty" to
// other apps reading the keys).
type sysProxyBackup struct {
	HasEnable   bool   `json:"hasEnable"`
	Enable      uint32 `json:"enable,omitempty"`
	HasServer   bool   `json:"hasServer"`
	Server      string `json:"server,omitempty"`
	HasOverride bool   `json:"hasOverride"`
	Override    string `json:"override,omitempty"`
}

func sysProxyBackupPath() string {
	dir, err := configDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "sysproxy-backup.json")
}

func readCurrentSysProxy() (sysProxyBackup, error) {
	var bk sysProxyBackup
	k, err := registry.OpenKey(registry.CURRENT_USER, inetSettingsKey, registry.QUERY_VALUE)
	if err != nil {
		return bk, err
	}
	defer k.Close()
	if v, _, err := k.GetIntegerValue("ProxyEnable"); err == nil {
		bk.HasEnable = true
		bk.Enable = uint32(v)
	}
	if v, _, err := k.GetStringValue("ProxyServer"); err == nil {
		bk.HasServer = true
		bk.Server = v
	}
	if v, _, err := k.GetStringValue("ProxyOverride"); err == nil {
		bk.HasOverride = true
		bk.Override = v
	}
	return bk, nil
}

func saveSysProxyBackup(bk sysProxyBackup) error {
	p := sysProxyBackupPath()
	if p == "" {
		return errors.New("could not resolve sysproxy backup path")
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

func loadSysProxyBackup() (sysProxyBackup, error) {
	var bk sysProxyBackup
	p := sysProxyBackupPath()
	if p == "" {
		return bk, errors.New("could not resolve sysproxy backup path")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return bk, err
	}
	return bk, json.Unmarshal(b, &bk)
}

// EnableSystemProxy points the current-user WinINET proxy at addr (host:port)
// and broadcasts the change so apps that watch for proxy updates pick it up
// immediately. The previous WinINET state (ProxyEnable, ProxyServer,
// ProxyOverride) is snapshotted to disk the first time Enable is called so
// DisableSystemProxy can fully restore any pre-existing corporate/custom
// proxy. If a backup already exists we leave it untouched — that file is
// the user's last-known-good state and must survive crash-restart cycles.
func EnableSystemProxy(addr string) error {
	// Snapshot only when no backup exists yet. This keeps Enable idempotent
	// and crash-safe: a second Enable after a crash won't overwrite the
	// pristine state with whatever we already stamped in last time.
	if _, err := os.Stat(sysProxyBackupPath()); os.IsNotExist(err) {
		if bk, readErr := readCurrentSysProxy(); readErr == nil {
			if saveErr := saveSysProxyBackup(bk); saveErr != nil {
				return fmt.Errorf("snapshot system proxy: %w", saveErr)
			}
		}
	}

	k, _, err := registry.CreateKey(registry.CURRENT_USER, inetSettingsKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.SetDWordValue("ProxyEnable", 1); err != nil {
		return err
	}
	if err := k.SetStringValue("ProxyServer", addr); err != nil {
		return err
	}
	if err := k.SetStringValue("ProxyOverride", "<local>"); err != nil {
		return err
	}
	notifyInetSettingsChanged()
	return nil
}

// DisableSystemProxy restores the WinINET state captured by EnableSystemProxy.
// Missing values are deleted (rather than left at our override) so any
// pre-existing corporate/custom proxy survives a CursorForge start/stop
// cycle. When no backup exists we fall back to just clearing ProxyEnable.
func DisableSystemProxy() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, inetSettingsKey, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()

	bk, loadErr := loadSysProxyBackup()
	if loadErr != nil {
		// No backup → best-effort: turn the flag off and leave the rest as
		// the user's state at the end of the session. Mirrors the original
		// behaviour for upgrade paths that predate the backup sidecar.
		if setErr := k.SetDWordValue("ProxyEnable", 0); setErr != nil {
			return setErr
		}
		notifyInetSettingsChanged()
		return nil
	}

	restoreDword := func(name string, has bool, val uint32) error {
		if has {
			return k.SetDWordValue(name, val)
		}
		// DeleteValue on a missing key returns ERROR_FILE_NOT_FOUND which is
		// fine — treat it as a no-op instead of a failure.
		if delErr := k.DeleteValue(name); delErr != nil && !errors.Is(delErr, registry.ErrNotExist) {
			return delErr
		}
		return nil
	}
	restoreString := func(name string, has bool, val string) error {
		if has {
			return k.SetStringValue(name, val)
		}
		if delErr := k.DeleteValue(name); delErr != nil && !errors.Is(delErr, registry.ErrNotExist) {
			return delErr
		}
		return nil
	}

	if err := restoreDword("ProxyEnable", bk.HasEnable, bk.Enable); err != nil {
		return err
	}
	if err := restoreString("ProxyServer", bk.HasServer, bk.Server); err != nil {
		return err
	}
	if err := restoreString("ProxyOverride", bk.HasOverride, bk.Override); err != nil {
		return err
	}

	// Remove the sidecar so the NEXT Enable re-snapshots a pristine state
	// (otherwise a user who changes their real proxy between sessions would
	// end up restoring the stale original on the following Stop).
	_ = os.Remove(sysProxyBackupPath())

	notifyInetSettingsChanged()
	return nil
}

// IsSystemProxyEnabled reports whether WinINET proxy is currently on AND
// pointed at our address.
func IsSystemProxyEnabled(addr string) bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, inetSettingsKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	enabled, _, err := k.GetIntegerValue("ProxyEnable")
	if err != nil || enabled != 1 {
		return false
	}
	server, _, err := k.GetStringValue("ProxyServer")
	if err != nil {
		return false
	}
	return server == addr
}

func notifyInetSettingsChanged() {
	wininet, err := syscall.LoadDLL("wininet.dll")
	if err != nil {
		return
	}
	defer wininet.Release()
	proc, err := wininet.FindProc("InternetSetOptionW")
	if err != nil {
		return
	}
	proc.Call(0, internetOptionSettingsChanged, uintptr(unsafe.Pointer(nil)), 0)
	proc.Call(0, internetOptionRefresh, uintptr(unsafe.Pointer(nil)), 0)
}
