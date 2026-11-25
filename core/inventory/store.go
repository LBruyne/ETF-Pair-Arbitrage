package inventory

import (
	"encoding/json"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// UseStore sets where the manager records its rows. Call it before Restore
// and Run.
func (m *InventoryManager) UseStore(s store.Sink) { m.store = s }

// record queues rows for the store; they may be dropped when it is full.
func (m *InventoryManager) record(rows ...store.Row) {
	if m.store != nil {
		m.store.Record(rows...)
	}
}

// writeSync commits rows in one transaction and returns after the commit.
// An error means they are not confirmed written.
func (m *InventoryManager) writeSync(rows ...store.Row) error {
	if m.store == nil {
		return nil
	}
	return m.store.WriteSync(rows...)
}

// event records one event row of this module. An alarm is an Error log
// line, written by the caller, plus an event of level "error".
func (m *InventoryManager) event(now time.Time, level, kind, message string, detail map[string]any) {
	m.record(eventRow(now, level, kind, message, detail))
}

// eventRow builds one event row of this module.
func eventRow(now time.Time, level, kind, message string, detail map[string]any) store.EventRow {
	b, err := json.Marshal(detail)
	if err != nil {
		b = nil
	}
	return store.EventRow{TsMs: now.UnixMilli(), Module: "inventory", Level: level, Kind: kind, Message: message, Detail: b}
}

// --- rows ---

// stateRow is the inventory_state row: the capital the ledger runs on, when
// it started, r_closed and the net dust of both legs.
func (m *InventoryManager) stateRow(rClosed decimal.Decimal, dust [2]Dust) store.InventoryStateRow {
	return store.InventoryStateRow{Capital: m.capital, SinceMs: unixMs(m.since), RClosed: rClosed, Dust: encodeDust(dust)}
}

// cycleRow is the cycle's row.
func cycleRow(c *Cycle) store.CycleRow {
	return store.CycleRow{CycleID: string(c.ID), SessionID: c.SessionID, Direction: string(c.Direction), Status: string(c.Status), EndReason: c.EndReason,
		PairAnchorKind: string(c.PairAnchors.Kind), PairAnchorBull: c.PairAnchors.Bull, PairAnchorBear: c.PairAnchors.Bear,
		OpenedMs: c.OpenedAt.UnixMilli(), ClosedMs: unixMs(c.ClosedAt),
		Gross: c.Gross, Fees: c.Fees, Funding: c.Funding, FundingKnown: c.FundingKnown, Net: c.Net,
		DustBull: c.DustLeft[cex.Bull], DustBear: c.DustLeft[cex.Bear]}
}

// lotRow is the lot's row.
func lotRow(l *Lot) store.LotRow {
	bull, bear := l.Legs[cex.Bull], l.Legs[cex.Bear]
	return store.LotRow{LotID: string(l.ID), CycleID: string(l.CycleID), Seq: l.Seq, Status: string(l.Status), VoidReason: l.VoidReason, Direction: string(l.Direction),
		XD: l.X, MuD: l.Mu, SigmaD: l.Sigma, FeeD: l.FeeD, FeeUSDTEst: l.FeeUSDTEst,
		EntryQtyBull: bull.EntryQty, EntryValueBull: bull.EntryValue, EntryFeeBull: bull.EntryFee, ExitQtyBull: bull.ExitQty, ExitValueBull: bull.ExitValue, ExitFeeBull: bull.ExitFee,
		EntryQtyBear: bear.EntryQty, EntryValueBear: bear.EntryValue, EntryFeeBear: bear.EntryFee, ExitQtyBear: bear.ExitQty, ExitValueBear: bear.ExitValue, ExitFeeBear: bear.ExitFee,
		OpenedMs: l.OpenedAt.UnixMilli(), ClosedMs: unixMs(l.ClosedAt), Gross: l.Gross, Fees: l.Fees, Net: l.Net}
}

// cycleFromRow rebuilds the unfinished cycle and its lots from their stored
// rows.
func cycleFromRow(row store.CycleRow, lots []store.LotRow) *Cycle {
	c := &Cycle{ID: CycleID(row.CycleID), Status: CycleStatus(row.Status), SessionID: row.SessionID, Direction: strategy.Direction(row.Direction),
		PairAnchors: cex.Anchors{SessionID: row.SessionID, Kind: cex.AnchorKind(row.PairAnchorKind), Bull: row.PairAnchorBull, Bear: row.PairAnchorBear},
		Gross:       row.Gross, Fees: row.Fees, Net: row.Net, Funding: row.Funding, FundingKnown: row.FundingKnown,
		OpenedAt: time.UnixMilli(row.OpenedMs), EndReason: row.EndReason}
	for _, r := range lots {
		c.Lots = append(c.Lots, &Lot{ID: LotID(r.LotID), CycleID: CycleID(r.CycleID), Seq: r.Seq, Status: LotStatus(r.Status), VoidReason: r.VoidReason,
			Direction: strategy.Direction(r.Direction),
			Legs: [2]LotLeg{
				cex.Bull: {EntryQty: r.EntryQtyBull, EntryValue: r.EntryValueBull, EntryFee: r.EntryFeeBull, ExitQty: r.ExitQtyBull, ExitValue: r.ExitValueBull, ExitFee: r.ExitFeeBull},
				cex.Bear: {EntryQty: r.EntryQtyBear, EntryValue: r.EntryValueBear, EntryFee: r.EntryFeeBear, ExitQty: r.ExitQtyBear, ExitValue: r.ExitValueBear, ExitFee: r.ExitFeeBear}},
			Gross: r.Gross, Fees: r.Fees, Net: r.Net, OpenedAt: time.UnixMilli(r.OpenedMs), ClosedAt: fromUnixMs(r.ClosedMs),
			X: r.XD, Mu: r.MuD, Sigma: r.SigmaD, FeeD: r.FeeD, FeeUSDTEst: r.FeeUSDTEst})
	}
	return c
}

// unixMs is t in unix milliseconds, 0 for the zero time (written as NULL).
func unixMs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// fromUnixMs reverses unixMs.
func fromUnixMs(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// --- dust columns ---

// dustEntry is one leg's dust in inventory_state.dust: an object keyed by
// leg name, holding only the legs that have dust.
type dustEntry struct {
	Qty      decimal.Decimal `json:"qty"`       // signed
	AvgPrice decimal.Decimal `json:"avg_price"` // USDT
}

// encodeDust writes the dust of both legs as a dust column; "{}" without
// dust.
func encodeDust(dust [2]Dust) json.RawMessage {
	entries := map[string]dustEntry{}
	for _, leg := range cex.Legs {
		if d := dust[leg]; !d.Qty.IsZero() {
			entries[leg.String()] = dustEntry{Qty: d.Qty, AvgPrice: d.AvgPrice}
		}
	}
	b, err := json.Marshal(entries)
	if err != nil {
		return nil // cannot happen: strings and decimals only
	}
	return b
}

// decodeDust reads a dust column; an empty one (NULL) is no dust.
func decodeDust(raw json.RawMessage) (dust [2]Dust, err error) {
	if len(raw) == 0 {
		return dust, nil
	}
	var entries map[string]dustEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return dust, err
	}
	for _, leg := range cex.Legs {
		if e, ok := entries[leg.String()]; ok {
			dust[leg] = Dust{Qty: e.Qty, AvgPrice: e.AvgPrice}
		}
	}
	return dust, nil
}

// --- awaited funding ---

// awaitedEntry is one settlement and leg in cycle.funding_awaited.
type awaitedEntry struct {
	SettlementMs int64  `json:"settlement_ms"`
	Leg          string `json:"leg"`
}

// encodeAwaited writes what a finished cycle's funding still awaits; empty
// (NULL) when nothing.
func encodeAwaited(awaited []fundingKey) json.RawMessage {
	if len(awaited) == 0 {
		return nil
	}
	entries := make([]awaitedEntry, len(awaited))
	for i, k := range awaited {
		entries[i] = awaitedEntry{SettlementMs: k.settlementMs, Leg: k.leg.String()}
	}
	b, err := json.Marshal(entries)
	if err != nil {
		return nil // cannot happen: integers and strings only
	}
	return b
}

// decodeAwaited reads cycle.funding_awaited.
func decodeAwaited(raw json.RawMessage) ([]fundingKey, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var entries []awaitedEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, err
	}
	awaited := make([]fundingKey, len(entries))
	for i, e := range entries {
		awaited[i] = fundingKey{settlementMs: e.SettlementMs, leg: cex.Bull}
		if e.Leg == cex.Bear.String() {
			awaited[i].leg = cex.Bear
		}
	}
	return awaited, nil
}
