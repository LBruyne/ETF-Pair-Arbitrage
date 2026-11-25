package bitget

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/logger"
)

// tradeConn is the connection orders are sent on with the ws order
// transport: a private connection that is logged in, subscribed to nothing
// and chosen by its ping, so that nothing but orders, their answers and the
// heartbeat travel on it. An order is one frame; the exchange answers with
// a frame that carries the request's id, which is the client order id.
//
// What is not known is never guessed. A request without an answer - the
// caller's deadline passed, the connection was lost, or an error frame came
// that names no request - may or may not have been carried out: each such
// request ends as cex.ErrOrderStateUnknown, and the order table asks the
// exchange for the order by its client id, as after a REST timeout. The
// late answer of such a request is logged and concerns no other. A request
// is written once and never sent again, on this connection or by REST.
type tradeConn struct {
	client *Client
	conn   atomic.Pointer[wsConn] // nil until start; the status line reads its ping from another goroutine

	mu       sync.Mutex
	up       bool                        // the connection is logged in
	send     func(msg []byte) error      // the connection's writer; nil while it is down
	inFlight map[string]chan tradeResult // by request id: the requests written and not answered
}

// The reduce-only flag of an order frame, as the channel's parameter table
// spells it and real orders confirmed; REST takes yes / no.
const (
	wsReduceOnlyYes = "YES"
	wsReduceOnlyNo  = "NO"
)

// tradeResult is the answer to one request.
type tradeResult struct {
	ack cex.OrderAck
	err error
}

// start opens the trade connection and keeps it up until ctx ends.
func (t *tradeConn) start(ctx context.Context) {
	c := t.client
	conn := &wsConn{stream: "trade", url: c.wsPrivateURL, timing: c.timing, choose: true,
		onOpen: func(send func([]byte) error) error {
			request, err := c.loginRequest()
			if err != nil {
				return err
			}
			t.mu.Lock()
			t.send = send
			t.mu.Unlock()
			logger.GlobalLogger.Info("bitget: logging in", "stream", "trade")
			return send(request)
		},
		onMessage: t.handle,
		onDown: func(err error) {
			t.failInFlight(fmt.Errorf("the trade connection was lost: %v", err), true)
		}}
	t.conn.Store(conn)
	go func() {
		conn.run(ctx)
		t.failInFlight(context.Cause(ctx), true)
	}()
}

// isUp reports whether an order can be sent on the connection now.
func (t *tradeConn) isUp() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.up
}

// ping returns the median of the connection's last pings.
func (t *tradeConn) ping() (time.Duration, bool) {
	conn := t.conn.Load()
	if conn == nil {
		return 0, false
	}
	return conn.ping()
}

// place sends one order as a frame and waits for its answer until ctx ends.
// sent is false when the connection is not up: nothing was written and the
// caller may use another transport. Otherwise the frame was handed to the
// connection, once, and the result is final for this request.
func (t *tradeConn) place(ctx context.Context, req cex.OrderRequest) (ack cex.OrderAck, err error, sent bool) {
	id := string(req.ClientOrderID)
	frame, err := orderFrame(req)
	if err != nil {
		return cex.OrderAck{}, fmt.Errorf("%w: %v", cex.ErrNotSent, err), true
	}
	answer := make(chan tradeResult, 1)
	t.mu.Lock()
	if !t.up {
		t.mu.Unlock()
		return cex.OrderAck{}, nil, false
	}
	send := t.send
	t.inFlight[id] = answer
	t.mu.Unlock()

	// The frames of two orders are written one after the other: the
	// connection has one writer. A write that fails may have put a part of
	// the frame on the wire, so its outcome is not known either.
	if err := send(frame); err != nil {
		t.forget(id)
		return cex.OrderAck{}, fmt.Errorf("%w: writing the order's frame: %v", cex.ErrOrderStateUnknown, err), true
	}
	select {
	case result := <-answer:
		return result.ack, result.err, true
	case <-ctx.Done():
		t.forget(id)
		return cex.OrderAck{}, fmt.Errorf("%w: no answer on the trade connection: %v", cex.ErrOrderStateUnknown, ctx.Err()), true
	}
}

