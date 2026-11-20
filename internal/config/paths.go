package config

import (
	"errors"
	"os"
	"path/filepath"
)

const appName = "lazylongarm"

// xdg returns $<env> when it is an absolute path, else ~/<fallback>. This
// intentionally uses XDG on macOS too (instead of Application Support).
func xdg(env, fallback string) (string, error) {
	if v := os.Getenv(env); v != "" && filepath.IsAbs(v) {
		return filepath.Join(v, appName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("cannot determine the home directory; set " + env)
	}
	return filepath.Join(home, fallback, appName), nil
}

// DefaultPath is $XDG_CONFIG_HOME/lazylongarm/config.toml.
func DefaultPath() (string, error) {
	dir, err := xdg("XDG_CONFIG_HOME", ".config")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.toml"), nil
}

// StateDir is $XDG_STATE_HOME/lazylongarm (operation journals, UI state).
func StateDir() (string, error) { return xdg("XDG_STATE_HOME", filepath.Join(".local", "state")) }

// CacheDir is $XDG_CACHE_HOME/lazylongarm (downloaded release files).
func CacheDir() (string, error) { return xdg("XDG_CACHE_HOME", ".cache") }

// ResolvePath applies the documented precedence: explicit flag, then
// LAZYLONGARM_CONFIG, then the XDG default. explicit reports whether a
// missing file is an error.
func ResolvePath(flag string) (path string, explicit bool, err error) {
	if flag != "" {
		return flag, true, nil
	}
	if env := os.Getenv("LAZYLONGARM_CONFIG"); env != "" {
		return env, true, nil
	}
	p, err := DefaultPath()
	return p, false, err
}
