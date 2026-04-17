//go:build windows

package cursor

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

const inetSettingsKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

const (
	internetOptionSettingsChanged = 39
	internetOptionRefresh         = 37
)

// EnableSystemProxy points the current-user WinINET proxy at addr (host:port)
// and broadcasts the change so apps that watch for proxy updates pick it up
// immediately.
func EnableSystemProxy(addr string) error {
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

// DisableSystemProxy clears ProxyEnable. ProxyServer is left in place so the
// user's previous custom proxy survives an uninstall — but for the typical
// case (no prior proxy) the disabled flag is enough.
func DisableSystemProxy() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, inetSettingsKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.SetDWordValue("ProxyEnable", 0); err != nil {
		return err
	}
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
