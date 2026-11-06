// Command streamkit-server runs the Elnova StreamKit media and signalling server.
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"elnova-streamkit.local/api/middleware"
	"elnova-streamkit.local/api/recorder"
	"elnova-streamkit.local/api/routes"
	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/api/webhook"
	"elnova-streamkit.local/server/agent"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/config"
	"elnova-streamkit.local/server/metrics"
	"elnova-streamkit.local/server/monitor"
	"elnova-streamkit.local/server/room"
	"elnova-streamkit.local/server/roomapi"
	"elnova-streamkit.local/server/routing"
	"elnova-streamkit.local/server/signaling"
	"elnova-streamkit.local/server/telemetry"
	"elnova-streamkit.local/server/transport"
	sipapi "elnova-streamkit.local/sip/api"
)

const defaultProject = auth.StaticProject

// How often one session can try to reconnect. A client backs off between tries, and manages a handful in a minute, so this
// only stops something that is not really trying to reconnect.
const (
	resumesPerMinute = 120
	resumeBurst      = 60
)

func main() {
	configPath := flag.String("config", "", "path to a YAML config file")
	dev := flag.Bool("dev", false, "development mode: well-known keys, loopback only")
	version := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *version {
		fmt.Println(config.Version)
		return
	}
	if err := run(*configPath, *dev); err != nil {
		fmt.Fprintln(os.Stderr, "streamkit-server:", err)
		os.Exit(1)
	}
}

func run(configPath string, dev bool) error {
	cfg, err := config.Load(configPath, dev)
	if err != nil {
		return err
	}

	level := slog.LevelInfo
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return fmt.Errorf("log_level: %w", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.OTLPEndpoint != "" {
		stopTracing, err := telemetry.Start(ctx, cfg.OTLPEndpoint, "streamkit-server", config.Version)
		if err != nil {
			return fmt.Errorf("tracing: %w", err)
		}
		defer func() {
			flushCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = stopTracing(flushCtx)
		}()
		log.Info("tracing enabled", "endpoint", cfg.OTLPEndpoint)
	}

	engine, err := transport.New(cfg.RTC)
	if err != nil {
		return err
	}
	defer engine.Close()

	var keys auth.KeyProvider = auth.StaticKeys{ProjectID: defaultProject, Keys: cfg.Keys}
	roomsCtx, stopRooms := context.WithCancel(context.Background())
	defer stopRooms()
	rooms := room.NewManager(roomsCtx, cfg, engine, log)

	mon := monitor.New(rooms)
	go mon.Run(ctx)

	readiness := map[string]func(context.Context) error{}
	mux := http.NewServeMux()
	var database *store.Store
	var router routing.Router = routing.Local{ID: rooms.NodeID()}
	var redis *routing.Redis
	if cfg.Redis.URL != "" {
		redis, err = routing.NewRedis(ctx, cfg.Redis.URL, rooms.NodeID())
		if err != nil {
			return err
		}
		defer func() { _ = redis.Close() }()
		router = redis
		readiness["redis"] = redis.Ping
	}
	if cfg.ControlPlane.DatabaseURL != "" {
		cached, st, err := startControlPlane(ctx, cfg, keys, rooms, router, mon, mux, log)
		if err != nil {
			return err
		}
		defer st.Close()
		keys = cached
		database = st
		readiness["database"] = st.DB.Ping
	}
	rtc := signaling.NewService(cfg, keys, rooms, log)
	rtc.Register(mux)
	agents := agent.NewBroker(rooms, keys, log)
	defer agents.Close()
	agents.Register(mux)
	roomAPI := roomapi.NewService(cfg, keys, rooms)
	roomAPI.Agents = agents
	roomAPI.Register(mux)
	sipapi.New(cfg, keys, database, log).Register(mux)

	if redis != nil {
		cluster := monitor.NewCluster(mon, rooms, redis, cfg.Redis.NodeURL, cfg.Region, log)
		go cluster.Run(ctx)
		mon.SetPeers(cluster.Peers)
		roomAPI.RemoteRooms = cluster.RoomsOf
		log.Info("cluster mode enabled", "nodeURL", cfg.Redis.NodeURL)
	}
	mux.Handle("/metrics", metricsHandler(cfg))
	live := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("OK")) }
	mux.HandleFunc("/livez", live)
	mux.HandleFunc("/healthz", live)
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if rooms.Draining() {
			http.Error(w, "draining", http.StatusServiceUnavailable)
			return
		}
		checkCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		for name, check := range readiness {
			if err := check(checkCtx); err != nil {
				log.Warn("readiness check failed", "check", name, "error", err)
				http.Error(w, name+" unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		_, _ = w.Write([]byte("OK"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("OK"))
	})

	// Routing goes outside CORS. A proxied response already has the hosting node's CORS headers, and browsers
	// reject a response with them twice.
	proxy := routing.NewProxy(router, keys, cfg.HopSecret(), log)
	handler := proxy.Wrap(withCORS(cfg.AllowedOrigins, mux))
	if perMinute, burst := cfg.RTCConnectLimit(); perMinute > 0 {
		byAddress := middleware.NewRateLimiter(float64(perMinute), burst)
		handler = rtc.Limit(signaling.ConnectLimits{
			New: func(r *http.Request) bool {
				return byAddress.Allow(middleware.AddressKey(middleware.ClientIP(r, cfg.ControlPlane.TrustProxyHeaders)))
			},
			Resume:   middleware.NewRateLimiter(resumesPerMinute, resumeBurst).Allow,
			FromPeer: proxy.FromPeer,
			// A browser can only read why this failed if it has the same headers as everything else.
			Refused: withCORS(cfg.AllowedOrigins, http.HandlerFunc(signaling.TooManyConnects)),
		}, handler)
	}
	if cfg.OTLPEndpoint != "" {
		handler = telemetry.Wrap(handler)
	}
	server := &http.Server{
		Addr:              net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Port)),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errs := make(chan error, 1)
	go func() {
		log.Info("server started", "address", server.Addr, "version", config.Version, "node", rooms.NodeID(),
			"rtcUDP", cfg.RTC.UDPPort, "rtcTCP", cfg.RTC.TCPPort, "development", cfg.Development)
		errs <- server.ListenAndServe()
	}()

	select {
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	log.Info("shutting down")
	stopRooms()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return server.Shutdown(shutdownCtx)
}

