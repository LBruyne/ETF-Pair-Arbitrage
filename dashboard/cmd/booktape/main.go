// booktape records the public WebSocket frames of the combos' perpetual
// contracts and writes the per-second rows of the Combo Spread board. It
// places no order and reads no account.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"etf-pair-arbitrage/dashboard/internal/booktape"
)

func main() {
	configPath := flag.String("config", "dashboard/config.yaml", "configuration: dsn_env and combos are read")
	output := flag.String("output", "experiments/book_tape", "directory of the raw recording")
	flag.Parse()
	time.Local = time.FixedZone("UTC+8", 8*3600) // log times in UTC+8; stored times are Unix milliseconds
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Error("booktape: config", "err", err)
		os.Exit(1)
	}
	dsn := os.Getenv(cfg.DSNEnv)
	if cfg.DSNEnv == "" || dsn == "" {
		log.Error("booktape: the DSN environment variable is empty", "name", cfg.DSNEnv)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mgr, err := booktape.NewManager(ctx, cfg, dsn, *output, log)
	if err != nil {
		log.Error("booktape: start", "err", err)
		os.Exit(1)
	}
	if err := mgr.Run(ctx); err != nil {
		log.Error("booktape: exiting", "err", err)
		os.Exit(1)
	}
	log.Info("booktape: stopped")
}

// loadConfig reads the yaml leniently: the keys other than dsn_env and
// combos belong to pairscan.
func loadConfig(path string) (booktape.Config, error) {
	var cfg booktape.Config
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}
