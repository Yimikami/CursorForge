//go:build !windows

package cursor

import "errors"

func EnableSystemProxy(addr string) error {
	return errors.New("system proxy auto-configuration is only implemented on Windows")
}

func DisableSystemProxy() error {
	return nil
}

func IsSystemProxyEnabled(addr string) bool {
	return false
}
