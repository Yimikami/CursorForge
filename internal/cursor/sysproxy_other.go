//go:build !windows && !darwin

package cursor

// On Linux there's no single vendor-neutral API for system-wide proxy
// configuration: each desktop (GNOME, KDE, Sway, plain Xorg, headless)
// wires it differently, and most apps only honour $http_proxy / $https_proxy
// anyway. Forcing one of those paths from a GUI app is fragile and can
// strand the user without a way to revert (e.g. gsettings edits survive
// a crash but the user doesn't know we wrote them).
//
// Cursor itself reads its HTTP proxy out of settings.json (ApplyCursorTweaks
// already writes that file), so the IDE routes its traffic through our
// MITM listener without any system-wide change. Leaving these as no-ops
// means Linux users get a working BYOK flow with zero risk of us clobbering
// their network config — users who DO want system-wide interception can
// export the env vars themselves before launching affected apps.
func EnableSystemProxy(addr string) error {
	return nil
}

func DisableSystemProxy() error {
	return nil
}

func IsSystemProxyEnabled(addr string) bool {
	return false
}
