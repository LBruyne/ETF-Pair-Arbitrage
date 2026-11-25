package bitget

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"etf-pair-arbitrage/core/logger"
)

// wsConn keeps one WebSocket connection up until its context ends: it dials,
// lets onOpen send the subscriptions or the login, reads on the calling
// goroutine, pings on another, and after a loss reconnects with a growing
// wait.
type wsConn struct {
	stream string // "public", "private" or "trade"; a log key
	symbol string // the contract or contracts the connection serves; a log key
	url    string
	timing wsTimings
	choose bool // pick the connection by its ping: see dial

	onOpen    func(send func(msg []byte) error) error // after every connect; send stays usable while the connection lives
	onMessage func(msg []byte, receivedAt time.Time)  // every text frame but "pong", on the read goroutine
	onDown    func(err error)                         // after every loss of a connection that was up

	pingNs atomic.Int64 // the median round trip of the connection's last pings, in nanoseconds; 0 while it has none

	// onAttempt, when set, sees the pings sampled on each connection tried
	// during a choice. Tests measure with it; the process leaves it nil.
	onAttempt func(attempt int, pings []time.Duration)
}

// pingKept is how many of a connection's last ping round trips its median
// is taken over: the pings of its choice and then the heartbeat's.
const pingKept = 9

// ping returns the median round trip of the connection's last pings, and
// false while there is no connection or no pong yet.
func (w *wsConn) ping() (time.Duration, bool) {
	ns := w.pingNs.Load()
	return time.Duration(ns), ns > 0
}

// run keeps the connection up until ctx ends.
func (w *wsConn) run(ctx context.Context) {
	log := logger.GlobalLogger
	wait := w.timing.reconnectMin
	for {
		// Dial. A failure only waits and tries again.
		began := time.Now()
		conn, pings, err := w.dial(ctx)
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
			err = w.serve(ctx, conn, pings)
			conn.Close()
			w.pingNs.Store(0)
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

// dial opens the connection. With choose it picks one by its ping: a new
// connection is pinged pingSamples times, pingGap apart, and when the median
// is above pingThreshold another is dialled, up to pingAttempts in all; the
// one with the lowest median is kept and the others are closed. It returns
// the connection and the ping round trips measured on it.
func (w *wsConn) dial(ctx context.Context) (*websocket.Conn, []time.Duration, error) {
	log := logger.GlobalLogger
	open := func() (*websocket.Conn, error) {
		dialCtx, cancel := context.WithTimeout(ctx, w.timing.dialTimeout)
		defer cancel()
		conn, _, err := websocket.DefaultDialer.DialContext(dialCtx, w.url, nil)
		return conn, err
	}
	if !w.choose || w.timing.pingSamples < 1 {
		conn, err := open()
		return conn, nil, err
	}
	var best *websocket.Conn
	var bestPings []time.Duration
	var lastErr error
	for attempt := 1; attempt <= max(w.timing.pingAttempts, 1) && ctx.Err() == nil; attempt++ {
		conn, err := open()
		if err == nil {
			var pings []time.Duration
			if pings, err = w.samplePings(ctx, conn); err == nil {
				if w.onAttempt != nil {
					w.onAttempt(attempt, pings)
				}
				better := best == nil || median(pings) < median(bestPings)
				log.Info("bitget: connection pinged", "stream", w.stream, "attempt", attempt, "ping_median_ms", milliseconds(median(pings)),
					"threshold_ms", milliseconds(w.timing.pingThreshold), "best_so_far", better)
				if better {
					if best != nil {
						best.Close()
					}
					best, bestPings = conn, pings
				} else {
					conn.Close()
				}
				if median(bestPings) <= w.timing.pingThreshold {
					break
				}
				continue
			}
			conn.Close()
		}
		lastErr = err
		log.Warn("bitget: connection attempt failed", "stream", w.stream, "attempt", attempt, "err", err)
	}
	if best == nil {
		return nil, nil, errors.Join(lastErr, ctx.Err())
	}
	return best, bestPings, nil
}

// samplePings sends pingSamples text pings, each after pingGap of silence,
// on a connection that carries nothing else yet and returns the round trip
// of each.
func (w *wsConn) samplePings(ctx context.Context, conn *websocket.Conn) ([]time.Duration, error) {
	pings := make([]time.Duration, 0, w.timing.pingSamples)
	for len(pings) < w.timing.pingSamples {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(w.timing.pingGap):
		}
		sent := time.Now()
		if err := conn.SetWriteDeadline(sent.Add(w.timing.writeTimeout)); err != nil {
			return nil, err
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
			return nil, err
		}
		if err := conn.SetReadDeadline(sent.Add(w.timing.pingWait)); err != nil {
			return nil, err
		}
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return nil, err
			}
			if string(msg) == "pong" {
				break
			}
		}
		pings = append(pings, time.Since(sent))
	}
	return pings, nil
}

// median returns the middle value; of an even number, the upper middle.
func median(values []time.Duration) time.Duration {
	sorted := slices.Sorted(slices.Values(values))
	return sorted[len(sorted)/2]
}

// milliseconds writes a duration for a log line: milliseconds with one
// decimal.
func milliseconds(d time.Duration) float64 {
	return float64(d.Round(100*time.Microsecond)) / float64(time.Millisecond)
}

// serve subscribes, then reads frames until the connection fails or ctx
// ends. A second goroutine sends the text ping the exchange asks for and
// closes the connection when ctx ends, which ends the read. Writes are
// serialised through writes: gorilla allows one writer at a time. Each
// pong's round trip is kept with pings, the ones measured before, and their
// median is published.
func (w *wsConn) serve(ctx context.Context, conn *websocket.Conn, pings []time.Duration) error {
	writes := make(chan struct{}, 1) // a one-slot lock around conn's writer
	send := func(msg []byte) error {
		writes <- struct{}{}
		defer func() { <-writes }()
		if err := conn.SetWriteDeadline(time.Now().Add(w.timing.writeTimeout)); err != nil {
			return err
		}
		return conn.WriteMessage(websocket.TextMessage, msg)
	}
	if len(pings) > 0 {
		w.pingNs.Store(int64(median(pings)))
	}
	if err := w.onOpen(send); err != nil {
		return err
	}

	// The ping goroutine.
	var pingSentNs atomic.Int64 // when the ping whose pong is timed was sent; 0 when none waits. Pings are seconds apart and a pong takes milliseconds, so one waits at most
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
				pingSentNs.CompareAndSwap(0, time.Now().UnixNano())
				if err := send([]byte("ping")); err != nil {
					conn.Close() // the read loop reports the loss
					return
				}
			}
		}
	}()

	// The read loop: every read is bounded, a pong proves the connection
	// alive and times its ping, anything else goes to onMessage.
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
			if sent := pingSentNs.Swap(0); sent != 0 {
				pings = append(pings, time.Duration(receivedAt.UnixNano()-sent))
				pings = pings[max(0, len(pings)-pingKept):]
				w.pingNs.Store(int64(median(pings)))
			}
			continue
		}
		w.onMessage(msg, receivedAt)
	}
}
