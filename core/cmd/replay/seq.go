package main

import (
	"encoding/json"
	"sort"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/store"
)

// The report of core/SPEC.md 12.10 for comparing the thin leg first and the
// two-stage exit with both legs together and one exit line, computed from a
// run's records alone: it tells "the thin leg first cut the cost of one-leg
// batches" apart from "it only traded less".

// heldPoint is the ledger's held value (inventory HeldNotional) from ms on.
type heldPoint struct {
	ms    int64
	value decimal.Decimal
}

// seqStats is that report for one Session or the whole window.
type seqStats struct {
	// Entry: every entry decision sent a batch (a valid signal).
	Signals         int             `json:"signals"`
	PlannedNotional decimal.Decimal `json:"planned_notional"` // both legs at the decisions' prices and quantities
	BuiltNotional   decimal.Decimal `json:"built_notional"`   // what stayed in the batches: per lot and leg, entered less trimmed, at the entry average
	EndStates       map[string]int  `json:"end_states"`       // batches by end state; both legs together are named by their fills (seqEndState)

	// One leg alone: from a batch's first fill to its last building fill
	// (entry, completion, trim), over the batches that filled.
	ExposedBatches  int     `json:"exposed_batches"`
	ExposureMedianS float64 `json:"exposure_median_s"`
	ExposureMaxS    float64 `json:"exposure_max_s"`

	// The building's costs, realised as the ledger books them.
	CompleteSlip   decimal.Decimal `json:"complete_slip"`    // completions (the thick leg's orders): paid beyond the decision's price of the leg; positive is worse
	UnwindNet      decimal.Decimal `json:"unwind_net"`       // trims other than profit closes: their realised net
	ProfitCloseNet decimal.Decimal `json:"profit_close_net"` // the thin leg's profit closes (12.4 step 7): their realised net

	// Exit batches: D1 (the pre-check failed, exited all the same) and the
	// D2 hard end of the thick leg; their realised net and, for D2, the
	// decisions' expected net.
	D1Batches  int             `json:"d1_batches"`
	D1Net      decimal.Decimal `json:"d1_net"`
	D2Batches  int             `json:"d2_batches"`
	D2Net      decimal.Decimal `json:"d2_net"`
	D2Expected decimal.Decimal `json:"d2_expected"`

	// What the exits closed (traded value) and realised, by line: exit (one
	// line), exit1, exit2 and forced.
	ClosedByLine map[string]decimal.Decimal `json:"closed_by_line"`
	NetByLine    map[string]decimal.Decimal `json:"net_by_line"`
	StagesSet    int                        `json:"stages_set"`  // half targets set at exit1
	StagesDone   int                        `json:"stages_done"` // and closed in full

	// From a cycle's first line-exit fill until it is flat (closed, or the
	// window's end): how long and how much quota it held, the ledger's held
	// value over time in USDT hours. With exit2 this is the wait for it.
	LineToFlatCycles  int     `json:"line_to_flat_cycles"`
	LineToFlatMedianS float64 `json:"line_to_flat_median_s"`
	LineToFlatMaxS    float64 `json:"line_to_flat_max_s"`
	LineToFlatUSDTh   float64 `json:"line_to_flat_usdt_h"`

	// The exit batches (one per exit decision): how many, and the longest
	// from the decision to the batch's last fill. A stuck exit shows here.
	ExitBatches   int     `json:"exit_batches"`
	ExitBatchMaxS float64 `json:"exit_batch_max_s"`

	// The orders the venue refused, and of them those refused as a reducing
	// quantity larger than the position (Bitget 40804 / 25231).
	Refused          int `json:"refused"`
	RefusedReduceQty int `json:"refused_reduce_qty"`

	// The closed cycles whose orders left a quantity on a leg (dust, such as
	// a per-batch rounding of the exit leaves), and that quantity by leg,
	// signed.
	ResidualCycles int                `json:"residual_cycles"`
	ResidualQty    [2]decimal.Decimal `json:"residual_qty"`

	// The orders' realised nets of the closed cycles, and that less the
	// cycles' round-trip net: zero up to the ledger's rounding.
	RealisedNet   decimal.Decimal `json:"realised_net"`
	RealisedCheck decimal.Decimal `json:"realised_check"`
}

