// Package inventory keeps the books of the strategy: cycles and lots, their
// profit and loss, the realised total R, the quota, and the comparison with
// the positions on the exchange. It sends no order.
package inventory

import (
	"errors"
	"fmt"

	"github.com/shopspring/decimal"
)

// --- configuration ---

// Config is the `inventory:` section of config.yaml. Neither key has a
// default: both must be written.
type Config struct {
	Capital  decimal.Decimal `yaml:"capital"`  // M: the capital given to the strategy, in USDT
	Leverage decimal.Decimal `yaml:"leverage"` // L: the quota is L x (M + min(0, R)); both contracts must be set to it on the account
}

// Validate rejects a configuration the manager could not run with. Every
// problem is reported, each starting with its yaml path.
func (c Config) Validate() error {
	var errs []error
	if !c.Capital.IsPositive() {
		errs = append(errs, fmt.Errorf("inventory.capital is required and must be positive, got %s", c.Capital))
	}
	if !c.Leverage.IsPositive() || !c.Leverage.IsInteger() {
		errs = append(errs, fmt.Errorf("inventory.leverage is required and must be a positive whole number, got %s", c.Leverage))
	}
	return errors.Join(errs...)
}
