// Package config loads the process's settings from two files:
//   - config.yaml: the public parameters (see config.example.yaml)
//   - the secrets file: JSON with the exchange key, the issuer's API key and
//     the database connection string (see .env.example)
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"gopkg.in/yaml.v3"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/exec"
	"etf-pair-arbitrage/core/inventory"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// LogDir is where the logger writes its files, relative to the working
// directory.
const LogDir = "./logs"

// DefaultEnvPath is the secrets file a paper run reads when none is named.
// A live run never uses it.
const DefaultEnvPath = ".env"

// Config is the whole of config.yaml, one section per module, plus the
// secrets.
type Config struct {
	LogLevel  string           `yaml:"log_level"` // debug, info, warn or error
	Cex       cex.Config       `yaml:"cex"`
	Strategy  strategy.Config  `yaml:"strategy"`
	Inventory inventory.Config `yaml:"inventory"`
	Exec      exec.Config      `yaml:"exec"`

	Store store.Config `yaml:"-"` // no yaml section: derived from exec.live
	Env   cex.Env      `yaml:"-"` // the secrets file; never logged or stored
}

// Load reads configPath (yaml) and envPath (json) and validates every
// section; all problems are reported together. Unknown yaml keys are
// rejected, so a typo surfaces at start.
//
// An empty envPath means that no secrets file was named. A paper run
// (exec.live false) then reads DefaultEnvPath; a live run is refused, so that
// it only ever runs on a credentials file its operator pointed it at.
//
// The secrets file's `bitget` section is decoded in live mode only. In paper
// mode it is not looked at: it may be missing or hold anything, and the
// exchange key is never in the process.
func Load(configPath, envPath string) (*Config, error) {
	// The defaults, which a key left out of the yaml keeps.
	cfg := &Config{LogLevel: "info", Cex: cex.DefaultConfig, Strategy: strategy.DefaultConfig, Exec: exec.DefaultConfig}

	// The yaml, with unknown keys refused. An empty file decodes to the
	// defaults and fails validation on the keys that have none.
	b, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", configPath, err)
	}
	cfg.Store.Live = cfg.Exec.Live

	// The secrets file. Live mode takes no default.
	if envPath == "" {
		if cfg.Exec.Live {
			return nil, fmt.Errorf("exec.live is true: -env must be given and point at the service's credentials file (the default %s is read in paper mode only)", DefaultEnvPath)
		}
		envPath = DefaultEnvPath
	}
	if b, err = os.ReadFile(envPath); err != nil {
		return nil, err
	}

	// The secrets, as JSON, with the exchange key left undecoded. Errors are
	// reported without the file's text.
	secrets := struct {
		*cex.Env
		Bitget json.RawMessage `json:"bitget"`
	}{Env: &cfg.Env}
	if err := json.Unmarshal(b, &secrets); err != nil {
		return nil, fmt.Errorf("%s: not valid JSON in the shape of .env.example", envPath)
	}
	if cfg.Exec.Live && len(secrets.Bitget) > 0 {
		if err := json.Unmarshal(secrets.Bitget, &cfg.Env.Bitget); err != nil {
			return nil, fmt.Errorf("%s: bitget must be an object with key, secret and passphrase", envPath)
		}
	}

	// Every section validates itself.
	var level slog.Level
	levelErr := level.UnmarshalText([]byte(cfg.LogLevel))
	if levelErr != nil {
		levelErr = fmt.Errorf("log_level must be debug, info, warn or error, got %q", cfg.LogLevel)
	}
	if err := errors.Join(levelErr, cfg.Cex.Validate(), cfg.Strategy.Validate(), cfg.Inventory.Validate(), cfg.Exec.Validate(), cfg.Env.Validate(cfg.Exec.Live)); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Effective is the configuration as loaded, without anything from the
// secrets file, for the process_run row: the keys of config.yaml, defaults
// included, with durations as Go writes them (60s reads "1m0s").
func (c *Config) Effective() json.RawMessage {
	public := *c
	public.Env = cex.Env{}
	text, err := yaml.Marshal(public)
	if err != nil {
		return json.RawMessage("null")
	}
	var tree map[string]any
	if err := yaml.Unmarshal(text, &tree); err != nil {
		return json.RawMessage("null")
	}
	out, err := json.Marshal(tree)
	if err != nil {
		return json.RawMessage("null")
	}
	return out
}
