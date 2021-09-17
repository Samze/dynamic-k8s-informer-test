/*
Copyright 2021.

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

package controllers

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	samzev1beta1 "github.com/samze/dynamic-test/api/v1beta1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

const resourceRefFinalizer = "samze.com/resourceRef"

// ResourceRefReconciler reconciles a ResourceRef object
type ResourceRefReconciler struct {
	client.Client
	Log           logr.Logger
	Scheme        *runtime.Scheme
	DynamicClient dynamic.Interface
	EventChan     chan event.GenericEvent
	Tracker       *TrackerInformerManager
}

//+kubebuilder:rbac:groups=samze.samze.com,resources=resourcerefs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=samze.samze.com,resources=resourcerefs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=samze.samze.com,resources=resourcerefs/finalizers,verbs=update

func (r *ResourceRefReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := r.Log.WithValues("resourceref", req.NamespacedName)

	l.Info("reconciled", "key", req)
	// your logic here
	resourceRef := samzev1beta1.ResourceRef{}
	if err := r.Get(context.Background(), req.NamespacedName, &resourceRef); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	//Maybe useful meta.UnsafeGuessKindToResource(ref.GroupVersionKind())
	gvr := schema.GroupVersionResource{Group: resourceRef.Spec.Group, Version: resourceRef.Spec.Version, Resource: resourceRef.Spec.Resource}

	//Add/Remove finalizers
	if resourceRef.ObjectMeta.DeletionTimestamp.IsZero() {
		if !containsString(resourceRef.GetFinalizers(), resourceRefFinalizer) {
			controllerutil.AddFinalizer(&resourceRef, resourceRefFinalizer)
			if err := r.Update(ctx, &resourceRef); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else {
		if containsString(resourceRef.GetFinalizers(), resourceRefFinalizer) {
			// Deletion
			r.Tracker.Untrack(&resourceRef, gvr)

			controllerutil.RemoveFinalizer(&resourceRef, resourceRefFinalizer)
			if err := r.Update(ctx, &resourceRef); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	r.Tracker.Track(&resourceRef, gvr)

	//Logic here.

	//Interacting with dynamic resources.
	lister, err := r.Tracker.GetLister(gvr)
	if err != nil {
		return ctrl.Result{}, err
	}

	//Put this into a client?
	obj, err := lister.ByNamespace(resourceRef.GetNamespace()).Get(resourceRef.Spec.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	l.Info("referred obj", " obj", obj)

	return ctrl.Result{}, nil
}

func (r *ResourceRefReconciler) SetupWithManager(mgr ctrl.Manager, events chan event.GenericEvent) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&samzev1beta1.ResourceRef{}).
		Watches(&source.Channel{Source: events}, &handler.EnqueueRequestForObject{}).
		Complete(r)
}

// TrackerInformerManager allows Tracking and Untracking of gvrs.
// Events are emitted on changes to the dynamic type with a refernece to the Parent to the provided Channel.
type TrackerInformerManager struct {
	trackedInformers       map[schema.GroupVersionResource]*TrackedInformer
	dynamicInformerFactory dynamicinformer.DynamicSharedInformerFactory
	eventChan              chan event.GenericEvent
	lock                   sync.RWMutex //todo more efficient way to lock
}

type TrackedInformer struct {
	parentResources map[types.NamespacedName]client.Object
	gvr             schema.GroupVersionResource
	stopChan        chan struct{}
	lister          cache.GenericLister
}

func NewTrackerInformerManager(client dynamic.Interface, eventChan chan event.GenericEvent) *TrackerInformerManager {
	return &TrackerInformerManager{
		dynamicInformerFactory: dynamicinformer.NewDynamicSharedInformerFactory(client, time.Minute*20),
		trackedInformers:       make(map[schema.GroupVersionResource]*TrackedInformer),
		eventChan:              eventChan,
	}
}

func (t *TrackerInformerManager) Track(parent *samzev1beta1.ResourceRef, gvr schema.GroupVersionResource) error {
	t.lock.Lock()
	defer t.lock.Unlock()

	fmt.Println("tracking")
	tracker, found := t.trackedInformers[gvr]
	if !found {
		stopChan := make(chan struct{})
		tracker = &TrackedInformer{
			gvr:             gvr,
			stopChan:        stopChan,
			parentResources: make(map[types.NamespacedName]client.Object),
		}

		t.trackedInformers[gvr] = tracker
		lister, err := t.startInformer(gvr, stopChan)
		if err != nil {
			return err
		}

		tracker.lister = lister
	}
	namespacedName := types.NamespacedName{Name: parent.Spec.Name, Namespace: parent.Namespace}
	tracker.parentResources[namespacedName] = parent

	return nil
}

func (t *TrackerInformerManager) Untrack(parent *samzev1beta1.ResourceRef, gvr schema.GroupVersionResource) bool {
	t.lock.Lock()
	defer t.lock.Unlock()

	fmt.Println("untracking")
	trackedMap, found := t.trackedInformers[gvr]
	if !found {
		return false
	}
	namenamed := types.NamespacedName{Name: parent.Spec.Name, Namespace: parent.Namespace}
	delete(trackedMap.parentResources, namenamed)

	// When there are no longer any parent resources, clean up watch
	if len(trackedMap.parentResources) == 0 {
		fmt.Println("stopping informer")
		informer := t.trackedInformers[gvr]
		informer.stopChan <- struct{}{}
		delete(t.trackedInformers, gvr)
	}
	return true
}

func (t *TrackerInformerManager) GetLister(gvr schema.GroupVersionResource) (cache.GenericLister, error) {
	trackedInformer, found := t.trackedInformers[gvr]
	if !found {
		return nil, fmt.Errorf("lister not found")
	}
	return trackedInformer.lister, nil

}

func (t *TrackerInformerManager) startInformer(gvr schema.GroupVersionResource, stopChan chan struct{}) (cache.GenericLister, error) {
	fmt.Println("starting informer")
	typeInformer := t.dynamicInformerFactory.ForResource(gvr)

	typeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			clientObj := obj.(client.Object)
			namespacedName := types.NamespacedName{Name: clientObj.GetName(), Namespace: clientObj.GetNamespace()}
			parent, found := t.trackedInformers[gvr].parentResources[namespacedName]
			if found {
				t.eventChan <- event.GenericEvent{Object: parent}
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			newClientObj := newObj.(client.Object)
			oldClientObj := oldObj.(client.Object)
			if newClientObj.GetResourceVersion() == oldClientObj.GetResourceVersion() {
				//No changes
				return
			}
			namespacedName := types.NamespacedName{Name: newClientObj.GetName(), Namespace: newClientObj.GetNamespace()}
			parent, found := t.trackedInformers[gvr].parentResources[namespacedName]
			if found {
				t.eventChan <- event.GenericEvent{Object: parent}
			}
		},
		DeleteFunc: func(obj interface{}) {
			clientObj := obj.(client.Object)
			namespacedName := types.NamespacedName{Name: clientObj.GetName(), Namespace: clientObj.GetNamespace()}
			parent, found := t.trackedInformers[gvr].parentResources[namespacedName]
			if found {
				t.eventChan <- event.GenericEvent{Object: parent}
			}
		},
	})

	go typeInformer.Informer().Run(stopChan)
	cache.WaitForCacheSync(stopChan, typeInformer.Informer().HasSynced)

	return typeInformer.Lister(), nil
}

func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}