// metricsHandler serves Prometheus metrics. They show who is active and how much, so production needs a token.
func metricsHandler(cfg *config.Config) http.Handler {
	metricsPage := promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{})
	token := []byte(cfg.MetricsToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(token) == 0 {
			if !cfg.Development {
				http.Error(w, "set metrics_token to enable metrics", http.StatusForbidden)
				return
			}
		} else if subtle.ConstantTimeCompare([]byte(auth.TokenFromHeader(r.Header.Get("Authorization"))), token) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		metricsPage.ServeHTTP(w, r)
	})
}

// startControlPlane connects to the database and starts the REST API and background writers.
func startControlPlane(ctx context.Context, cfg *config.Config, static auth.KeyProvider, rooms *room.Manager, router routing.Router,
	mon *monitor.Monitor, mux *http.ServeMux, log *slog.Logger) (*store.KeyCache, *store.Store, error) {
	st, err := store.Open(ctx, cfg.ControlPlane.DatabaseURL, cfg.ControlPlane.MasterKey)
	if err != nil {
		return nil, nil, err
	}
	st.MaxIngressPerProject = cfg.Media.Ingress.MaxSessionsPerProject
	if password := cfg.ControlPlane.WorkerDBPassword; password != "" {
		if err := st.SetWorkerPassword(ctx, password); err != nil {
			return nil, nil, fmt.Errorf("could not set the media workers' database password: %w", err)
		}
	}
	keys := store.NewKeyCache(st, static)
	dispatcher := webhook.New(st, log, cfg.ControlPlane.AllowPrivateWebhooks)
	go dispatcher.Run(ctx)
	rec := recorder.New(st, mon, rooms, dispatcher, cfg, log)
	go rec.Run(ctx)

	api := routes.New(cfg, st, keys, rooms, mon, dispatcher, routes.NewMailer(cfg.ControlPlane.Mail, log), log)
	api.SetRouter(router)
	api.Register(mux)
	api.StartMedia(ctx, mux)
	rooms.SetProjectConfig(api.ProjectConfig)
	rooms.SetAdmission(api)
	log.Info("control plane enabled")
	return keys, st, nil
}

// withCORS allows browsers to call the HTTP endpoints from another origin. Clients need this for the
// validate endpoint, which they fetch from the page. Everything is token-authenticated rather than
// cookie-authenticated, so reflecting the origin grants nothing a page could not already do.
func withCORS(allowed []string, next http.Handler) http.Handler {
	isAllowed := func(origin string) bool {
		if len(allowed) == 0 {
			return true
		}
		for _, a := range allowed {
			if a == origin || a == "*" {
				return true
			}
		}
		return false
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && isAllowed(origin) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Twirp-Version, X-Project-ID")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
