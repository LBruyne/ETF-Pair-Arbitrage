package bitget

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/logger"
)

// SubscribePrivate opens the private connection and keeps it up until ctx
// ends. The connection is chosen by its ping: order outcomes arrive on it,
// and a slow one would deliver each of them tens of milliseconds late.
// After every connect it logs in, waits for the login answer, and
// subscribes the order, fill and position topics. Events are handed to
// onEvent as they arrive, on the connection's read goroutine; onDown is
// called after every loss of the connection.
//
// The topics are account-wide and the account is traded by other programs
// too, so the stream is filtered here: only the given contracts, and of
// orders and fills only those whose client order id carries this program's
// prefix. A position event always holds the whole set of the given
// contracts: one that is absent from it is flat.
func (c *Client) SubscribePrivate(ctx context.Context, symbols []cex.Symbol, onEvent func(cex.PrivateEvent), onDown func(error)) error {
	if c.creds.Key == "" {
		return errNoCredentials
	}
	if len(symbols) == 0 {
		return errors.New("bitget: SubscribePrivate needs at least one contract")
	}
	subscribe, err := json.Marshal(wsRequest{Op: "subscribe", Args: []wsTopicArg{
		{InstType: wsPrivateUTA, Topic: topicOrder}, {InstType: wsPrivateUTA, Topic: topicFill}, {InstType: wsPrivateUTA, Topic: topicPosition}}})
	if err != nil {
		return err
	}
	names := make([]string, len(symbols))
	ours := make(map[string]bool, len(symbols))
	for i, s := range symbols {
		names[i], ours[string(s)] = string(s), true
	}
	p := &privateStream{client: c, ours: ours, subscribe: subscribe, onEvent: onEvent}
	conn := &wsConn{stream: "private", symbol: strings.Join(names, ","), url: c.wsPrivateURL, timing: c.timing, choose: true,
		onOpen: p.login,
		onMessage: func(msg []byte, receivedAt time.Time) {
			if c.onRawPrivateFrame != nil {
				c.onRawPrivateFrame(msg, receivedAt)
			}
			p.handle(msg, receivedAt)
		},
		onDown: func(err error) {
			c.privateUp.Store(false)
			onDown(err)
		}}
	c.privateConn.Store(conn)
	go func() {
		conn.run(ctx)
		c.privateUp.Store(false)
	}()
	return nil
}

// privateStream is the state of the private stream. Everything below the
// first four fields belongs to the current connection: login starts it
// again at every connect, and the positions start again with the
// connection's snapshot. Only the connection's goroutine touches it.
type privateStream struct {
	client    *Client
	ours      map[string]bool // the contracts kept
	subscribe []byte          // the subscribe request of the three topics
	onEvent   func(cex.PrivateEvent)

	send        func(msg []byte) error
	subscribed  map[string]bool             // the topics the exchange acknowledged
	hasSnapshot bool                        // the position topic's first frame arrived on this connection
	positions   map[cex.Symbol]cex.Position // the kept contracts' positions since that frame; a flat one has no entry
}

// login starts a connection: it forgets the last connection's state and
// sends the login. The subscription follows the exchange's answer, in
// handle: a subscribe sent before it is refused.
func (p *privateStream) login(send func(msg []byte) error) error {
	p.send, p.subscribed, p.hasSnapshot = send, map[string]bool{}, false
	request, err := p.client.loginRequest()
	if err != nil {
		return err
	}
	logger.GlobalLogger.Info("bitget: logging in", "stream", "private")
	return send(request)
}

// loginRequest is the login frame of a private connection, signed over
// timestamp + "GET" + wsLoginPath with the timestamp in unix milliseconds.
func (c *Client) loginRequest() ([]byte, error) {
	ts := strconv.FormatInt(c.now().UnixMilli(), 10)
	return json.Marshal(wsLoginRequest{Op: "login", Args: []wsLoginArg{
		{APIKey: c.creds.Key, Passphrase: c.creds.Passphrase, Timestamp: ts, Sign: sign(c.creds.Secret, ts+"GET"+wsLoginPath)}}})
}

