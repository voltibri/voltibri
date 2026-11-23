package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/voltibri/voltibri/internal/config"
	_ "github.com/voltibri/voltibri/internal/drivers/all"
	"github.com/voltibri/voltibri/internal/store"
)

func demoConfig(producerSite string) *config.Config {
	c := config.Default()
	c.Site.DryRun = false
	c.Roles.Grid = "demo"
	c.Roles.PV = []string{"demo", "extra"}
	c.Devices = []config.Device{
		{ID: "demo", Type: "demo", Mode: "owned"},
		{ID: "extra", Type: "demo-producer", Params: map[string]any{"site": producerSite}},
	}
	return c
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// A configuration that validates but cannot be started must not stop the
// running one.
func TestBrokenConfigKeepsRunningVersion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rt := New(st, "")
	go rt.Run(ctx)

	waitFor(t, "setup state", func() bool { return rt.Status().State == "setup" })

	v1, err := st.Save(ctx, demoConfig("demo"), "test", "ok")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "version 1 running", func() bool { s := rt.Status(); return s.State == "running" && s.Version == v1 })

	// valid according to the schema, but the producer references a missing site
	if _, err := st.Save(ctx, demoConfig("missing"), "test", "kaputt"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "rejection", func() bool { return rt.Status().State == "error" })
	s := rt.Status()
	if s.Version != v1 || !strings.Contains(s.Message, "Version 1 läuft weiter") {
		t.Fatalf("unexpected status %+v", s)
	}
	if len(rt.State().Producers) != 2 {
		t.Fatalf("old site should still deliver data: %+v", rt.State())
	}

	v3, _ := st.Save(ctx, demoConfig("demo"), "test", "repariert")
	waitFor(t, "version 3 running", func() bool { s := rt.Status(); return s.State == "running" && s.Version == v3 })

	// removing everything deliberately returns to setup
	st.Save(ctx, config.Default(), "test", "leer")
	waitFor(t, "setup after clearing", func() bool { return rt.Status().State == "setup" })
}
