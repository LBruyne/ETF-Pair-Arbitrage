package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/strategy"
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
	if cfg.Cex.Pair.Bull.Symbol != "SOXLUSDT" || cfg.Cex.Pair.Bull.BookTopic != cex.TopicBooks5 || cfg.Cex.Pair.Bear.BookTopic != cex.TopicBooks5 {
		t.Errorf("pair not read: %+v, want books5 on both legs", cfg.Cex.Pair)
	}
	if got := cfg.Strategy.PricingLevel; got != (strategy.LegLevels{Bull: 2, Bear: 5}) {
		t.Errorf("strategy.pricing_level = %+v, want bull 2, bear 5", got)
	}
	if got := cfg.Cex.CorporateActions; len(got) != 2 || got[0].ExDate != "2026-12-22" {
		t.Errorf("cex.corporate_actions = %+v, want two actions on 2026-12-22", got)
	}
	// Today's behaviour (SPEC 12.8): both legs together and one exit line.
	if e := cfg.Exec; e.ThinLeg != "" || e.PairWindow != 30*time.Second || e.PairStopD != 10 || !e.ThinTopupMax.Equal(decimal.NewFromInt(2)) || e.ExitPairWindow != 30*time.Second {
		t.Errorf("exec thin-leg keys = %q %s %v %s %s, want \"\" 30s 10 2 30s", e.ThinLeg, e.PairWindow, e.PairStopD, e.ThinTopupMax, e.ExitPairWindow)
	}
	if _, ok := cfg.Exec.Thin(); ok {
		t.Error("the example must not take a thin leg first")
	}
	if cfg.Strategy.Exit2 != nil {
		t.Errorf("strategy.exit2 = %v, want absent", *cfg.Strategy.Exit2)
	}
}

