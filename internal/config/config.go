// Package config loads nx-debug-cli's optional user config file. It's a
// plain "key = value" text file (no new dependency for something this
// small) at the platform's standard per-user config location.
package config

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Config holds the settings a user can override outside of env vars/flags.
type Config struct {
	// OutputDir is where locally-written artifacts land by default when a
	// command isn't given an explicit path.
	OutputDir string
}

// Path returns the config file's location: nx-debug-cli/config under the
// OS's standard per-user config directory (%AppData% on Windows,
// $XDG_CONFIG_HOME or ~/.config on Linux, ~/Library/Application Support on
// macOS).
func Path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("config: %w", err)
	}
	return filepath.Join(dir, "nx-debug-cli", "config"), nil
}

// Load reads the config file if it exists. A missing file is not an error;
// it just means every field is left at its zero value.
func Load() (Config, error) {
	var cfg Config

	path, err := Path()
	if err != nil {
		return cfg, err
	}

	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("config: %w", err)
	}
	defer f.Close()

	cfg, err = parse(f)
	if err != nil {
		return cfg, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}

// parse reads "key = value" lines (blank lines and "#" comments ignored)
// into a Config. Split out from Load so the parsing logic is testable
// without touching the filesystem.
func parse(r io.Reader) (Config, error) {
	var cfg Config
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "output_dir":
			cfg.OutputDir = value
		}
	}
	return cfg, scanner.Err()
}
