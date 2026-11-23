package actions

import "sync"

type MutationOwner struct {
	Action              string
	CmdID               string
	ProtectNodeIdentity bool
}

type MutationGate struct {
	mu                sync.Mutex
	owner             MutationOwner
	nodeIdentityEpoch uint64
}

func (g *MutationGate) TryAcquire(owner MutationOwner) (release func(), blocker MutationOwner, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.owner != (MutationOwner{}) {
		return nil, g.owner, false
	}
	g.owner = owner
	if owner.ProtectNodeIdentity {
		g.nodeIdentityEpoch++
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			if g.owner == owner {
				g.owner = MutationOwner{}
			}
			g.mu.Unlock()
		})
	}, MutationOwner{}, true
}

// NodeIdentityObservation identifies the state of the gate when a read
// of the on-disk node begins.
func (g *MutationGate) NodeIdentityObservation() (uint64, bool) {
	if g == nil {
		return 0, true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.nodeIdentityEpoch, !g.owner.ProtectNodeIdentity
}

// WithNodeIdentityObservation commits a short, local observation only if
// no protected node mutation was active or started since the read began.
func (g *MutationGate) WithNodeIdentityObservation(epoch uint64, commit func()) bool {
	if g != nil {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.owner.ProtectNodeIdentity || epoch != g.nodeIdentityEpoch {
			return false
		}
	}
	commit()
	return true
}

func (g *MutationGate) Owner() MutationOwner {
	if g == nil {
		return MutationOwner{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.owner
}

func IsMutatingAction(action string) bool {
	switch action {
	case "install", "migrate", "start", "stop", "restart_node", "update_node",
		"set_dev_node_auto_update", "set_releases_node_auto_update", "switch_node_source", "cleanup_residue",
		"delete_node_store", "delete_node_store_backup", "qclient_manage_action",
		"repair_fd_limit", "install_qclient", "update_agent", "set_node_diagnostics",
		"qclient_log_preview", "qclient_log_approve", "qclient_log_deny", "qclient_log_discard":
		return true
	default:
		return false
	}
}
