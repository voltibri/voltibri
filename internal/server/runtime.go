// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Package server supervises the control loop: it builds a site from the
// current configuration and rebuilds it whenever a new configuration
// version is saved (no restart needed).
package server

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/internal/planner"
	"github.com/voltibri/voltibri/internal/site"
	"github.com/voltibri/voltibri/internal/store"
)

// Status describes the runtime state for the UI.
type Status struct {
	State     string `json:"state"` // "running", "setup" (not configured yet), "error"
	Message   string `json:"message,omitempty"`
	Version   int64  `json:"version"`
	ReadOnly  bool   `json:"readOnly"`             // configuration managed by a YAML file
	ConfigSrc string `json:"configFile,omitempty"` // that file
}

// Runtime owns the current site.
type Runtime struct {
	store    *store.Store
	yamlPath string // read-only mode if set

	mu     sync.RWMutex
	site   *site.Site
	status Status
}

// New creates the runtime. yamlPath enables read-only mode.
func New(st *store.Store, yamlPath string) *Runtime {
	return &Runtime{store: st, yamlPath: yamlPath, status: Status{State: "setup", ReadOnly: yamlPath != "", ConfigSrc: yamlPath}}
}

// ReadOnly reports whether the configuration is managed by a YAML file.
func (r *Runtime) ReadOnly() bool { return r.yamlPath != "" }

// Status returns the runtime status.
func (r *Runtime) Status() Status {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.status
}

// State implements api.Source.
func (r *Runtime) State() site.State {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.site == nil {
		return site.State{Time: time.Now(), Producers: []site.Reading{}, Consumers: []site.Reading{}}
	}
	return r.site.State()
}

// Plan implements api.Source.
func (r *Runtime) Plan() planner.Plan {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.site == nil {
		return planner.Plan{Created: time.Now(), Mode: planner.ModeNoData, Reasons: []string{r.status.Message}, Slots: []planner.SlotPlan{}}
	}
	return r.site.Plan()
}

func (r *Runtime) set(s *site.Site, st Status) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st.ReadOnly, st.ConfigSrc = r.yamlPath != "", r.yamlPath
	r.site, r.status = s, st
}

// Run supervises the site until ctx is cancelled.
func (r *Runtime) Run(ctx context.Context) error {
	changes := make(chan int64, 1)
	r.store.OnChange(func(v int64) {
		select {
		case changes <- v:
		default:
		}
	})
	if r.yamlPath != "" {
		if err := r.syncYAML(ctx); err != nil {
			return err
		}
		go r.watchYAML(ctx)
	}
	for {
		cfg, vi, err := r.store.Current(ctx)
		var s *site.Site
		switch {
		case errors.Is(err, store.ErrNoConfig):
			r.set(nil, Status{State: "setup", Message: "voltibri ist noch nicht eingerichtet."})
		case err != nil:
			r.set(nil, Status{State: "error", Message: err.Error()})
		default:
			s, err = site.New(cfg, r.store)
			if err != nil {
				state := "error"
				if cfg.Ready() != nil {
					state = "setup"
				}
				r.set(nil, Status{State: state, Message: err.Error(), Version: vi.ID})
				slog.Warn("site not started", "version", vi.ID, "reason", err)
			} else {
				r.set(s, Status{State: "running", Version: vi.ID})
				slog.Info("site started", "version", vi.ID, "author", vi.Author, "comment", vi.Comment)
			}
		}

		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		if s != nil {
			go func() { done <- s.Run(runCtx) }()
		}
		select {
		case <-ctx.Done():
			cancel()
			if s != nil {
				<-done
			}
			return nil
		case v := <-changes:
			cancel()
			if s != nil {
				<-done
			}
			slog.Info("configuration changed, restarting site", "version", v)
		case err := <-done:
			cancel()
			r.set(nil, Status{State: "error", Message: errString(err), Version: vi.ID})
			slog.Error("site stopped", "err", err)
			select { // wait for a fix
			case <-ctx.Done():
				return nil
			case <-changes:
			}
		}
	}
}

func errString(err error) string {
	if err == nil {
		return "site stopped"
	}
	return err.Error()
}

// syncYAML stores the YAML file as a new version if it differs from the
// current configuration.
func (r *Runtime) syncYAML(ctx context.Context) error {
	cfg, err := config.Load(r.yamlPath)
	if err != nil {
		return err
	}
	cur, _, err := r.store.Current(ctx)
	if err != nil && !errors.Is(err, store.ErrNoConfig) {
		return err
	}
	if cur != nil {
		a, _ := cur.YAML()
		b, _ := cfg.Clone().YAML()
		if string(a) == string(b) {
			return nil
		}
	}
	_, err = r.store.Save(ctx, cfg, "yaml", "aus "+r.yamlPath+" übernommen")
	return err
}

// watchYAML reloads the file when it changes (e.g. a Kubernetes ConfigMap).
func (r *Runtime) watchYAML(ctx context.Context) {
	var last time.Time
	if fi, err := os.Stat(r.yamlPath); err == nil {
		last = fi.ModTime()
	}
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fi, err := os.Stat(r.yamlPath)
			if err != nil || !fi.ModTime().After(last) {
				continue
			}
			last = fi.ModTime()
			if err := r.syncYAML(ctx); err != nil {
				slog.Error("configuration file invalid, keeping previous version", "file", r.yamlPath, "err", err)
			}
		}
	}
}
