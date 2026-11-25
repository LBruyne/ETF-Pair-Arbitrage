package cex

import (
	"encoding/json"
	"time"

	"etf-pair-arbitrage/core/store"
)

// UseStore sets where the manager records its rows. Call it before Run.
func (m *CexManager) UseStore(s store.Sink) { m.store = s }

// recordFact writes one anchor_fact row. detail holds the raw inputs behind
// the value.
func (m *CexManager) recordFact(f AnchorFact, detail map[string]any) {
	if m.store == nil {
		return
	}
	m.store.Record(store.AnchorFactRow{SessionID: f.SessionID, Kind: f.Kind, Leg: f.Leg.String(), KnownMs: f.KnownAt.UnixMilli(),
		Value: f.Value, F: f.F, Source: f.Source, Detail: marshalDetail(detail)})
}

// recordEvent writes one event row of this module. An alarm is an Error log
// line, written by the caller, plus an event of level "error".
func (m *CexManager) recordEvent(level, kind, message string, detail map[string]any) {
	if m.store == nil {
		return
	}
	m.store.Record(store.EventRow{TsMs: m.timing.clock().UnixMilli(), Module: "cex", Level: level, Kind: kind, Message: message, Detail: marshalDetail(detail)})
}

// marshalDetail encodes a detail column; nil stays empty and is written as
// NULL.
func marshalDetail(detail map[string]any) json.RawMessage {
	if detail == nil {
		return nil
	}
	b, err := json.Marshal(detail)
	if err != nil {
		return nil
	}
	return b
}

// AnchorFactsFromRows converts the anchor_fact rows read back at start into
// facts for Restore. A row of a kind or leg this version does not know is
// left out.
func AnchorFactsFromRows(rows []store.AnchorFactRow) []AnchorFact {
	out := make([]AnchorFact, 0, len(rows))
	for _, r := range rows {
		var leg Leg
		switch r.Leg {
		case Bull.String():
			leg = Bull
		case Bear.String():
			leg = Bear
		default:
			continue
		}
		switch r.Kind {
		case FactBellF, FactBell, FactNAVSeen, FactNAV, FactDividend:
		default:
			continue
		}
		out = append(out, AnchorFact{SessionID: r.SessionID, Kind: r.Kind, Leg: leg, Value: r.Value, F: r.F, Source: r.Source, KnownAt: time.UnixMilli(r.KnownMs)})
	}
	return out
}

// orderRow is the cex_order row of an order record.
func orderRow(r OrderRecord) store.OrderRow {
	q := r.Request
	return store.OrderRow{ClientOrderID: string(q.ClientOrderID), CycleID: q.CycleID, LotID: q.LotID, Leg: q.Leg.String(), Symbol: string(q.Symbol),
		Purpose: string(q.Purpose), Side: string(q.Side), ReduceOnly: q.ReduceOnly, Price: q.Price, Qty: q.Qty, QuotaPrice: q.QuotaPrice,
		Status: string(r.Status), Unknown: r.Unknown, OrderID: string(r.OrderID), CumExecQty: r.CumExecQty, CumExecValue: r.CumExecValue, AvgPrice: r.AvgPrice,
		Fee: r.Fee, FeeKnown: r.FeeKnown, NotFound: r.NotFound, CancelReason: r.CancelReason, ErrorCode: r.ErrorCode, ErrorMessage: r.ErrorMessage,
		UnknownSinceMs: unixMs(r.UnknownSince), SubmittedMs: unixMs(r.SubmittedAt), SentMs: unixMs(r.SentAt), AckedMs: unixMs(r.AckedAt),
		UpdateReceivedMs: unixMs(r.UpdateReceivedAt), ExchangeUpdatedMs: unixMs(r.ExchangeUpdatedAt),
		AcceptedMs: unixMs(r.AcceptedAt), ExchangeCreatedMs: unixMs(r.ExchangeCreatedAt), FinalMs: unixMs(r.FinalAt)}
}

// OrderRecordsFromRows converts the cex_order rows read back at start into
// order records for Restore. UpdateSeq is not stored: Restore gives each
// record this process's own.
func OrderRecordsFromRows(rows []store.OrderRow) []OrderRecord {
	out := make([]OrderRecord, 0, len(rows))
	for _, row := range rows {
		leg := Bull
		if row.Leg == Bear.String() {
			leg = Bear
		}
		out = append(out, OrderRecord{
			Request: OrderRequest{ClientOrderID: ClientOrderID(row.ClientOrderID), Symbol: Symbol(row.Symbol), Leg: leg, Side: Side(row.Side), Price: row.Price, Qty: row.Qty,
				QuotaPrice: row.QuotaPrice, ReduceOnly: row.ReduceOnly, Purpose: OrderPurpose(row.Purpose), CycleID: row.CycleID, LotID: row.LotID},
			OrderID: OrderID(row.OrderID), Status: OrderStatus(row.Status), Unknown: row.Unknown, UnknownSince: fromUnixMs(row.UnknownSinceMs),
			FeeKnown: row.FeeKnown, NotFound: row.NotFound, CumExecQty: row.CumExecQty, CumExecValue: row.CumExecValue, AvgPrice: row.AvgPrice, Fee: row.Fee,
			CancelReason: row.CancelReason, ErrorCode: row.ErrorCode, ErrorMessage: row.ErrorMessage,
			SubmittedAt: fromUnixMs(row.SubmittedMs), SentAt: fromUnixMs(row.SentMs), AckedAt: fromUnixMs(row.AckedMs),
			ExchangeUpdatedAt: fromUnixMs(row.ExchangeUpdatedMs), UpdateReceivedAt: fromUnixMs(row.UpdateReceivedMs),
			AcceptedAt: fromUnixMs(row.AcceptedMs), ExchangeCreatedAt: fromUnixMs(row.ExchangeCreatedMs), FinalAt: fromUnixMs(row.FinalMs)})
	}
	return out
}

// fillRow is the cex_fill row of a fill.
func fillRow(f Fill) store.FillRow {
	return store.FillRow{ExecID: f.ExecID, ClientOrderID: string(f.ClientOrderID), OrderID: string(f.OrderID), Symbol: string(f.Symbol), Side: string(f.Side),
		Price: f.Price, Qty: f.Qty, Value: f.Value, Fee: f.Fee, ExecMs: unixMs(f.ExecTime), ReceivedMs: unixMs(f.ReceivedAt)}
}

// unixMs is a time as a *_ms column: zero for the zero time.
func unixMs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// fromUnixMs reads a *_ms column: the zero time for zero.
func fromUnixMs(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
