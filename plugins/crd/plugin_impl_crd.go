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

// //go:generate protoc -I ./model/pod --go_out=plugins=grpc:./model/pod ./model/pod/pod.proto

package netmeshplugincrd

import (
	"fmt"
	"reflect"
	"sync"
	"time"

	apiextv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	apiextcs "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/workqueue"

	"github.com/ligato/cn-infra/config"
	"github.com/ligato/cn-infra/flavors/local"
	"github.com/ligato/cn-infra/health/statuscheck"
	"github.com/ligato/cn-infra/logging"
	"github.com/ligato/networkservicemesh/pkg/apis/networkservicemesh.io/v1"
	client "github.com/ligato/networkservicemesh/pkg/client/clientset/versioned"
	factory "github.com/ligato/networkservicemesh/pkg/client/informers/externalversions"
)

// Plugin watches K8s resources and causes all changes to be reflected in the ETCD
// data store.
type Plugin struct {
	Deps

	pluginStopCh    chan struct{}
	wg              sync.WaitGroup
	k8sClientConfig *rest.Config
	k8sClientset    *kubernetes.Clientset
	apiclientset    *apiextcs.Clientset
	crdClient       client.Interface
	StatusMonitor   statuscheck.StatusReader

	// Used to signal hard errors while processing the queue
	queueError chan bool
	// These can be used to stop all the informers, as well as control loops
	// within the application.
	stopChNS  chan struct{}
	stopChNSE chan struct{}
	stopChNSC chan struct{}
	// sharedFactory's are shared informer factorys used as a cache for
	// items in the API server. They saves each informer listing and watch the
	// same resources independently of each other, thus providing more up to
	// date results with less 'effort'
	sharedFactoryNS  factory.SharedInformerFactory
	sharedFactoryNSE factory.SharedInformerFactory
	sharedFactoryNSC factory.SharedInformerFactory
}

var (
	// queue is a queue of resources to be processed. It performs exponential
	// backoff rate limiting, with a minimum retry period of 5 seconds and a
	// maximum of 1 minute.
	queueNS  = workqueue.NewRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(time.Second*5, time.Minute))
	queueNSC = workqueue.NewRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(time.Second*5, time.Minute))
	queueNSE = workqueue.NewRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(time.Second*5, time.Minute))
)

// Deps defines dependencies of netmesh plugin.
type Deps struct {
	local.PluginInfraDeps
	// Kubeconfig with k8s cluster address and access credentials to use.
	KubeConfig config.PluginConfig
}

// Init builds K8s client-set based on the supplied kubeconfig and initializes
// all reflectors.
func (plugin *Plugin) Init() error {
	var err error
	plugin.Log.SetLevel(logging.DebugLevel)
	plugin.pluginStopCh = make(chan struct{})

	kubeconfig := plugin.KubeConfig.GetConfigName()
	plugin.Log.WithField("kubeconfig", kubeconfig).Info("Loading kubernetes client config")
	plugin.k8sClientConfig, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return fmt.Errorf("Failed to build kubernetes client config: %s", err)
	}

	plugin.k8sClientset, err = kubernetes.NewForConfig(plugin.k8sClientConfig)
	if err != nil {
		return fmt.Errorf("Failed to build kubernetes client: %s", err)
	}

	plugin.stopChNS = make(chan struct{})
	plugin.stopChNSC = make(chan struct{})
	plugin.stopChNSE = make(chan struct{})
	plugin.queueError = make(chan bool, 1)

	return nil
}

// Create the CRD resource, ignore error if it already exists
func createCRD(plugin *Plugin, FullName, Group, Version, Plural, Name string) error {
	crd := &apiextv1beta1.CustomResourceDefinition{
		ObjectMeta: meta.ObjectMeta{Name: FullName},
		Spec: apiextv1beta1.CustomResourceDefinitionSpec{
			Group:   Group,
			Version: Version,
			Scope:   apiextv1beta1.NamespaceScoped,
			Names: apiextv1beta1.CustomResourceDefinitionNames{
				Plural: Plural,
				Kind:   Name,
			},
		},
	}

	_, cserr := plugin.apiclientset.ApiextensionsV1beta1().CustomResourceDefinitions().Create(crd)
	if cserr != nil && apierrors.IsAlreadyExists(cserr) {
		plugin.Log.Infof("Created CRD %s succesfully, though it already existed", Name)
		return nil
	} else if cserr != nil {
		plugin.Log.Infof("Error creating CRD %s: %s", Name, cserr)
	} else {
		plugin.Log.Infof("Created CRD %s succesfully", Name)
	}

	return cserr
}

func informerNetworkServices(plugin *Plugin) {
	// We use a shared informer from the informer factory, to save calls to the
	// API as we grow our application and so state is consistent between our
	// control loops. We set a resync period of 30 seconds, in case any
	// create/replace/update/delete operations are missed when watching
	plugin.sharedFactoryNS = factory.NewSharedInformerFactory(plugin.crdClient, time.Second*30)

	informer := plugin.sharedFactoryNS.Networkservice().V1().NetworkServices().Informer()
	// We add a new event handler, watching for changes to API resources.
	informer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: networkserviceEnqueue,
			UpdateFunc: func(old, cur interface{}) {
				if !reflect.DeepEqual(old, cur) {
					networkserviceEnqueue(cur)
				}
			},
			DeleteFunc: networkserviceEnqueue,
		},
	)

	// Start the informer. This will cause it to begin receiving updates from
	// the configured API server and firing event handlers in response.
	plugin.sharedFactoryNS.Start(plugin.stopChNS)
	plugin.Log.Info("Started NetworkService informer factory.")

	// Wait for the informer cache to finish performing it's initial sync of
	// resources
	if !cache.WaitForCacheSync(plugin.stopChNS, informer.HasSynced) {
		plugin.Log.Error("Error waiting for informer cache to sync")
	}

	plugin.Log.Info("NetworkService Informer is ready")

	// Read forever from the work queue
	networkserviceWork(plugin)
}

