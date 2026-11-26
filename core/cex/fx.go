package cex

import (
	"context"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/logger"
)

// runFX reads F every fxPoll until ctx ends.
func (m *CexManager) runFX(ctx context.Context) {
	ticker := time.NewTicker(m.timing.fxPoll)
	defer ticker.Stop()
	for {
		m.pollFX(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// pollFX reads the source's best bid and ask once. A valid quote is
// published as F = (bid + ask) / 2 with the arrival time of the answer. A
// failed read or an invalid quote (a side not positive, or the bid above
// the ask) publishes nothing, so it does not extend the life of the F read
// before it.
func (m *CexManager) pollFX(ctx context.Context) {
	log := logger.GlobalLogger
	// A read is given one interval at most: an answer that does not come is
	// a failed read, and the next poll is not held up behind it.
	callCtx, cancel := context.WithTimeout(ctx, m.timing.fxPoll)
	bid, ask, err := m.fxSource.Quote(callCtx)
	cancel()
	if ctx.Err() != nil {
		return
	}
	now := m.timing.clock()
	valid := err == nil && bid.IsPositive() && ask.IsPositive() && !bid.GreaterThan(ask)

	// The outcome of the read, logged when it changes.
	switch {
	case !valid && !m.fxFailing:
		log.Warn("cex: fx read failed or invalid; retrying", "bid", bid, "ask", ask, "err", err, "retry_in", m.timing.fxPoll)
	case valid && m.fxFailing:
		log.Info("cex: fx read again", "bid", bid, "ask", ask)
	}
	m.fxFailing = !valid

	if valid {
		fx := FX{Price: bid.Add(ask).Div(decimal.NewFromInt(2)), Bid: bid, Ask: ask, ReceivedAt: now}
		m.editReference(func(v *referenceView) { v.fx = fx })
		log.Debug("cex: fx", "price", fx.Price, "bid", bid, "ask", ask)
	}

	// Freshness, logged when it changes.
	fx, fresh := m.FX(now)
	switch {
	case !fresh && !m.fxStale:
		log.Warn("cex: fx stale", "age", fxAge(fx, now), "fx_max_age", m.cfg.FXMaxAge)
	case fresh && m.fxStale:
		log.Info("cex: fx fresh again", "price", fx.Price)
	}
	m.fxStale = !fresh
}

// FX returns the latest F. fresh is false when no valid quote was read
// within fx_max_age: entries stop, exits go on with this last value.
func (m *CexManager) FX(now time.Time) (fx FX, fresh bool) {
	fx = m.reference.Load().fx
	return fx, !fx.ReceivedAt.IsZero() && now.Sub(fx.ReceivedAt) <= m.cfg.FXMaxAge
}

// FXMinuteCloses reads the source's minute closes of [from, to), oldest
// first, for minutes the process did not record itself.
func (m *CexManager) FXMinuteCloses(ctx context.Context, from, to time.Time) ([]FXMinute, error) {
	return m.fxSource.MinuteCloses(ctx, from, to)
}

// fxAge writes the age of F for a log line.
func fxAge(fx FX, now time.Time) string {
	if fx.ReceivedAt.IsZero() {
		return "never read"
	}
	return now.Sub(fx.ReceivedAt).Round(time.Millisecond).String()
}
