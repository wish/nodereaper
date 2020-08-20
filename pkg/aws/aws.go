package aws

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/sirupsen/logrus"
	"github.com/wish/nodereaper/pkg/config"
	core_v1 "k8s.io/api/core/v1"
)

// APIProvider handles AWS specific logic
type APIProvider struct {
	client                    *autoscaling.AutoScaling
	ec2Client                 *ec2.EC2
	filters                   map[string]string
	nameTag                   string
	cacheMu                   *sync.Mutex
	asgCache                  []*asg
	nodeInstanceConfiguration map[string]*string
	pollPeriod                time.Duration
}

// NewAPIProvider creates an AWS api instance
func NewAPIProvider(pollPeriod time.Duration, filters map[string]string, nameTag string) (*APIProvider, error) {
	sess := session.Must(session.NewSession())
	provider := &APIProvider{
		client:                    autoscaling.New(sess),
		ec2Client:                 ec2.New(sess),
		filters:                   filters,
		nameTag:                   nameTag,
		cacheMu:                   &sync.Mutex{},
		asgCache:                  make([]*asg, 0),
		nodeInstanceConfiguration: make(map[string]*string),
		pollPeriod:                pollPeriod,
	}
	return provider, nil
}

// Run starts the polling loop that pulls information about the AWS ASGs
func (d *APIProvider) Run(stopCh <-chan struct{}) {
	d.sync()
	go wait.Until(func() {
		d.sync()
	}, d.pollPeriod, stopCh)
}

// Sync queries the AWS API to fetch the asgs and instances in the cluster
func (d *APIProvider) sync() {
	logrus.Tracef("Syncing AWS cache")
	newAsgs, err := getAsgs(d.client, d.ec2Client, d.filters, d.nameTag)
	if err != nil {
		logrus.Errorf("Could not update AWS ASG cache: %v", err)
		return
	}
	d.cacheMu.Lock()
	d.asgCache = newAsgs

	for _, asg := range newAsgs {
		for _, instance := range asg.Instances {
			if instance.InstanceId != nil {
				if instance.LaunchConfigurationName != nil {
					d.nodeInstanceConfiguration[*instance.InstanceId] = instance.LaunchConfigurationName
				} else if instance.LaunchTemplate != nil {
					launchTemplate := fmt.Sprintf("%v-%v", *instance.LaunchTemplate.LaunchTemplateId, *instance.LaunchTemplate.Version)
					d.nodeInstanceConfiguration[*instance.InstanceId] = &launchTemplate
				}
			}
		}
	}

	detachedInstances := getDetachedInstances(d.ec2Client, d.filters)
	for _, detachedInstance := range detachedInstances {
		//Delete all detached instances
		d.nodeInstanceConfiguration[*detachedInstance.InstanceId] = nil
	}

	d.cacheMu.Unlock()
	logrus.Tracef("Finished syncing AWS cache")
}

// DesiredGroupSize returns the size that the instanceGroup (ASG in AWS) should be.
// The deletion controller shouldn't delete a node whose instanceGroup is already depleted
func (d *APIProvider) DesiredGroupSize(groupName string) (int, error) {
	d.cacheMu.Lock()
	defer d.cacheMu.Unlock()
	for _, group := range d.asgCache {
		if group.Name == groupName {
			return int(*group.DesiredCapacity), nil
		}
	}

	return 0, fmt.Errorf("Could not find ASG with name %v", groupName)
}

// OutdatedLaunchConfig checks if a node has become outdated compared to the ASG configuration
func (d *APIProvider) OutdatedLaunchConfig(opts *config.Ops, node *core_v1.Node) (bool, error) {
	d.cacheMu.Lock()
	defer d.cacheMu.Unlock()

	if node.Labels[opts.InstanceGroupLabel] == "" {
		return false, nil
	}

	groupLaunchConfig := ""
	for _, group := range d.asgCache {
		if node.Labels[opts.InstanceGroupLabel] == group.Name {
			groupLaunchConfig = group.LaunchVersion
			break
		}
	}
	if groupLaunchConfig == "" {
		return false, fmt.Errorf("Could not find asg for node %v named '%v'", node.Name, node.Labels[opts.InstanceGroupLabel])
	}

	instanceID, err := nodeInstanceID(node)
	if err != nil {
		return false, err
	}

	config, exists := d.nodeInstanceConfiguration[instanceID]
	if !exists {
		return false, fmt.Errorf("Node %v (ID %v)'s instance config could not be found", node.Name, instanceID)
	}
	// nil config means that the node's launch config is so old that it has been deleted.
	//  So it's definitely out of sync
	if config == nil || groupLaunchConfig != *config {
		return true, nil
	}

	return false, nil
}

