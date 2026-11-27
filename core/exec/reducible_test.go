package exec

import (
	"testing"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/strategy"
)

// A reducing order may not exceed the venue's net position on its leg. The
// cycle's holding is what its lots hold; the dust of earlier cycles sits on
// the same venue position. When the dust has the other sign (a long cycle
// holding 7.80 next to dust of -0.01) the venue nets 7.79, and a reduce-only
// sell of 7.80 is refused (Bitget 40804, live 2026-11-27 05:06-11:38).
func TestReducibleCapsAtTheVenueNet(t *testing.T) {
	hold := [2]decimal.Decimal{dec("7.80"), dec("4.02")}
	cases := []struct {
		name string
		dir  strategy.Direction
		dust [2]decimal.Decimal
		want [2]decimal.Decimal
	}{
		{"long, dust against", strategy.Long, [2]decimal.Decimal{dec("-0.01"), dec("0")}, [2]decimal.Decimal{dec("7.79"), dec("4.02")}},
		{"long, dust with", strategy.Long, [2]decimal.Decimal{dec("0.01"), dec("0.02")}, [2]decimal.Decimal{dec("7.80"), dec("4.02")}},
		{"short, dust against", strategy.Short, [2]decimal.Decimal{dec("0"), dec("0.03")}, [2]decimal.Decimal{dec("7.80"), dec("3.99")}},
		{"short, dust with", strategy.Short, [2]decimal.Decimal{dec("-0.05"), dec("0")}, [2]decimal.Decimal{dec("7.80"), dec("4.02")}},
		{"dust bigger than the holding", strategy.Long, [2]decimal.Decimal{dec("-9"), dec("0")}, [2]decimal.Decimal{dec("0"), dec("4.02")}},
	}
	for _, c := range cases {
		got := reducible(hold, c.dir, c.dust)
		for _, leg := range cex.Legs {
			if !got[leg].Equal(c.want[leg]) {
				t.Errorf("%s: %s reducible %s, want %s", c.name, leg, got[leg], c.want[leg])
			}
		}
	}
}
