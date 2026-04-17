package certs

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

const caCommonName = "CursorForge Local CA"

// InstallUserRoot adds the CA at certPath into the current-user Trusted Root
// store. No admin rights required on Windows for the user store.
func InstallUserRoot(certPath string) error {
	switch runtime.GOOS {
	case "windows":
		cmd := exec.Command("certutil", "-user", "-addstore", "Root", certPath)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("certutil: %w (%s)", err, trim(out))
		}
		return nil
	default:
		return errors.New("CA auto-install is only supported on Windows for now")
	}
}

// UninstallUserRoot removes our CA from the current-user Root store, matching
// on the CA common name.
func UninstallUserRoot() error {
	switch runtime.GOOS {
	case "windows":
		cmd := exec.Command("certutil", "-user", "-delstore", "Root", caCommonName)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("certutil: %w (%s)", err, trim(out))
		}
		return nil
	default:
		return errors.New("CA uninstall is only supported on Windows for now")
	}
}

// IsInstalledUserRoot reports whether our CA is present in the current-user
// Root store. Matching is done by CA common name, which is stable across CA
// rebuilds because we never change it.
func IsInstalledUserRoot() bool {
	switch runtime.GOOS {
	case "windows":
		out, err := exec.Command("certutil", "-user", "-store", "Root").CombinedOutput()
		if err != nil {
			return false
		}
		return strings.Contains(string(out), caCommonName)
	default:
		return false
	}
}

func trim(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
