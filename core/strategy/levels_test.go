package strategy

import (
	"strings"
	"testing"

	"etf-pair-arbitrage/core/cex"
)

// A completion is capped by quantity x the limit it would be sent with, on
// the raw book.
func TestCapByLimit(t *testing.T) {
	rule := testRules[cex.Bull]
	tests := []struct {
		name       string
		book       *cex.Book
		dir        Direction
		qty, quota string
		wantQty    string
		wantLimit  string
		wantFills  []cex.BookLevel
	}{
		// PRD 6.4: 200 USDT left, 20 wanted. Down to the second level the
		// limit is 11 and 200 / 11 = 18.18; the fills are worth 179.98.
		{"the quota caps at the second level", book(nil, lv("9x10", "11x10")), Long, "20", "200", "18.18", "11", lv("9x10", "11x8.18")},
		{"the quota is not reached", book(nil, lv("9x10", "11x10")), Long, "20", "1000", "20", "11", lv("9x10", "11x10")},
		{"the first level is enough", book(nil, lv("9x10", "11x10")), Long, "8", "1000", "8", "9", lv("9x8")},
		// 100 / 11 = 9.09 is less than the 10 the first level gives at 9.
		{"a deeper level would hold less", book(nil, lv("9x10", "11x10")), Long, "20", "100", "10", "9", lv("9x10")},
		// 110 / 11 = 10, no more than the first level gives at 9.
		{"a tie keeps the better limit", book(nil, lv("9x10", "11x10")), Long, "20", "110", "10", "9", lv("9x10")},
		// A sell's limit falls with depth, but the sell fills at the bids
		// above its limit: it counts at the best bid 11 whatever its limit,
		// 200 / 11 = 18.18, and its fills are worth 183.62.
		{"a sell counts at the best bid", book(lv("11x10", "9x10"), nil), Short, "20", "200", "18.18", "9", lv("11x10", "9x8.18")},
		{"a sell the quota holds", book(lv("11x10", "9x10"), nil), Short, "20", "220", "20", "9", lv("11x10", "9x10")},
		{"the levels do not hold the quantity", book(nil, lv("9x10", "11x5")), Long, "20", "1000", "0", "0", nil},
		{"no quota left", book(nil, lv("9x10", "11x10")), Long, "20", "0", "0", "0", nil},
		{"an empty book", book(nil, nil), Long, "20", "1000", "0", "0", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			qty, limit, fills := capByLimit(tc.book, tc.dir, dec(tc.qty), dec(tc.quota), rule)
			wantDec(t, "quantity", qty, tc.wantQty)
			wantDec(t, "limit", limit, tc.wantLimit)
			wantLevels(t, "fills", fills, tc.wantFills)
		})
	}
	_, _, fills := capByLimit(book(nil, lv("9x10", "11x10")), Long, dec("20"), dec("200"), rule)
	wantDec(t, "value of the capped fills", value(fills), "179.98")

	// The quota price: a buy's limit; a sell's raw best bid, never below its
	// limit.
	bids := book(lv("11x10", "9x10"), lv("12x10"))
	wantDec(t, "quota price of a buy", quotaPrice(bids, Long, dec("12")), "12")
	wantDec(t, "quota price of a sell", quotaPrice(bids, Short, dec("9")), "11")
	wantDec(t, "quota price of a sell without a best bid", quotaPrice(&cex.Book{}, Short, dec("9")), "9")
}

func TestWalkOrLast(t *testing.T) {
	tests := []struct {
		name string
		book *cex.Book
		dir  Direction
		qty  string
		want []cex.BookLevel
	}{
		{"within the first level", book(nil, lv("40.47x300")), Long, "15.30", lv("40.47x15.30")},
		{"across levels (appendix A example 4)", book(nil, lv("40.47x0.4", "40.50x300")), Long, "15.30", lv("40.47x0.4", "40.50x14.9")},
		{"a sell walks the bids", book(lv("59.418x2", "59.36x200"), nil), Short, "10.20", lv("59.418x2", "59.36x8.2")},
		{"short levels: the rest at the last price", book(nil, lv("40.47x0.4", "40.50x3")), Long, "15.30", lv("40.47x0.4", "40.50x14.9")},
		{"nothing to take", book(nil, lv("40.47x300")), Long, "0", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wantLevels(t, "fills", walkOrLast(tc.book, tc.dir, dec(tc.qty)), tc.want)
		})
	}
}

