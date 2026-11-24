// pairscan fills the tables of the Combo Spread board from public
// market data. It places no order and reads no account.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"etf-pair-arbitrage/dashboard/internal/scan"
)

func main() {
	configPath := flag.String("config", "dashboard/config.yaml", "collector configuration")
	flag.Parse()
	time.Local = time.FixedZone("UTC+8", 8*3600) // log times in UTC+8; stored times are Unix milliseconds
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Error("pairscan: config", "err", err)
		os.Exit(1)
	}
	dsn := os.Getenv(cfg.DSNEnv)
	if dsn == "" {
		log.Error("pairscan: the DSN environment variable is empty", "name", cfg.DSNEnv)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mgr, err := scan.NewManager(ctx, cfg, dsn, log)
	if err != nil {
		log.Error("pairscan: start", "err", err)
		os.Exit(1)
	}
	log.Info("pairscan started", "combos", len(cfg.Combos), "history_start", cfg.HistoryStart.Format("2006-01-02"))
	if err := mgr.Run(ctx); err != nil {
		log.Error("pairscan exiting", "err", err)
		os.Exit(1)
	}
	log.Info("pairscan stopped")
}

// loadConfig reads the yaml and rejects unknown keys, so a typo fails the start.
func loadConfig(path string) (scan.Config, error) {
	var cfg scan.Config
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil // NewManager validates
}
