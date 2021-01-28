package deletion

import (
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/wish/nodereaper/pkg/cron"
)

// StateTransitionFunction attempts to move a node from oldState to newState
type StateTransitionFunction func(nodeName string, oldState, newState State) (bool, error)

// State is an enumeration of the stages of the deletion process
type State string

const (
	// DontWantDelete means the controller doesn't want to delete the node
	DontWantDelete State = "dont_want_delete"
	// WantDelete means the controller does want to delete the node, but hasn't started yet
	WantDelete State = "want_delete"
	// Detached means the controller has detached the node from the underlying ASG, and is waiting for overprovision before deleting
	Detached State = "detached"
	// ReadyToDelete means the controller is ready to actually begin deleting a node
	ReadyToDelete State = "ready_to_delete"
	// Deleting means the controller has instructed nodereaperd to delete the node
	Deleting State = "deleting"
)

// NodeState represents the state of deletion for a single node
type NodeState struct {
	Name        string `json:"-"`
	State       State  `json:"state"`
	NeverDelete bool   `json:"-"`
}

func (n *NodeState) changeState(newState State, f StateTransitionFunction) bool {
	yes, err := f(n.Name, n.State, newState)
	if yes {
		logrus.Infof("Successfully changed state of %v from %v to %v", n.Name, n.State, newState)
		n.State = newState
	} else if err != nil {
		logrus.Errorf("Failed to change state of %v from %v to %v: %v", n.Name, n.State, newState, err)
	}
	return yes
}

// Group represents the deletion states and settings for a single group
type Group struct {
	Name             string
	Key              string
	IsReal           bool
	MaxSurge         int
	MaxUnavailable   int
	DeletionSchedule *cron.Schedule
	NumDesired       int
	Nodes            map[string]*NodeState
	PriorityNodes    map[string]struct{}
}

// GroupStates represents a set of state machines describing the progress in deleting nodes
// from each group
type GroupStates struct {
	Groups map[string]*Group
}

// SerializedState is a snapshot of the deletion state for every node.
// Can be serialized to and from a configmap.
type SerializedState struct {
	NodeStates map[string]NodeState `json:"nodeStates"`
}

// SerializeState extracts the basic information about node states to a separate struct
func (gs *GroupStates) SerializeState() SerializedState {
	nodeStates := map[string]NodeState{}
	for _, group := range gs.Groups {
		for _, node := range group.Nodes {
			nodeStates[node.Name] = *node
		}
	}
	return SerializedState{
		NodeStates: nodeStates,
	}
}

func (g *Group) size() int {
	return len(g.Nodes)
}

func (g *Group) stateCount(states ...State) int {
	i := 0
	for _, node := range g.Nodes {
		for _, state := range states {
			if node.State == state {
				i++
				break
			}
		}
	}
	return i
}

func (g *Group) iterateNodes() []*NodeState {
	// If there are any priority nodes (like the node this is running on)
	// We focus on them exclusively
	ret := []*NodeState{}
	for name := range g.PriorityNodes {
		if node, ok := g.Nodes[name]; ok && !node.NeverDelete {
			ret = append(ret, node)
		} else {
			delete(g.PriorityNodes, name)
		}
	}
	if len(ret) == 0 {
		for _, node := range g.Nodes {
			if !node.NeverDelete {
				ret = append(ret, node)
			}
		}
	}
	return ret
}

// Advance tries to move as many nodes in the group as possible to deletion
func (g *Group) Advance(f StateTransitionFunction) {
	// Move whatever nodes need to be moved from DontWantDelete -> WantDelete
	for _, node := range g.iterateNodes() {
		if node.State == DontWantDelete {
			node.changeState(WantDelete, f)
		}
	}

	// First attempt to move as many nodes as possible from Detached -> ReadyToDelete and then WantDelete -> ReadyToDelete
	totalNumberOfNodes := g.size()
	numBeingDeleted := g.stateCount(ReadyToDelete, Deleting)
	numNotBeingDeleted := totalNumberOfNodes - numBeingDeleted
	numCanBeDeleted := numNotBeingDeleted - g.NumDesired + g.MaxUnavailable

	// If a deletionSchedule was specified, make sure that we are in an allowed time before
	// moving any nodes in WantDelete into the deletion process
	scheduleAllowsDeletion := g.DeletionSchedule == nil || g.DeletionSchedule.Matches(time.Now().In(time.UTC))
	if !scheduleAllowsDeletion && g.stateCount(WantDelete) > 0 {
		logrus.Debugf("Group %s can't delete because of crontab", g.Name)
		logrus.Tracef("Spec: %s, current time %v", g.DeletionSchedule.Source(), time.Now().In(time.UTC))
	}

	// Detached -> ReadyToDelete
	for _, node := range g.iterateNodes() {
		if numCanBeDeleted <= 0 {
			break
		}
		if node.State == Detached {
			if ok := node.changeState(ReadyToDelete, f); ok {
				numCanBeDeleted--
			}
		}
	}

	// WantDelete -> ReadyToDelete
	if scheduleAllowsDeletion {
		for _, node := range g.iterateNodes() {
			if numCanBeDeleted <= 0 {
				break
			}
			if node.State == WantDelete {
				if ok := node.changeState(ReadyToDelete, f); ok {
					numCanBeDeleted--
				}
			}
		}
	}

	// Now try to move as many nodes as possible from ReadyToDelete -> Deleting
	for _, node := range g.iterateNodes() {
		if node.State == ReadyToDelete {
			node.changeState(Deleting, f)
		}
	}

	// Now try to move as many nodes as possible from WantDelete -> Detached
	if scheduleAllowsDeletion {
		numCanBeDetached := g.MaxSurge - g.stateCount(Detached, ReadyToDelete, Deleting)
		if numCanBeDetached < 0 {
			numCanBeDetached = 0
		}
		for _, node := range g.iterateNodes() {
			if numCanBeDetached == 0 {
				break
			}
			if node.State == WantDelete {
				if ok := node.changeState(Detached, f); ok {
					numCanBeDetached--
				}
			}
		}
	}
}

// Advance tries to advance deletion for all groups, in parallel
func (gs *GroupStates) Advance(f StateTransitionFunction) {
	wait := sync.WaitGroup{}
	for _, group := range gs.Groups {
		wait.Add(1)
		go func(group *Group) {
			defer wait.Done()
			group.Advance(f)
		}(group)
	}
	wait.Wait()
}

// Debug outputs some quick stats about each groups' state
func (gs *GroupStates) Debug() {
	for groupKey, group := range gs.Groups {
		logrus.Debugf("Group: %v, name %v, isReal %v, desires %v, has %v", groupKey, group.Name, group.IsReal, group.NumDesired, group.size())
		for nodeName, node := range group.Nodes {
			logrus.Debugf("      %v: %v", nodeName, node.State)
		}
	}
}