// reduceQtyCodes are Bitget's refusals of a reducing quantity larger than
// the position: 40804 by WebSocket, 25231 by REST (refusingPaper gives
// 40804).
var reduceQtyCodes = map[string]bool{"40804": true, "25231": true}

// The exit lines as the report names them.
const (
	lineOne    = "exit"
	lineForced = "forced"
)

// seqReport computes the report over the cycles in inSession; endMs is the
// window's end (0: the summary does not end there), up to which a cycle
// still open holds its quota.
func seqReport(s *memSink, inSession map[string]bool, endMs int64) seqStats {
	q := seqStats{EndStates: map[string]int{}, ClosedByLine: map[string]decimal.Decimal{}, NetByLine: map[string]decimal.Decimal{}}
	realised, trimmed, entered := realiseOrders(s, inSession)

	// The decisions: the entries' planned prices, the trims' reasons, the
	// exit batches.
	type exitBatch struct {
		cycle, line   string
		at, lastFill  int64
		d1, d2        bool
		expected, net decimal.Decimal
	}
	planPrice := map[string][2]decimal.Decimal{} // lot -> decision price by leg
	trimWhy := map[string]string{}               // client id -> the pair decision's reason
	var batches []*exitBatch
	for _, dec := range s.decisions {
		if !inSession[dec.CycleID] {
			continue
		}
		var detail struct {
			PriceBull   decimal.Decimal `json:"price_bull"`
			PriceBear   decimal.Decimal `json:"price_bear"`
			QtyBull     decimal.Decimal `json:"qty_bull"`
			QtyBear     decimal.Decimal `json:"qty_bear"`
			Line        string          `json:"line"`
			ExpectedNet decimal.Decimal `json:"expected_net"`
			Action      string          `json:"action"`
			ClientID    string          `json:"client_order_id"`
		}
		_ = json.Unmarshal(dec.Detail, &detail) // a detail that does not decode leaves zeros
		switch dec.Kind {
		case "entry":
			q.Signals++
			q.PlannedNotional = q.PlannedNotional.Add(detail.PriceBull.Mul(detail.QtyBull)).Add(detail.PriceBear.Mul(detail.QtyBear))
			planPrice[dec.LotID] = [2]decimal.Decimal{detail.PriceBull, detail.PriceBear}
		case "pair":
			if detail.ClientID != "" {
				trimWhy[detail.ClientID] = dec.Reason
			}
		case "exit":
			line := detail.Line
			if line == "" {
				line = lineOne
			}
			batches = append(batches, &exitBatch{cycle: dec.CycleID, line: line, at: dec.TsMs, d1: dec.Reason == "d1_loss", expected: detail.ExpectedNet})
		case "exit_pair":
			if detail.Action == "risk_exit" {
				for i := len(batches) - 1; i >= 0; i-- {
					if batches[i].cycle == dec.CycleID {
						batches[i].d2 = true
						break
					}
				}
			}
		}
	}
	batchOf := func(o store.OrderRow) *exitBatch { // the cycle's last exit decision at or before the order
		for i := len(batches) - 1; i >= 0; i-- {
			if b := batches[i]; b.cycle == o.CycleID && b.at <= o.SubmittedMs {
				return b
			}
		}
		return nil
	}

	// The orders: completions, trims, exits.
	lineOf := map[string]string{}           // exit client id -> line
	batchOfOrder := map[string]*exitBatch{} // exit client id -> its batch
	for _, id := range s.orderSeq {
		o := s.orders[id]
		if inSession[o.CycleID] && o.Status == "rejected" {
			q.Refused++
			if reduceQtyCodes[o.ErrorCode] {
				q.RefusedReduceQty++
			}
		}
		if !inSession[o.CycleID] || !o.CumExecQty.IsPositive() {
			continue
		}
		switch o.Purpose {
		case "complete":
			if ref, ok := planPrice[o.LotID]; ok {
				refValue := ref[legIndex(o.Leg)].Mul(o.CumExecQty)
				if o.Side == "buy" {
					q.CompleteSlip = q.CompleteSlip.Add(o.CumExecValue.Sub(refValue))
				} else {
					q.CompleteSlip = q.CompleteSlip.Add(refValue.Sub(o.CumExecValue))
				}
			}
		case "trim":
			if trimWhy[id] == "profit" {
				q.ProfitCloseNet = q.ProfitCloseNet.Add(realised[id])
			} else {
				q.UnwindNet = q.UnwindNet.Add(realised[id])
			}
		case "exit", "forced":
			line := lineForced
			if o.Purpose == "exit" {
				line = lineOne
				if b := batchOf(o); b != nil {
					line = b.line
					b.net = b.net.Add(realised[id])
					batchOfOrder[id] = b
				}
			}
			lineOf[id] = line
			q.ClosedByLine[line] = q.ClosedByLine[line].Add(o.CumExecValue)
			q.NetByLine[line] = q.NetByLine[line].Add(realised[id])
		}
	}
	for _, fid := range s.fillSeq {
		f := s.fills[fid]
		if b := batchOfOrder[f.ClientOrderID]; b != nil && f.ExecMs > b.lastFill {
			b.lastFill = f.ExecMs
		}
	}
	q.ExitBatches = len(batches)
	for _, b := range batches {
		if b.lastFill > 0 {
			q.ExitBatchMaxS = max(q.ExitBatchMaxS, float64(b.lastFill-b.at)/1000)
		}
		if b.d1 {
			q.D1Batches++
			q.D1Net = q.D1Net.Add(b.net)
		}
		if b.d2 {
			q.D2Batches++
			q.D2Net, q.D2Expected = q.D2Net.Add(b.net), q.D2Expected.Add(b.expected)
		}
	}

	// The lots: end states, what stayed built, one leg alone.
	first, last := map[string]int64{}, map[string]int64{}
	building := map[string]bool{"entry": true, "complete": true, "trim": true}
	firstLineFill := map[string]int64{} // cycle -> its first line-exit fill
	for _, fid := range s.fillSeq {
		f := s.fills[fid]
		o, ok := s.orders[f.ClientOrderID]
		if !ok || !inSession[o.CycleID] {
			continue
		}
		if building[o.Purpose] && o.LotID != "" {
			if at, seen := first[o.LotID]; !seen || f.ExecMs < at {
				first[o.LotID] = f.ExecMs
			}
			if f.ExecMs > last[o.LotID] {
				last[o.LotID] = f.ExecMs
			}
		}
		if line := lineOf[f.ClientOrderID]; line != "" && line != lineForced {
			if at, seen := firstLineFill[o.CycleID]; !seen || f.ExecMs < at {
				firstLineFill[o.CycleID] = f.ExecMs
			}
		}
	}
	var exposures []float64
	for _, id := range s.lotSeq {
		lot := s.lots[id]
		if !inSession[lot.CycleID] {
			continue
		}
		if state := seqEndState(lot, trimmed[id]); state != "" {
			q.EndStates[state]++
		}
		for leg, in := range entered[id] {
			if kept := in[0].Sub(trimmed[id][leg]); kept.IsPositive() {
				q.BuiltNotional = q.BuiltNotional.Add(in[1].Mul(kept).Div(in[0]))
			}
		}
		if at, ok := first[id]; ok {
			exposures = append(exposures, float64(last[id]-at)/1000)
		}
	}
	q.ExposedBatches = len(exposures)
	q.ExposureMedianS, q.ExposureMaxS = medianMax(exposures)

	// The half targets: one per cycle and time set.
	set, done := map[[2]int64]bool{}, map[[2]int64]bool{}
	cycleNo := map[string]int64{}
	for i, id := range s.cycleSeq {
		cycleNo[id] = int64(i)
	}
	for _, r := range s.stages {
		if !inSession[r.CycleID] {
			continue
		}
		key := [2]int64{cycleNo[r.CycleID], r.SetMs}
		set[key] = true
		if r.DoneMs != 0 {
			done[key] = true
		}
	}
	q.StagesSet, q.StagesDone = len(set), len(done)

	// From the first line exit to flat, and the round-trip check.
	var spans []float64
	for _, id := range s.cycleSeq {
		c := s.cycles[id]
		if !inSession[id] {
			continue
		}
		if c.Status == "closed" {
			var left [2]decimal.Decimal
			for _, oid := range s.orderSeq {
				if o := s.orders[oid]; o.CycleID == id {
					q.RealisedNet = q.RealisedNet.Add(realised[oid])
					if o.Side == "buy" {
						left[legIndex(o.Leg)] = left[legIndex(o.Leg)].Add(o.CumExecQty)
					} else {
						left[legIndex(o.Leg)] = left[legIndex(o.Leg)].Sub(o.CumExecQty)
					}
				}
			}
			q.RealisedCheck = q.RealisedCheck.Add(c.Net)
			if !left[0].IsZero() || !left[1].IsZero() {
				q.ResidualCycles++
				q.ResidualQty[0], q.ResidualQty[1] = q.ResidualQty[0].Add(left[0]), q.ResidualQty[1].Add(left[1])
			}
		}
		from, ok := firstLineFill[id]
		if !ok {
			continue
		}
		to := c.ClosedMs
		if c.Status != "closed" {
			if to = endMs; to == 0 {
				continue
			}
		}
		spans = append(spans, float64(to-from)/1000)
		q.LineToFlatUSDTh += heldOver(s.held, from, to) / 3600
	}
	q.RealisedCheck = q.RealisedNet.Sub(q.RealisedCheck)
	q.LineToFlatCycles = len(spans)
	q.LineToFlatMedianS, q.LineToFlatMaxS = medianMax(spans)
	return q
}

