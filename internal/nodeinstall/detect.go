package nodeinstall

import "os"

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
	if exists(paths.StatePath) {
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
