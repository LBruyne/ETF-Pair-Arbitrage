package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"etf-pair-arbitrage/core/cex"
)

// The secrets files the cases use. Every value is fake.
const (
	envFull     = `{"bitget":{"key":"FAKE-KEY-111","secret":"FAKE-SECRET-222","passphrase":"FAKE-PASS-333"},"direxion":{"site_api_key":"FAKE-DIREXION-444"},"store":{"dsn":"postgres://etfpair_trade:FAKE-PW-555@127.0.0.1:5441/etfpair"}}`
	envNoBitget = `{"direxion":{"site_api_key":""},"store":{"dsn":"postgres://etfpair_trade:FAKE-PW-555@127.0.0.1:5441/etfpair"}}`
	envNoPass   = `{"bitget":{"key":"FAKE-KEY-111","secret":"FAKE-SECRET-222"},"store":{"dsn":"postgres://etfpair_trade:FAKE-PW-555@127.0.0.1:5441/etfpair"}}`
	envNoDSN    = `{"bitget":{"key":"FAKE-KEY-111","secret":"FAKE-SECRET-222","passphrase":"FAKE-PASS-333"}}`
	envGarbage  = `{"bitget":"garbage FAKE-KEY-111","store":{"dsn":"postgres://etfpair_trade:FAKE-PW-555@127.0.0.1:5441/etfpair"}}`
)

// edit is one change to the text of config.example.yaml: old is replaced by
// new once. An old that is not in the file fails the test, so a case cannot
// pass because its edit silently did nothing.
type edit struct{ old, new string }

// exampleWith returns the example configuration with edits applied.
func exampleWith(t *testing.T, edits ...edit) string {
	t.Helper()
	b, err := os.ReadFile("../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, e := range edits {
		if !strings.Contains(text, e.old) {
			t.Fatalf("config.example.yaml no longer contains %q", e.old)
		}
		text = strings.Replace(text, e.old, e.new, 1)
	}
	return text
}

// loadTexts writes the two texts to temporary files and loads them.
func loadTexts(t *testing.T, yamlText, envText string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	configPath, envPath := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "env.json")
	if err := os.WriteFile(configPath, []byte(yamlText), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte(envText), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(configPath, envPath)
}

// The files in the repository load as they are: the example configuration
// with the example secrets file (placeholders, paper mode).
func TestExampleFilesLoad(t *testing.T) {
	cfg, err := Load("../config.example.yaml", "../.env.example")
	if err != nil {
		t.Fatalf("the example configuration must load: %v", err)
	}
	if cfg.Exec.Live || cfg.Store.Live {
		t.Errorf("the example must be paper mode, got exec.live=%t store.live=%t", cfg.Exec.Live, cfg.Store.Live)
	}
	if cfg.Cex.Pair.Bull.Symbol != "SOXLUSDT" || cfg.Cex.Pair.Bear.BookTopic != cex.TopicBooks50 {
		t.Errorf("pair not read: %+v", cfg.Cex.Pair)
	}
	if got := cfg.Strategy.LevelBucket.String(); got != "0.01" {
		t.Errorf("strategy.level_bucket = %s, want 0.01", got)
	}
	if got := cfg.Cex.CorporateActions; len(got) != 2 || got[0].ExDate != "2026-12-22" {
		t.Errorf("cex.corporate_actions = %+v, want two actions on 2026-12-22", got)
	}
}

