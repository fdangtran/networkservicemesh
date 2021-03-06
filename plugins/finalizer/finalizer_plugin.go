// Copyright (c) 2018 Cisco and/or its affiliates.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package finalizer

import (
	"sync"
	"time"

	"github.com/ligato/cn-infra/health/statuscheck"
	"github.com/ligato/networkservicemesh/plugins/k8sclient"
	"github.com/ligato/networkservicemesh/plugins/logger"
	"github.com/ligato/networkservicemesh/plugins/objectstore"
	"github.com/ligato/networkservicemesh/utils/command"
	"github.com/ligato/networkservicemesh/utils/helper/deptools"
	"github.com/ligato/networkservicemesh/utils/helper/plugintools"
	"github.com/ligato/networkservicemesh/utils/idempotent"
	"github.com/ligato/networkservicemesh/utils/registry"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const (
	// Label to select pods treated by NSM controller
	nsmLabel     = "networkservicemesh.io"
	nsmAppLabel  = "networkservicemesh.io/app"
	nsmAppNSE    = "nse"
	nsmAppClient = "nsm-client"
)

// Plugin watches K8s resources and causes all changes to be reflected in the ETCD
// data store.
type Plugin struct {
	idempotent.Impl
	Deps

	pluginStopCh chan struct{}
	wg           sync.WaitGroup

	StatusMonitor statuscheck.StatusReader

	stopCh   chan struct{}
	informer cache.SharedIndexInformer
}

// Deps defines dependencies of CRD plugin.
type Deps struct {
	Name string
	Log  logger.FieldLoggerPlugin
	// Kubeconfig with k8s cluster address and access credentials to use.
	KubeConfig  string `empty_value_ok:"true"`
	ObjectStore objectstore.Interface
	K8sclient   k8sclient.API
}

// Init builds K8s client-set based on the supplied kubeconfig and initializes
// all reflectors.
func (plugin *Plugin) Init() error {
	return plugin.IdempotentInit(plugintools.LoggingInitFunc(plugin.Log, plugin, plugin.init))
}

func (plugin *Plugin) init() error {
	plugin.pluginStopCh = make(chan struct{})
	err := deptools.Init(plugin)
	if err != nil {
		return err
	}
	plugin.KubeConfig = command.RootCmd().Flags().Lookup(KubeConfigFlagName).Value.String()

	plugin.Log.WithField("kubeconfig", plugin.KubeConfig).Info("Loading kubernetes client config")

	plugin.stopCh = make(chan struct{})

	return plugin.afterInit()
}

func setupInformer(informer cache.SharedIndexInformer, queue workqueue.RateLimitingInterface) {
	informer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			DeleteFunc: func(obj interface{}) {
				queue.Add(obj)
			},
		},
	)
}

func (plugin *Plugin) afterInit() error {
	var err error

	err = nil
	if err != nil {
		plugin.Log.Error("Error initializing Finalizer plugin")
		return err
	}

	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	plugin.informer = cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				options.LabelSelector = nsmLabel + "=true"
				return plugin.K8sclient.GetClientset().CoreV1().Pods(metav1.NamespaceAll).List(options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				options.LabelSelector = nsmLabel + "=true"
				return plugin.K8sclient.GetClientset().CoreV1().Pods(metav1.NamespaceAll).Watch(options)
			},
		},
		&v1.Pod{},
		10*time.Second,
		cache.Indexers{},
	)

	setupInformer(plugin.informer, queue)

	go plugin.informer.Run(plugin.stopCh)
	plugin.Log.Info("Started  Finalizer's shared informer factory.")

	// Wait for the informer caches to finish performing it's initial sync of
	// resources
	if !cache.WaitForCacheSync(plugin.stopCh, plugin.informer.HasSynced) {
		plugin.Log.Error("Error waiting for informer cache to sync")
	}
	plugin.Log.Info("Finalizer's Informer cache is ready")

	// Read forever from the work queue
	go workforever(plugin, queue, plugin.informer, plugin.stopCh)

	return nil
}

// Close stops all reflectors.
func (plugin *Plugin) Close() error {
	return plugin.IdempotentClose(plugintools.LoggingCloseFunc(plugin.Log, plugin, plugin.close))
}

func (plugin *Plugin) close() error {
	plugin.Log.Info("Close")
	close(plugin.pluginStopCh)
	plugin.wg.Wait()
	registry.Shared().Delete(plugin)
	return deptools.Close(plugin)
}