// orderFrame is the frame of one order: op trade, topic place-order, the
// client order id as the request id.
func orderFrame(req cex.OrderRequest) ([]byte, error) {
	id := string(req.ClientOrderID)
	return json.Marshal(tradeRequest{Op: "trade", ID: id, Category: wsInstType, Topic: topicPlaceOrder, Args: []tradeOrderArg{{
		Symbol: string(req.Symbol), OrderType: "limit", Qty: req.Qty.String(), Price: req.Price.String(), Side: string(req.Side), TimeInForce: "ioc",
		ReduceOnly: reduceOnlyText(req, wsReduceOnlyYes, wsReduceOnlyNo), ClientOid: id, MarginMode: marginModeCrossed}}})
}

// forget drops a request nobody waits for any more; its late answer is
// then logged and ignored.
func (t *tradeConn) forget(id string) {
	t.mu.Lock()
	delete(t.inFlight, id)
	t.mu.Unlock()
}

// failInFlight ends every request in flight as "may have arrived", and with
// down marks the connection as not usable.
func (t *tradeConn) failInFlight(cause error, down bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if down {
		t.up, t.send = false, nil
	}
	for id, answer := range t.inFlight {
		answer <- tradeResult{err: fmt.Errorf("%w: %v", cex.ErrOrderStateUnknown, cause)}
		delete(t.inFlight, id)
	}
}

// handle takes one frame of the trade connection, on its read goroutine:
// the login's answer, an order's acceptance, or an error.
func (t *tradeConn) handle(msg []byte, receivedAt time.Time) {
	log := logger.GlobalLogger
	if t.client.onRawTradeFrame != nil {
		t.client.onRawTradeFrame(msg, receivedAt)
	}
	var f tradeFrame
	if err := json.Unmarshal(msg, &f); err != nil {
		log.Warn("bitget: undecodable frame", "stream", "trade", "err", err, "bytes", len(msg))
		return
	}
	id := string(f.ID)
	switch {
	case f.Event == "login" && f.Code == "0":
		t.mu.Lock()
		t.up = true
		t.mu.Unlock()
		log.Info("bitget: trade connection ready")
	case f.Event == "login" || (f.Event == "error" && !t.isUp()):
		// Before the login's answer an error is the login that was refused.
		log.Error("bitget: trade connection refused the login", "event", f.Event, "code", string(f.Code), "msg", f.Msg)
	case f.Event == "trade" && f.Code == "0":
		result := tradeResult{err: fmt.Errorf("%w: an acceptance on the trade connection that cannot be read", cex.ErrOrderStateUnknown)}
		if len(f.Args) == 1 {
			a := f.Args[0]
			result = tradeResult{ack: cex.OrderAck{OrderID: cex.OrderID(a.OrderID), ReceivedAt: receivedAt, AcceptedAt: wireMicros(f.ReceiveTime), CreatedAt: wireTime(a.CTime)}}
		}
		if !t.answer(id, result) {
			// The order's outcome was given up as unknown: the exchange took it after all.
			log.Warn("bitget: trade connection accepted an order nobody waits for", "client_order_id", id, "order_id", result.ack.OrderID, "code", string(f.Code))
		}
	case f.Event == "trade" || f.Event == "error":
		refusal := &cex.VenueError{Code: string(f.Code), Message: f.Msg, Kind: errorKind(string(f.Code))}
		if t.answer(id, tradeResult{err: refusalOrUnknown(refusal, fmt.Errorf("bitget: trade %s: %w", topicPlaceOrder, refusal))}) {
			return
		}
		if id != "" {
			// The late answer to a request given up as unknown; the others in
			// flight are not concerned.
			log.Warn("bitget: trade connection refused an order nobody waits for", "client_order_id", id, "code", string(f.Code), "msg", f.Msg)
			return
		}
		// An error without a request id may be about any request in flight:
		// the outcome of each is unknown.
		log.Warn("bitget: trade connection reported an error that names no request", "code", string(f.Code), "msg", f.Msg)
		t.failInFlight(fmt.Errorf("the trade connection reported code %s (%s) without naming a request in flight", f.Code, f.Msg), false)
	default:
		log.Debug("bitget: trade connection event", "event", f.Event)
	}
}

// answer hands a result to the request with the id; it reports false when
// no such request is in flight.
func (t *tradeConn) answer(id string, result tradeResult) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	answer, waiting := t.inFlight[id]
	if !waiting {
		return false
	}
	delete(t.inFlight, id)
	answer <- result
	return true
}
