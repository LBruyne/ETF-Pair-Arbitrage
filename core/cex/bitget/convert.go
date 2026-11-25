package bitget

import (
	"fmt"
	"strconv"
	"strings"
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

// accountSettings converts the settings answer for the given contracts. A
// contract the account has no futures setting for is left out.
func accountSettings(data settingsData, symbols []cex.Symbol) (cex.AccountSettings, error) {
	out := cex.AccountSettings{OneWay: data.HoldMode == holdModeOneWay, BySymbol: map[cex.Symbol]cex.SymbolSetting{}}
	for _, row := range data.SymbolConfigList {
		if row.Category != restCategory {
			continue
		}
		for _, symbol := range symbols {
			if row.Symbol != string(symbol) {
				continue
			}
			leverage, err := strconv.Atoi(row.Leverage)
			if err != nil {
				return out, fmt.Errorf("bitget: settings %s: leverage %q", row.Symbol, row.Leverage)
			}
			out.BySymbol[symbol] = cex.SymbolSetting{Crossed: row.MarginMode == marginModeCrossed, Leverage: leverage}
		}
	}
	return out, nil
}

// addPosition adds one position row to the positions by contract, signed: a
// short position is negative. The quantity is the row's total (REST) or size
// (private stream). A row without a quantity left adds nothing.
func addPosition(positions map[cex.Symbol]cex.Position, row positionRow) error {
	text := row.Total
	if text == "" {
		text = row.Size
	}
	qty, err := decimal.NewFromString(text)
	if err != nil || qty.IsNegative() {
		return fmt.Errorf("bitget: position %s: quantity %q", row.Symbol, text)
	}
	if qty.IsZero() {
		return nil
	}
	if row.PosSide == posSideShort {
		qty = qty.Neg()
	}
	avg, err := decimal.NewFromString(row.AvgPrice)
	if err != nil {
		return fmt.Errorf("bitget: position %s: avgPrice %q", row.Symbol, row.AvgPrice)
	}
	liq := row.LiquidationPrice
	if liq == "" {
		liq = row.LiqPrice
	}
	symbol := cex.Symbol(row.Symbol)
	held := positions[symbol]
	positions[symbol] = cex.Position{Symbol: symbol, Qty: held.Qty.Add(qty), AvgPrice: avg, UpdatedAt: wireTime(row.UpdatedTime),
		MarkPrice: boardDecimal(row.MarkPrice), UnrealisedPnL: boardDecimal(row.UnrealisedPnl), LiqPrice: boardDecimal(liq)}
	return nil
}

// boardDecimal reads a field kept for the board only: one that is missing
// or not a number is not valid, and fails nothing.
func boardDecimal(text string) decimal.NullDecimal {
	d, err := decimal.NewFromString(text)
	if err != nil {
		return decimal.NullDecimal{}
	}
	return decimal.NewNullDecimal(d)
}

// accountAssets converts the account assets answer, received at.
func accountAssets(data assetsData, at time.Time) (cex.AccountAssets, error) {
	equity, err := decimal.NewFromString(data.USDTEquity)
	if err != nil {
		return cex.AccountAssets{}, fmt.Errorf("bitget: assets: usdtEquity %q", data.USDTEquity)
	}
	ratio, err := decimal.NewFromString(data.MgnRatio)
	if err != nil {
		return cex.AccountAssets{}, fmt.Errorf("bitget: assets: mgnRatio %q", data.MgnRatio)
	}
	return cex.AccountAssets{Equity: equity, MarginRatio: ratio, ReceivedAt: at}, nil
}

// orderRecord converts one order as the exchange reports it, by query or by
// push: what the venue knows of an order. The leg, purpose, cycle and lot
// are the program's own and stay empty. A status that is not final (live,
// new, partially_filled) is acked. An order the exchange cancelled because
// its price left the allowed band carries the price-limit kind, like an
// order refused for it.
//
// The fee is the sum of the fee details, rounded to feeDecimals, and counts
// as known once it is not zero: the account pays a taker fee on every fill,
// so a zero beside a fill is a fee not reported yet. This takes the fee of
// a report to cover the filled quantity of the same report; a report whose
// fee lagged behind its quantity could not be told apart here.
func orderRecord(row orderRow) (cex.OrderRecord, error) {
	rec := cex.OrderRecord{OrderID: cex.OrderID(row.OrderID), CancelReason: row.CancelReason,
		SubmittedAt: wireTime(row.CreatedTime), ExchangeCreatedAt: wireTime(row.CreatedTime), ExchangeUpdatedAt: wireTime(row.UpdatedTime),
		Request: cex.OrderRequest{ClientOrderID: cex.ClientOrderID(row.ClientOid), Symbol: cex.Symbol(row.Symbol), Side: cex.Side(row.Side), ReduceOnly: strings.EqualFold(row.ReduceOnly, "yes")}}
	switch row.OrderStatus {
	case "live", "new", "partially_filled":
		rec.Status = cex.StatusAcked
	case "filled":
		rec.Status = cex.StatusFilled
	case "cancelled", "canceled":
		rec.Status = cex.StatusCancelled
	default:
		return rec, fmt.Errorf("bitget: order %s: status %q", row.ClientOid, row.OrderStatus)
	}
	if row.CancelReason == cancelReasonPriceLimit {
		rec.ErrorKind = cex.ErrorPriceLimit
	}
	for _, f := range []struct {
		name, text string
		into       *decimal.Decimal
	}{{"price", row.Price, &rec.Request.Price}, {"qty", row.Qty, &rec.Request.Qty}, {"cumExecQty", row.CumExecQty, &rec.CumExecQty},
		{"cumExecValue", row.CumExecValue, &rec.CumExecValue}, {"avgPrice", row.AvgPrice, &rec.AvgPrice}} {
		if f.text == "" {
			continue // nothing executed yet
		}
		d, err := decimal.NewFromString(f.text)
		if err != nil {
			return rec, fmt.Errorf("bitget: order %s: %s %q", row.ClientOid, f.name, f.text)
		}
		*f.into = d
	}
	fee, err := feeOf(row.FeeDetail)
	if err != nil {
		return rec, fmt.Errorf("bitget: order %s: %w", row.ClientOid, err)
	}
	rec.Fee = fee.Round(feeDecimals)
	rec.FeeKnown = rec.CumExecQty.IsPositive() && !rec.Fee.IsZero()
	return rec, nil
}

// fill converts one execution. Its time is the stream's execTime or the
// REST row's createdTime.
func fill(row fillRow, receivedAt time.Time) (cex.Fill, error) {
	out := cex.Fill{ExecID: row.ExecID, OrderID: cex.OrderID(row.OrderID), ClientOrderID: cex.ClientOrderID(row.ClientOid), Symbol: cex.Symbol(row.Symbol),
		Side: cex.Side(row.Side), ExecTime: wireTime(row.ExecTime), ReceivedAt: receivedAt}
	if row.ExecTime == "" {
		out.ExecTime = wireTime(row.CreatedTime)
	}
	for _, f := range []struct {
		name, text string
		into       *decimal.Decimal
	}{{"execPrice", row.ExecPrice, &out.Price}, {"execQty", row.ExecQty, &out.Qty}, {"execValue", row.ExecValue, &out.Value}} {
		d, err := decimal.NewFromString(f.text)
		if err != nil {
			return out, fmt.Errorf("bitget: fill %s: %s %q", row.ExecID, f.name, f.text)
		}
		*f.into = d
	}
	fee, err := feeOf(row.FeeDetail)
	if err != nil {
		return out, fmt.Errorf("bitget: fill %s: %w", row.ExecID, err)
	}
	out.Fee = fee
	return out, nil
}

// feeOf sums the fee details of an order or a fill: positive is paid.
func feeOf(details []feeDetailRow) (decimal.Decimal, error) {
	total := decimal.Zero
	for _, d := range details {
		fee, err := decimal.NewFromString(d.Fee)
		if err != nil {
			return total, fmt.Errorf("fee %q", d.Fee)
		}
		total = total.Add(fee)
	}
	return total, nil
}

// financialRecord converts one ledger row of a funding type: the amount is
// signed, positive when received.
func financialRecord(row financialRecordRow) (cex.FinancialRecord, error) {
	amount, err := decimal.NewFromString(row.Amount)
	if err != nil {
		return cex.FinancialRecord{}, fmt.Errorf("bitget: financial record %s: amount %q", row.ID, row.Amount)
	}
	return cex.FinancialRecord{ID: row.ID, Symbol: cex.Symbol(row.Symbol), Type: row.Type, Amount: amount, At: wireTime(row.Ts)}, nil
}

// wireTime reads unix milliseconds; text that is not a number is the zero
// time.
func wireTime(n wireNumber) time.Time {
	ms, err := strconv.ParseInt(string(n), 10, 64)
	if err != nil || ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// wireMicros reads unix microseconds; text that is not a number is the zero
// time.
func wireMicros(n wireNumber) time.Time {
	us, err := strconv.ParseInt(string(n), 10, 64)
	if err != nil || us == 0 {
		return time.Time{}
	}
	return time.UnixMicro(us)
}
