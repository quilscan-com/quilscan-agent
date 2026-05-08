package nodeinstall

import "os"

type Paths struct {
	BinaryPath       string
	ManagedConfigDir string
	StatePath        string
	UnitFilePath     string
	ProcessRunning   bool
}

type Detection struct {
	HasNode  bool
	Residues []string
}

func Detect(paths Paths) Detection {
	binaryExists := exists(paths.BinaryPath)
	configExists := exists(paths.ManagedConfigDir)
	if binaryExists {
		return Detection{HasNode: true, Residues: []string{}}
	}

	residues := []string{}
	if configExists {
		residues = append(residues, "managed_config")
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
