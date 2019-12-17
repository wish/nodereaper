package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wish/nodereaper/pkg/configmap"

	_ "net/http/pprof"

	flags "github.com/jessevdk/go-flags"

	"github.com/sirupsen/logrus"
	"github.com/wish/nodereaper/pkg/aws"
	"github.com/wish/nodereaper/pkg/config"
	"github.com/wish/nodereaper/pkg/controller"
	"github.com/wish/nodereaper/pkg/deletion"
	"github.com/wish/nodereaper/pkg/metrics"
)

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

func parseKvList(s string) map[string]string {
	filter := map[string]string{}
	for _, item := range strings.Split(s, ",") {
		if !strings.Contains(item, "=") {
			continue
		} else {
			spl := strings.Split(item, "=")
			filter[spl[0]] = spl[1]
		}
	}
	return filter
}

func main() {
	opts := &config.Ops{}
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

	// Validate poll period
	if opts.PollPeriod != "" {
		_, err := config.ParseDuration(opts.PollPeriod)
		if err != nil {
			logrus.Fatalf("Error parsing poll period: %v", err)
		}
	}

	// Validate aws period
	if opts.AwsPollPeriod != "" {
		_, err := config.ParseDuration(opts.AwsPollPeriod)
		if err != nil {
			logrus.Fatalf("Error parsing AWS poll period: %v", err)
		}
	}

	logrus.Info("Starting controller...")

	// Handle termination
	stopCh := make(chan struct{})
	srv := &http.Server{
		Addr: opts.BindAddr,
	}

	defer srv.Shutdown(context.Background())
	defer close(stopCh)

	// Controller watches nodes for changes
	c, err := controller.NewController(nil, nil)
	if err != nil {
		logrus.Fatalf("Error creating controller: %v", err)
	}

	// Prometheus metrics
	metrics := metrics.New()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "OK\n")
	})
	http.HandleFunc("/healthcheck", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "OK\n")
	})
	http.HandleFunc("/metrics", metrics.Handler)
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			logrus.Errorf("Error serving HTTP at %v: %v", opts.BindAddr, err)
		}
	}()

	locks, err := configmap.New(c.Clientset, opts.Namespace, opts.LockConfigMapName)
	if err != nil {
		logrus.Fatalf("Error creating locks configmap: %v", err)
	}

	randomID := int(time.Now().UnixNano() % 9999999)
	leaderLease := configmap.NewLeaderLease(locks, "leader", opts.NodeName+"_"+strconv.Itoa(randomID))
	for {
		logrus.Info("Trying to acquire leader lease")
		got, err := leaderLease.TryAcquireLease()
		if !got || err != nil {
			logrus.Warnf("Could not acquire leader lease: %v", err)
		} else {
			break
		}
		time.Sleep(10 * time.Second)
	}
	logrus.Infof("Got leader lease")
	go leaderLease.ManageLease(stopCh)

	awsPollPeriod, _ := config.ParseDuration(opts.AwsPollPeriod)
	// APIProvider handles cloud-specific info and actions
	provider, err := aws.NewAPIProvider(awsPollPeriod, parseKvList(opts.AwsAsgFilter), opts.AwsAsgNameTag)
	if err != nil {
		logrus.Fatalf("Error creating AWS informer: %v", err)
	}

	// The thing that actually performs the deletion
	deleter := deletion.New(opts, c, provider, locks, metrics)

	c.Run(stopCh)
	provider.Run(stopCh)
	deleter.Run(stopCh)

	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGTERM)
	signal.Notify(sigterm, syscall.SIGINT)
	<-sigterm

	logrus.Infof("Received SIGTERM or SIGINT. Shutting down.")

}
