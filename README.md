# (Don't fear the) nodereaper [![GoDoc](https://godoc.org/github.com/wish/nodereaper?status.svg)](https://godoc.org/github.com/wish/nodereaper) [![Go Report Card](https://goreportcard.com/badge/github.com/wish/nodereaper)](https://goreportcard.com/report/github.com/wish/nodereaper)  [![Docker Repository on Quay](https://quay.io/repository/wish/nodereaper/status "Docker Repository on Quay")](https://quay.io/repository/wish/nodereaper)

Configurable controller & daemonset for gracefully terminating nodes.

## How it works

`nodereaper` consists of two parts: a controller (`nodereaper`) and a daemonset (`nodereaperd`).

The controller is responsible for coordinating deletions. The controller will schedule deletions so that they respect
`maxSurge` and `maxUnavailable`. The controller can be configured to delete nodes based on a variety of factors such as
node age, configration, and labels. When it decides to delete a node, it applies a label to it that causes `nodereaperd` to perform the
actual deletion.

The daemonset does nothing but watch the node on which it is running. When it sees the label that marks
it for deletion, it drains the node, applies a `NoExecute` taint to force the termination of most
daemonset pods, then calls `systemctl shutdown` on the underlying instance.

`nodereaper` assumes that your nodes are grouped into multiple "instance groups", each backed by a cloud-provider's version of this concept,
such as an AWS `AutoScalingGroup`. This should be the case if you are using `kops` to create your cluster.
`nodereaper` should work fine even if all of your nodes are in a single group.

## Configuration

### Command-line

`nodereaper` can be configured by the following command-line options:

Flag | Environment Variable | Type | Default | Required | Description
---- | -------------------- | ---- | ------- | -------- | -----------
`node-name` | `NODE_NAME` | `string` |  | yes | The name of the host node.
`log-level` | `LOG_LEVEL` | `string` | `info` | no | The level of log detail.
`bind-address` | `BIND_ADDRESS` | `string` | `:9656` | no | The address for binding metrics listener.
`poll-period` | `POLL_PERIOD` | `time.Duration` | `15s` | no | How often to check for deletion.
`namespace` | `NAMESPACE` | `string` | | yes | The namespace the controller resides in.
`lock-configmap-name` | `LOCK_CONFIGMAP_NAME` | `string` | `nodereaper-locks` | no | The controller will store state in a configmap named `$NAMESPACE/$LOCK_CONFIGMAP_NAME`.
`instance-group-label` | `INSTANCE_GROUP_LABEL` | `string` | | yes | The k8s label that specifies the group of the node.
`request-deletion-label` | `REQUEST_DELETION_LABEL` | `string` | `nodereaper.wish.com/request-delete` | no | The k8s label that requests the controller to safely delete the node.
`force-deletion-label` | `FORCE_DELETION_LABEL` | `string` | `nodereaper.wish.com/force-delete` | no | The k8s label that requests the daemonset to immediately delete the node.
`aws-poll-period` | `AWS_POLL_PERIOD` | `time.Duration` | `30s` | no | How often to query AWS for ASG information.
`aws-asg-filter` | `AWS_ASG_FILTER` | `string` | | no | Restrict the AWS ASGs that this tool considers based on tags. Comma separated map (e.g. `k1=v1,k2=v2`).
`aws-asg-name-tag` | `AWS_ASG_NAME_TAG` | `string` | | no | The tag on an AWS ASG that should be interpreted as its name. For every group, the value of this tag must match the value of `INSTANCE_GROUP_LABEL` for the nodes in the group.

### Configmap

All configmap configuration is hot-reloadable. Every setting in the table below can be specified both globally (as `global.$SETTING: value`) and per-group
(as `group.$GROUP_NAME.$SETTING: value`). The controller will first read the per-group setting, and fall back to the global setting if it doesn't exist.
The configmap must be mounted to the controller container at `/etc/config`.

Setting Name | Type | Default | Description
------------ | ---- | ------- | -----------
`maxSurge` | `int` or percentage | `1` | The maximum number of nodes that can be in the cluster beyond the desired amount for the group. Can be specified either as an absolute number (eg `2`) or as a percentage of the desired number (eg `7%`), which is rounded up to the nearest whole number.
`maxUnavailable` | `int` or percentage | `0` | The maximum number of nodes that can be in the cluster beyond the desired amount for the group. Can be specified either as an absolute number (eg `2`) or as a percentage of the desired number (eg `7%`), which is rounded down to the nearest whole number.
`deleteOldLaunchConfig` | `bool` | `false` | Whether to delete nodes with a different Launch Configuration than their group. With this set, `nodereaper` can perform the function of `kops rolling-update cluster` automatically after a change to configuration is made.
`deletionAge` | `*time.Duration` | `nil` | If set, the controller will delete any node older than this value.
`deletionAgeJitter` | `*time.Duration` | `nil` | If this is set, along with `deletionAge`, the controller will randomly delete nodes when their age is somewhere between `deletionAge` and `deletionAge + deletionAgeJitter`.
`deletionSchedule` | `*cron.Schedule` | `nil` | A crontab schedule defining when, in UTC (**not local time!**), nodes can be deleted (ex. `weekends from 6 to 8 pm` -> `* 18-20 * * 0,6`)
`startupGracePeriod` | `*time.Duration` | `nil` | Ignore nodes newer than this. Useful to allow time for new nodes to become `Ready`, schedule pods, etc before terminating more.
`ignoreSelector` | `string` | `kubernetes.io/role=master` | Ignore any node that matches this label selector. Ignored nodes still count towards group size, but they will never be deleted.
`ignore` | `bool` | `false` | Ignore every single node in the group (if specified per-group), or ignore every node in the cluster (if specified globally).


## Daemonset configuration

`nodereaperd` can be configured with the following command-line options:


Flag | Environment Variable | Type | Default | Required | Description
---- | -------------------- | ---- | ------- | -------- | -----------
`node-name` | `NODE_NAME` | `string` |  | yes | The name of the host node.
`log-level` | `LOG_LEVEL` | `string` | `info` | no | The level of log detail.
`force-deletion-label` | `FORCE_DELETION_LABEL` | `string` | `nodereaper.wish.com/force-delete` | no | The k8s label that requests the daemonset to immediately delete the node.
`dry-run` | `DRY_RUN` | `bool` | `false` | no | If set the daemonset will not actually perform any deletion steps, just log if it would have done so.

## IAM Permissions

The `nodereaperd` daemonset requires no IAM permissions. The `nodereaper` controller requires the following permissions:

- `autoscaling:DescribeAutoScalingGroups`
- `autoscaling:DetachInstances`
- `ec2:ModifyInstanceAttribute`
- `ec2:DescribeLaunchTemplates`

The needed k8s RBAC permissions can be found in the `deploy` folder.

## Limitations

Right now, `nodereaper` works in AWS only. It should be very easy to add other cloud providers, or bare metal, by implmenting the `APIProvider` interface in `deletion.go`. PRs are welcome!

Be very careful about enabling nodereaper on the k8s master nodes. By default, `ignoreSelector` is set globally to ignore any masters. `nodereaper` should
be able to safely restart masters in a multi-master (HA) cluster if they are grouped together in their own group. However if `maxSurge`/`maxUnavailable` are not set correctly, `nodereaper` may cause control plane downtime.



