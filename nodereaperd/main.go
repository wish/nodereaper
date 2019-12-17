package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	drain "github.com/openshift/kubernetes-drain"
	"github.com/wish/nodereaper/pkg/controller"

	flags "github.com/jessevdk/go-flags"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/sirupsen/logrus"

	core_v1 "k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	deletionTaintName = "NodereaperDeletingNode"
)

type ops struct {
	NodeName      string `long:"node-name" env:"NODE_NAME" description:"The name of the host node" required:"yes"`
	LogLevel      string `long:"log-level" env:"LOG_LEVEL" description:"Log level" default:"info"`
	DeletionLabel string `long:"force-deletion-label" env:"FORCE_DELETION_LABEL" description:"Delete this node if it has this label"`
	DryRun        bool   `long:"dry-run" env:"DRY_RUN" description:"Don't actually perform deletions if true"`
}

type wrappedLogger struct {
	logger *logrus.Logger
}

func (l *wrappedLogger) Log(v ...interface{}) {
	l.logger.Info(v...)
}

func (l *wrappedLogger) Logf(format string, v ...interface{}) {
	l.logger.Infof(format, v...)
}

func setupLogging(logLevel string) {
	// Use log level
	level, err := logrus.ParseLevel(logLevel)
	if err != nil {
		logrus.Fatalf("Unknown log level %s: %v", logLevel, err)
	}
	logrus.SetLevel(level)

	// Set the log format to have a reasonable timestamp
	formatter := &logrus.TextFormatter{
		FullTimestamp: true,
	}
	logrus.SetFormatter(formatter)
}

func getClientset() (*kubernetes.Clientset, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return clientset, nil
}

func shouldShutdown(opts *ops, node *core_v1.Node) bool {
	logrus.Trace("Checking if shutdown is needed")

	// Delete the node if it is labeled for deletion
	if opts.DeletionLabel != "" {
		for label := range node.Labels {
			if label == opts.DeletionLabel {
				logrus.Infof("Node %v has deletion label %v", node.Name, opts.DeletionLabel)
				return true
			}
		}
	}

	return false
}

func drainNode(opts *ops, clientset *kubernetes.Clientset) error {
	logrus.Infof("Attempting shutdown of node %v", opts.NodeName)

	// Drain the node of non-daemonset pods
	node, err := clientset.CoreV1().Nodes().Get(opts.NodeName, meta_v1.GetOptions{})
	if err != nil {
		return fmt.Errorf("Error fetching node %v for deletion: %v", opts.NodeName, err)
	}
	err = drain.Drain(clientset, []*core_v1.Node{
		node,
	}, &drain.DrainOptions{
		Force:            true,
		IgnoreDaemonsets: true,
		Timeout:          2 * time.Minute,
		DeleteLocalData:  true,
		Logger:           &wrappedLogger{logrus.StandardLogger()},
	})
	if err != nil {
		return fmt.Errorf("Error draining pods from node %v: %v", opts.NodeName, err)
	}

	// Add NoExecute taint to gracefully remove DaemonSet pods
	node, err = clientset.CoreV1().Nodes().Get(opts.NodeName, meta_v1.GetOptions{})
	if err != nil {
		return fmt.Errorf("Error fetching node %v for deletion: %v", opts.NodeName, err)
	}

	alreadyHasDeletionTaint := false
	for _, taint := range node.Spec.Taints {
		if taint.Key == deletionTaintName {
			alreadyHasDeletionTaint = true
			break
		}
	}

	if !alreadyHasDeletionTaint {
		node.Spec.Taints = append(node.Spec.Taints, core_v1.Taint{
			Key:    deletionTaintName,
			Value:  "true",
			Effect: "NoExecute",
		})
		_, err := clientset.CoreV1().Nodes().Update(node)
		if err != nil {
			return fmt.Errorf("Error adding taint to node %v: %v", opts.NodeName, err)
		}
		logrus.Infof("Applied deletion taint to node %v", node.Name)
	}

	err = waitForPodTermination(clientset, node.Name)
	if err != nil {
		return err
	}

	return nil
}

