// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"time"

	"github.com/voltibri/voltibri/sdk"
)

// Cache returns a persistent cache for one device (namespace).
func (s *Store) Cache(namespace string) sdk.Cache { return &deviceCache{s: s, ns: namespace} }

type deviceCache struct {
	s  *Store
	ns string
}

func (c *deviceCache) Get(key string) ([]byte, time.Time, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var v []byte
	var ts int64
	err := c.s.db.QueryRowContext(ctx, `SELECT value, stored_at FROM cache WHERE namespace = ? AND key = ?`, c.ns, key).Scan(&v, &ts)
	if err != nil {
		return nil, time.Time{}, false
	}
	return v, time.Unix(ts, 0), true
}

func (c *deviceCache) Put(key string, value []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.s.db.ExecContext(ctx, `INSERT OR REPLACE INTO cache (namespace, key, stored_at, value) VALUES (?, ?, ?, ?)`,
		c.ns, key, time.Now().Unix(), value)
	return err
}