// PreDrain removes the node from its ASG
// and sets the delete behavior to terminate, instead of stop
func (d *APIProvider) PreDrain(opts *config.Ops, node *core_v1.Node) error {
	// Get the node instance ID
	id, err := nodeInstanceID(node)
	if err != nil {
		return fmt.Errorf("Could not get instance-id for node %v: %v", node.Name, err)
	}

	// Find the asg of the node
	var nodeGroup *asg
	for _, group := range d.asgCache {
		if node.Labels[opts.InstanceGroupLabel] == group.Name {
			nodeGroup = group
			break
		}
	}
	if nodeGroup == nil {
		return fmt.Errorf("Could not find ASG for node %v", node.Name)
	}

	// Make sure that when nodereaperd shuts down the node, it is actually terminated
	// as opposed to just stopped
	behavior := "terminate"
	_, err = d.ec2Client.ModifyInstanceAttribute(&ec2.ModifyInstanceAttributeInput{
		InstanceId: &id,
		InstanceInitiatedShutdownBehavior: &ec2.AttributeValue{
			Value: &behavior,
		},
	})
	if err != nil {
		return fmt.Errorf("Error setting shutdown behaviour for node %v (%v): %v", node.Name, id, err)
	}
	logrus.Infof("Set shutdown behaviour for %v", node.Name)
	return nil
}

func (d *APIProvider) DetachNode(opts *config.Ops, node *core_v1.Node) error {
	// Get the node instance ID
	id, err := nodeInstanceID(node)
	if err != nil {
		return fmt.Errorf("Could not get instance-id for node %v: %v", node.Name, err)
	}

	// Find the asg of the node
	var nodeGroup *asg
	for _, group := range d.asgCache {
		if node.Labels[opts.InstanceGroupLabel] == group.Name {
			nodeGroup = group
			break
		}
	}
	if nodeGroup == nil {
		return fmt.Errorf("Could not find ASG for node %v", node.Name)
	}

	// Detatch the node from the ASG. This should cause the autoscaler to spin up a new node to replace it
	decrementAsgCapacity := false
	_, err = d.client.DetachInstances(&autoscaling.DetachInstancesInput{
		AutoScalingGroupName: nodeGroup.AutoScalingGroupName,
		InstanceIds: []*string{
			&id,
		},
		ShouldDecrementDesiredCapacity: &decrementAsgCapacity,
	})
	if err != nil {
		return fmt.Errorf("Error detaching node %v (%v) from ASG %v: %v", node.Name, id, nodeGroup.AutoScalingGroupName, err)
	}
	logrus.Infof("Detached %v from ASG", node.Name)
	return nil

}

func nodeInstanceID(node *core_v1.Node) (string, error) {
	parts := strings.Split(node.Spec.ProviderID, "/")
	if len(parts) != 5 || parts[0] != "aws:" {
		return "", fmt.Errorf("Could not parse instanceid '%v' for node %v", node.Spec.ProviderID, node.Name)
	}
	return parts[4], nil
}

// Asg represents an AWS AutoScalingGroup
type asg struct {
	autoscaling.Group
	Name           string
	Tags           map[string]string
	InstanceStatus map[string]int

	// Custom string to determine if launch config or launch template matches expectations
	LaunchVersion string
}