// handle decodes one text frame of the private stream: the login answer,
// a subscription's acknowledgement, a refusal, or a push of one topic.
func (p *privateStream) handle(msg []byte, receivedAt time.Time) {
	log := logger.GlobalLogger
	var f wsFrame
	if err := json.Unmarshal(msg, &f); err != nil {
		log.Warn("bitget: undecodable frame", "stream", "private", "err", err, "bytes", len(msg))
		return
	}
	switch {
	case f.Action != "":
		p.push(f, receivedAt)
	case f.Event == "login" && f.Code == "0":
		log.Info("bitget: logged in; subscribing", "stream", "private", "topics", 3)
		if err := p.send(p.subscribe); err != nil {
			log.Warn("bitget: subscribe not sent", "stream", "private", "err", err) // the read loop reports the loss
		}
	case f.Event == "subscribe":
		p.subscribed[f.Arg.Topic] = true
		if len(p.subscribed) == 3 && !p.client.privateUp.Swap(true) {
			log.Info("bitget: private stream ready", "topics", len(p.subscribed))
		}
	case f.Event == "error" || f.Event == "login":
		// A refused login or subscription: the stream will not deliver.
		log.Error("bitget: private stream refused a request", "event", f.Event, "code", string(f.Code), "msg", f.Msg, "topic", f.Arg.Topic)
	default:
		log.Debug("bitget: private event", "event", f.Event, "topic", f.Arg.Topic)
	}
}

// push converts the rows of one data frame and hands over those that are
// this program's.
func (p *privateStream) push(f wsFrame, receivedAt time.Time) {
	log := logger.GlobalLogger
	switch f.Arg.Topic {
	case topicOrder:
		var rows []orderRow
		if err := json.Unmarshal(f.Data, &rows); err != nil {
			log.Warn("bitget: undecodable private data", "topic", f.Arg.Topic, "err", err)
			return
		}
		for _, row := range rows {
			if !p.ours[row.Symbol] || !cex.OwnOrder(cex.ClientOrderID(row.ClientOid)) {
				continue
			}
			rec, err := orderRecord(row)
			if err != nil {
				log.Warn("bitget: order push skipped", "client_order_id", row.ClientOid, "err", err)
				continue
			}
			p.onEvent(cex.PrivateEvent{Order: &rec, ReceivedAt: receivedAt})
		}
	case topicFill:
		var rows []fillRow
		if err := json.Unmarshal(f.Data, &rows); err != nil {
			log.Warn("bitget: undecodable private data", "topic", f.Arg.Topic, "err", err)
			return
		}
		for _, row := range rows {
			if !p.ours[row.Symbol] || !cex.OwnOrder(cex.ClientOrderID(row.ClientOid)) {
				continue
			}
			out, err := fill(row, receivedAt)
			if err != nil {
				log.Warn("bitget: fill push skipped", "client_order_id", row.ClientOid, "err", err)
				continue
			}
			p.onEvent(cex.PrivateEvent{Fill: &out, ReceivedAt: receivedAt})
		}
	case topicPosition:
		var rows []positionRow
		if err := json.Unmarshal(f.Data, &rows); err != nil {
			log.Warn("bitget: undecodable private data", "topic", f.Arg.Topic, "err", err)
			return
		}
		p.positionFrame(f.Action, rows, receivedAt)
	default:
		log.Debug("bitget: private push of another topic", "topic", f.Arg.Topic)
	}
}

// positionFrame applies one frame of the position topic. The first frame
// after subscribing is a snapshot of every position of the account that is
// not zero, so a kept contract absent from it is flat. A later frame is an
// update: it holds the positions that changed, a closed one with size 0.
// A frame that touches the kept contracts, and every snapshot, is handed
// over as the whole set.
func (p *privateStream) positionFrame(action string, rows []positionRow, receivedAt time.Time) {
	kept := rows[:0:0]
	for _, row := range rows {
		if p.ours[row.Symbol] {
			kept = append(kept, row)
		}
	}
	if action == "snapshot" {
		p.hasSnapshot, p.positions = true, map[cex.Symbol]cex.Position{}
	} else if !p.hasSnapshot || len(kept) == 0 {
		return
	}
	for _, row := range kept {
		delete(p.positions, cex.Symbol(row.Symbol)) // the row replaces what was held
	}
	for _, row := range kept {
		if err := addPosition(p.positions, row); err != nil {
			logger.GlobalLogger.Warn("bitget: position push skipped", "symbol", row.Symbol, "err", err)
		}
	}
	set := cex.PositionSet{BySymbol: make(map[cex.Symbol]cex.Position, len(p.positions)), ReceivedAt: receivedAt, Loaded: true}
	for symbol, position := range p.positions {
		set.BySymbol[symbol] = position
	}
	p.onEvent(cex.PrivateEvent{Positions: &set, ReceivedAt: receivedAt})
}
