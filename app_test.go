package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestAppQuiesceReconfigureAndShutdown(t *testing.T) {
	a := &App{}
	t.Cleanup(a.shutdown)
	cfg := map[string]any{"state_path": filepath.Join(t.TempDir(), "state.json"), "dry_run": true, "poll_seconds": 3600}
	yaml, _ := json.Marshal(cfg)
	raw, _ := json.Marshal(map[string]any{"config_yaml": yaml, "schema_version": 6})
	if _, err := a.handle("plugin.register", raw); err != nil {
		t.Fatal(err)
	}
	original := a.engine
	done := a.done
	if _, err := a.handle("plugin.quiesce", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	default:
		t.Fatal("quiesce did not join worker")
	}
	if a.engine != nil {
		t.Fatal("quiesce did not release state owner")
	}
	if _, err := a.handle("plugin.reconfigure", raw); err != nil {
		t.Fatal(err)
	}
	if a.engine == nil || a.engine == original {
		t.Fatal("reconfigure did not start new generation")
	}
	if _, err := a.handle("plugin.shutdown", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if a.engine != nil {
		t.Fatal("shutdown did not detach engine")
	}
	a.shutdown()
}

func TestAppManagementResourcesContainNoState(t *testing.T) {
	a := &App{}
	raw, _ := json.Marshal(managementRequest{Method: "GET", Path: "/dashboard"})
	out, err := a.management(raw)
	if err != nil {
		t.Fatal(err)
	}
	var e struct {
		Result managementResponse `json:"result"`
	}
	if err = json.Unmarshal(out, &e); err != nil {
		t.Fatal(err)
	}
	if e.Result.StatusCode != 200 || len(e.Result.Body) < 1000 {
		t.Fatal("dashboard missing")
	}
	raw, _ = json.Marshal(managementRequest{Method: "POST", Path: "/dashboard"})
	out, err = a.management(raw)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(out, &e)
	if e.Result.StatusCode != 405 {
		t.Fatal("resource accepts state-changing method")
	}
	raw, _ = json.Marshal(managementRequest{Method: "POST", Path: "/cpa-window-keeper/check"})
	out, err = a.management(raw)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(out, &e)
	if e.Result.StatusCode != 503 {
		t.Fatal("stopped worker accepts trigger")
	}
}
