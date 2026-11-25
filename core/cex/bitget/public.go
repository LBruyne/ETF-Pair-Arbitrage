package bitget

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/logger"
)

// SubscribeBooks opens one public connection for the given topics and keeps
// it up until ctx ends, subscribing them again after every reconnect. Every
// book frame is handed to onEvent as it arrives, on the connection's read
// goroutine; this client does not order or filter frames, the manager's leg
// books do. onDown is called after every loss of the connection. The
// connection's log lines carry the first topic's contract as their symbol:
// the manager subscribes one contract per call.
func (c *Client) SubscribeBooks(ctx context.Context, subs []cex.BookSubscription, onEvent func(cex.BookEvent), onDown func(error)) error {
	if len(subs) == 0 {
		return errors.New("bitget: SubscribeBooks needs at least one topic")
	}
	args := make([]wsTopicArg, 0, len(subs))
	for _, s := range subs {
		args = append(args, wsTopicArg{InstType: wsInstType, Topic: s.Topic, Symbol: string(s.Symbol)})
	}
	request, err := json.Marshal(wsRequest{Op: "subscribe", Args: args})
	if err != nil {
		return err
	}
	conn := &wsConn{stream: "public", symbol: string(subs[0].Symbol), url: c.wsPublicURL, timing: c.timing, onDown: onDown,
		onOpen: func(send func([]byte) error) error {
			logger.GlobalLogger.Info("bitget: subscribing", "stream", "public", "symbol", subs[0].Symbol, "topics", len(args))
			return send(request)
		},
		onMessage: func(msg []byte, receivedAt time.Time) {
			if c.onRawFrame != nil {
				c.onRawFrame(msg, receivedAt)
			}
			handleBookFrame(msg, receivedAt, onEvent)
		}}
	go conn.run(ctx)
	return nil
}

// handleBookFrame decodes one text frame of the public stream. A data frame
// becomes one BookEvent per row; a control frame is only logged, a refused
// subscription as an Error because its topic will never deliver.
func handleBookFrame(msg []byte, receivedAt time.Time, onEvent func(cex.BookEvent)) {
	log := logger.GlobalLogger
	var f wsFrame
	if err := json.Unmarshal(msg, &f); err != nil {
		log.Warn("bitget: undecodable frame", "err", err, "bytes", len(msg))
		return
	}
	if f.Action == "" {
		if f.Event == "error" {
			log.Error("bitget: public stream refused a request", "code", string(f.Code), "msg", f.Msg, "symbol", f.Arg.Symbol, "topic", f.Arg.Topic)
		} else {
			log.Debug("bitget: public event", "event", f.Event, "symbol", f.Arg.Symbol, "topic", f.Arg.Topic)
		}
		return
	}
	var rows []bookRow
	if err := json.Unmarshal(f.Data, &rows); err != nil {
		log.Warn("bitget: undecodable book data", "symbol", f.Arg.Symbol, "topic", f.Arg.Topic, "err", err)
		return
	}
	for _, row := range rows {
		ev, err := bookEvent(f.Arg, f.Action, row, receivedAt)
		if err != nil {
			log.Warn("bitget: book frame skipped", "symbol", f.Arg.Symbol, "topic", f.Arg.Topic, "err", err)
			continue
		}
		onEvent(ev)
	}
}
