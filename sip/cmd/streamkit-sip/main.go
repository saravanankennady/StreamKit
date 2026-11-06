// Command streamkit-sip connects phone calls to rooms. See docs/deployment/sip.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/server/config"
	"elnova-streamkit.local/sip"
)

func main() {
	configPath := flag.String("config", "", "path to the server's YAML config file")
	dev := flag.Bool("dev", false, "development mode")
	version := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *version {
		fmt.Println(config.Version)
		return
	}
	if err := run(*configPath, *dev); err != nil {
		fmt.Fprintln(os.Stderr, "streamkit-sip:", err)
		os.Exit(1)
	}
}

func run(configPath string, dev bool) error {
	cfg, err := config.Load(configPath, dev)
	if err != nil {
		return err
	}
	if cfg.ControlPlane.DatabaseURL == "" {
		return fmt.Errorf("trunks and calls are in the database: set control_plane.database_url")
	}
	level := slog.LevelInfo
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return fmt.Errorf("log_level: %w", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := store.Open(ctx, cfg.ControlPlane.DatabaseURL, cfg.ControlPlane.MasterKey)
	if err != nil {
		return err
	}
	defer st.Close()
	w, err := sip.New(sip.Options{Config: cfg, Store: st, Log: log, StaticKeys: cfg.Keys})
	if err != nil {
		return err
	}
	return w.Run(ctx)
}
