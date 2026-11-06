// Command streamkit-egress runs recording jobs: it claims them from the database, records rooms with headless Chrome, and
// stores the results. Run as many as there is capacity for.
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
	"elnova-streamkit.local/egress/storage"
	"elnova-streamkit.local/egress/worker"
	"elnova-streamkit.local/server/config"
)

func main() {
	confine.RunHelperIfAsked()
	configPath := flag.String("config", "", "path to the server's YAML config file")
	dev := flag.Bool("dev", false, "development mode")
	maxJobs := flag.Int("max-jobs", 0, "recordings to run at once; default is media.egress.max_jobs, or NumCPU/3")
	flag.Parse()
	if err := run(*configPath, *dev, *maxJobs); err != nil {
		fmt.Fprintln(os.Stderr, "streamkit-egress:", err)
		os.Exit(1)
	}
}

func run(configPath string, dev bool, maxJobs int) error {
	cfg, err := config.Load(configPath, dev)
	if err != nil {
		return err
	}
	if cfg.ControlPlane.DatabaseURL == "" {
		return fmt.Errorf("the job queue is in the database: set control_plane.database_url")
	}
	if maxJobs > 0 {
		cfg.Media.Egress.MaxJobs = maxJobs
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
	backend, err := storage.New(cfg.Media.Storage)
	if err != nil {
		return fmt.Errorf("media storage: %w", err)
	}
	w, err := worker.New(worker.Options{Config: cfg, Store: st, Storage: backend, Log: log, StaticKeys: cfg.Keys})
	if err != nil {
		return err
	}
	w.Run(ctx)
	return nil
}
