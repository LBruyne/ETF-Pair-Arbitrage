package exec

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/inventory"
	"etf-pair-arbitrage/core/store"
)

// randomBook is a book of both legs around the rig's calibration mids
// (40.40 / 59.42), moved at random: SOXL two levels a side, SOXS five, each
// level's quantity at random, so that entries, partial and missed thick
// orders, top-ups, profit closes, unwinds and exits all occur.
func randomBook(rng *rand.Rand) pairBook {
	bullMid := 40.40 + float64(rng.Intn(21)-10)*0.01
	bearMid := 59.42 + float64(rng.Intn(41)-20)*0.002
	level := func(p float64, places int, qmin, qmax float64) cex.BookLevel {
		q := qmin + rng.Float64()*(qmax-qmin)
		return lv(fmt.Sprintf("%.*f", places, p), fmt.Sprintf("%.2f", q))
	}
	var b pairBook
	half := float64(1+rng.Intn(2)) * 0.01
	for i := 0; i < 2; i++ {
		b.bullBids = append(b.bullBids, level(bullMid-half-float64(i)*0.01, 2, 1, 400))
		b.bullAsks = append(b.bullAsks, level(bullMid+half+float64(i)*0.01, 2, 1, 400))
	}
	bearHalf := float64(1+rng.Intn(3)) * 0.002
	for i := 0; i < 5; i++ {
		b.bearBids = append(b.bearBids, level(bearMid-bearHalf-float64(i)*0.002, 3, 0.05, 40))
		b.bearAsks = append(b.bearAsks, level(bearMid+bearHalf+float64(i)*0.002, 3, 0.05, 40))
	}
	return b
}

// SPEC 12.10, the property test: on random book sequences every thin-leg-
// first batch reaches an end state, and what is paired (the quota's used,
// which leaves the unpaired thin leg out) never exceeds the quota. Every
// order meets the book it was decided on: a book that improves between the
// send and the fill can take a little more than the quota (PRD 6.4: "发单
// 之后价格再改善，仍可能有小额超出").
func TestThinFirstPropertyRandomBooks(t *testing.T) {
	for seed := int64(1); seed <= 4; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			r := thinRig(t, rigOptions{capital: "400", latency: time.Duration(1+rng.Intn(3)) * time.Millisecond})
			now := utc8Time(11, 25, 5, 3, 1)
			var last pairBook
			// What is paired: the quota's used less the thin leg's unpaired
			// rest of the batches that ended (dust too small to pair or to
			// close, which then takes quota as the dust it is, PRD 6.4).
			check := func() {
				t.Helper()
				total, used, _ := r.inv.Quota()
				paired := used
				if c := r.inv.Cycle(); c != nil {
					for _, l := range c.Lots {
						thin, thick := l.Legs[cex.Bear], l.Legs[cex.Bull]
						if rest := thin.Open().Sub(thick.Open().Mul(dec("40")).Div(dec("60"))); l.End != "" && rest.IsPositive() {
							paired = paired.Sub(rest.Mul(thin.EntryValue).Div(thin.EntryQty))
						}
					}
				}
				if paired.GreaterThan(total) {
					t.Fatalf("at %s the paired notional %s exceeds the quota %s (used %s)", showTime(now), paired, total, used)
				}
			}
			for i := 0; i < 400; i++ {
				now = now.Add(time.Duration(50+rng.Intn(1500)) * time.Millisecond)
				r.at(now)
				if rng.Intn(3) > 0 {
					last = randomBook(rng)
					r.books(last)
				}
				r.step()
				r.results()
				if rng.Intn(2) == 0 { // the books move between a result and the round it wakes
					last = randomBook(rng)
					r.books(last)
				}
				r.step()
				r.results() // an order sent by that round meets the book it was decided on
				check()
			}
			// No new entries; the books stay; the windows end every batch.
			r.m.stop.noEntries = true
			for i := 0; i < 40; i++ {
				now = now.Add(2 * time.Second)
				r.at(now)
				r.books(last)
				r.step()
				r.results()
				r.step()
				r.results()
				check()
			}
			lots, building := map[string]store.LotRow{}, 0
			for _, row := range r.sink.Rows("lot") {
				l := row.(store.LotRow)
				lots[l.LotID] = l
			}
			ends := map[string]int{}
			for _, l := range lots {
				if l.Status == string(inventory.LotBuilding) {
					building++
					continue
				}
				if l.EndState == "" && l.VoidReason != inventory.VoidStoreError {
					t.Errorf("lot %s %s without an end state", l.LotID, l.Status)
				}
				ends[l.EndState]++
			}
			if building > 0 {
				t.Errorf("%d of %d batches never ended", building, len(lots))
			}
			if len(lots) < 3 {
				t.Errorf("only %d batches in the sequence: the books do not exercise the pairing", len(lots))
			}
			t.Logf("batches %d, end states %v, orders %d", len(lots), ends, len(r.orders()))
		})
	}
}
