package config

import "time"

// Source revisions distinguish wallet/process changes from ordinary inventory
// saves. They are process-local, like state generations, and are never persisted.
type stateSourceIdentity struct {
	configPath, peerID, binaryPath, binarySHA256         string
	qclientPath, nodeSource, installSource, migratedFrom string
	installedAt, qclientInstalledAt, startedAt           time.Time
}

type stateSourceObservation struct {
	revision uint64
	identity stateSourceIdentity
	exists   bool
	observed bool
}

var stateSources = map[string]stateSourceObservation{}

func sourceIdentity(s *State) stateSourceIdentity {
	return stateSourceIdentity{
		configPath: s.ConfigPath, peerID: s.PeerID, binaryPath: s.BinaryPath, binarySHA256: s.NodeBinarySHA256,
		qclientPath: s.QClientBinaryPath, nodeSource: s.NodeSource, installSource: s.InstallSource, migratedFrom: s.MigratedFrom,
		installedAt: s.InstalledAt.UTC(), qclientInstalledAt: s.QClientInstalledAt.UTC(), startedAt: s.LastStartedAt.UTC(),
	}
}

func observeStateSourceUnlocked(path string, s *State, exists bool) {
	key := stateGenerationKey(path)
	observation := stateSources[key]
	identity := sourceIdentity(s)
	if !observation.observed || observation.exists != exists || observation.identity != identity {
		observation.revision++
	}
	observation.observed, observation.exists, observation.identity = true, exists, identity
	stateSources[key] = observation
	s.sourceRevision, s.sourceRevisionSet = observation.revision, true
}

func invalidateStateSourceUnlocked(path string) {
	key := stateGenerationKey(path)
	observation := stateSources[key]
	observation.revision++
	observation.observed = false
	stateSources[key] = observation
}

// StateSourceIsCurrent atomically reloads the source identity and checks its
// revision. Metadata-only saves preserve this revision. Observed external source
// changes, and all in-process remove/move/source transitions, invalidate it,
// including transitions back to the same identity.
func StateSourceIsCurrent(path string, snapshot *State) bool {
	stateMu.Lock()
	defer stateMu.Unlock()
	if snapshot == nil || !snapshot.sourceRevisionSet {
		return false
	}
	current, err := loadStateUnlocked(path)
	return err == nil && current.sourceRevision == snapshot.sourceRevision && sourceIdentity(current) == sourceIdentity(snapshot)
}
