package deletion

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/wish/nodereaper/pkg/config"
	"github.com/wish/nodereaper/pkg/configmap"
	"github.com/wish/nodereaper/pkg/controller"
	"github.com/wish/nodereaper/pkg/metrics"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"

	core_v1 "k8s.io/api/core/v1"
	k8s_types "k8s.io/apimachinery/pkg/types"
)

const (
	k8sRoleLabel = "kubernetes.io/role"
)

// APIProvider handles the provider-specific API requests needed for
// getting the needed instanceGroupsize and any provider-specific drain logic
type APIProvider interface {
	Run(<-chan struct{})
	DesiredGroupSize(string) (int, error)
	OutdatedLaunchConfig(*config.Ops, *core_v1.Node) (bool, error)
	PreDrain(*config.Ops, *core_v1.Node) error
	DetachNode(*config.Ops, *core_v1.Node) error
}

// Deleter handles the actual deletion logic
type Deleter struct {
	opts           *config.Ops
	controller     *controller.Controller
	provider       APIProvider
	stateConfigmap *configmap.ConfigMap
	metrics        *metrics.Reporter
	states         GroupStates
}

// New creates the deleter
func New(opts *config.Ops, controller *controller.Controller, provider APIProvider, stateMap *configmap.ConfigMap, metrics *metrics.Reporter) *Deleter {
	return &Deleter{
		opts,
		controller,
		provider,
		stateMap,
		metrics,
		GroupStates{
			Groups: make(map[string]*Group),
		},
	}
}

// Run starts the deleter deleting nodes
func (d *Deleter) Run(stopCh <-chan struct{}) {
	// go d.pollRecordMetrics(stopCh)
	pollPeriod, _ := config.ParseDuration(d.opts.PollPeriod)
	go wait.Until(func() {
		t := time.Now()
		d.pollDeletions()
		tookSeconds := time.Now().Sub(t)
		logrus.Debugf("Poll cycle finished in %v", tookSeconds)
	}, pollPeriod, stopCh)
}

func (d *Deleter) pollDeletions() {
	// Reload configuration from the mounted configmap
	err := d.opts.Reload()
	if err != nil {
		logrus.Errorf("Error loading config: %v", err)
		return
	}

	// Load the old node states from configmap
	// we will adopt these if we didn't already have that node
	oldNodeStates := SerializedState{
		NodeStates: make(map[string]NodeState),
	}
	r, err := d.stateConfigmap.Load("state")
	if err == nil && r != nil {
		err = json.Unmarshal([]byte(*r), &oldNodeStates)
		if err != nil {
			logrus.Errorf("Error unmarshalling node states: %v", err)
			return
		}
	}

	allNodes, err := d.controller.ListNodes()
	if err != nil {
		logrus.Errorf("Could not list nodes: %v", err)
		return
	}
	allNodeNames := map[string]struct{}{}

	for _, node := range allNodes {
		if d.totallyIgnore(node) {
			continue
		}
		groupKey := d.nodeGroupKey(node)
		allNodeNames[node.Name] = struct{}{}
		if _, ok := d.states.Groups[groupKey]; !ok {
			desired := metrics.VeryHighFalseDesiredSize
			if groupKey == "___master___" {
				desired = 3
			}
			d.states.Groups[groupKey] = &Group{
				Name:           node.Labels[d.opts.InstanceGroupLabel],
				Key:            groupKey,
				IsReal:         groupKey == "___ig___"+node.Labels[d.opts.InstanceGroupLabel],
				MaxSurge:       1,
				MaxUnavailable: 0,
				NumDesired:     desired,
				Nodes:          make(map[string]*NodeState),
				PriorityNodes:  make(map[string]struct{}),
			}
		}
		if _, ok := d.states.Groups[groupKey].Nodes[node.Name]; !ok {
			state := DontWantDelete
			if oldState, ok := oldNodeStates.NodeStates[node.Name]; ok {
				logrus.Tracef("Adopted old state of %v for node %v", oldState.State, node.Name)
				state = oldState.State
			}
			d.states.Groups[groupKey].Nodes[node.Name] = &NodeState{
				Name:  node.Name,
				State: state,
			}
		}
	}

	for groupKey, group := range d.states.Groups {
		if group.IsReal {
			desired, err := d.provider.DesiredGroupSize(group.Name)
			if err == nil {
				d.states.Groups[groupKey].NumDesired = desired
			} else {
				logrus.Warnf("Error getting desired size for group %v: %v", group.Key, err)
			}

			group.MaxSurge = percentOrNumToNum(d.opts.GetString(group.Name, "maxSurge"), group.NumDesired, true)
			group.MaxUnavailable = percentOrNumToNum(d.opts.GetString(group.Name, "maxUnavailable"), group.NumDesired, false)
			group.DeletionSchedule = d.opts.GetSchedule(group.Name, "deletionSchedule")
		}

		for nodeName, node := range group.Nodes {
			if _, ok := allNodeNames[nodeName]; !ok {
				logrus.Infof("Removing non-existent node %v from memory (last state %v)", nodeName, node.State)
				delete(group.Nodes, nodeName)
				continue
			}

			realNode, err := d.controller.NodeByName(nodeName)
			if realNode == nil || err != nil {
				logrus.Errorf("Error fetching node %v: %v", nodeName, err)
				continue
			}
			node.NeverDelete = d.countButNeverDelete(realNode)
		}
	}

	if d.killMyselfFirst() {
		// If we are killing our own node, do only that
		myNode, err := d.controller.NodeByName(d.opts.NodeName)
		if err != nil || myNode == nil {
			logrus.Warnf("Couldn't find my own node %v while trying to delete it: %v", d.opts.NodeName, err)
			return
		}
		d.states.Groups[d.nodeGroupKey(myNode)].Advance(d.StateTransitionFunction)
	} else {
		// If we aren't killing our node, advance everything
		d.states.Advance(d.StateTransitionFunction)
	}

	// Save node states to configmap in case of restart
	saved, err := json.Marshal(d.states.SerializeState())
	if err != nil {
		logrus.Errorf("Error serializing deletion state: %v", err)
		return
	}
	s := string(saved)
	d.stateConfigmap.Store("state", &s)

	// Update metrics with the new states
	d.recordMetrics()
}