// The keys the example leaves at their defaults keep them when they are not
// written at all, so a config.yaml from before section 12 loads with the same
// behaviour; written, thin_leg and exit2 read as given.
func TestThinLegAndExit2(t *testing.T) {
	bare, err := loadTexts(t, exampleWith(t, edit{`  thin_leg: ""`, ""}, edit{"  pair_window: 30s", ""}, edit{"  pair_stop_d: 10", ""},
		edit{`  thin_topup_max: "2"`, ""}, edit{"  exit_pair_window: 30s", ""}), envFull)
	if err != nil {
		t.Fatal(err)
	}
	if bare.Exec.ThinLeg != "" || bare.Exec.PairWindow != 30*time.Second || bare.Exec.PairStopD != 10 || !bare.Exec.ThinTopupMax.Equal(decimal.NewFromInt(2)) || bare.Exec.ExitPairWindow != 30*time.Second {
		t.Errorf("defaults = %+v", bare.Exec)
	}

	cfg, err := loadTexts(t, exampleWith(t, edit{`thin_leg: ""`, "thin_leg: bear"}, edit{"  # exit2: -1 ", "  exit2: -1 "}, edit{`thin_topup_max: "2"`, "thin_topup_max: 2"}), envFull)
	if err != nil {
		t.Fatal(err)
	}
	if leg, ok := cfg.Exec.Thin(); !ok || leg != cex.Bear {
		t.Errorf("Thin() = %v %t, want bear", leg, ok)
	}
	if cfg.Strategy.Exit2 == nil || *cfg.Strategy.Exit2 != -1 {
		t.Errorf("strategy.exit2 = %v, want -1", cfg.Strategy.Exit2)
	}
	if !cfg.Exec.ThinTopupMax.Equal(decimal.NewFromInt(2)) {
		t.Errorf("thin_topup_max unquoted = %s, want 2", cfg.Exec.ThinTopupMax)
	}
	if got := string(cfg.Effective()); !strings.Contains(got, `"exit2":-1`) || !strings.Contains(got, `"thin_leg":"bear"`) {
		t.Errorf("effective config lacks exit2 or thin_leg:\n%s", got)
	}
	// Without exit2 the stored configuration has no such key (the board reads
	// its absence as one exit line).
	if got := string(bare.Effective()); strings.Contains(got, "exit2") || got == "null" {
		t.Errorf("effective config without exit2 = %s", got)
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
		{name: "entry_out equal to minus entry_in is allowed", edits: []edit{{"entry_out: -0.5 ", "entry_out: -2 "}}, env: envFull},
		{name: "entry_out written as zero is not missing", edits: []edit{{"entry_out: -0.5 ", "entry_out: 0 "}}, env: envFull},
		{name: "taker_fee_bps written as zero is not missing", edits: []edit{{"taker_fee_bps: 0.65", "taker_fee_bps: 0"}}, env: envFull},
		{name: "live with the full key", edits: []edit{{"live: false", "live: true"}}, env: envFull},

		{name: "missing entry_out", edits: []edit{{"  entry_out: -0.5 ", "  # entry_out: -0.5 "}}, env: envFull,
			wantErr: []string{"strategy.entry_out is required"}},
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
		{name: "every problem is reported together", edits: []edit{{"  entry_out: -0.5 ", "  # entry_out: -0.5 "}, {`  leverage: "10"`, `  leverage: "0"`}}, env: envNoDSN,
			wantErr: []string{"strategy.entry_out is required", "inventory.leverage", ".env store.dsn is required"}},

		{name: "entry_out equal to entry_in", edits: []edit{{"entry_out: -0.5 ", "entry_out: 2 "}}, env: envFull,
			wantErr: []string{"strategy.entry_out must satisfy -entry_in <= entry_out < entry_in"}},
		{name: "entry_out above entry_in", edits: []edit{{"entry_out: -0.5 ", "entry_out: 3 "}}, env: envFull,
			wantErr: []string{"strategy.entry_out must satisfy -entry_in <= entry_out < entry_in"}},
		{name: "entry_out below minus entry_in", edits: []edit{{"entry_out: -0.5 ", "entry_out: -2.5 "}}, env: envFull,
			wantErr: []string{"strategy.entry_out must satisfy -entry_in <= entry_out < entry_in"}},
		// SPEC 12.8: d_in and d_out were renamed; the old keys are refused, not read.
		{name: "the old d_in is refused", edits: []edit{{"  entry_in: 2 ", "  d_in: 2 "}}, env: envFull,
			wantErr: []string{"field d_in not found"}},
		{name: "the old d_out is refused", edits: []edit{{"  entry_out: -0.5 ", "  d_out: -0.5 "}}, env: envFull,
			wantErr: []string{"field d_out not found"}},

		// exit2: optional; -entry_in <= exit2 <= entry_out (SPEC 12.6).
		{name: "exit2 written", edits: []edit{{"  # exit2: -1 ", "  exit2: -1 "}}, env: envFull},
		{name: "exit2 equal to entry_out is allowed", edits: []edit{{"  # exit2: -1 ", "  exit2: -0.5 "}}, env: envFull},
		{name: "exit2 equal to minus entry_in is allowed", edits: []edit{{"  # exit2: -1 ", "  exit2: -2 "}}, env: envFull},
		{name: "exit2 before entry_out", edits: []edit{{"  # exit2: -1 ", "  exit2: 0 "}}, env: envFull,
			wantErr: []string{"strategy.exit2 must satisfy -entry_in <= exit2 <= entry_out, got exit2 0 with entry_in 2 and entry_out -0.5"}},
		{name: "exit2 beyond minus entry_in", edits: []edit{{"  # exit2: -1 ", "  exit2: -2.5 "}}, env: envFull,
			wantErr: []string{"strategy.exit2 must satisfy -entry_in <= exit2 <= entry_out"}},

		// The thin-leg-first execution (SPEC 12.4, 12.7, 12.8).
		{name: "thin_leg bear", edits: []edit{{`thin_leg: ""`, "thin_leg: bear"}}, env: envFull},
		{name: "thin_leg bull", edits: []edit{{`thin_leg: ""`, "thin_leg: bull"}}, env: envFull},
		{name: "thin_leg left out keeps both legs together", edits: []edit{{`  thin_leg: ""`, `  # thin_leg: ""`}}, env: envFull},
		{name: "thin_leg not a leg", edits: []edit{{`thin_leg: ""`, "thin_leg: soxs"}}, env: envFull,
			wantErr: []string{`exec.thin_leg must be bull, bear or "", got "soxs"`}},
		{name: "pair_window zero without a thin leg is not used", edits: []edit{{"pair_window: 30s", "pair_window: 0s"}}, env: envFull},
		{name: "pair_window zero with a thin leg", edits: []edit{{`thin_leg: ""`, "thin_leg: bear"}, {"pair_window: 30s", "pair_window: 0s"}}, env: envFull,
			wantErr: []string{"exec.pair_window must be positive when exec.thin_leg is set, got 0s"}},
		{name: "pair_stop_d zero", edits: []edit{{"pair_stop_d: 10", "pair_stop_d: 0"}}, env: envFull,
			wantErr: []string{"exec.pair_stop_d must be positive, got 0"}},
		{name: "thin_topup_max one is allowed", edits: []edit{{`thin_topup_max: "2"`, `thin_topup_max: "1"`}}, env: envFull},
		{name: "thin_topup_max below one", edits: []edit{{`thin_topup_max: "2"`, `thin_topup_max: "0.5"`}}, env: envFull,
			wantErr: []string{"exec.thin_topup_max must be at least 1, got 0.5"}},
		{name: "exit_pair_window zero", edits: []edit{{"exit_pair_window: 30s", "exit_pair_window: 0s"}}, env: envFull,
			wantErr: []string{"exec.exit_pair_window must be positive, got 0s"}},

		{name: "pricing_level left out keeps the default", edits: []edit{{"  pricing_level: {", "  # pricing_level: {"}}, env: envFull},
		{name: "pricing_level of one leg written", edits: []edit{{"pricing_level: { bull: 2, bear: 5 }", "pricing_level: { bear: 4 }"}}, env: envFull},
		{name: "pricing_level out of range", edits: []edit{{"pricing_level: { bull: 2, bear: 5 }", "pricing_level: { bull: 0, bear: 6 }"}}, env: envFull,
			wantErr: []string{"strategy.pricing_level.bull must be 1 to 5, got 0", "strategy.pricing_level.bear must be 1 to 5, got 6"}},
		{name: "pricing_level in the old words", edits: []edit{{"pricing_level: { bull: 2, bear: 5 }", "pricing_level: { bull: second, bear: worst }"}}, env: envFull,
			wantErr: []string{"cannot unmarshal !!str `second` into int"}},
		{name: "the removed level_bucket is refused", edits: []edit{{"  take_ratio:", "  level_bucket: \"0.01\"\n  take_ratio:"}}, env: envFull,
			wantErr: []string{"level_bucket"}},
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
	for _, want := range []string{`"entry_in":2`, `"entry_out":-0.5`, `"thin_leg":""`, `"pair_window":"30s"`, `"pair_stop_d":10`, `"thin_topup_max":"2"`, `"exit_pair_window":"30s"`, `"mean_model":"ema"`, `"depth_max_age":"10s"`, `"capital":"1000"`, `"live":true`, `"symbol":"SOXLUSDT"`, `"bull":"15ms"`} {
		if !strings.Contains(got, want) {
			t.Errorf("effective config lacks %s:\n%s", want, got)
		}
	}
}

// Parse reads a configuration without a secrets file: the yaml of
// config.yaml, or the JSON a run stored in process_run.config (the replay
// reads both). Defaults fill what is left out; unknown keys and invalid
// values are refused.
func TestParseReadsAStoredRunConfig(t *testing.T) {
	stored := `{"log_level":"info","cex":{"pair":{"bull":{"symbol":"SOXLUSDT","ticker":"SOXL","book_topic":"books5"},"bear":{"symbol":"SOXSUSDT","ticker":"SOXS","book_topic":"books5"}},"book_connections":3,"depth_max_age":"10s","fx_max_age":"1m0s"},
"strategy":{"mean_model":"ema","mean_minutes":30,"sigma_minutes":30,"entry_in":2,"entry_out":-0.5,"taker_fee_bps":0.65},"inventory":{"capital":"1000","leverage":"10"},
"exec":{"live":false,"stop_alarm_after":"1m0s","sim_order_latency":{"bear":"15ms","bull":"15ms"}}}`
	cfg, err := Parse([]byte(stored))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Exec.SimOrderLatency.Bear != 15*time.Millisecond || cfg.Cex.FXMaxAge != time.Minute || !cfg.Inventory.Capital.Equal(decimal.RequireFromString("1000")) || cfg.Strategy.EntryOut != -0.5 || cfg.Cex.BookConnections != 3 {
		t.Errorf("parsed %+v", cfg)
	}
	if _, err := Parse([]byte(`{"strategy":{"entry_inn":2}}`)); err == nil {
		t.Error("an unknown key was accepted")
	}
	if _, err := Parse([]byte(strings.Replace(stored, `"entry_out":-0.5`, `"entry_out":3`, 1))); err == nil {
		t.Error("entry_out >= entry_in was accepted")
	}
	// A run stored before the rename is refused rather than read without its lines.
	if _, err := Parse([]byte(strings.Replace(stored, `"entry_in":2,"entry_out":-0.5`, `"d_in":2,"d_out":-0.5`, 1))); err == nil || !strings.Contains(err.Error(), "d_in") {
		t.Errorf("a stored config with d_in = %v, want it refused", err)
	}
}