// GetAsgs gets the AutoScalingGroups that match the given filters
func getAsgs(svc *autoscaling.AutoScaling, svcEC2 *ec2.EC2, filter map[string]string, nametag string) ([]*asg, error) {

	input := &autoscaling.DescribeAutoScalingGroupsInput{}
	groups := []*asg{}

	err := svc.DescribeAutoScalingGroupsPages(input,
		func(page *autoscaling.DescribeAutoScalingGroupsOutput, lastPage bool) bool {
		loop:
			for _, group := range page.AutoScalingGroups {
				a, err := convertGroup(group)
				if err != nil {
					return false
				}

				for fk, fv := range filter {
					tagv, ok := a.Tags[fk]
					if !ok {
						continue loop
					}
					if tagv != fv {
						continue loop
					}
				}
				if nametag != "" {
					v, ok := a.Tags[nametag]
					if ok {
						a.Name = v
					}
				}
				groups = append(groups, a)
			}
			return true
		})

	if err != nil {
		return nil, err
	}

	launchTempIds := []*string{}
	for _, group := range groups {
		if group.MixedInstancesPolicy != nil && group.MixedInstancesPolicy.LaunchTemplate != nil {
			launchTempIds = append(launchTempIds, group.MixedInstancesPolicy.LaunchTemplate.LaunchTemplateSpecification.LaunchTemplateId)
		}
		if group.LaunchTemplate != nil {
			launchTempIds = append(launchTempIds, group.LaunchTemplate.LaunchTemplateId)
		}
	}

	canonicalLaunchTemps := make(map[string]string)
	if len(launchTempIds) > 0 {
		err = svcEC2.DescribeLaunchTemplatesPages(&ec2.DescribeLaunchTemplatesInput{
			LaunchTemplateIds: launchTempIds,
		}, func(page *ec2.DescribeLaunchTemplatesOutput, lastPage bool) bool {
			for _, launchTemplate := range page.LaunchTemplates {
				if launchTemplate.LaunchTemplateId == nil {
					return false
				}
				if launchTemplate.DefaultVersionNumber == nil {
					return false
				}

				id := *launchTemplate.LaunchTemplateId
				canonicalLaunchTemps[id] = fmt.Sprintf("%v-%v", id, *launchTemplate.DefaultVersionNumber)
			}
			return true
		})
		if err != nil {
			return nil, err
		}
	}

	for _, group := range groups {
		if group.LaunchConfigurationName != nil {
			group.LaunchVersion = *group.LaunchConfigurationName
			continue
		}
		if group.MixedInstancesPolicy != nil && group.MixedInstancesPolicy.LaunchTemplate != nil {
			group.LaunchVersion = canonicalLaunchTemps[*group.MixedInstancesPolicy.LaunchTemplate.LaunchTemplateSpecification.LaunchTemplateId]
			continue
		}
		if group.LaunchTemplate != nil {
			group.LaunchVersion = canonicalLaunchTemps[*group.LaunchTemplate.LaunchTemplateId]
			continue
		}
	}

	return groups, nil
}

func convertGroup(g *autoscaling.Group) (*asg, error) {
	a := &asg{
		*g,
		*g.AutoScalingGroupName,
		make(map[string]string),
		make(map[string]int),
		"",
	}
	for _, tag := range g.Tags {
		a.Tags[*tag.Key] = *tag.Value
	}
	for _, inst := range g.Instances {
		v, ok := a.InstanceStatus[*inst.HealthStatus]
		if !ok {
			a.InstanceStatus[*inst.HealthStatus] = 1
		} else {
			a.InstanceStatus[*inst.HealthStatus] = v + 1
		}
	}
	return a, nil
}

func getDetachedInstances(svcEC2 *ec2.EC2, filter map[string]string) []*ec2.Instance {
	detachedInstances := []*ec2.Instance{}
	input := &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{},
	}

	input.Filters = append(input.Filters, &ec2.Filter{
		Name: aws.String("instance-state-name"),
		Values: []*string{
			aws.String("running"),
		},
	})

	for fkey, fvalue := range filter {
		input.Filters = append(input.Filters, &ec2.Filter{
			Name: aws.String(fmt.Sprintf("tag:%s", fkey)),
			Values: []*string{
				&fvalue,
			},
		})
	}

	svcEC2.DescribeInstancesPages(input,
		func(page *ec2.DescribeInstancesOutput, lastPage bool) bool {
			for _, res := range page.Reservations {
			instanceLoop:
				for _, instance := range res.Instances {
					// Check that it's not attached to an asg
					for _, tag := range instance.Tags {
						if *tag.Key == "aws:autoscaling:groupName" {
							continue instanceLoop
						}
					}
					detachedInstances = append(detachedInstances, instance)
				}
			}
			return true
		})
	return detachedInstances
}
