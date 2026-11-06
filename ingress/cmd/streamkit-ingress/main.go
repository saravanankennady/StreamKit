// Command streamkit-ingress accepts RTMP streams and pulls from URLs, and publishes them into rooms.
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
	"elnova-streamkit.local/egress/confine"
	"elnova-streamkit.local/ingress"
	"elnova-streamkit.local/server/config"
)

func main() {
	confine.RunHelperIfAsked()
	configPath := flag.String("config", "", "path to the server's YAML config file")
	dev := flag.Bool("dev", false, "development mode")
	flag.Parse()
	if err := run(*configPath, *dev); err != nil {
		fmt.Fprintln(os.Stderr, "streamkit-ingress:", err)
		os.Exit(1)
	}
}

func run(configPath string, dev bool) error {
	cfg, err := config.Load(configPath, dev)
	if err != nil {
		return err
	}
	if cfg.ControlPlane.DatabaseURL == "" {
		return fmt.Errorf("stream keys are in the database: set control_plane.database_url")
	}
	level := slog.LevelInfo
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return fmt.Errorf("log_level: %w", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	if err := confine.Protect(); err != nil {
		log.Warn("media processes may be able to read this worker's memory and environment", "error", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := store.OpenWorker(ctx, cfg.ControlPlane.DatabaseURL, cfg.ControlPlane.MasterKey)
	if err != nil {
		return err
	}
	defer st.Close()
	st.MaxIngressPerProject = cfg.Media.Ingress.MaxSessionsPerProject
	w, err := ingress.New(ingress.Options{Config: cfg, Store: st, Log: log, StaticKeys: cfg.Keys})
	if err != nil {
		return err
	}
	return w.Run(ctx)
}
