//go:build darwin

package cursor

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// macOS exposes per-network-service proxy config through the `networksetup`
// CLI. We apply our HTTP + HTTPS proxy to every active (non-disabled)
// service so whichever interface the user actually routes traffic on
// (Wi-Fi, Ethernet, USB tether, …) ends up pointed at our MITM listener.
//
// Every service has independent state, so we snapshot the pre-change values
// for each one and restore them on Disable. The JSON sidecar lives next to
// the Windows equivalent in the CursorForge config dir.

type darwinServiceProxy struct {
	Service string `json:"service"`
	// HTTPEnabled is the state of the "web proxy" (plain HTTP) setting; HTTPS
	// is tracked separately because macOS splits them into two controls.
	HTTPEnabled  bool   `json:"httpEnabled"`
	HTTPServer   string `json:"httpServer,omitempty"`
	HTTPPort     string `json:"httpPort,omitempty"`
	HTTPSEnabled bool   `json:"httpsEnabled"`
	HTTPSServer  string `json:"httpsServer,omitempty"`
	HTTPSPort    string `json:"httpsPort,omitempty"`
}

type darwinSysProxyBackup struct {
	Services []darwinServiceProxy `json:"services"`
}

func sysProxyBackupPath() string {
	dir, err := configDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "sysproxy-backup.json")
}

// listNetworkServices returns the names of every configured network service
// that isn't disabled. `networksetup -listallnetworkservices` prints a
// leading note line we strip, and prefixes disabled services with a "*".
func listNetworkServices() ([]string, error) {
	out, err := exec.Command("networksetup", "-listallnetworkservices").Output()
	if err != nil {
		return nil, fmt.Errorf("listallnetworkservices: %w", err)
	}
	var services []string
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	first := true
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if first {
			// Skip the banner: "An asterisk (*) denotes that a network
			// service is disabled."
			first = false
			if strings.Contains(strings.ToLower(line), "asterisk") {
				continue
			}
		}
		if strings.HasPrefix(line, "*") {
			// Disabled service — skip.
			continue
		}
		services = append(services, line)
	}
	return services, scanner.Err()
}

// parseProxyGet decodes the 4-line response of
// `networksetup -getwebproxy <service>`:
//
//	Enabled: Yes
//	Server: 127.0.0.1
//	Port: 8080
//	Authenticated Proxy Enabled: 0
func parseProxyGet(out string) (enabled bool, server string, port string) {
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "Enabled:"):
			enabled = strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(line, "Enabled:")), "yes")
		case strings.HasPrefix(line, "Server:"):
			server = strings.TrimSpace(strings.TrimPrefix(line, "Server:"))
		case strings.HasPrefix(line, "Port:"):
			port = strings.TrimSpace(strings.TrimPrefix(line, "Port:"))
		}
	}
	return
}

func readServiceProxy(service string) darwinServiceProxy {
	sp := darwinServiceProxy{Service: service}
	if out, err := exec.Command("networksetup", "-getwebproxy", service).Output(); err == nil {
		sp.HTTPEnabled, sp.HTTPServer, sp.HTTPPort = parseProxyGet(string(out))
	}
	if out, err := exec.Command("networksetup", "-getsecurewebproxy", service).Output(); err == nil {
		sp.HTTPSEnabled, sp.HTTPSServer, sp.HTTPSPort = parseProxyGet(string(out))
	}
	return sp
}

