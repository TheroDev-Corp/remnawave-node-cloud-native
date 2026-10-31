// Package ha holds the leader-election state the health endpoints and peer
// sync read. Milestone 1 ships the standalone mode (election disabled): like
// LeaderElectionService with K8S_LEADER_ELECTION_ENABLED=false, the node is
// always the leader.
package ha

type State interface {
	IsLeader() bool
	IsDraining() bool
	IsServingTraffic() bool
	Identity() string
}

// Standalone is the single-node mode.
type Standalone struct{ ID string }

func (s Standalone) IsLeader() bool         { return true }
func (s Standalone) IsDraining() bool       { return false }
func (s Standalone) IsServingTraffic() bool { return true }
func (s Standalone) Identity() string       { return s.ID }
