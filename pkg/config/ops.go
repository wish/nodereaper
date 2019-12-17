package config

import (
	"fmt"
	"strconv"
	"time"
)

// Ops represents the commandline/environment options for the program
type Ops struct {
	DynamicConfig
	NodeName             string `long:"node-name" env:"NODE_NAME" description:"The name of the host node" required:"yes"`
	LogLevel             string `long:"log-level" env:"LOG_LEVEL" description:"Log level" default:"info"`
	BindAddr             string `long:"bind-address" short:"p" env:"BIND_ADDRESS" default:":9656" description:"address for binding metrics listener"`
	PollPeriod           string `long:"poll-period" env:"POLL_PERIOD" description:"Check for deletion every period (5s, 3m, 1h, ...)" default:"15s"`
	AwsPollPeriod        string `long:"aws-poll-period" env:"AWS_POLL_PERIOD" description:"Update aws state every period" default:"30s"`
	InstanceGroupLabel   string `long:"instance-group-label" env:"INSTANCE_GROUP_LABEL" description:"The node label whose value is the name of the instance group"`
	RequestDeletionLabel string `long:"request-deletion-label" env:"REQUEST_DELETION_LABEL" description:"Delete this node if it has this label"`
	ForceDeletionLabel   string `long:"force-deletion-label" env:"FORCE_DELETION_LABEL" description:"The controller sets this label to force a node to delete itself" required:"true"`
	AwsAsgFilter         string `long:"aws-asg-filter" env:"AWS_ASG_FILTER" description:"Restrict the AWS ASGs that this tool considers. Comma separated map (e.g. k1=v1,k2=v2)"`
	AwsAsgNameTag        string `long:"aws-asg-name-tag" env:"AWS_ASG_NAME_TAG" description:"The tag on an ASG that should be interpreted as its name"`
	Namespace            string `long:"namespace" env:"NAMESPACE" description:"The namespace the controller resides in" required:"true"`
	LockConfigMapName    string `long:"lock-configmap-name" env:"LOCK_CONFIGMAP_NAME" description:"The name of the configmap to store locks" default:"nodereaper-locks"`
}

// ParseDuration parses the exact same duration values as time.ParseDuration
// with the addition of 'd' (day) values, like "30d"
func ParseDuration(duration string) (time.Duration, error) {
	if duration[len(duration)-1:] == "d" {
		num, err := strconv.ParseInt(duration[:len(duration)-1], 10, 64)
		if err != nil {
			var ret time.Duration
			return ret, fmt.Errorf("Error parsing '%v' as number of days: %v", duration, err)
		}
		duration = fmt.Sprintf("%vh", num*24)
	}

	return time.ParseDuration(duration)
}