func waitForPodTermination(clientset *kubernetes.Clientset, nodeName string) error {
	for {
		time.Sleep(10 * time.Second)
		podsOnNode, err := clientset.CoreV1().Pods("").List(meta_v1.ListOptions{
			FieldSelector: fmt.Sprintf("spec.nodeName=%v", nodeName),
		})
		if err != nil {
			return fmt.Errorf("Error waiting for node %v to drain: %v", nodeName, err)
		}

		numTerminatingPodsOnNode := 0
		for _, pod := range podsOnNode.Items {
			if pod.DeletionTimestamp != nil {
				numTerminatingPodsOnNode++
			}
		}
		if numTerminatingPodsOnNode == 0 {
			break
		} else {
			logrus.Infof("Still terminating %v pods on %v", numTerminatingPodsOnNode, nodeName)
		}
	}
	logrus.Infof("Successfully drained all drainable pods from %v", nodeName)
	return nil
}

func deleteK8sNode(clientset *kubernetes.Clientset, nodeName string) error {
	err := clientset.CoreV1().Nodes().Delete(nodeName, &meta_v1.DeleteOptions{})
	if err != nil {
		return err
	}
	logrus.Infof("Successfully deleted node %v from kubernetes", nodeName)
	return nil
}

func runShutdownCommand() error {
	logrus.Info("Attempting shutdown of node")
	cmd := exec.Command("/usr/bin/nsenter", "-m/proc/1/ns/mnt", "/bin/systemctl", "poweroff")
	cmd.Stdout = logrus.NewEntry(logrus.StandardLogger()).WriterLevel(logrus.InfoLevel)
	cmd.Stderr = logrus.NewEntry(logrus.StandardLogger()).WriterLevel(logrus.WarnLevel)
	return cmd.Run()
}

func tryDelete(opts *ops, clientset *kubernetes.Clientset, node *core_v1.Node) bool {
	if shouldShutdown(opts, node) {
		if opts.DryRun {
			logrus.Infof("Would delete node if --dry-run/DRY_RUN was not true")
			return false
		}

		err := drainNode(opts, clientset)
		if err != nil {
			logrus.Errorf("Error draining node: %v", err)
			return false
		}

		err = deleteK8sNode(clientset, opts.NodeName)
		if err != nil {
			logrus.Errorf("Node was drained successfully but could not be deleted from k8s: %v", err)
			return false
		}

		err = runShutdownCommand()
		if err != nil {
			logrus.Errorf("Node was drained successfully but could not be shutdown: %v", err)
			return false
		}

		// If we got this far, prepare to be deleted
		return true
	}
	return false
}

func main() {
	opts := &ops{}
	parser := flags.NewParser(opts, flags.Default)
	if _, err := parser.Parse(); err != nil {
		// If the error was from the parser, then we can simply return
		// as Parse() prints the error already
		if _, ok := err.(*flags.Error); ok {
			os.Exit(1)
		}

		logrus.Fatalf("Error parsing flags: %v", err)
	}
	setupLogging(opts.LogLevel)

	clientset, err := getClientset()
	if err != nil {
		logrus.Fatalf("Failed to create k8s clientset: %v", err)
	}

	// Handle termination
	stopCh := make(chan struct{})
	defer close(stopCh)

	isDeleted := false
	isHandling := sync.Mutex{}
	upFunc := func(node *core_v1.Node) {
		isHandling.Lock()
		defer isHandling.Unlock()
		if !isDeleted {
			isDeleted = tryDelete(opts, clientset, node)
		}
	}
	c, err := controller.NewController(&opts.NodeName, &upFunc)
	if err != nil {
		logrus.Fatalf("Error creating node watcher: %v", err)
	}
	c.Run(stopCh)

	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGTERM)
	signal.Notify(sigterm, syscall.SIGINT)
	<-sigterm

	logrus.Infof("Received SIGTERM or SIGINT. Shutting down.")
}
