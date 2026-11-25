package strategy

import "github.com/shopspring/decimal"

// Strategy carries the validated parameters every computation of the package
// reads. It holds nothing else: its methods are pure functions of their
// inputs.
type Strategy struct {
	cfg Config
}

// New validates cfg and builds the strategy.
func New(cfg Config) (*Strategy, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Strategy{cfg: cfg}, nil
}

// Config returns the parameters the strategy was built with.
func (s *Strategy) Config() Config { return s.cfg }

// feeRate is the taker fee per leg as a fraction of the traded notional.
func (s *Strategy) feeRate() decimal.Decimal {
	return decimal.NewFromFloat(s.cfg.TakerFeeBps).Div(bpsPerUnit)
}

// bpsPerUnit converts basis points to a fraction.
var bpsPerUnit = decimal.NewFromInt(10000)