// seqEndState is a lot's end state (SPEC 12.4). A batch of both legs
// together has none recorded; it is named by its fills: built, built_partial
// when a trim cut it, entry_failed when nothing filled, unwound_stop when one
// leg filled and was trimmed back. A lot still building is "building"; one
// never sent (store_error) has none.
func seqEndState(lot store.LotRow, trimmed [2]decimal.Decimal) string {
	switch {
	case lot.EndState != "":
		return lot.EndState
	case lot.Status == "open" || lot.Status == "closed":
		if trimmed[0].IsPositive() || trimmed[1].IsPositive() {
			return "built_partial"
		}
		return "built"
	case lot.Status == "building":
		return "building"
	case lot.VoidReason == "no_fill":
		return "entry_failed"
	case lot.VoidReason == "one_leg":
		return "unwound_stop"
	}
	return ""
}

// heldOver is the integral of the held value over [from, to], in USDT
// seconds: the series is a step function, each value holding until the next.
func heldOver(series []heldPoint, from, to int64) float64 {
	total := 0.0
	for i, p := range series {
		end := to
		if i+1 < len(series) && series[i+1].ms < end {
			end = series[i+1].ms
		}
		start := max(p.ms, from)
		if end > start {
			total += p.value.InexactFloat64() * float64(end-start) / 1000
		}
	}
	return total
}

