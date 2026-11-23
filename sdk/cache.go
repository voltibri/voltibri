// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: Apache-2.0

package sdk

import (
	"sync"
	"time"
)

// Cache is a small persistent key/value store the core offers to each
// device (namespaced per device). Drivers use it to keep expensive or
// rate-limited data, e.g. forecasts, across restarts.
type Cache interface {
	// Get returns the value and the time it was stored.
	Get(key string) (value []byte, stored time.Time, ok bool)
	// Put stores a value (overwriting an existing one).
	Put(key string, value []byte) error
}

// MemCache is an in-memory Cache (used when the host provides none).
type MemCache struct {
	mu sync.Mutex
	m  map[string]memEntry
}

type memEntry struct {
	v []byte
	t time.Time
}

// NewMemCache creates an empty in-memory cache.
func NewMemCache() *MemCache { return &MemCache{m: map[string]memEntry{}} }

// Get implements Cache.
func (c *MemCache) Get(key string) ([]byte, time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	return e.v, e.t, ok
}

// Put implements Cache.
func (c *MemCache) Put(key string, v []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = memEntry{append([]byte(nil), v...), time.Now()}
	return nil
}
