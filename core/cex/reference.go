package cex

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/logger"
)

// referenceRead is one periodic read of the reference loop.
type referenceRead struct {
	name    string
	every   time.Duration // the interval after a read that succeeded
	once    bool          // read until it succeeds, then never again
	read    func(ctx context.Context, now time.Time) error
	next    time.Time // zero: due at once
	failing bool      // the last read failed; logged when it changes
	done    bool      // a once read that succeeded
}

// runReference reads the market rules, the account settings, the positions
// (once: afterwards ReadPositions refreshes them), the funding records, the
// settled funding rates and the venue's trading schedule, each at its own
// interval, until ctx ends. A read that fails is tried again after
// referenceRetry.
func (m *CexManager) runReference(ctx context.Context) {
	reads := m.referenceReads()
	ticker := time.NewTicker(m.timing.referenceTick)
	defer ticker.Stop()
	for {
		m.stepReference(ctx, reads, m.timing.clock())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// referenceReads lists the reads of the reference loop.
func (m *CexManager) referenceReads() []*referenceRead {
	return []*referenceRead{
		{name: "market rules", every: m.timing.rulesPoll, read: m.readRules},
		{name: "account settings", every: m.timing.settingsPoll, read: m.readSettings},
		{name: "positions", once: true, read: func(ctx context.Context, _ time.Time) error { _, err := m.ReadPositions(ctx); return err }},
		{name: "funding records", every: m.timing.recordsPoll, read: m.readFinancialRecords},
		{name: "funding rates", every: m.timing.fundingPoll, read: m.readFundingRates},
		{name: "trading schedule", every: m.timing.schedulePoll, read: m.checkSchedule},
	}
}

// stepReference runs every read that is due at now. The outcome of a read is
// logged when it changes: the first failure and the recovery, not every
// retry.
func (m *CexManager) stepReference(ctx context.Context, reads []*referenceRead, now time.Time) {
	log := logger.GlobalLogger
	for _, r := range reads {
		if r.done || now.Before(r.next) || ctx.Err() != nil {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, m.timing.sourceCall)
		err := r.read(callCtx, now)
		cancel()
		if err != nil {
			if !r.failing {
				log.Warn("cex: reference read failed; retrying", "read", r.name, "err", err, "retry_in", m.timing.referenceRetry)
			}
			r.failing, r.next = true, now.Add(m.timing.referenceRetry)
			continue
		}
		if r.failing {
			log.Info("cex: reference read succeeded again", "read", r.name)
		}
		r.failing, r.next, r.done = false, now.Add(r.every), r.once
	}
}

// --- market rules ---

// readRules reads both contracts' rules and publishes them when they are new
// or changed.
func (m *CexManager) readRules(ctx context.Context, _ time.Time) error {
	rules, err := m.venue.MarketRules(ctx, m.symbols[:])
	if err != nil {
		return err
	}
	for _, symbol := range m.symbols {
		if _, ok := rules[symbol]; !ok {
			return fmt.Errorf("the venue returned no rule for %s", symbol)
		}
	}
	if old := m.reference.Load().rules; old != nil && sameRule(old[m.symbols[Bull]], rules[m.symbols[Bull]]) && sameRule(old[m.symbols[Bear]], rules[m.symbols[Bear]]) {
		return nil
	}
	m.editReference(func(v *referenceView) { v.rules = rules })
	for _, leg := range Legs {
		r := rules[m.symbols[leg]]
		logger.GlobalLogger.Info("cex: market rule", "symbol", r.Symbol, "price_tick", r.PriceTick, "qty_step", r.QtyStep, "min_qty", r.MinQty, "min_notional_usdt", r.MinNotional, "online", r.Online)
	}
	return nil
}

// sameRule compares two rules by value.
func sameRule(a, b MarketRule) bool {
	return a.Symbol == b.Symbol && a.Online == b.Online && a.PriceTick.Equal(b.PriceTick) && a.QtyStep.Equal(b.QtyStep) && a.MinQty.Equal(b.MinQty) && a.MinNotional.Equal(b.MinNotional)
}

// Rule returns the leg's market rule; ok is false until the rules are
// loaded.
func (m *CexManager) Rule(leg Leg) (MarketRule, bool) {
	rule, ok := m.reference.Load().rules[m.symbols[leg]]
	return rule, ok
}

// TradingReady reports whether the venue side is ready for orders: the rules
// are loaded and both contracts are online.
func (m *CexManager) TradingReady() bool {
	bull, okBull := m.Rule(Bull)
	bear, okBear := m.Rule(Bear)
	return okBull && okBear && bull.Online && bear.Online
}

// --- account settings ---

// readSettings reads the account's position mode and the two contracts'
// margin mode and leverage, and publishes them. The process never changes
// them.
func (m *CexManager) readSettings(ctx context.Context, _ time.Time) error {
	settings, err := m.venue.AccountSettings(ctx, m.symbols[:])
	if err != nil {
		return err
	}
	m.editReference(func(v *referenceView) { v.settings = &settings })
	return nil
}

// SettingsOK reports whether the account is set up as the strategy needs:
// one-way position mode, and cross margin with this leverage on both
// contracts. detail says what was read and what differs; it is empty when ok.
// Settings that differ are an alarm: an Error line and an event, once per
// distinct detail.
func (m *CexManager) SettingsOK(leverage int) (ok bool, detail string) {
	settings := m.reference.Load().settings
	if settings == nil {
		return false, "the account settings have not been read yet"
	}
	var problems []string
	if !settings.OneWay {
		problems = append(problems, "position mode is not one-way")
	}
	for _, symbol := range m.symbols {
		s, found := settings.BySymbol[symbol]
		switch {
		case !found:
			problems = append(problems, fmt.Sprintf("%s has no setting on the account", symbol))
		case !s.Crossed:
			problems = append(problems, fmt.Sprintf("%s margin mode is not crossed", symbol))
		case s.Leverage != leverage:
			problems = append(problems, fmt.Sprintf("%s leverage is %d, want %d", symbol, s.Leverage, leverage))
		}
	}
	detail = strings.Join(problems, "; ")

	m.settingsLogMu.Lock()
	changed := detail != m.settingsLog
	m.settingsLog = detail
	m.settingsLogMu.Unlock()
	if changed && detail != "" {
		logger.GlobalLogger.Error("cex: account settings differ", "want", fmt.Sprintf("one-way, crossed, leverage %d on %s and %s", leverage, m.symbols[Bull], m.symbols[Bear]), "read", detail)
		m.recordEvent("error", "account_settings_differ", "the account settings differ from what the strategy needs: no entries", map[string]any{"leverage": leverage, "problems": problems})
	} else if changed {
		logger.GlobalLogger.Info("cex: account settings match", "leverage", leverage)
	}
	return detail == "", detail
}

// --- positions ---

// Positions returns the latest snapshot of the two legs' positions. Loaded
// is false until the first read succeeded.
func (m *CexManager) Positions() PositionSet { return m.account.Load().positions }

// ReadPositions reads the two legs' positions from the venue now, publishes
// them and returns them.
func (m *CexManager) ReadPositions(ctx context.Context) (PositionSet, error) {
	read, err := m.venue.Positions(ctx, m.symbols[:])
	if err != nil {
		return PositionSet{}, err
	}
	positions := PositionSet{BySymbol: map[Symbol]Position{}, ReceivedAt: m.timing.clock(), Loaded: true}
	for _, symbol := range m.symbols {
		if p, ok := read.BySymbol[symbol]; ok {
			positions.BySymbol[symbol] = p
		}
	}
	m.editAccount(func(v *accountView) { v.positions = positions })
	return positions, nil
}

// --- funding records and settled rates ---

// readFinancialRecords reads the account's funding records since the last
// read (with one interval of overlap; the first read goes back
// recordsLookback), keeps those of the two legs that are new, and publishes
// when there are any.
func (m *CexManager) readFinancialRecords(ctx context.Context, now time.Time) error {
	since := m.recordsSince
	if since.IsZero() {
		since = now.Add(-m.timing.recordsLookback)
	}
	read, err := m.venue.FinancialRecords(ctx, since)
	if err != nil {
		return err
	}
	m.recordsSince = now.Add(-m.timing.recordsPoll)

	held := m.account.Load().records
	known := make(map[string]bool, len(held))
	for _, r := range held {
		known[r.ID] = true
	}
	var fresh []FinancialRecord
	for _, r := range read {
		if _, ours := m.legOf(r.Symbol); ours && !known[r.ID] {
			known[r.ID] = true
			fresh = append(fresh, r)
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	records := append(append([]FinancialRecord(nil), held...), fresh...)
	sort.SliceStable(records, func(i, j int) bool { return records[i].At.Before(records[j].At) })
	m.editAccount(func(v *accountView) { v.records = records })
	for _, r := range fresh {
		logger.GlobalLogger.Info("cex: funding record", "id", r.ID, "symbol", r.Symbol, "type", r.Type, "amount_usdt", r.Amount, "at", showTime(r.At))
	}
	return nil
}

// FinancialRecords returns the funding records of the two legs read so far,
// oldest first. The slice is shared and must not be changed.
func (m *CexManager) FinancialRecords() []FinancialRecord { return m.account.Load().records }

// readFundingRates reads both contracts' recent settled funding rates and
// publishes the settlements that are new.
func (m *CexManager) readFundingRates(ctx context.Context, _ time.Time) error {
	old := m.reference.Load().funding
	var next [2]map[int64]decimal.Decimal
	added := 0
	for _, leg := range Legs {
		settlements, err := m.venue.FundingHistory(ctx, m.symbols[leg], m.timing.fundingRates)
		if err != nil {
			return err
		}
		next[leg] = maps.Clone(old[leg])
		if next[leg] == nil {
			next[leg] = map[int64]decimal.Decimal{}
		}
		for _, s := range settlements {
			if _, held := next[leg][s.At.UnixMilli()]; !held {
				next[leg][s.At.UnixMilli()] = s.Rate
				added++
			}
		}
	}
	if added > 0 {
		m.editReference(func(v *referenceView) { v.funding = next })
		logger.GlobalLogger.Debug("cex: funding rates", "new_settlements", added)
	}
	return nil
}

// FundingRateAt returns the funding rate the leg's contract settled at the
// given settlement time; ok is false while the venue has not published it or
// it has not been read.
func (m *CexManager) FundingRateAt(leg Leg, settlement time.Time) (rate decimal.Decimal, ok bool) {
	rate, ok = m.reference.Load().funding[leg][settlement.UnixMilli()]
	return rate, ok
}

// --- the calendar check ---

// checkSchedule compares the calendar with the venue's schedule of the US
// equity market. A difference is an alarm, an Error line and an event, when
// it first appears and again whenever its content changes; an unchanged
// difference is not repeated at every check. The calendar is never changed
// to follow the venue.
func (m *CexManager) checkSchedule(ctx context.Context, _ time.Time) error {
	days, err := m.index.TradingSchedule(ctx)
	if err != nil {
		return err
	}
	if len(days) == 0 {
		return fmt.Errorf("the venue's schedule lists no regular session")
	}
	differences := m.calendar.CompareSchedule(days)
	text := strings.Join(differences, "; ")
	changed := text != m.scheduleDiff
	m.scheduleDiff = text
	switch {
	case !changed:
		logger.GlobalLogger.Debug("cex: calendar check unchanged", "days", len(days), "differences", len(differences))
	case text == "":
		logger.GlobalLogger.Info("cex: calendar agrees with the venue schedule again", "days", len(days))
	default:
		logger.GlobalLogger.Error("cex: calendar differs from the venue schedule", "differences", text)
		m.recordEvent("error", "calendar_differs", "the calendar differs from the venue's trading schedule; the calendar is kept", map[string]any{"differences": differences})
	}
	return nil
}
