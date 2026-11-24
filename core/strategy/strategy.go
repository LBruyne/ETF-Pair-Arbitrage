package strategy

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
