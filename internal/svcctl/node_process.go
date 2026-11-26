package svcctl

import (
	"path/filepath"
	"strings"
)

// NodeProcess describes the running service, independently of Agent state.
type NodeProcess struct {
	PID        int
	ConfigFile string
}

func nodeConfigFile(args []string, cwd string) string {
	dir := ".config"
	for i := 0; i < len(args); i++ {
		flag, value, equals := strings.Cut(args[i], "=")
		if flag == "--" {
			break
		}
		if flag != "--config" && flag != "-config" && flag != "-c" {
			continue
		}
		if !equals && i+1 < len(args) {
			i++
			value = args[i]
		}
		if value == "" || strings.HasPrefix(value, "-") {
			return ""
		}
		dir = value
	}
	if !filepath.IsAbs(dir) {
		if !filepath.IsAbs(cwd) {
			return ""
		}
		dir = filepath.Join(cwd, dir)
	}
	return filepath.Join(dir, "config.yml")
}
