package bitget

import (
	"encoding/json"
	"testing"
)

// A cancelled IOC order with no fill comes back from Bitget's order history
// with an empty fee detail (live 2026-11-27 16:58: etfp1795768350882-000CBX,
// a reduce-only buy of SOXLUSDT that filled nothing); its fee is zero, and
// reading it must not fail the round's recovery.
func TestFeeOfEmptyFeeIsZero(t *testing.T) {
	var details []feeDetailRow
	if err := json.Unmarshal([]byte(`[{"feeCoin":"","fee":""}]`), &details); err != nil {
		t.Fatal(err)
	}
	fee, err := feeOf(details)
	if err != nil {
		t.Fatalf("feeOf(empty fee) failed: %v", err)
	}
	if !fee.IsZero() {
		t.Fatalf("feeOf(empty fee) = %s, want 0", fee)
	}
}

// Text that is not a number and not empty is still refused.
func TestFeeOfBadFeeIsRefused(t *testing.T) {
	if _, err := feeOf([]feeDetailRow{{FeeCoin: "USDT", Fee: "n/a"}}); err == nil {
		t.Fatal("feeOf(\"n/a\") did not fail")
	}
}
