/*
 * Copyright (c) 2021 by the OnMetal authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package source

import (
	"context"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

type Dynamic struct {
	mu        sync.Mutex
	isStarted bool

	startWatches     map[schema.GroupVersionKind]struct{}
	gvkToCancelWatch map[schema.GroupVersionKind]context.CancelFunc

	cache  cache.Cache
	scheme *runtime.Scheme

	ctx        context.Context
	handler    handler.EventHandler
	queue      workqueue.RateLimitingInterface
	predicates []predicate.Predicate
}

func (d *Dynamic) startWatch(gvk schema.GroupVersionKind) error {
	if _, ok := d.gvkToCancelWatch[gvk]; ok {
		return fmt.Errorf("duplicate watch registered for %v", gvk)
	}

	ctx, cancel := context.WithCancel(d.ctx)
	inf, err := d.cache.GetInformerForKind(d.ctx, gvk)
	if err != nil {
		cancel()
		return fmt.Errorf("error getting informer for %v: %w", gvk, err)
	}

	src := source.Informer{Informer: inf}
	if err := src.Start(ctx, d.handler, d.queue, d.predicates...); err != nil {
		cancel()
		return fmt.Errorf("error starting informer source for %v: %w", gvk, err)
	}

	d.gvkToCancelWatch[gvk] = cancel
	return nil
}

func (d *Dynamic) AddWatchForKind(gvk schema.GroupVersionKind) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.isStarted {
		return d.startWatch(gvk)
	}

	d.startWatches[gvk] = struct{}{}
	return nil
}

func (d *Dynamic) AddWatch(obj client.Object) error {
	gvk, err := apiutil.GVKForObject(obj, d.scheme)
	if err != nil {
		return fmt.Errorf("error obtaining scheme for object: %w", err)
	}

	return d.AddWatchForKind(gvk)
}

func (d *Dynamic) RemoveWatchForKind(gvk schema.GroupVersionKind) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.isStarted {
		cancel, ok := d.gvkToCancelWatch[gvk]
		if !ok {
			return fmt.Errorf("no watch registered for %v", gvk)
		}

		cancel()
		delete(d.gvkToCancelWatch, gvk)
		return nil
	}

	if _, ok := d.startWatches[gvk]; !ok {
		return fmt.Errorf("no watch registered for %v", gvk)
	}
	delete(d.startWatches, gvk)
	return nil
}

func (d *Dynamic) RemoveWatch(obj client.Object) error {
	gvk, err := apiutil.GVKForObject(obj, d.scheme)
	if err != nil {
		return fmt.Errorf("error obtaining scheme for object: %w", err)
	}

	return d.RemoveWatchForKind(gvk)
}

func (d *Dynamic) Start(ctx context.Context, handler handler.EventHandler, queue workqueue.RateLimitingInterface, prct ...predicate.Predicate) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.isStarted {
		return fmt.Errorf("dynamic source was started more than once. This is likely to be caused by being added to a manager multiple times")
	}

	d.isStarted = true
	d.ctx = ctx
	d.handler = handler
	d.queue = queue
	d.predicates = prct

	for gvk := range d.startWatches {
		if err := d.startWatch(gvk); err != nil {
			return err
		}
	}

	return nil
}

type DynamicOptions struct {
	Cache  cache.Cache
	Scheme *runtime.Scheme
}

func NewDynamic(opts DynamicOptions) (*Dynamic, error) {
	if opts.Cache == nil {
		return nil, fmt.Errorf("cache needs to be set")
	}
	if opts.Scheme == nil {
		return nil, fmt.Errorf("scheme needs to be set")
	}
	return &Dynamic{
		startWatches:     make(map[schema.GroupVersionKind]struct{}),
		gvkToCancelWatch: make(map[schema.GroupVersionKind]context.CancelFunc),
		cache:            opts.Cache,
		scheme:           opts.Scheme,
	}, nil
}
