package actions

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// MigrateDeps wires the install handler migrate delegates to.
type MigrateDeps struct {
	Install Handler
}

// NewMigrateHandler validates a user-supplied .config directory, tags the
// install args with source=migrated, then forwards. Migrated starts use the
// config parent as WorkingDirectory so relative paths inside config.yml keep
// working. The install flow must not patch RPC for migrated configs; users own
// those settings.
func NewMigrateHandler(d MigrateDeps) Handler {
	return func(c Command, emit Emitter) error {
		cfgPath, _ := c.Args["config_path"].(string)
		cfgPath = normalizeConfigPath(cfgPath)
		if err := validateConfigPath(cfgPath); err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}
		emit(Status{ID: c.ID, Step: "validated", Progress: 0.10})
		c.Args["config_path"] = cfgPath
		c.Args["source"] = "migrated"
		return d.Install(c, emit)
	}
}

func normalizeConfigPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	return filepath.Clean(p)
}

func validateConfigPath(p string) error {
	p = strings.TrimSpace(p)
	if p == "" {
		return fmt.Errorf("config_path required")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("config_path contains control character")
		}
	}
	if !filepath.IsAbs(p) {
		return fmt.Errorf("config_path must be absolute")
	}
	if strings.Contains(p, "..") {
		return fmt.Errorf("config_path must not contain '..'")
	}
	p = filepath.Clean(p)
	info, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("config_path: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("config_path must be a directory")
	}
	if filepath.Base(filepath.Clean(p)) != ".config" {
		return fmt.Errorf("config_path directory must be named .config")
	}
	keyPath, err := readConfigKeyPath(filepath.Join(p, "config.yml"))
	if err != nil {
		return err
	}
	if keyPath != ".config/keys.yml" {
		return fmt.Errorf("config.yml key path must be .config/keys.yml")
	}
	return nil
}

func readConfigKeyPath(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read config.yml: %w", err)
	}
	root := map[string]interface{}{}
	if err := yaml.Unmarshal(b, &root); err != nil {
		return "", fmt.Errorf("parse config.yml: %w", err)
	}
	if keyPath := nestedString(root, "key", "keyManagerFile", "path"); keyPath != "" {
		return keyPath, nil
	}
	if keyPath := nestedString(root, "keys", "path"); keyPath != "" {
		return keyPath, nil
	}
	return "", fmt.Errorf("config.yml key path must be .config/keys.yml")
}

func nestedString(root map[string]interface{}, keys ...string) string {
	var cur interface{} = root
	for _, key := range keys {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return ""
		}
		cur = m[key]
	}
	s, ok := cur.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}
