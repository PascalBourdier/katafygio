// Package controller list and keep watching a specific Kubernetes resource kind
// (ie. "apps/v1 Deployment", "v1 Namespace", etc) and notifies a recorder whenever
// a change happens (an object changed, was created, or deleted). This is a generic
// implementation: the resource kind to watch is provided at runtime. We should
// start several such controllers to watch for distinct resources.
package controller

import (
	"fmt"
	"time"

	"github.com/bpineau/katafygio/config"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/ghodss/yaml"
)

const maxProcessRetry = 6

// Action represents the kind of object change we're notifying
type Action int

const (
	// Delete is the object deletion Action
	Delete Action = iota

	// Upsert is the update or create Action
	Upsert
)

// Event conveys an object delete/upsert notification
type Event struct {
	Action Action
	Key    string
	Kind   string
	Obj    string
}

// Controller is a generic kubernetes controller
type Controller struct {
	stopCh   chan struct{}
	doneCh   chan struct{}
	evchan   chan Event
	name     string
	config   *config.KfConfig
	queue    workqueue.RateLimitingInterface
	informer cache.SharedIndexInformer
}

// New return an untyped, generic Kubernetes controller
func New(lw cache.ListerWatcher, evchan chan Event, name string, config *config.KfConfig) *Controller {
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	informer := cache.NewSharedIndexInformer(
		lw,
		&unstructured.Unstructured{},
		config.ResyncIntv,
		cache.Indexers{},
	)

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(key)
			}
		},
		UpdateFunc: func(old, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			if err == nil {
				queue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(key)
			}
		},
	})

	return &Controller{
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
		evchan:   evchan,
		name:     name,
		config:   config,
		queue:    queue,
		informer: informer,
	}
}

// Start launchs the controller in the background
func (c *Controller) Start() {
	c.config.Logger.Infof("Starting %s controller", c.name)
	defer utilruntime.HandleCrash()

	go c.informer.Run(c.stopCh)

	if !cache.WaitForCacheSync(c.stopCh, c.informer.HasSynced) {
		utilruntime.HandleError(fmt.Errorf("Timed out waiting for caches to sync"))
		return
	}

	go wait.Until(c.runWorker, time.Second, c.stopCh)
}

// Stop halts the controller
func (c *Controller) Stop() {
	close(c.stopCh)
	c.queue.ShutDown()
	<-c.doneCh
	c.config.Logger.Infof("Stopping %s controller", c.name)
}

func (c *Controller) runWorker() {
	defer close(c.doneCh)
	for c.processNextItem() {
		// continue looping
	}
}

func (c *Controller) processNextItem() bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)

	err := c.processItem(key.(string))

	if err == nil {
		// No error, reset the ratelimit counters
		c.queue.Forget(key)
	} else if c.queue.NumRequeues(key) < maxProcessRetry {
		c.config.Logger.Errorf("Error processing %s (will retry): %v", key, err)
		c.queue.AddRateLimited(key)
	} else {
		// err != nil and too many retries
		c.config.Logger.Errorf("Error processing %s (giving up): %v", key, err)
		c.queue.Forget(key)
	}

	return true
}

func (c *Controller) processItem(key string) error {
	rawobj, exists, err := c.informer.GetIndexer().GetByKey(key)

	if err != nil {
		return fmt.Errorf("error fetching object with key %s from store: %v", key, err)
	}

	if !exists {
		// deleted object
		c.enqueue(Event{Action: Delete, Key: key, Kind: c.name, Obj: ""})
		return nil
	}

	obj := rawobj.(*unstructured.Unstructured).DeepCopy()

	// clear irrelevant attributes
	uc := obj.UnstructuredContent()
	md := uc["metadata"].(map[string]interface{})
	delete(uc, "status")
	delete(md, "selfLink")
	delete(md, "uid")
	delete(md, "resourceVersion")
	delete(md, "generation")

	c.config.Logger.Debugf("Found %s/%s [%s]", obj.GetAPIVersion(), obj.GetKind(), key)

	yml, err := yaml.Marshal(obj)
	if err != nil {
		return fmt.Errorf("failed to marshal %s: %v", key, err)
	}

	c.enqueue(Event{Action: Upsert, Key: key, Kind: c.name, Obj: string(yml)})
	return nil
}

func (c *Controller) enqueue(ev Event) {
	c.evchan <- ev
}