func TestLoad(t *testing.T) {
	cases := []struct {
		name    string
		edits   []edit
		env     string
		wantErr []string // every text must be in the error; empty: must load
	}{
		{name: "example with full secrets", env: envFull},
		{name: "paper mode needs no exchange key", env: envNoBitget},
		{name: "paper mode does not look at the bitget section", env: envGarbage},
		{name: "d_out equal to minus d_in is allowed", edits: []edit{{"d_out: -0.5 ", "d_out: -2 "}}, env: envFull},
		{name: "d_out written as zero is not missing", edits: []edit{{"d_out: -0.5 ", "d_out: 0 "}}, env: envFull},
		{name: "taker_fee_bps written as zero is not missing", edits: []edit{{"taker_fee_bps: 0.65", "taker_fee_bps: 0"}}, env: envFull},
		{name: "live with the full key", edits: []edit{{"live: false", "live: true"}}, env: envFull},

		{name: "missing d_out", edits: []edit{{"  d_out: -0.5 ", "  # d_out: -0.5 "}}, env: envFull,
			wantErr: []string{"strategy.d_out is required"}},
		{name: "missing mean_minutes", edits: []edit{{"  mean_minutes: 30", "  # mean_minutes: 30"}}, env: envFull,
			wantErr: []string{"strategy.mean_minutes is required"}},
		{name: "missing mean_model", edits: []edit{{"  mean_model: ema", "  # mean_model: ema"}}, env: envFull,
			wantErr: []string{"strategy.mean_model is required"}},
		{name: "missing taker_fee_bps", edits: []edit{{"  taker_fee_bps: 0.65", "  # taker_fee_bps: 0.65"}}, env: envFull,
			wantErr: []string{"strategy.taker_fee_bps is required"}},
		{name: "negative taker_fee_bps", edits: []edit{{"taker_fee_bps: 0.65", "taker_fee_bps: -1"}}, env: envFull,
			wantErr: []string{"strategy.taker_fee_bps must not be negative"}},
		{name: "missing capital", edits: []edit{{`  capital: "1000"`, `  # capital: "1000"`}}, env: envFull,
			wantErr: []string{"inventory.capital is required"}},
		{name: "missing sim_order_latency", edits: []edit{{"  sim_order_latency:", "  # sim_order_latency:"}}, env: envFull,
			wantErr: []string{"exec.sim_order_latency.bull is required", "exec.sim_order_latency.bear is required"}},
		{name: "missing pair", edits: []edit{{"    bull: {", "    # bull: {"}}, env: envFull,
			wantErr: []string{"cex.pair.bull.symbol is required"}},
		{name: "every problem is reported together", edits: []edit{{"  d_out: -0.5 ", "  # d_out: -0.5 "}, {`  leverage: "10"`, `  leverage: "0"`}}, env: envNoDSN,
			wantErr: []string{"strategy.d_out is required", "inventory.leverage", ".env store.dsn is required"}},

		{name: "d_out equal to d_in", edits: []edit{{"d_out: -0.5 ", "d_out: 2 "}}, env: envFull,
			wantErr: []string{"strategy.d_out must satisfy -d_in <= d_out < d_in"}},
		{name: "d_out above d_in", edits: []edit{{"d_out: -0.5 ", "d_out: 3 "}}, env: envFull,
			wantErr: []string{"strategy.d_out must satisfy -d_in <= d_out < d_in"}},
		{name: "d_out below minus d_in", edits: []edit{{"d_out: -0.5 ", "d_out: -2.5 "}}, env: envFull,
			wantErr: []string{"strategy.d_out must satisfy -d_in <= d_out < d_in"}},
		{name: "unknown mean model", edits: []edit{{"mean_model: ema", "mean_model: wma"}}, env: envFull,
			wantErr: []string{"strategy.mean_model"}},

		{name: "unknown key in a section", edits: []edit{{"  mean_model: ema", "  mean_model: ema\n  bogus_key: 1"}}, env: envFull,
			wantErr: []string{"bogus_key"}},
		{name: "unknown section", edits: []edit{{"log_level: info", "log_level: info\nstore: {dsn: x}"}}, env: envFull,
			wantErr: []string{"store"}},

		{name: "live without the exchange key", edits: []edit{{"live: false", "live: true"}}, env: envNoBitget,
			wantErr: []string{".env bitget.key is required when exec.live is true", ".env bitget.secret is required", ".env bitget.passphrase is required"}},
		{name: "live with a bitget section that is not an object", edits: []edit{{"live: false", "live: true"}}, env: envGarbage,
			wantErr: []string{"bitget must be an object with key, secret and passphrase"}},
		{name: "live with an incomplete key", edits: []edit{{"live: false", "live: true"}}, env: envNoPass,
			wantErr: []string{".env bitget.passphrase is required when exec.live is true"}},
		{name: "missing database connection string", env: envNoDSN,
			wantErr: []string{".env store.dsn is required"}},
		{name: "secrets file is not JSON", env: "bitget: FAKE-KEY-111",
			wantErr: []string{"not valid JSON"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := loadTexts(t, exampleWith(t, c.edits...), c.env)
			if len(c.wantErr) == 0 {
				if err != nil {
					t.Fatalf("must load, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("must fail with %q, but loaded: %+v", c.wantErr, cfg.Strategy)
			}
			for _, want := range c.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error lacks %q:\n%v", want, err)
				}
			}
			// No message may carry a secret, whatever went wrong.
			for _, secret := range []string{"FAKE-KEY-111", "FAKE-SECRET-222", "FAKE-PASS-333", "FAKE-PW-555"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error carries the secret %s:\n%v", secret, err)
				}
			}
		})
	}
}

