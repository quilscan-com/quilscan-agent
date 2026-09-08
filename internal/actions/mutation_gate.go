package actions

import "sync"

type MutationOwner struct {
	Action string
	CmdID  string
}

type MutationGate struct {
	mu    sync.Mutex
	owner MutationOwner
}

func (g *MutationGate) TryAcquire(owner MutationOwner) (release func(), blocker MutationOwner, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.owner != (MutationOwner{}) {
		return nil, g.owner, false
	}
	g.owner = owner
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
		"set_dev_node_auto_update", "switch_node_source", "cleanup_residue",
		"delete_node_store", "delete_node_store_backup", "qclient_manage_action",
		"repair_fd_limit", "install_qclient", "update_agent":
		return true
	default:
		return false
	}
}
