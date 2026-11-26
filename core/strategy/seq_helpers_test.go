package strategy

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
)

// The thin-leg-first tests use the setting of SPEC 12.5: anchors 32.0287 /
// 62.1464 USD, F = 0.998685, mu = -2, sigma = 3, entry_in 1, entry_out -0.5
// (exit2 -1 where a test sets it), taker fee 0.65 bps, no margin, pricing
// levels 2 (SOXL) and 5 (SOXS), SOXS the thin leg. The expected numbers come
// from experiments/seq_taker_design_20261126/calc.py and a separate Python
// check of the same formulas, never from the code under test.

var seqAnchors = anchors("32.0287", "62.1464")

const seqF = 0.998685

// seqPairing is the exec keys of SPEC 12.8 with SOXS first.
var seqPairing = Pairing{ThinFirst: true, Thin: cex.Bear, Window: 30 * time.Second, StopD: 10, TopupMax: dec("2"), ExitWindow: 30 * time.Second}

// seqStrategy builds the strategy of SPEC 12.5, changed by edit when it is
// not nil.
func seqStrategy(t *testing.T, edit func(*Config)) *Strategy {
	t.Helper()
	return newStrategy(t, func(c *Config) {
		c.EntryIn, c.EntryOut = 1, -0.5
		if edit != nil {
			edit(c)
		}
	})
}

// statsFor returns ready statistics whose mean is mu and whose sigma is
// sigma under the anchors a: the bear leg at its anchor, the bull leg mu
// units off it, and a window of two minutes sigma units either side.
func statsFor(a cex.Anchors, mu, sigma float64) Stats {
	aBull, aBear := a.Bull.InexactFloat64(), a.Bear.InexactFloat64()
	mBull, d := aBull*(1+mu/1e4), aBull*sigma/1e4
	return Stats{
		Ready:       true,
		MeanBullUSD: mBull,
		MeanBearUSD: aBear,
		Window:      [][2]float64{{mBull - d, aBear}, {mBull + d, aBear}},
	}
}

// seqD is the spread at SOXL / SOXS prices under the 12.5 anchors.
func seqD(pBull, pBear float64) float64 {
	return Spread(seqF, pBull, 32.0287, pBear, 62.1464)
}

func wantNear(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.IsNaN(got) || math.Abs(got-want) > tol {
		t.Errorf("%s = %.9f, want %.9f (within %g)", name, got, want, tol)
	}
}

// wantRound checks a decimal rounded to places digits.
func wantRound(t *testing.T, name string, got decimal.Decimal, want string, places int32) {
	t.Helper()
	if !got.Round(places).Equal(dec(want)) {
		t.Errorf("%s = %s (rounds to %s), want %s", name, got, got.Round(places), want)
	}
}

// --- a ledger for the valuations ---

// fakeLeg is one leg of a lot: what was opened and what was closed of it.
type fakeLeg struct {
	entryQty, entryValue decimal.Decimal
	exitQty, exitValue   decimal.Decimal
}

// fakeLedger books closes as inventory does (PRD 7.3-7.5): an exit first in,
// first out over the lots, a trim first into its lot; per leg the closed
// part makes the direction's sign times (exit value - its share of the
// entry value) and costs its exit fee plus the entry fee in proportion.
// Closes in done are booked already and count once.
type fakeLedger struct {
	dir  Direction
	fee  decimal.Decimal
	lots [][2]fakeLeg
}

// lot adds a lot holding bull and bear, each "qty@price"; "" is none.
func (l *fakeLedger) lot(bull, bear string) *fakeLedger {
	var legs [2]fakeLeg
	for i, s := range []string{bull, bear} {
		if s == "" {
			legs[i] = fakeLeg{entryQty: decimal.Zero, entryValue: decimal.Zero, exitQty: decimal.Zero, exitValue: decimal.Zero}
			continue
		}
		q, p, ok := strings.Cut(s, "@")
		if !ok {
			panic("a lot's leg must be written as qty@price: " + s)
		}
		legs[i] = fakeLeg{entryQty: dec(q), entryValue: dec(q).Mul(dec(p)), exitQty: decimal.Zero, exitValue: decimal.Zero}
	}
	l.lots = append(l.lots, legs)
	return l
}

// book closes qty of leg at price into the ledger, first into lot when it is
// not negative.
func (l *fakeLedger) book(lot int, leg cex.Leg, qty, price decimal.Decimal) error {
	order := make([]int, 0, len(l.lots))
	if lot >= 0 {
		order = append(order, lot)
	}
	for i := range l.lots {
		if i != lot {
			order = append(order, i)
		}
	}
	left := qty
	for _, i := range order {
		g := &l.lots[i][leg]
		take := decimal.Min(left, g.entryQty.Sub(g.exitQty))
		if !take.IsPositive() {
			continue
		}
		g.exitQty, g.exitValue = g.exitQty.Add(take), g.exitValue.Add(take.Mul(price))
		left = left.Sub(take)
	}
	if left.IsPositive() {
		return fmt.Errorf("closing %s more than held", left)
	}
	return nil
}

// net is what the ledger has realised.
func (l *fakeLedger) net() decimal.Decimal {
	sum := decimal.Zero
	for _, lot := range l.lots {
		for _, g := range lot {
			if !g.exitQty.IsPositive() {
				continue
			}
			cost := g.entryValue.Mul(g.exitQty).Div(g.entryQty)
			gross := g.exitValue.Sub(cost)
			if l.dir == Short {
				gross = cost.Sub(g.exitValue)
			}
			sum = sum.Add(gross).Sub(l.fee.Mul(g.exitValue)).Sub(l.fee.Mul(cost))
		}
	}
	return sum
}

func (l *fakeLedger) clone() *fakeLedger {
	c := &fakeLedger{dir: l.dir, fee: l.fee}
	c.lots = append(c.lots, l.lots...)
	return c
}

// valuer is CloseNet bound to lot (a trim's lot; -1 for none).
func (l *fakeLedger) valuer(lot int) CloseValuer {
	return func(purpose cex.OrderPurpose, close [2]CloseLeg, feeRate decimal.Decimal) (decimal.Decimal, error) {
		if !feeRate.Equal(l.fee) {
			return decimal.Zero, fmt.Errorf("fee rate %s, want %s", feeRate, l.fee)
		}
		c := l.clone()
		into := -1
		if purpose == cex.PurposeTrim {
			into = lot
		}
		before := c.net()
		for _, leg := range cex.Legs {
			if close[leg].Qty.IsPositive() {
				if err := c.book(into, leg, close[leg].Qty, close[leg].Price); err != nil {
					return decimal.Zero, err
				}
			}
		}
		if purpose == cex.PurposeTrim {
			return c.net().Sub(before), nil
		}
		return c.net(), nil
	}
}

// fixedValuer returns net whatever it is asked.
func fixedValuer(net string) CloseValuer {
	return func(cex.OrderPurpose, [2]CloseLeg, decimal.Decimal) (decimal.Decimal, error) { return dec(net), nil }
}

// failValuer fails the test when it is called.
func failValuer(t *testing.T) CloseValuer {
	return func(cex.OrderPurpose, [2]CloseLeg, decimal.Decimal) (decimal.Decimal, error) {
		t.Helper()
		t.Errorf("CloseNet was called")
		return decimal.Zero, nil
	}
}

var seqFee = dec("0.000065")
