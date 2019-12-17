package configmap

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/sirupsen/logrus"
)

type LeaderLease struct {
	configmap *ConfigMap
	key       string
	myID      string
}

type lease struct {
	Leader        string   `json:"leader"`
	LastLeaseTime jsonTime `json:"lastLeaseTime"`
}

type jsonTime struct {
	time.Time
}

func (t *jsonTime) MarshalJSON() ([]byte, error) {
	return []byte("\"" + t.Format(time.RFC3339) + "\""), nil
}

func (t *jsonTime) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), "\"")
	pt, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return err
	}
	t.Time = pt
	return nil
}

func NewLeaderLease(cm *ConfigMap, leaseKey, myID string) *LeaderLease {
	return &LeaderLease{
		cm,
		leaseKey,
		myID,
	}
}

func (l *LeaderLease) ManageLease(stopCh <-chan struct{}) {
	wait.Until(func() {
		good, err := l.TryAcquireLease()
		if err != nil || !good {
			logrus.Errorf("Could not refresh leader lease (%v): %v", good, err)
		}
	}, 15*time.Second, stopCh)
}

func (l *LeaderLease) TryAcquireLease() (bool, error) {
	leaseString, err := l.configmap.Load(l.key)
	if err != nil {
		return false, err
	}

	leaseVal := lease{}
	if leaseString != nil {
		err := json.Unmarshal([]byte(*leaseString), &leaseVal)
		if err != nil {
			return false, fmt.Errorf("Error reading leader lease: %v", err)
		}
	}

	// Handle new lease or refreshing lease
	if leaseVal.Leader == "" || leaseVal.Leader == l.myID {
		err := l.writeLease()
		return err == nil, err
	}

	// Handle expired lease
	if time.Now().Sub(leaseVal.LastLeaseTime.Time) > 1*time.Minute {
		logrus.Infof("Old leader lease (id %v) expired. Taking over", leaseVal.Leader)
		err := l.writeLease()
		return err == nil, err
	}

	logrus.Warnf("Different leader still active (%v). Could not get lease", leaseVal.Leader)
	return false, nil
}

func (l *LeaderLease) writeLease() error {
	leaseVal := lease{
		l.myID,
		jsonTime{time.Now()},
	}
	o, err := json.Marshal(&leaseVal)
	if err != nil {
		return err
	}
	logrus.Tracef("Writing %v", string(o))
	s := string(o)
	err = l.configmap.Store(l.key, &s)
	if err != nil {
		return fmt.Errorf("Error writing leader lease: %v", err)
	}
	return nil
}