// Paper mode does not keep the exchange key even when the secrets file has
// one; live mode keeps it.
func TestExchangeKeyIsKeptInLiveModeOnly(t *testing.T) {
	paper, err := loadTexts(t, exampleWith(t), envFull)
	if err != nil {
		t.Fatal(err)
	}
	if paper.Env.Bitget != (cex.Credentials{}) {
		t.Error("paper mode kept the exchange key")
	}
	live, err := loadTexts(t, exampleWith(t, edit{"live: false", "live: true"}), envFull)
	if err != nil {
		t.Fatal(err)
	}
	if live.Env.Bitget.Key != "FAKE-KEY-111" || live.Env.Bitget.Secret != "FAKE-SECRET-222" || live.Env.Bitget.Passphrase != "FAKE-PASS-333" {
		t.Error("live mode did not keep the exchange key")
	}
	if !live.Store.Live {
		t.Error("store.live must follow exec.live")
	}
}

// Without a named secrets file a paper run reads .env of the working
// directory, and a live run is refused before any file is read.
func TestDefaultSecretsFileIsForPaperOnly(t *testing.T) {
	dir := t.TempDir()
	paperPath, livePath := filepath.Join(dir, "paper.yaml"), filepath.Join(dir, "live.yaml")
	if err := os.WriteFile(paperPath, []byte(exampleWith(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(livePath, []byte(exampleWith(t, edit{"live: false", "live: true"})), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, DefaultEnvPath), []byte(envFull), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	if cfg, err := Load(paperPath, ""); err != nil || cfg.Env.Store.DSN == "" {
		t.Errorf("paper without a named secrets file must read %s: %v", DefaultEnvPath, err)
	}
	if _, err := Load(livePath, ""); err == nil || !strings.Contains(err.Error(), "-env must be given") {
		t.Errorf("live without a named secrets file = %v, want it refused", err)
	}
	if cfg, err := Load(livePath, filepath.Join(dir, DefaultEnvPath)); err != nil || cfg.Env.Bitget.Key != "FAKE-KEY-111" {
		t.Errorf("live with a named secrets file must load: %v", err)
	}
}

// The effective configuration written to process_run holds the yaml keys as
// config.yaml writes them and nothing from the secrets file, also in live
// mode, where the exchange key is in memory.
func TestEffectiveHoldsNoSecrets(t *testing.T) {
	cfg, err := loadTexts(t, exampleWith(t, edit{"live: false", "live: true"}), envFull)
	if err != nil {
		t.Fatal(err)
	}
	got := string(cfg.Effective())
	for _, secret := range []string{"FAKE-KEY-111", "FAKE-SECRET-222", "FAKE-PASS-333", "FAKE-DIREXION-444", "FAKE-PW-555", "postgres://", "dsn", "passphrase", "site_api_key"} {
		if strings.Contains(got, secret) {
			t.Errorf("effective config carries %q:\n%s", secret, got)
		}
	}
	for _, want := range []string{`"d_in":2`, `"d_out":-0.5`, `"mean_model":"ema"`, `"depth_max_age":"10s"`, `"capital":"1000"`, `"live":true`, `"symbol":"SOXLUSDT"`, `"bull":"15ms"`} {
		if !strings.Contains(got, want) {
			t.Errorf("effective config lacks %s:\n%s", want, got)
		}
	}
}
