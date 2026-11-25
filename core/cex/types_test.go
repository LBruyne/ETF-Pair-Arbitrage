package cex

import (
	"strings"
	"testing"
)

// The same corporate action written twice is refused: it would be applied
// twice. Actions that differ in ticker, ex-date or kind are separate.
func TestConfigRejectsDuplicateCorporateActions(t *testing.T) {
	cfg := testConfig()
	cfg.CorporateActions = []CorporateAction{
		{Ticker: "SOXS", ExDate: "2026-12-22", Kind: ActionDividend, Source: "the issuer's page"},
		{Ticker: "SOXL", ExDate: "2026-12-22", Kind: ActionDividend},
		{Ticker: "SOXS", ExDate: "2026-12-22", Kind: ActionSplit},
		{Ticker: "SOXS", ExDate: "2027-03-23", Kind: ActionDividend},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("four different actions were refused: %v", err)
	}
	// The same dividend again, from another source note.
	cfg.CorporateActions = append(cfg.CorporateActions, CorporateAction{Ticker: "SOXS", ExDate: "2026-12-22", Kind: ActionDividend, Source: "merged from the board's configuration"})
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "cex.corporate_actions[4] repeats cex.corporate_actions[0] (SOXS 2026-12-22 dividend)") {
		t.Errorf("Validate = %v, want the repeated entry named by its yaml path", err)
	}
	if _, err := NewManager(cfg, newFakeVenue(), &fakeIndex{}, &fakeFX{}, &fakeIssuer{}); err == nil {
		t.Error("NewManager accepted the repeated action")
	}
}
