/*
SPDX-License-Identifier: Apache-2.0

Copyright Contributors to the Submariner project.

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

package controller

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"github.com/kelseyhightower/envconfig"
	"github.com/pkg/errors"
	"github.com/submariner-io/admiral/pkg/log"
	"github.com/submariner-io/admiral/pkg/watcher"
	subv1 "github.com/submariner-io/submariner/pkg/apis/submariner.io/v1"
	"github.com/submariner-io/submariner/pkg/event"
	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

type specification struct {
	ClusterID string
	Namespace string
}

type handlerStateImpl struct {
	isOnGateway     atomic.Bool
	wasOnGateway    bool
	remoteEndpoints sync.Map
}

func (s *handlerStateImpl) setIsOnGateway(v bool) {
	s.isOnGateway.Store(v)
}

func (s *handlerStateImpl) IsOnGateway() bool {
	return s.isOnGateway.Load()
}

func (s *handlerStateImpl) GetRemoteEndpoints() []subv1.Endpoint {
	var endpoints []subv1.Endpoint

	s.remoteEndpoints.Range(func(_, value any) bool {
		endpoints = append(endpoints, *value.(*subv1.Endpoint))
		return true
	})

	return endpoints
}

type Controller struct {
	env             specification
	resourceWatcher watcher.Interface

	handlers     *event.Registry
	handlerState handlerStateImpl

	syncMutex sync.Mutex
	hostname  string
}

// If the handler cannot recover from a failure, even after retrying for maximum requeue attempts,
// it's best to disregard the event. This prevents the logs from being flooded with repetitive errors.
const maxRequeues = 20

type Config struct {
	// Registry is the event handler registry where controller events will be sent.
	Registry *event.Registry

	// RestConfig the REST config used to access the resources to watch.
	RestConfig *rest.Config

	// RestMapper can be provided for unit testing. By default New will create its own RestMapper.
	RestMapper meta.RESTMapper

	// Client can be provided for unit testing. By default New will create its own dynamic client.
	Client dynamic.Interface

	Scheme *runtime.Scheme
}

var logger = log.Logger{Logger: logf.Log.WithName("EventController")}

func New(config *Config) (*Controller, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return nil, errors.Wrapf(err, "unable to read hostname")
	}

	ctl := Controller{
		handlers: config.Registry,
		hostname: hostname,
	}

	err = envconfig.Process("submariner", &ctl.env)
	if err != nil {
		return nil, errors.Wrap(err, "error processing env vars")
	}

	err = subv1.AddToScheme(scheme.Scheme)
	if err != nil {
		return nil, errors.Wrap(err, "error adding submariner types to the scheme")
	}

	ctl.resourceWatcher, err = watcher.New(&watcher.Config{
		Scheme:     config.Scheme,
		RestConfig: config.RestConfig,
		ResourceConfigs: []watcher.ResourceConfig{
			{
				Name:            fmt.Sprintf("Endpoint watcher for %s registry", ctl.handlers.GetName()),
				ResourceType:    &subv1.Endpoint{},
				SourceNamespace: ctl.env.Namespace,
				Handler: watcher.EventHandlerFuncs{
					OnCreateFunc: ctl.handleCreatedEndpoint,
					OnUpdateFunc: ctl.handleUpdatedEndpoint,
					OnDeleteFunc: ctl.handleRemovedEndpoint,
				},
			}, {
				Name:                fmt.Sprintf("Node watcher for %s registry", ctl.handlers.GetName()),
				ResourceType:        &k8sv1.Node{},
				ResourcesEquivalent: ctl.isNodeEquivalent,
				Handler: watcher.EventHandlerFuncs{
					OnCreateFunc: ctl.handleCreatedNode,
					OnUpdateFunc: ctl.handleUpdatedNode,
					OnDeleteFunc: ctl.handleRemovedNode,
				},
			},
		},
		Client:     config.Client,
		RestMapper: config.RestMapper,
	})

	if err != nil {
		return nil, errors.Wrap(err, "error creating resource watcher")
	}

	ctl.handlers.SetHandlerState(&ctl.handlerState)

	return &ctl, nil
}

// Start starts the controller.
func (c *Controller) Start(stopCh <-chan struct{}) error {
	logger.Info("Starting the Event controller...")

	err := c.resourceWatcher.Start(stopCh)
	if err != nil {
		return errors.Wrap(err, "error starting the resource watcher")
	}

	logger.Info("Event controller started")

	return nil
}

func (c *Controller) Stop() {
	logger.Info("Event controller stopping")

	if err := c.handlers.StopHandlers(); err != nil {
		logger.Warningf("In Event Controller, StopHandlers returned error: %v", err)
	}
}
