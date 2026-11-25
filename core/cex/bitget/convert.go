package bitget

import (
	"fmt"
	"strconv"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
)

// bookEvent converts one row of a book frame.
func bookEvent(arg wsTopicArg, action string, row bookRow, receivedAt time.Time) (cex.BookEvent, error) {
	ev := cex.BookEvent{Symbol: cex.Symbol(arg.Symbol), Topic: arg.Topic, Action: cex.BookAction(action), ReceivedAt: receivedAt}
	if ev.Action != cex.ActionSnapshot && ev.Action != cex.ActionUpdate {
		return ev, fmt.Errorf("action %q", action)
	}
	var err error
	if ev.Seq, err = strconv.ParseUint(string(row.Seq), 10, 64); err != nil {
		return ev, fmt.Errorf("seq %q", row.Seq)
	}
	if ev.PrevSeq, err = strconv.ParseUint(string(row.PrevSeq), 10, 64); err != nil {
		return ev, fmt.Errorf("pseq %q", row.PrevSeq)
	}
	ts, err := strconv.ParseInt(string(row.Ts), 10, 64)
	if err != nil {
		return ev, fmt.Errorf("ts %q", row.Ts)
	}
	ev.ExchangeTime = time.UnixMilli(ts)
	if ev.Bids, err = levels(row.Bids); err != nil {
		return ev, err
	}
	if ev.Asks, err = levels(row.Asks); err != nil {
		return ev, err
	}
	return ev, nil
}

// levels converts one side's [price, quantity] rows, keeping their order. A
// price must be positive; a quantity may be zero (an update deleting the
// price) and never negative.
func levels(rows [][]string) ([]cex.BookLevel, error) {
	out := make([]cex.BookLevel, 0, len(rows))
	for _, r := range rows {
		if len(r) < 2 {
			return nil, fmt.Errorf("level %v", r)
		}
		price, err := decimal.NewFromString(r[0])
		if err != nil || !price.IsPositive() {
			return nil, fmt.Errorf("level price %q", r[0])
		}
		qty, err := decimal.NewFromString(r[1])
		if err != nil || qty.IsNegative() {
			return nil, fmt.Errorf("level quantity %q", r[1])
		}
		out = append(out, cex.BookLevel{Price: price, Qty: qty})
	}
	return out, nil
}

// marketRule converts one contract of the instruments endpoint.
func marketRule(row instrumentRow) (cex.MarketRule, error) {
	rule := cex.MarketRule{Symbol: cex.Symbol(row.Symbol), Online: row.Status == statusOnline}
	for _, f := range []struct {
		name, text string
		into       *decimal.Decimal
	}{{"priceMultiplier", row.PriceMultiplier, &rule.PriceTick}, {"quantityMultiplier", row.QuantityMultiplier, &rule.QtyStep},
		{"minOrderQty", row.MinOrderQty, &rule.MinQty}, {"minOrderAmount", row.MinOrderAmount, &rule.MinNotional}} {
		d, err := decimal.NewFromString(f.text)
		if err != nil || !d.IsPositive() {
			return rule, fmt.Errorf("bitget: instruments %s: %s %q", row.Symbol, f.name, f.text)
		}
		*f.into = d
	}
	return rule, nil
}

// fundingSettlement converts one past settlement of symbol.
func fundingSettlement(symbol cex.Symbol, row fundingHistoryRow) (cex.FundingSettlement, error) {
	rate, err := decimal.NewFromString(row.FundingRate)
	if err != nil {
		return cex.FundingSettlement{}, fmt.Errorf("bitget: funding history %s: fundingRate %q", symbol, row.FundingRate)
	}
	ms, err := strconv.ParseInt(string(row.FundingRateTimestamp), 10, 64)
	if err != nil {
		return cex.FundingSettlement{}, fmt.Errorf("bitget: funding history %s: fundingRateTimestamp %q", symbol, row.FundingRateTimestamp)
	}
	return cex.FundingSettlement{Symbol: symbol, Rate: rate, At: time.UnixMilli(ms)}, nil
}
