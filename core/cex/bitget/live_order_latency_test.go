package bitget

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
)

// orderLatencyAuthorised is the text the second gate of
// TestLiveOrderLatency must hold.
const orderLatencyAuthorised = "owner-authorised"

// THIS TEST SENDS REAL ORDERS. It runs only with BITGET_ORDER_LATENCY_TEST=1
// and BITGET_ORDER_LATENCY_AUTHORISED=owner-authorised, which nobody sets
// without the account owner's word for this run.
//
// It compares the two order transports on real orders that cannot fill: N
// IOC limit buys of one contract by each (BITGET_ORDER_LATENCY_N, default
// 10; BITGET_ORDER_LATENCY_SYMBOL, SOXLUSDT by default or SOXSUSDT), in
// turn, more than a second apart, each priced 3 % below the best bid of the
// moment - no ask stands there, so the exchange cancels it at once - for a
// quantity just above the minimum notional, with client ids that start with
// "lat", not with the program's prefix. For every order it records, on the
// local clock, when it was sent, when its acceptance was in hand and when
// its final state arrived by push; and on the exchange's clock, when the
// gateway took it, when the order was created and when it last changed.
// Should anything have filled all the same, the filled quantity is sold
// back with one reduce-only IOC. The results are saved under
// experiments/core_step5/latency/ and summarised per transport.
func TestLiveOrderLatency(t *testing.T) {
	if os.Getenv("BITGET_ORDER_LATENCY_TEST") != "1" || os.Getenv("BITGET_ORDER_LATENCY_AUTHORISED") != orderLatencyAuthorised {
		t.Skip("this test sends real orders: it needs BITGET_ORDER_LATENCY_TEST=1 and BITGET_ORDER_LATENCY_AUTHORISED=" + orderLatencyAuthorised + ", set only with the account owner's authorisation")
	}
	latencyDir(t)
	perTransport := 10
	if n, err := strconv.Atoi(os.Getenv("BITGET_ORDER_LATENCY_N")); err == nil && n >= 1 && n <= 20 {
		perTransport = n
	}
	symbol, leg := cex.Symbol("SOXLUSDT"), cex.Bull
	switch os.Getenv("BITGET_ORDER_LATENCY_SYMBOL") {
	case "", "SOXLUSDT":
	case "SOXSUSDT":
		symbol, leg = "SOXSUSDT", cex.Bear
	default:
		t.Fatal("BITGET_ORDER_LATENCY_SYMBOL must be SOXLUSDT or SOXSUSDT: no order was sent")
	}
	client := NewClient(liveCredentials(t))
	client.UseOrderTransport(cex.OrderTransportWS)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.Run(ctx)

	// The best bid, from books1; the order pushes of this test's ids, from
	// the private stream's raw frames (the stream itself hands over only
	// the program's own ids).
	var mu sync.Mutex
	var bestBid decimal.Decimal
	var bestBidAt time.Time
	type push struct {
		at                   time.Time
		status               string
		cumExecQty           decimal.Decimal
		createdMs, updatedMs int64
	}
	finals := map[string]push{}
	if err := client.SubscribeBooks(ctx, []cex.BookSubscription{{Symbol: symbol, Topic: cex.TopicBooks1}}, func(ev cex.BookEvent) {
		if len(ev.Bids) > 0 {
			mu.Lock()
			bestBid, bestBidAt = ev.Bids[0].Price, ev.ReceivedAt
			mu.Unlock()
		}
	}, func(error) {}); err != nil {
		t.Fatal(err)
	}
	client.onRawPrivateFrame = func(raw []byte, at time.Time) {
		var f wsFrame
		if json.Unmarshal(raw, &f) != nil || f.Arg.Topic != topicOrder {
			return
		}
		var rows []orderRow
		if json.Unmarshal(f.Data, &rows) != nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		for _, row := range rows {
			if _, seen := finals[row.ClientOid]; seen || !strings.HasPrefix(row.ClientOid, "lat") || (row.OrderStatus != "cancelled" && row.OrderStatus != "canceled" && row.OrderStatus != "filled") {
				continue
			}
			qty, _ := decimal.NewFromString(row.CumExecQty)
			created, _ := strconv.ParseInt(string(row.CreatedTime), 10, 64)
			updated, _ := strconv.ParseInt(string(row.UpdatedTime), 10, 64)
			finals[row.ClientOid] = push{at: at, status: row.OrderStatus, cumExecQty: qty, createdMs: created, updatedMs: updated}
		}
	}
	// The trade connection's answers to this test's orders, as received
	// (they hold the order's ids and the exchange's times, nothing else).
	var tradeFrames []string
	client.onRawTradeFrame = func(raw []byte, _ time.Time) {
		if strings.Contains(string(raw), `"lat`) {
			mu.Lock()
			tradeFrames = append(tradeFrames, string(raw))
			mu.Unlock()
		}
	}
	if err := client.SubscribePrivate(ctx, []cex.Symbol{symbol}, func(cex.PrivateEvent) {}, func(error) {}); err != nil {
		t.Fatal(err)
	}
	rules, err := client.MarketRules(ctx, []cex.Symbol{symbol})
	if err != nil {
		t.Fatal(err)
	}
	rule := rules[symbol]
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		mu.Lock()
		priced := bestBid.IsPositive()
		mu.Unlock()
		if priced && client.PrivateUp() && client.trade.isUp() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no best bid, private stream or trade connection within 15 s: no order was sent")
		}
	}
	before, err := client.Positions(ctx, []cex.Symbol{symbol})
	if err != nil {
		t.Fatal(err)
	}

	// The orders.
	type result struct {
		ID             string  `json:"client_order_id"`
		Transport      string  `json:"transport"`
		Price          string  `json:"price"`
		Qty            string  `json:"qty"`
		SentNs         int64   `json:"sent_unix_ns"`
		AckNs          int64   `json:"ack_unix_ns"`
		Error          string  `json:"error,omitempty"`
		AcceptedMicros int64   `json:"exchange_accepted_unix_us"`
		CreatedMs      int64   `json:"exchange_created_ms"`
		UpdatedMs      int64   `json:"exchange_updated_ms"`
		FinalNs        int64   `json:"final_push_unix_ns"`
		FinalStatus    string  `json:"final_status"`
		CumExecQty     string  `json:"cum_exec_qty"`
		SentToAckMs    float64 `json:"sent_to_ack_ms"`
		SentToFinalMs  float64 `json:"sent_to_final_push_ms"`
	}
	var results []result
	run := time.Now().UnixMilli()
	failed := 0
	for i := 0; i < 2*perTransport && failed < 3; i++ {
		transport := []string{"rest", "ws"}[i%2]
		mu.Lock()
		bid, bidAge := bestBid, time.Since(bestBidAt)
		mu.Unlock()
		price := bid.Mul(decimal.RequireFromString("0.97")).Div(rule.PriceTick).Floor().Mul(rule.PriceTick)
		qty := decimal.Max(rule.MinNotional.Mul(decimal.RequireFromString("1.1")).Div(price).Div(rule.QtyStep).Ceil().Mul(rule.QtyStep), rule.MinQty)
		// An order is sent only on a fresh bid and for the small size meant.
		if bidAge > 2*time.Second || !price.IsPositive() || price.Mul(qty).GreaterThan(rule.MinNotional.Mul(decimal.NewFromInt(3))) {
			t.Errorf("stopped before order %d: best bid %s is %s old, price %s, quantity %s", i, bid, bidAge.Round(time.Millisecond), price, qty)
			break
		}
		req := cex.OrderRequest{ClientOrderID: cex.ClientOrderID(fmt.Sprintf("lat%d-%02d%s", run, i, strings.ToUpper(transport[:1]))), Symbol: symbol, Leg: leg, Side: cex.SideBuy, Price: price, Qty: qty}
		res := result{ID: string(req.ClientOrderID), Transport: transport, Price: price.String(), Qty: qty.String()}
		var ack cex.OrderAck
		orderCtx, orderDone := context.WithTimeout(ctx, 5*time.Second)
		sent := time.Now()
		if transport == "rest" {
			ack, err = client.placeOrderREST(orderCtx, req)
		} else {
			var written bool
			if ack, err, written = client.trade.place(orderCtx, req); !written {
				err = fmt.Errorf("the trade connection was not up: nothing was sent")
			}
		}
		acked := time.Now()
		orderDone()
		res.SentNs, res.AckNs, res.SentToAckMs = sent.UnixNano(), acked.UnixNano(), msOf(acked.Sub(sent))
		if err != nil {
			res.Error = err.Error()
			failed++ // the run stops at the third order that failed
			t.Errorf("%s: %v", req.ClientOrderID, err)
		} else {
			res.AcceptedMicros, res.CreatedMs = ack.AcceptedAt.UnixMicro(), ack.CreatedAt.UnixMilli()
			if ack.CreatedAt.IsZero() {
				res.CreatedMs = 0
			}
		}
		results = append(results, res)
		time.Sleep(1100 * time.Millisecond)
	}
	time.Sleep(3 * time.Second) // the last pushes

	// The outcomes: by push, or by a query for an order without one.
	filled := decimal.Zero
	mu.Lock()
	for i := range results {
		r := &results[i]
		if p, pushed := finals[r.ID]; pushed {
			r.FinalNs, r.FinalStatus, r.CumExecQty, r.UpdatedMs = p.at.UnixNano(), p.status, p.cumExecQty.String(), p.updatedMs
			r.SentToFinalMs = msOf(p.at.Sub(time.Unix(0, r.SentNs)))
			if r.CreatedMs == 0 {
				r.CreatedMs = p.createdMs
			}
			filled = filled.Add(p.cumExecQty)
		}
	}
	mu.Unlock()
	for i := range results {
		if r := &results[i]; r.FinalStatus == "" {
			rec, err := client.OrderByClientID(ctx, symbol, cex.ClientOrderID(r.ID))
			if err != nil {
				r.FinalStatus = "no push; query: " + err.Error()
				continue
			}
			r.FinalStatus, r.CumExecQty = "no push; query: "+string(rec.Status), rec.CumExecQty.String()
			filled = filled.Add(rec.CumExecQty)
		}
	}
	saveResult(t, "order_latency", results)
	mu.Lock()
	saveResult(t, "order_latency_trade_frames", tradeFrames)
	mu.Unlock()

	// Nothing should have filled. If something did, sell it back.
	after, err := client.Positions(ctx, []cex.Symbol{symbol})
	if err != nil {
		t.Errorf("positions after the orders: %v", err)
	}
	if grew := after.BySymbol[symbol].Qty.Sub(before.BySymbol[symbol].Qty); grew.GreaterThan(filled) {
		filled = grew
	}
	if filled.IsPositive() {
		mu.Lock()
		bid := bestBid
		mu.Unlock()
		closing := cex.OrderRequest{ClientOrderID: cex.ClientOrderID(fmt.Sprintf("lat%d-close", run)), Symbol: symbol, Leg: leg, Side: cex.SideSell, ReduceOnly: true,
			Price: bid.Mul(decimal.RequireFromString("0.97")).Div(rule.PriceTick).Floor().Mul(rule.PriceTick), Qty: filled}
		_, err := client.placeOrderREST(ctx, closing)
		t.Errorf("%s of %s filled although priced 3 %% below the bid; sold back with reduce-only IOC %s: %v - check the position by hand", filled, symbol, closing.ClientOrderID, err)
	}

	for _, transport := range []string{"rest", "ws"} {
		var sentAck, sentFinal, sentAccept, acceptCreated, createdUpdated []float64
		for _, r := range results {
			if r.Transport != transport || r.Error != "" {
				continue
			}
			sentAck = append(sentAck, r.SentToAckMs)
			if r.FinalNs != 0 {
				sentFinal = append(sentFinal, r.SentToFinalMs)
			}
			if r.AcceptedMicros > 0 {
				sentAccept = append(sentAccept, float64(r.AcceptedMicros*1000-r.SentNs)/1e6)
				if r.CreatedMs > 0 {
					acceptCreated = append(acceptCreated, float64(r.CreatedMs*1000-r.AcceptedMicros)/1e3)
				}
			}
			if r.CreatedMs > 0 && r.UpdatedMs > 0 {
				createdUpdated = append(createdUpdated, float64(r.UpdatedMs-r.CreatedMs))
			}
		}
		t.Logf("%s: sent -> acceptance in hand: %s", transport, distOf(sentAck))
		t.Logf("%s: sent -> final state by push: %s", transport, distOf(sentFinal))
		t.Logf("%s: sent (local clock) -> taken by the exchange's gateway (its clock; includes the clocks' offset): %s", transport, distOf(sentAccept))
		t.Logf("%s: gateway -> order created (exchange clock): %s; created -> last change: %s", transport, distOf(acceptCreated), distOf(createdUpdated))
	}
}
