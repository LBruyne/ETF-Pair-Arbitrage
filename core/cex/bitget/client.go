package bitget

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/logger"
)

// Client is the Bitget venue. It is built with the account's API key in live
// mode and with empty credentials in paper mode, where it can only read
// public data: every account-side method then fails before anything is
// sent.
//
// Orders go out by REST, on a connection Run keeps warm, or - with
// UseOrderTransport(cex.OrderTransportWS) - as frames on a logged-in
// WebSocket connection that Run keeps up for nothing else.
type Client struct {
	creds        cex.Credentials // never logged
	restURL      string
	wsPublicURL  string
	wsPrivateURL string
	http         *http.Client
	rest         restTimings
	timing       wsTimings
	now          func() time.Time // the clock of request timestamps

	orderTransport cex.OrderTransport // set before Run
	trade          *tradeConn         // the connection orders are sent on with the ws transport; never nil

	privateUp   atomic.Bool            // the private stream is logged in and its three topics are subscribed
	privateConn atomic.Pointer[wsConn] // the private stream's connection, for its ping; nil before SubscribePrivate
	warmNs      atomic.Int64           // the round trip of the last keep-warm request, in nanoseconds; 0 before the first

	// onRawFrame and onRawPrivateFrame, when set, see every text frame of
	// the public and of the private stream as it arrived. Tests record with
	// them; the process leaves them nil.
	onRawFrame        func(raw []byte, receivedAt time.Time)
	onRawPrivateFrame func(raw []byte, receivedAt time.Time)
	onRawTradeFrame   func(raw []byte, receivedAt time.Time) // the trade connection's frames, for the gated live tests
}

var (
	_ cex.Venue               = (*Client)(nil)
	_ cex.PrivateStreamStatus = (*Client)(nil)
	_ cex.VenueStatus         = (*Client)(nil)
)

// NewClient builds the client. Nothing is connected until a method is
// called. Its HTTP transport is its own: HTTP/2 where the server offers it,
// and idle connections kept for restTimings.idleConn instead of the 90
// seconds of Go's default, after which the next order would pay for a new
// connection.
func NewClient(creds cex.Credentials) *Client {
	c := &Client{creds: creds, restURL: restURL, wsPublicURL: wsPublicURL, wsPrivateURL: wsPrivateURL,
		rest: defaultRESTTimings, timing: defaultWSTimings, now: time.Now, orderTransport: cex.OrderTransportREST}
	c.http = &http.Client{Timeout: c.rest.timeout, Transport: newTransport(c.rest)}
	c.trade = &tradeConn{client: c, inFlight: map[string]chan tradeResult{}}
	return c
}

// newTransport is the client's HTTP transport: Go's defaults (proxy from
// the environment, HTTP/2 attempted) with its own idle limits.
func newTransport(t restTimings) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.IdleConnTimeout = t.idleConn
	transport.MaxIdleConns = restIdleConns
	transport.MaxIdleConnsPerHost = restIdleConns
	return transport
}

// Name is the venue's name in logs and records.
func (c *Client) Name() string { return "bitget" }

// UseOrderTransport sets how PlaceOrder sends orders. Call it before Run.
// A client sends by REST until it is told otherwise; the process passes
// cex.order_transport, ws by default.
func (c *Client) UseOrderTransport(transport cex.OrderTransport) { c.orderTransport = transport }

// Run keeps the client ready for orders until ctx ends. It opens the REST
// connection at once and keeps it warm with a public request every
// restTimings.keepWarm, and with the ws order transport it keeps the trade
// connection up. A process that sends no orders through this client - paper
// mode - does not need it.
func (c *Client) Run(ctx context.Context) {
	log := logger.GlobalLogger
	log.Info("bitget: running", "order_transport", string(c.orderTransport), "keep_warm", c.rest.keepWarm, "idle_conn", c.rest.idleConn)
	if c.orderTransport == cex.OrderTransportWS {
		c.trade.start(ctx)
	}
	failing := false
	ticker := time.NewTicker(c.rest.keepWarm)
	defer ticker.Stop()
	for {
		began := time.Now()
		err := c.call(ctx, http.MethodGet, pathServerTime, nil, nil, false, nil)
		if ctx.Err() != nil {
			log.Info("bitget: stopped")
			return
		}
		// A failure is retried at the next turn; it is logged when it
		// starts and when it ends.
		if err == nil {
			c.warmNs.Store(int64(time.Since(began)))
		}
		if (err != nil) != failing {
			failing = err != nil
			if failing {
				log.Warn("bitget: keep-warm request failed; retrying", "err", err)
			} else {
				log.Info("bitget: keep-warm request answered again")
			}
		}
		select {
		case <-ctx.Done():
			log.Info("bitget: stopped")
			return
		case <-ticker.C:
		}
	}
}

// PrivateUp reports whether the private stream is logged in with the order,
// fill and position topics subscribed: only then do order outcomes and
// position changes arrive by push.
func (c *Client) PrivateUp() bool { return c.privateUp.Load() }

// StatusAttrs is the client's part of the status line: whether the private
// stream is up and the median of its last pings, the round trip of the last
// keep-warm REST request, and with the ws order transport the same of the
// trade connection. A time of -1 means none was measured yet.
func (c *Client) StatusAttrs() []any {
	ms := func(d time.Duration, ok bool) float64 {
		if !ok {
			return -1
		}
		return milliseconds(d)
	}
	var privatePing time.Duration
	var pinged bool
	if conn := c.privateConn.Load(); conn != nil {
		privatePing, pinged = conn.ping()
	}
	warm := time.Duration(c.warmNs.Load())
	attrs := []any{"private_up", c.PrivateUp(), "private_ping_ms", ms(privatePing, pinged), "rest_warm_ms", ms(warm, warm > 0), "order_transport", string(c.orderTransport)}
	if c.orderTransport == cex.OrderTransportWS {
		ping, ok := c.trade.ping()
		attrs = append(attrs, "trade_up", c.trade.isUp(), "trade_ping_ms", ms(ping, ok))
	}
	return attrs
}
