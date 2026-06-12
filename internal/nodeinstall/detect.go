package nodeinstall

import (
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Paths struct {
	BinaryPath        string
	ManagedConfigDir  string
	RecordedConfigDir string
	StatePath         string
	UnitFilePath      string
	ProcessRunning    bool
}

type Detection struct {
	HasNode  bool
	Residues []string
}

func Detect(paths Paths) Detection {
	binaryExists := exists(paths.BinaryPath)
	recordedConfigExists := exists(paths.RecordedConfigDir)
	configExists := exists(paths.ManagedConfigDir)
	if binaryExists && recordedConfigExists {
		return Detection{HasNode: true, Residues: []string{}}
	}

	residues := []string{}
	if binaryExists {
		residues = append(residues, "node_binary")
	}
	if configExists {
		residues = append(residues, "managed_config")
	}
	if paths.RecordedConfigDir != "" && !recordedConfigExists {
		residues = append(residues, "missing_recorded_config")
	}
	if stateFileHasNodeRecord(paths.StatePath) {
		residues = append(residues, "state_file")
	}
	if exists(paths.UnitFilePath) {
		residues = append(residues, "systemd_unit")
	}
	if paths.ProcessRunning {
		residues = append(residues, "process")
	}
	return Detection{HasNode: false, Residues: residues}
}

func exists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func stateFileHasNodeRecord(path string) bool {
	if path == "" {
		return false
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		return true
	}
	var state map[string]interface{}
	if err := yaml.Unmarshal(raw, &state); err != nil {
		return true
	}
	for key, value := range state {
		if key == "schema_version" {
			continue
		}
		if meaningfulValue(value) {
			return true
		}
	}
	return false
}

func meaningfulValue(value interface{}) bool {
	switch v := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(v) != ""
	case bool:
		return v
	case int:
		return v != 0
	case int8:
		return v != 0
	case int16:
		return v != 0
	case int32:
		return v != 0
	case int64:
		return v != 0
	case uint:
		return v != 0
	case uint8:
		return v != 0
	case uint16:
		return v != 0
	case uint32:
		return v != 0
	case uint64:
		return v != 0
	case float32:
		return v != 0
	case float64:
		return v != 0
	case []interface{}:
		return len(v) > 0
	case map[string]interface{}:
		return len(v) > 0
	default:
		return true
	}
}
