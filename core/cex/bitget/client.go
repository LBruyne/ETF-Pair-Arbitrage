package bitget

import (
	"context"
	"net/http"
	"time"

	"etf-pair-arbitrage/core/cex"
)

// Client is the Bitget venue. It is built with the account's API key in live
// mode and with empty credentials in paper mode, where it can only read
// public data.
type Client struct {
	creds       cex.Credentials // never logged
	restURL     string
	wsPublicURL string
	http        *http.Client
	timing      wsTimings

	// onRawFrame, when set, sees every text frame of the public stream as it
	// arrived. Tests record with it; the process leaves it nil.
	onRawFrame func(raw []byte, receivedAt time.Time)
}

var _ cex.Venue = (*Client)(nil)

// NewClient builds the client. Nothing is connected until a method is
// called.
func NewClient(creds cex.Credentials) *Client {
	return &Client{creds: creds, restURL: restURL, wsPublicURL: wsPublicURL, http: &http.Client{Timeout: restTimeout}, timing: defaultWSTimings}
}

// Name is the venue's name in logs and records.
func (c *Client) Name() string { return "bitget" }

// --- the account side: not implemented, every method says so ---

func (c *Client) AccountSettings(ctx context.Context, symbols []cex.Symbol) (cex.AccountSettings, error) {
	return cex.AccountSettings{}, errNotImplemented
}

func (c *Client) Positions(ctx context.Context, symbols []cex.Symbol) (cex.PositionSet, error) {
	return cex.PositionSet{}, errNotImplemented
}

func (c *Client) OrderByClientID(ctx context.Context, symbol cex.Symbol, id cex.ClientOrderID) (cex.OrderRecord, error) {
	return cex.OrderRecord{}, errNotImplemented
}

func (c *Client) RecentOrders(ctx context.Context, symbol cex.Symbol, since time.Time) ([]cex.OrderRecord, error) {
	return nil, errNotImplemented
}

func (c *Client) Fills(ctx context.Context, since time.Time) ([]cex.Fill, error) {
	return nil, errNotImplemented
}

func (c *Client) FinancialRecords(ctx context.Context, since time.Time) ([]cex.FinancialRecord, error) {
	return nil, errNotImplemented
}

func (c *Client) SubscribePrivate(ctx context.Context, symbols []cex.Symbol, onEvent func(cex.PrivateEvent), onDown func(error)) error {
	return errNotImplemented
}

// PlaceOrder sends nothing: the error says so in the terms of
// cex.ErrNotSent.
func (c *Client) PlaceOrder(ctx context.Context, req cex.OrderRequest) (cex.OrderAck, error) {
	return cex.OrderAck{}, errNotSent
}