// medianMax is the median and the largest of the values; zeros without any.
func medianMax(v []float64) (median, most float64) {
	if len(v) == 0 {
		return 0, 0
	}
	sorted := append([]float64(nil), v...)
	sort.Float64s(sorted)
	n := len(sorted)
	median = sorted[n/2]
	if n%2 == 0 {
		median = (sorted[n/2-1] + sorted[n/2]) / 2
	}
	return median, sorted[n-1]
}

// --- the orders' realised nets, booked as the ledger books them ---

// realiseOrders books every cycle's orders as inventory's ledger does
// (rebuild and book: in the order sent, an entry or a completion on its lot,
// a trim on its lot first and the rest like an exit, an exit or a forced
// exit first in first out across the cycle's lots, a part's value and fee
// in proportion to its quantity) and returns each closing order's realised
// net: per part, the direction's sign times (its value less its share of
// the lot leg's entry value), less its fee and its share of the entry fee.
// It also returns, per lot and leg, the quantity its own trims closed and
// the quantity and value that entered it.
func realiseOrders(s *memSink, inSession map[string]bool) (realised map[string]decimal.Decimal, trimmed map[string][2]decimal.Decimal, entered map[string][2][2]decimal.Decimal) {
	type leg struct{ entryQty, entryValue, entryFee, exitQty decimal.Decimal }
	type part struct {
		order, lot string
		leg        int
		qty, value decimal.Decimal
		fee        decimal.Decimal
	}
	realised, trimmed, entered = map[string]decimal.Decimal{}, map[string][2]decimal.Decimal{}, map[string][2][2]decimal.Decimal{}
	for _, cid := range s.cycleSeq {
		if !inSession[cid] {
			continue
		}
		var lots []string
		legs := map[string]*[2]leg{}
		for _, id := range s.lotSeq {
			if s.lots[id].CycleID == cid {
				lots = append(lots, id)
				legs[id] = &[2]leg{}
			}
		}
		sort.SliceStable(lots, func(i, j int) bool { return s.lots[lots[i]].Seq < s.lots[lots[j]].Seq })
		var orders []store.OrderRow
		for _, id := range s.orderSeq {
			if o := s.orders[id]; o.CycleID == cid && o.CumExecQty.IsPositive() {
				orders = append(orders, o)
			}
		}
		sort.SliceStable(orders, func(i, j int) bool { return orders[i].ClientOrderID < orders[j].ClientOrderID })
		long := s.cycles[cid].Direction == "long"
		if s.cycles[cid].Direction == "" && len(orders) > 0 { // a cycle row without its direction: the first fill's side says it
			long = orders[0].Side == "buy"
		}

		var parts []part
		for _, o := range orders {
			l := legIndex(o.Leg)
			rest := share{qty: o.CumExecQty, value: o.CumExecValue, fee: o.Fee}
			closeOn := func(lot string) {
				g := &legs[lot][l]
				if q := decimal.Min(rest.qty, g.entryQty.Sub(g.exitQty)); q.IsPositive() {
					p := rest.take(q)
					g.exitQty = g.exitQty.Add(p.qty)
					parts = append(parts, part{order: o.ClientOrderID, lot: lot, leg: l, qty: p.qty, value: p.value, fee: p.fee})
					if o.Purpose == "trim" && lot == o.LotID {
						t := trimmed[lot]
						t[l] = t[l].Add(p.qty)
						trimmed[lot] = t
					}
				}
			}
			switch o.Purpose {
			case "entry", "complete":
				if g := legs[o.LotID]; g != nil {
					g[l].entryQty, g[l].entryValue, g[l].entryFee = g[l].entryQty.Add(rest.qty), g[l].entryValue.Add(rest.value), g[l].entryFee.Add(rest.fee)
				}
				continue
			case "trim":
				if legs[o.LotID] != nil {
					closeOn(o.LotID)
				}
			}
			for _, lot := range lots {
				closeOn(lot)
			}
		}
		for _, p := range parts {
			g := legs[p.lot][p.leg]
			cost := portion(g.entryValue, p.qty, g.entryQty)
			gross := p.value.Sub(cost)
			if !long {
				gross = gross.Neg()
			}
			realised[p.order] = realised[p.order].Add(gross).Sub(p.fee).Sub(portion(g.entryFee, p.qty, g.entryQty))
		}
		for lot, g := range legs {
			entered[lot] = [2][2]decimal.Decimal{{g[0].entryQty, g[0].entryValue}, {g[1].entryQty, g[1].entryValue}}
		}
	}
	return realised, trimmed, entered
}

// share is what of one order is not booked yet; take cuts a part out of it
// with its value and fee in proportion, rounded to allocDecimals, the last
// part taking exactly what is left (inventory's share.take).
type share struct{ qty, value, fee decimal.Decimal }

const allocDecimals = 8

func (s *share) take(q decimal.Decimal) share {
	if q.GreaterThanOrEqual(s.qty) {
		part := *s
		*s = share{}
		return part
	}
	part := share{qty: q, value: s.value.Mul(q).Div(s.qty).Round(allocDecimals), fee: s.fee.Mul(q).Div(s.qty).Round(allocDecimals)}
	s.qty, s.value, s.fee = s.qty.Sub(q), s.value.Sub(part.value), s.fee.Sub(part.fee)
	return part
}

// portion is total x part / whole, rounded; exactly total when part is the
// whole, and zero without a whole (inventory's portion).
func portion(total, part, whole decimal.Decimal) decimal.Decimal {
	switch {
	case whole.IsZero():
		return decimal.Zero
	case part.Equal(whole):
		return total
	}
	return total.Mul(part).Div(whole).Round(allocDecimals)
}