func TestValueAndAdverse(t *testing.T) {
	wantDec(t, "value", value(lv("40.47x0.4", "40.50x14.9")), "619.638") // 16.188 + 603.45
	wantDec(t, "a buy that pays more", adverse(dec("619.191"), dec("618.732"), Long), "0.459")
	wantDec(t, "a sell that receives less", adverse(dec("606.0636"), dec("606.084"), Short), "0.0204")
	wantDec(t, "a buy that pays less", adverse(dec("618"), dec("618.732"), Long), "-0.732")
}

// The limit of a reducing order gives up the slip past the raw best price
// and rounds to the tick against ourselves.
func TestThroughBest(t *testing.T) {
	bull := book(lv("40.45x300"), lv("40.47x300"))
	bear := book(lv("59.418x200"), lv("59.385x100"))
	tests := []struct {
		name string
		book *cex.Book
		dir  Direction
		slip float64
		tick string
		want string
	}{
		{"a buy rounds up (the SPEC's example)", bear, Long, 10, "0.001", "59.445"}, // 59.385 x 1.001 = 59.444385
		{"a buy on a coarser tick", bull, Long, 10, "0.01", "40.52"},                // 40.47 x 1.001 = 40.51047
		{"a sell rounds down", bull, Short, 10, "0.01", "40.40"},                    // 40.45 x 0.999 = 40.40955
		{"a sell on a finer tick", bear, Short, 10, "0.001", "59.358"},              // 59.418 x 0.999 = 59.358582
		{"no slip, already on the tick", bull, Long, 0, "0.01", "40.47"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wantDec(t, "limit", throughBest(tc.book, tc.dir, tc.slip, dec(tc.tick)), tc.want)
		})
	}

	// The best price comes from the newer of the two streams, not from the
	// depth's first level.
	newer := book(lv("40.45x300"), lv("40.47x300"))
	newer.BestAsk = lv("40.50x10")[0]
	wantDec(t, "limit from the newer best price", throughBest(newer, Long, 10, dec("0.01")), "40.55") // 40.50 x 1.001 = 40.5405
}

