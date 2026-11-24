package main

import "testing"

// The example configuration loads (every key known, dates and figures as
// written) and passes the collector's checks.
func TestExampleConfig(t *testing.T) {
	cfg, err := loadConfig("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if a := cfg.CorporateActions[1]; a.Ticker != "SOXS" || a.ExDate != "2026-12-22" || a.Value != "" || a.Status != "" {
		t.Fatalf("second action %+v", a)
	}
}
