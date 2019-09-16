/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"

	ddpkubernetes "github.com/AmitKumarDas/storage-provisioner/pkg/client/generated/clientset/versioned"
	ddpinformers "github.com/AmitKumarDas/storage-provisioner/pkg/client/generated/informers/externalversions"
	"github.com/AmitKumarDas/storage-provisioner/pkg/storage"
	"github.com/kubernetes-csi/csi-lib-utils/leaderelection"
)

const (
	leaderElectionTypeLeases     = "leases"
	leaderElectionTypeConfigMaps = "configmaps"

	controllerName = "ddp-storage-provisioner"
)

// Command line flags
var (
	kubeconfig = flag.String(
		"kubeconfig", "",
		`Absolute path to the kubeconfig file. 
		Required only when running outside of cluster.`,
	)

	resync = flag.Duration(
		"resync", 10*time.Minute,
		"Resync interval of the controller.",
	)

	showVersion = flag.Bool("version", false, "Shows storage-provisioner's version.")

	timeout = flag.Duration(
		"timeout", 15*time.Second,
		"Timeout for waiting for attaching or detaching the volume.",
	)

	workerThreads = flag.Uint(
		"worker-threads", 10,
		"Number of storage provisioner worker threads",
	)

	retryIntervalStart = flag.Duration(
		"retry-interval-start", time.Second,
		`Initial retry interval of failed create volume or delete volume. 
		It doubles with each failure, up to retry-interval-max.`,
	)

	retryIntervalMax = flag.Duration(
		"retry-interval-max", 5*time.Minute,
		"Maximum retry interval of failed create volume or delete volume.",
	)

	enableLeaderElection = flag.Bool(
		"leader-election", false,
		"Enable leader election.",
	)

	leaderElectionNamespace = flag.String(
		"leader-election-namespace", "",
		`Namespace where the leader election resource lives. 
		Defaults to this pod namespace if not set.`,
	)
)

var (
	version = "unknown"
)

type leaderElection interface {
	Run() error
	WithNamespace(namespace string)
}

func main() {
	klog.InitFlags(nil)
	flag.Set("logtostderr", "true")
	flag.Parse()

	if *showVersion {
		fmt.Println(os.Args[0], version)
		return
	}
	klog.Infof("Version: %s", version)

	// Create the kubernetes client config.
	// Use kubeconfig if given, otherwise assume in-cluster.
	config, err := buildConfig(*kubeconfig)
	if err != nil {
		klog.Error(err.Error())
		os.Exit(1)
	}

	if *workerThreads == 0 {
		klog.Error("option -worker-threads must be greater than zero")
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Error(err.Error())
		os.Exit(1)
	}

	ddpClientset, err := ddpkubernetes.NewForConfig(config)
	if err != nil {
		klog.Error(err.Error())
		os.Exit(1)
	}

	factory := informers.NewSharedInformerFactory(clientset, *resync)
	ddpFactory := ddpinformers.NewSharedInformerFactory(ddpClientset, *resync)

	storageQ := workqueue.NewNamedRateLimitingQueue(
		workqueue.NewItemExponentialFailureRateLimiter(*retryIntervalStart, *retryIntervalMax),
		"ddp-storage-q",
	)
	pvcQ := workqueue.NewNamedRateLimitingQueue(
		workqueue.NewItemExponentialFailureRateLimiter(*retryIntervalStart, *retryIntervalMax),
		"ddp-pvc-q",
	)

	// create a new instance of storage controller
	ctrl := &storage.Controller{
		Name:               controllerName,
		InformerFactory:    factory,
		DDPInformerFactory: ddpFactory,
		StorageQueue:       storageQ,
		PVCQueue:           pvcQ,
	}

	// initialize the controller before running
	err = ctrl.Init()
	if err != nil {
		klog.Error(err.Error())
		os.Exit(1)
	}

	// define the run func
	run := func(ctx context.Context) {
		// create a stop channel & pass this wherever needed
		stopCh := ctx.Done()
		factory.Start(stopCh)
		ddpFactory.Start(stopCh)

		// run the storage controller
		ctrl.Run(int(*workerThreads), stopCh)
	}

	if !*enableLeaderElection {
		run(context.TODO())
	} else {
		// Name of config map with leader election lock
		lockName := controllerName + "-leader"
		le := leaderelection.NewLeaderElection(clientset, lockName, run)

		if *leaderElectionNamespace != "" {
			le.WithNamespace(*leaderElectionNamespace)
		}

		if err := le.Run(); err != nil {
			klog.Fatalf("failed to initialize leader election: %v", err)
		}
	}
}

func buildConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}
