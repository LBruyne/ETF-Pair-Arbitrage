package strategy

import (
	"testing"

	"etf-pair-arbitrage/core/cex"
)

// V2: asks round up to their bucket and bids round down, quantities in one
// bucket add up, and a bucket below the minimum is skipped without lending
// its quantity to the next.
func TestEffectiveLevels(t *testing.T) {
	tests := []struct {
		name   string
		raw    []cex.BookLevel
		asks   bool
		minQty string
		want   []cex.BookLevel
	}{
		{"asks round up and one bucket adds up (PRD 3.2)", lv("72.003x1", "72.005x5"), true, "1", lv("72.01x6")},
		{"a dust best bid is skipped (PRD 3.2)", lv("40.45x0.4", "40.44x500"), false, "1", lv("40.44x500")},
		{"bids round down (appendix A example 1)", lv("59.437x0.4", "59.431x60", "59.425x80", "59.418x200"), false, "1",
			lv("59.43x60.4", "59.42x80", "59.41x200")},
		{"asks of appendix A example 2", lv("59.385x100", "59.396x100"), true, "1", lv("59.39x100", "59.40x100")},
		{"a price on the bucket's edge stays in it", lv("72.01x2", "72.011x3"), true, "1", lv("72.01x2", "72.02x3")},
		{"a bid on the bucket's edge stays in it", lv("59.43x2", "59.429x3"), false, "1", lv("59.43x2", "59.42x3")},
		{"a bucket of exactly the minimum counts", lv("72.003x0.4", "72.005x0.6", "72.012x0.99"), true, "1", lv("72.01x1")},
		{"dust is not carried into the next bucket", lv("72.001x0.6", "72.012x0.6", "72.03x4"), true, "1", lv("72.03x4")},
		{"the minimum is configurable", lv("72.001x0.6", "72.012x0.6"), true, "0.5", lv("72.01x0.6", "72.02x0.6")},
		{"an empty side", nil, true, "1", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wantLevels(t, "levels", EffectiveLevels(tc.raw, tc.asks, dec("0.01"), dec(tc.minQty)), tc.want)
		})
	}
}

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