// PRD 3.2: a leg's pricing level is its pricing_level-th level of the side
// the order takes, as the book holds it: bull 2, bear 5. Its price is that
// level's, at the leg's own tick, and its quantity is levels 1 to N
// together; levels beyond N are not counted, and a side with fewer than N
// levels has none. A level counts whatever its size.
func TestPricingLevel(t *testing.T) {
	tests := []struct {
		name      string
		leg       cex.Leg
		dir       Direction // Short takes the bids, Long the asks
		levels    []cex.BookLevel
		edit      func(*Config)
		wantPrice string // "" when there is no pricing level
		wantQty   string
	}{
		// The bear leg: the fifth price at 0.001, nothing rounded.
		{name: "bear bids across several 0.01 steps: the fifth", leg: cex.Bear, dir: Short,
			levels: lv("59.452x10", "59.441x20", "59.438x30", "59.431x5", "59.427x40"), wantPrice: "59.427", wantQty: "105"},
		{name: "bear bids inside one 0.01 step: the fifth", leg: cex.Bear, dir: Short,
			levels: lv("59.438x40", "59.437x30", "59.436x50", "59.435x20", "59.434x60"), wantPrice: "59.434", wantQty: "200"},
		{name: "bear bids of appendix A example 1", leg: cex.Bear, dir: Short,
			levels: lv("59.437x0.4", "59.431x60", "59.428x30", "59.425x30", "59.420x20"), wantPrice: "59.42", wantQty: "140.4"},
		{name: "bear asks across several 0.01 steps: the fifth", leg: cex.Bear, dir: Long,
			levels: lv("59.418x10", "59.429x20", "59.432x30", "59.439x5", "59.443x40"), wantPrice: "59.443", wantQty: "105"},
		{name: "bear asks of appendix A example 2", leg: cex.Bear, dir: Long,
			levels: lv("59.385x100", "59.391x40", "59.394x30", "59.396x20", "59.400x10"), wantPrice: "59.40", wantQty: "200"},
		{name: "bear bids, a tiny best level is a level", leg: cex.Bear, dir: Short,
			levels: lv("59.441x0.4", "59.438x40", "59.437x30", "59.436x50", "59.435x20"), wantPrice: "59.435", wantQty: "140.4"},
		{name: "bear bids, a sixth level is not counted", leg: cex.Bear, dir: Short,
			levels: lv("59.452x10", "59.441x20", "59.438x30", "59.431x5", "59.427x40", "59.42x500"), wantPrice: "59.427", wantQty: "105"},
		{name: "bear bids, four levels: none", leg: cex.Bear, dir: Short, levels: lv("59.452x10", "59.441x20", "59.438x30", "59.431x5")},
		{name: "bear asks, four levels: none", leg: cex.Bear, dir: Long, levels: lv("59.418x10", "59.429x20", "59.432x30", "59.439x5")},
		{name: "bear, an empty side: none", leg: cex.Bear, dir: Long},

		// The bull leg: the second price, with the first one's quantity.
		{name: "bull bids: the second level, the first counted", leg: cex.Bull, dir: Short,
			levels: lv("40.45x300", "40.44x500"), wantPrice: "40.44", wantQty: "800"},
		{name: "bull bids, five levels: the first two only", leg: cex.Bull, dir: Short,
			levels: lv("40.45x300", "40.44x500", "40.43x100", "40.42x100", "40.41x100"), wantPrice: "40.44", wantQty: "800"},
		{name: "bull bids, a tiny best level is a level", leg: cex.Bull, dir: Short,
			levels: lv("40.45x0.4", "40.44x500", "40.43x300"), wantPrice: "40.44", wantQty: "500.4"},
		{name: "bull asks, a tiny best level is a level", leg: cex.Bull, dir: Long,
			levels: lv("40.46x0.4", "40.47x500", "40.48x100"), wantPrice: "40.47", wantQty: "500.4"},
		{name: "bull bids, one level: none", leg: cex.Bull, dir: Short, levels: lv("40.45x300")},
		{name: "bull asks, one level: none", leg: cex.Bull, dir: Long, levels: lv("40.46x300")},

		// The level number is a parameter of each leg.
		{name: "bull at level 1: the best alone", leg: cex.Bull, dir: Short, edit: func(c *Config) { c.PricingLevel.Bull = 1 },
			levels: lv("40.45x300", "40.44x500"), wantPrice: "40.45", wantQty: "300"},
		{name: "bear at level 3", leg: cex.Bear, dir: Short, edit: func(c *Config) { c.PricingLevel.Bear = 3 },
			levels: lv("59.452x10", "59.441x20", "59.438x30", "59.431x5", "59.427x40"), wantPrice: "59.438", wantQty: "60"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := book(tc.levels, nil)
			if tc.dir == Long {
				b = book(nil, tc.levels)
			}
			level, ok := newStrategy(t, tc.edit).pricingLevel(b, tc.leg, tc.dir)
			if ok != (tc.wantPrice != "") {
				t.Fatalf("pricing level %v, ok %t; want ok %t", level, ok, tc.wantPrice != "")
			}
			if ok {
				wantDec(t, "price", level.Price, tc.wantPrice)
				wantDec(t, "quantity counted", level.Qty, tc.wantQty)
			}
		})
	}
}

// pricing_level is 1 to 5 on each leg (both legs subscribe books5); the
// default is bull 2, bear 5.
func TestConfigPricingLevel(t *testing.T) {
	for _, bad := range []LegLevels{{Bull: 0, Bear: 5}, {Bull: 2, Bear: 6}, {Bull: -1, Bear: 5}, {}} {
		cfg := newStrategy(t, nil).cfg
		cfg.PricingLevel = bad
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "strategy.pricing_level.") {
			t.Errorf("pricing_level %+v: Validate = %v, want it refused", bad, err)
		}
	}
	for _, good := range []LegLevels{{Bull: 1, Bear: 1}, {Bull: 5, Bear: 5}} {
		cfg := newStrategy(t, nil).cfg
		cfg.PricingLevel = good
		if err := cfg.Validate(); err != nil {
			t.Errorf("pricing_level %+v: Validate = %v, want it accepted", good, err)
		}
	}
	if got := DefaultConfig.PricingLevel; got != (LegLevels{Bull: 2, Bear: 5}) {
		t.Errorf("default pricing_level = %+v, want bull 2, bear 5", got)
	}
}
