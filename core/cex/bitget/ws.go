package bitget

import (
	"context"
	"time"

	"github.com/gorilla/websocket"

	"etf-pair-arbitrage/core/logger"
)

// wsConn keeps one WebSocket connection up until its context ends: it dials,
// lets onOpen send the subscriptions, reads on the calling goroutine, pings
// on another, and after a loss reconnects with a growing wait.
type wsConn struct {
	stream string // "public"; a log key
	symbol string // the contract the connection serves; a log key
	url    string
	timing wsTimings

	onOpen    func(send func(msg []byte) error) error // after every connect
	onMessage func(msg []byte, receivedAt time.Time)  // every text frame but "pong", on the read goroutine
	onDown    func(err error)                         // after every loss of a connection that was up
}

// run keeps the connection up until ctx ends.
func (w *wsConn) run(ctx context.Context) {
	log := logger.GlobalLogger
	wait := w.timing.reconnectMin
	for {
		// Dial. A failure only waits and tries again.
		dialCtx, cancel := context.WithTimeout(ctx, w.timing.dialTimeout)
		began := time.Now()
		conn, _, err := websocket.DefaultDialer.DialContext(dialCtx, w.url, nil)
		cancel()
		if ctx.Err() != nil {
			if conn != nil {
				conn.Close()
			}
			return
		}
		if err != nil {
			log.Warn("bitget: stream dial failed; retrying", "stream", w.stream, "symbol", w.symbol, "url", w.url, "err", err, "wait", wait)
		} else {
			// Serve the connection until it fails or ctx ends.
			log.Info("bitget: stream connected", "stream", w.stream, "symbol", w.symbol, "url", w.url, "dial_ms", time.Since(began).Milliseconds())
			opened := time.Now()
			err = w.serve(ctx, conn)
			conn.Close()
			lived := time.Since(opened)
			if ctx.Err() != nil {
				log.Info("bitget: stream closed", "stream", w.stream, "symbol", w.symbol, "lived", lived.Round(time.Millisecond))
				return
			}
			if lived >= w.timing.reconnectMax {
				wait = w.timing.reconnectMin
			}
			log.Warn("bitget: stream down; reconnecting", "stream", w.stream, "symbol", w.symbol, "err", err, "lived", lived.Round(time.Millisecond), "wait", wait)
			w.onDown(err)
		}
		// Wait, then double the wait up to the maximum.
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, w.timing.reconnectMax)
	}
}

// serve subscribes, then reads frames until the connection fails or ctx
// ends. A second goroutine sends the text ping the exchange asks for and
// closes the connection when ctx ends, which ends the read. Writes are
// serialised through writes: gorilla allows one writer at a time.
func (w *wsConn) serve(ctx context.Context, conn *websocket.Conn) error {
	writes := make(chan struct{}, 1) // a one-slot lock around conn's writer
	send := func(msg []byte) error {
		writes <- struct{}{}
		defer func() { <-writes }()
		if err := conn.SetWriteDeadline(time.Now().Add(w.timing.writeTimeout)); err != nil {
			return err
		}
		return conn.WriteMessage(websocket.TextMessage, msg)
	}
	if err := w.onOpen(send); err != nil {
		return err
	}

	// The ping goroutine.
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(w.timing.pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				conn.Close()
				return
			case <-ticker.C:
				if err := send([]byte("ping")); err != nil {
					conn.Close() // the read loop reports the loss
					return
				}
			}
		}
	}()

	// The read loop: every read is bounded, a pong only proves the
	// connection alive, anything else goes to onMessage.
	for {
		if err := conn.SetReadDeadline(time.Now().Add(w.timing.readTimeout)); err != nil {
			return err
		}
		_, msg, err := conn.ReadMessage()
		receivedAt := time.Now()
		if err != nil {
			return err
		}
		if string(msg) == "pong" {
			continue
		}
		w.onMessage(msg, receivedAt)
	}
}