func (d *Deleter) killMyselfFirst() bool {
	// If for any reason we should be killing the node we are running on
	// we drop everything else and just commit suicide as quick as possible

	// If we can't find our own node, we're probably deleted already
	myNode, err := d.controller.NodeByName(d.opts.NodeName)
	if err != nil || myNode == nil {
		return true
	}
	groupKey := d.nodeGroupKey(myNode)
	//sometimes the k8s api does not return own node in list node right on creation so the map might have not been populated yet.
	if d.states.Groups[groupKey] == nil || d.states.Groups[groupKey].Nodes[myNode.Name] == nil {
		logrus.Infof("Own node %v not found, skipping... ", d.opts.NodeName)
		return false
	}
	// Keep going if we're already deleting
	if d.states.Groups[groupKey].Nodes[myNode.Name].State != DontWantDelete {
		return true
	}
	// Don't delete if we wouldn't want to
	if killMyself, _ := d.StateTransitionFunction(d.opts.NodeName, DontWantDelete, WantDelete); !killMyself {
		return false
	}

	// Begin our deletion. We set our own node as a priority node, and just Advance the group it's in
	logrus.Infof("Detected that my own node %v needs delete. Deleting myself...", d.opts.NodeName)
	d.states.Groups[d.nodeGroupKey(myNode)].PriorityNodes[myNode.Name] = struct{}{}
	return true
}

func percentOrNumToNum(value string, total int, roundUp bool) int {
	if strings.HasSuffix(value, "%") {
		n := value[:len(value)-1]
		pct, err := strconv.ParseFloat(n, 64)
		if err != nil {
			logrus.Errorf("Could not parse %v as percentage", value)
			return 0
		}
		if roundUp {
			return int(math.Ceil((float64(total) * pct) / 100.0))
		}
		return int((float64(total) * pct) / 100.0)
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		logrus.Errorf("Could not parse %v as integer", value)
		return 0
	}
	return n
}

// StateTransitionFunction makes the needed decisions and API calls to move a node between states
func (d *Deleter) StateTransitionFunction(nodeName string, oldState, newState State) (bool, error) {
	node, err := d.controller.NodeByName(nodeName)
	if err != nil {
		return false, fmt.Errorf("Error reading node %v", nodeName)
	} else if node == nil {
		return false, fmt.Errorf("Could not find node %v", nodeName)
	}

	// Check if we want to delete
	if oldState == DontWantDelete && newState == WantDelete {
		wantDelete, _ := d.WantToDelete(node)
		return wantDelete, nil
	}

	// Detach the node from the autoscaling group
	if oldState == WantDelete && newState == Detached {
		err := d.provider.DetachNode(d.opts, node)
		return err == nil, err
	}

	// If the machine thinks we're ready to delete this node
	// we're ready
	if oldState == WantDelete && newState == ReadyToDelete {
		return true, nil
	}
	if oldState == Detached && newState == ReadyToDelete {
		return true, nil
	}

	// Try actually deleting the node
	if oldState == ReadyToDelete && newState == Deleting {
		err := d.provider.PreDrain(d.opts, node)
		if err != nil {
			return false, err
		}
		err = d.applyDeletionLabel(node.Name)
		if err != nil {
			return false, err
		}
		return true, nil
	}

	return false, fmt.Errorf("No transition available for %v -> %v", oldState, newState)
}