func saveSysProxyBackup(bk darwinSysProxyBackup) error {
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

func loadSysProxyBackup() (darwinSysProxyBackup, error) {
	var bk darwinSysProxyBackup
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

// EnableSystemProxy points every active network service's HTTP + HTTPS proxy
// at addr (host:port). The first Enable call snapshots whatever proxy config
// was already in place to sysproxy-backup.json so DisableSystemProxy can
// fully restore it — including corporate / custom proxies the user had set
// before CursorForge ran. A pre-existing backup is left untouched so a
// crash-restart cycle doesn't clobber the pristine state.
func EnableSystemProxy(addr string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listen addr %q: %w", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("invalid port %q: %w", portStr, err)
	}
	services, err := listNetworkServices()
	if err != nil {
		return err
	}
	if len(services) == 0 {
		return errors.New("no active macOS network services found")
	}

	// Snapshot only when no backup exists — idempotent + crash-safe.
	if _, statErr := os.Stat(sysProxyBackupPath()); os.IsNotExist(statErr) {
		bk := darwinSysProxyBackup{}
		for _, svc := range services {
			bk.Services = append(bk.Services, readServiceProxy(svc))
		}
		if saveErr := saveSysProxyBackup(bk); saveErr != nil {
			return fmt.Errorf("snapshot system proxy: %w", saveErr)
		}
	}

	var firstErr error
	for _, svc := range services {
		if err := runNetworksetup("-setwebproxy", svc, host, strconv.Itoa(port)); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := runNetworksetup("-setsecurewebproxy", svc, host, strconv.Itoa(port)); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// DisableSystemProxy restores every service's HTTP + HTTPS proxy state from
// the sidecar backup. Services absent from the backup (e.g. added after we
// snapshotted) get their HTTP/HTTPS proxies turned off rather than left
// pointing at our stopped listener. The sidecar is removed on success so
// the NEXT Enable re-snapshots a fresh state.
func DisableSystemProxy() error {
	services, listErr := listNetworkServices()
	if listErr != nil {
		return listErr
	}

	bk, loadErr := loadSysProxyBackup()
	if loadErr != nil {
		// No backup → best-effort: just turn both proxies off on every
		// active service so traffic isn't silently routed at the dead
		// MITM listener.
		var firstErr error
		for _, svc := range services {
			if err := runNetworksetup("-setwebproxystate", svc, "off"); err != nil && firstErr == nil {
				firstErr = err
			}
			if err := runNetworksetup("-setsecurewebproxystate", svc, "off"); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}

	byName := map[string]darwinServiceProxy{}
	for _, s := range bk.Services {
		byName[s.Service] = s
	}

	var firstErr error
	restore := func(svc string, sp darwinServiceProxy) {
		// HTTP
		if sp.HTTPEnabled && sp.HTTPServer != "" && sp.HTTPPort != "" {
			if err := runNetworksetup("-setwebproxy", svc, sp.HTTPServer, sp.HTTPPort); err != nil && firstErr == nil {
				firstErr = err
			}
		} else {
			if err := runNetworksetup("-setwebproxystate", svc, "off"); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		// HTTPS
		if sp.HTTPSEnabled && sp.HTTPSServer != "" && sp.HTTPSPort != "" {
			if err := runNetworksetup("-setsecurewebproxy", svc, sp.HTTPSServer, sp.HTTPSPort); err != nil && firstErr == nil {
				firstErr = err
			}
		} else {
			if err := runNetworksetup("-setsecurewebproxystate", svc, "off"); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}

	for _, svc := range services {
		if sp, ok := byName[svc]; ok {
			restore(svc, sp)
			continue
		}
		// Service we didn't snapshot (added mid-session) — disable proxies
		// on it so we don't leave it pointed at us.
		restore(svc, darwinServiceProxy{Service: svc})
	}

	_ = os.Remove(sysProxyBackupPath())
	return firstErr
}

// IsSystemProxyEnabled reports whether at least one active network service
// is currently pointed at addr (host:port) on its HTTP web proxy. Used by
// the UI to reflect the running state; a false negative here is harmless.
func IsSystemProxyEnabled(addr string) bool {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	services, err := listNetworkServices()
	if err != nil {
		return false
	}
	for _, svc := range services {
		out, err := exec.Command("networksetup", "-getwebproxy", svc).Output()
		if err != nil {
			continue
		}
		enabled, server, port := parseProxyGet(string(out))
		if enabled && server == host && port == portStr {
			return true
		}
	}
	return false
}

// runNetworksetup invokes networksetup with the given args and returns a
// wrapped error that includes stderr so the UI surfaces actionable detail
// (most common cause: user lacks the permission to edit the System-level
// network service — solved by granting Full Disk Access or running the
// app the first time with admin rights).
func runNetworksetup(args ...string) error {
	cmd := exec.Command("networksetup", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("networksetup %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
