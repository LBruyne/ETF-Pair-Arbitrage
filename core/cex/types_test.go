package cex

import (
	"strings"
	"testing"
)

// The number of book connections per leg is 1 to 5 and the order transport
// is ws or rest; the defaults are 3 and ws.
func TestConfigBookConnectionsAndOrderTransport(t *testing.T) {
	cfg := DefaultConfig
	cfg.Pair = testConfig().Pair
	if err := cfg.Validate(); err != nil || cfg.BookConnections != 3 || cfg.OrderTransport != OrderTransportWS {
		t.Fatalf("the defaults: %d connections, transport %q, %v", cfg.BookConnections, cfg.OrderTransport, err)
	}
	for _, n := range []int{1, 5} {
		cfg.BookConnections = n
		if err := cfg.Validate(); err != nil {
			t.Errorf("book_connections %d: %v", n, err)
		}
	}
	for _, n := range []int{0, -1, 6} {
		cfg.BookConnections = n
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "cex.book_connections") {
			t.Errorf("book_connections %d: %v, want it refused", n, err)
		}
	}
	cfg.BookConnections = 3
	cfg.OrderTransport = OrderTransportREST
	if err := cfg.Validate(); err != nil {
		t.Errorf("order_transport rest: %v", err)
	}
	for _, transport := range []OrderTransport{"", "wss", "REST"} {
		cfg.OrderTransport = transport
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "cex.order_transport") {
			t.Errorf("order_transport %q: %v, want it refused", transport, err)
		}
	}
}

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