func informerNetworkServiceChannels(plugin *Plugin) {
	// We use a shared informer from the informer factory, to save calls to the
	// API as we grow our application and so state is consistent between our
	// control loops. We set a resync period of 30 seconds, in case any
	// create/replace/update/delete operations are missed when watching
	plugin.sharedFactoryNSC = factory.NewSharedInformerFactory(plugin.crdClient, time.Second*30)

	informer := plugin.sharedFactoryNSC.Networkservice().V1().NetworkServiceChannels().Informer()
	// we add a new event handler, watching for changes to API resources.
	informer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: networkservicechannelEnqueue,
			UpdateFunc: func(old, cur interface{}) {
				if !reflect.DeepEqual(old, cur) {
					networkservicechannelEnqueue(cur)
				}
			},
			DeleteFunc: networkservicechannelEnqueue,
		},
	)

	// Start the informer. This will cause it to begin receiving updates from
	// the configured API server and firing event handlers in response.
	plugin.sharedFactoryNSC.Start(plugin.stopChNSC)
	plugin.Log.Info("Started NetworkServiceChannel informer factory.")

	// Wait for the informer cache to finish performing it's initial sync of
	// resources
	if !cache.WaitForCacheSync(plugin.stopChNSC, informer.HasSynced) {
		plugin.Log.Errorf("Error waiting for informer cache to sync")
	}

	plugin.Log.Info("NetworkServiceChannel Informer is ready")

	// Read forever from the work queue
	networkservicechannelWork(plugin)
}

func informerNetworkServiceEndpoints(plugin *Plugin) {
	// We use a shared informer from the informer factory, to save calls to the
	// API as we grow our application and so state is consistent between our
	// control loops. We set a resync period of 30 seconds, in case any
	// create/replace/update/delete operations are missed when watching
	plugin.sharedFactoryNSE = factory.NewSharedInformerFactory(plugin.crdClient, time.Second*30)

	informer := plugin.sharedFactoryNSE.Networkservice().V1().NetworkServiceEndpoints().Informer()
	// we add a new event handler, watching for changes to API resources.
	informer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: networkserviceendpointEnqueue,
			UpdateFunc: func(old, cur interface{}) {
				if !reflect.DeepEqual(old, cur) {
					networkserviceendpointEnqueue(cur)
				}
			},
			DeleteFunc: networkserviceendpointEnqueue,
		},
	)

	// Start the informer. This will cause it to begin receiving updates from
	// the configured API server and firing event handlers in response.
	plugin.sharedFactoryNSE.Start(plugin.stopChNSE)
	plugin.Log.Info("Started NetworkServiceEndpoints informer factory.")

	// Wait for the informer cache to finish performing it's initial sync of
	// resources
	if !cache.WaitForCacheSync(plugin.stopChNSE, informer.HasSynced) {
		plugin.Log.Errorf("Error waiting for informer cache to sync")
	}

	plugin.Log.Info("NetworkServiceEndpoint Informer is ready")

	// Read forever from the work queue
	networkserviceendpointWork(plugin)
}

// AfterInit This will create all of the CRDs for NetworkServiceMesh.
func (plugin *Plugin) AfterInit() error {
	var err error
	var crdname string

	// Create clientset and create our CRD, this only needs to run once
	plugin.apiclientset, err = apiextcs.NewForConfig(plugin.k8sClientConfig)
	if err != nil {
		panic(err.Error())
	}

	// Create an instance of our own API client
	plugin.crdClient, err = client.NewForConfig(plugin.k8sClientConfig)
	if err != nil {
		plugin.Log.Errorf("Error creating CRD client: %s", err.Error())
		panic(err.Error())
	}

	crdname = reflect.TypeOf(v1.NetworkServiceEndpoint{}).Name()
	err = createCRD(plugin, v1.FullNSMEPName,
		v1.NSMGroup,
		v1.NSMGroupVersion,
		v1.NSMEPPlural,
		crdname)

	if err != nil {
		plugin.Log.Error("Error initializing NetworkServiceEndpoint CRD")
		return err
	}

	crdname = reflect.TypeOf(v1.NetworkServiceChannel{}).Name()
	err = createCRD(plugin, v1.FullNSMChannelName,
		v1.NSMGroup,
		v1.NSMGroupVersion,
		v1.NSMChannelPlural,
		crdname)

	if err != nil {
		plugin.Log.Error("Error initializing NetworkServiceChannel CRD")
		return err
	}

	crdname = reflect.TypeOf(v1.NetworkService{}).Name()
	err = createCRD(plugin, v1.FullNSMName,
		v1.NSMGroup,
		v1.NSMGroupVersion,
		v1.NSMPlural,
		crdname)

	if err != nil {
		plugin.Log.Error("Error initializing NetworkService CRD")
		return err
	}

	go informerNetworkServices(plugin)
	go informerNetworkServiceChannels(plugin)
	go informerNetworkServiceEndpoints(plugin)
	go handleQueueErrors(plugin)

	return nil
}

// handleQueueErrors monitors the queueError channel for errors from the
// dequeueing functions and closes the plugin down if one is received.
func handleQueueErrors(plugin *Plugin) {
	<-plugin.queueError
	plugin.Log.Error("Error processing queues, shutting plugin down")

	plugin.Close()
}

// Close stops all reflectors.
func (plugin *Plugin) Close() error {
	close(plugin.pluginStopCh)
	plugin.wg.Wait()
	return nil
}
