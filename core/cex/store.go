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
