package metrics

import (
	"io"
	"net/http"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/sirupsen/logrus"
)

const (
	contentTypeHeader     = "Content-Type"
	contentEncodingHeader = "Content-Encoding"
)

// Reason represents a reason that the controller would want to delete a node
type Reason string

const (
	// HasDeletionLabel means the node has the label specified in config.Ops.RequestDeletionLabel
	HasDeletionLabel Reason = "has_deletion_label"
	// TooOld means the node is older than the duration specified by config.Ops.DeletionAge
	TooOld Reason = "too_old"
	// ConfigurationChanged means the node configuration is out of sync with the ASG config
	ConfigurationChanged Reason = "configuration_changed"
)

// Reporter is responsible for storing and serving prometheus metrics
type Reporter struct {
	info                  map[string]GroupState
	seenStateReasonCombos map[Node]time.Time
	cacheMu               sync.Mutex
}

// Node represents the state of a node's deletion,
// and the reason why we want it deleted
type Node struct {
	State  string
	Reason Reason
}

// GroupState represents a group of nodes and their states
type GroupState struct {
	GroupName   string
	WantedNodes int
	Nodes       []Node
}

// New returns a new metrics reporter
func New() *Reporter {
	return &Reporter{
		info:                  make(map[string]GroupState),
		seenStateReasonCombos: make(map[Node]time.Time),
		cacheMu:               sync.Mutex{},
	}
}

// SetGroupState sets what the controller thinks is the state of the group
func (m *Reporter) SetGroupState(s map[string]GroupState) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	m.info = s
}

func (m *Reporter) generateMetrics() []*dto.MetricFamily {

	timeMs := int64(time.Now().Unix()) * 1000

	generateGaugeFamily := func(name, help string) *dto.MetricFamily {
		g := dto.MetricType_GAUGE
		return &dto.MetricFamily{
			Name:   &name,
			Help:   &help,
			Type:   &g,
			Metric: []*dto.Metric{},
		}
	}

	desiredFamily := generateGaugeFamily("nodereaper_instance_group_desired_size", "Desired number of nodes in the instance group")
	statesFamily := generateGaugeFamily("nodereaper_instance_group_state", "The number of nodes in a particular state of deletion")

	for groupName, group := range m.info {
		groupKey := "group"
		groupVal := groupName
		desired := float64(group.WantedNodes)

		desiredFamily.Metric = append(desiredFamily.Metric, &dto.Metric{
			Label: []*dto.LabelPair{
				&dto.LabelPair{Name: &groupKey, Value: &groupVal},
			},
			Gauge:       &dto.Gauge{Value: &desired},
			TimestampMs: &timeMs,
		})

		stateReasonCounts := map[string]map[Reason]int{}
		for _, node := range group.Nodes {
			if _, ok := stateReasonCounts[node.State]; !ok {
				stateReasonCounts[node.State] = make(map[Reason]int)
			}
			if _, ok := stateReasonCounts[node.State][node.Reason]; !ok {
				stateReasonCounts[node.State][node.Reason] = 0
			}
			stateReasonCounts[node.State][node.Reason]++
			m.seenStateReasonCombos[node] = time.Now()
		}

		for stateReason := range m.seenStateReasonCombos {
			if _, ok := stateReasonCounts[stateReason.State]; !ok {
				stateReasonCounts[stateReason.State] = map[Reason]int{}
			}
			if _, ok := stateReasonCounts[stateReason.State][stateReason.Reason]; !ok {
				stateReasonCounts[stateReason.State][stateReason.Reason] = 0
			}
			n := float64(stateReasonCounts[stateReason.State][stateReason.Reason])
			statesFamily.Metric = append(statesFamily.Metric, &dto.Metric{
				Label: []*dto.LabelPair{
					&dto.LabelPair{Name: &groupKey, Value: &groupVal},
					&dto.LabelPair{Name: s("state"), Value: s(stateReason.State)},
					&dto.LabelPair{Name: s("reason"), Value: s(string(stateReason.Reason))},
				},
				Gauge:       &dto.Gauge{Value: &n},
				TimestampMs: &timeMs,
			})
		}
	}

	// Clear really old state/reason combos. We keep them around to avoid
	// their last actual values lingering around in prometheus. But they should eventually die
	for combo, lastTime := range m.seenStateReasonCombos {
		if time.Now().Sub(lastTime) > 5*time.Minute {
			delete(m.seenStateReasonCombos, combo)
		}
	}

	out := []*dto.MetricFamily{}
	if len(desiredFamily.Metric) > 0 {
		out = append(out, desiredFamily)
	}
	if len(statesFamily.Metric) > 0 {
		out = append(out, statesFamily)
	}

	return out
}

// Handler returns metrics in response to an HTTP request
func (m *Reporter) Handler(rsp http.ResponseWriter, req *http.Request) {
	logrus.Trace("Serving prometheus metrics")
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()

	metrics := m.generateMetrics()
	contentType := expfmt.Negotiate(req.Header)
	header := rsp.Header()
	header.Set(contentTypeHeader, string(contentType))
	w := io.Writer(rsp)
	enc := expfmt.NewEncoder(w, contentType)

	var lastErr error
	for _, mf := range metrics {
		if err := enc.Encode(mf); err != nil {
			lastErr = err
			httpError(rsp, err)
			return
		}
	}

	if lastErr != nil {
		httpError(rsp, lastErr)
	}
}

func httpError(rsp http.ResponseWriter, err error) {
	rsp.Header().Del(contentEncodingHeader)
	http.Error(
		rsp,
		"An error has occurred while serving metrics:\n\n"+err.Error(),
		http.StatusInternalServerError,
	)
}

func s(ss string) *string {
	return &ss
}