func (d *Deleter) totallyIgnore(node *core_v1.Node) bool {
	groupName := node.Labels[d.opts.InstanceGroupLabel]
	if gp := d.opts.GetDuration(groupName, "startupGracePeriod"); gp != nil {
		if node.CreationTimestamp.Add(*gp).After(time.Now()) {
			logrus.Tracef("Ignoring node %v because it is too new", node.Name)
			return true
		}
	}

	foundReady := false
	for _, condition := range node.Status.Conditions {
		if condition.Type == "Ready" && condition.Status == "True" {
			foundReady = true
			break
		}
	}
	if !foundReady {
		logrus.Tracef("Ignoring node %v because it is not Ready", node.Name)
		return true
	}

	return false
}

func (d *Deleter) countButNeverDelete(node *core_v1.Node) bool {
	groupName := node.Labels[d.opts.InstanceGroupLabel]
	if d.opts.GetBool(groupName, "ignore") {
		logrus.Tracef("Ignoring node %v in group %v", node.Name, groupName)
		return true
	}

	if ignoreSelector := d.opts.GetString(groupName, "ignoreSelector"); ignoreSelector != "" {
		selector, _ := labels.Parse(ignoreSelector)
		if selector.Matches(labels.Set(node.Labels)) {
			logrus.Tracef("Ignoring node %v, as it matches the ignore selector %v", node.Name, ignoreSelector)
			return true
		}
	}

	return false
}

// WantToDelete determines whether the controller wants delete the node and returns the reason why if it does
func (d *Deleter) WantToDelete(node *core_v1.Node) (bool, metrics.Reason) {
	groupName := node.Labels[d.opts.InstanceGroupLabel]

	// Delete the node if it is requested for deletion
	if d.opts.RequestDeletionLabel != "" {
		for label := range node.Labels {
			if label == d.opts.RequestDeletionLabel {
				logrus.Tracef("Node %v has deletion label %v", node.Name, d.opts.RequestDeletionLabel)
				return true, metrics.HasDeletionLabel
			}
		}
	}

	// Delete the node if it is past its maximum age
	if deletionAge := d.opts.GetDuration(groupName, "deletionAge"); deletionAge != nil {
		t := time.Now()

		// Based on a hash of the node name, wait for up to DeletionAgeJitter after the node's
		// DeletionAge before deleting.
		jitter := 0 * time.Second
		if maxAfter := d.opts.GetDuration(groupName, "deletionAgeJitter"); maxAfter != nil {
			hasher := fnv.New32a()
			hasher.Write([]byte(node.Name))
			jitter = time.Duration((int64((hasher.Sum32() % 100)) * int64(*maxAfter)) / 100)
		}

		if t.After(node.CreationTimestamp.Add(*deletionAge).Add(jitter)) {
			logrus.Tracef("Node %v is more than %v old", node.Name, *deletionAge)
			return true, metrics.TooOld
		}
	}

	if d.opts.GetBool(groupName, "deleteOldLaunchConfig") {
		// Delete the node if the API-specific logic thinks we should
		providerWantsDelete, err := d.provider.OutdatedLaunchConfig(d.opts, node)
		if err != nil {
			logrus.Warnf("Error checking if %v has an outdated config: %v", node.Name, err)
		} else if providerWantsDelete {
			logrus.Tracef("Node %v has a different configuration than its instanceGroup", node.Name)
			return true, metrics.ConfigurationChanged
		}

	}

	return false, ""
}

func (d *Deleter) applyDeletionLabel(nodeName string) error {
	patch, _ := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{
			"labels": map[string]interface{}{
				d.opts.ForceDeletionLabel: "nodereaper",
			},
		},
	})
	_, err := d.controller.Clientset.CoreV1().Nodes().Patch(nodeName, k8s_types.MergePatchType, patch)
	if err != nil {
		return fmt.Errorf("Error applying deletion label: %v", err)
	}
	return nil
}

func (d *Deleter) nodeGroupKey(node *core_v1.Node) string {
	if node.Labels[d.opts.InstanceGroupLabel] == "" {
		return "___nogroup___"
	}
	return "___ig___" + node.Labels[d.opts.InstanceGroupLabel]
}

func (d *Deleter) recordMetrics() {
	groupStates := map[string]metrics.GroupState{}

	for _, group := range d.states.Groups {
		nodes := []metrics.Node{}
		for _, node := range group.Nodes {
			actualNode, err := d.controller.NodeByName(node.Name)
			if actualNode == nil || err != nil {
				continue
			}
			_, reason := d.WantToDelete(actualNode)
			nodes = append(nodes, metrics.Node{
				State:  string(node.State),
				Reason: reason,
			})
		}

		g := metrics.GroupState{
			GroupName:   group.Name,
			WantedNodes: group.NumDesired,
			Nodes:       nodes,
		}
		groupStates[g.GroupName] = g
	}
	d.metrics.SetGroupState(groupStates)
}
