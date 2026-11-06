// Package sip connects phone calls to rooms. A worker answers INVITEs that its trunks allow and places the calls the API queues,
// and joins each call to its room as an ordinary participant, so the SFU knows nothing about telephony. Everything it needs from the
// control plane is in the database, the way recording and ingest work: trunks and rules to read, and calls held by a lease.
package sip

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo"
	sipmsg "github.com/emiago/sipgo/sip"

	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/egress/heartbeat"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/config"
	"elnova-streamkit.local/server/participant"
	"elnova-streamkit.local/sip/media"
)

const (
	claimInterval      = 250 * time.Millisecond
	leaseEvery         = time.Second
	defaultLease       = 30 * time.Second
	defaultMaxCallTime = 24 * time.Hour
	defaultRinging     = 180 * time.Second
	defaultMediaIdle   = 15 * time.Second
	initialMediaIdle   = 30 * time.Second
	defaultDrain       = 60 * time.Second
	defaultCallsPerCPU = 25
	defaultProjectCall = 50
	defaultRTPMin      = 10000
	defaultRTPMax      = 20000
	defaultSIPPort     = ":5060"
	addrOff            = "off"
)

// Options configures a Worker.
type Options struct {
	Config     *config.Config
	Store      *store.Store
	Log        *slog.Logger
	StaticKeys map[string]string
}

// Worker runs calls.
type Worker struct {
	opts     Options
	conf     config.SIPConf
	log      *slog.Logger
	id       string
	wsURL    string
	maxCalls int
	limit    int
	lease    time.Duration
	maxCall  time.Duration
	ringing  time.Duration
	idle     time.Duration
	drain    time.Duration
	host     string
	mediaIP  netip.Addr
	ports    *media.Ports
	guard    *guard
	digest   *digestAuth
	reporter *heartbeat.Reporter

	ua     *sipgo.UserAgent
	server *sipgo.Server
	client *sipgo.Client

	listenPorts map[string]int
	stopping    atomic.Bool
	active      atomic.Int32
	calls       sync.WaitGroup

	mu       sync.Mutex
	running  map[string]*call
	byDialog map[string]*call
}

func hostOf(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return host
}

func (w *Worker) enabled(addr string) bool { return addr != "" && addr != addrOff }

// New checks the configuration, and sets up everything a call needs.
func New(opts Options) (*Worker, error) {
	conf := opts.Config.Media.SIP
	if conf.UDPAddr == "" {
		conf.UDPAddr = defaultSIPPort
	}
	if conf.TCPAddr == "" {
		conf.TCPAddr = defaultSIPPort
	}
	w := &Worker{
		opts: opts, conf: conf, log: opts.Log.With("service", "sip"), id: participant.NewID("SW_"),
		running: map[string]*call{}, byDialog: map[string]*call{}, listenPorts: map[string]int{},
		lease: time.Duration(conf.LeaseSeconds) * time.Second, maxCalls: conf.MaxCalls, limit: conf.MaxCallsPerProject,
		maxCall: time.Duration(conf.MaxCallSeconds) * time.Second, ringing: time.Duration(conf.RingingSeconds) * time.Second,
		idle: time.Duration(conf.MediaTimeoutSeconds) * time.Second, drain: time.Duration(conf.DrainSeconds) * time.Second,
	}
	if w.lease <= 0 {
		w.lease = defaultLease
	}
	if w.maxCalls <= 0 {
		w.maxCalls = defaultCallsPerCPU * runtime.NumCPU()
	}
	if w.limit <= 0 {
		w.limit = defaultProjectCall
	}
	if w.maxCall <= 0 {
		w.maxCall = defaultMaxCallTime
	}
	if w.ringing <= 0 {
		w.ringing = defaultRinging
	}
	if w.idle <= 0 {
		w.idle = defaultMediaIdle
	}
	if w.drain <= 0 {
		w.drain = defaultDrain
	}
	w.wsURL = opts.Config.Media.WSURL
	if w.wsURL == "" {
		w.wsURL = opts.Config.ControlPlane.PublicWSURL
	}
	if w.wsURL == "" {
		return nil, errors.New("workers need the server's address: set media.ws_url")
	}
	w.host = conf.PublicHost
	listenHost := ""
	if w.enabled(conf.UDPAddr) {
		listenHost = hostOf(conf.UDPAddr)
	} else if w.enabled(conf.TCPAddr) {
		listenHost = hostOf(conf.TCPAddr)
	}
	var bindIP net.IP
	if ip := net.ParseIP(listenHost); ip != nil && !ip.IsUnspecified() {
		bindIP = ip
		if w.host == "" {
			w.host = listenHost
		}
	}
	if w.host == "" {
		return nil, errors.New("calls need an address to give peers: set media.sip.public_host")
	}
	mediaIP := conf.MediaIP
	if mediaIP == "" {
		mediaIP = w.host
	}
	ip, err := netip.ParseAddr(mediaIP)
	if err != nil {
		return nil, errors.New("media needs an IP address to give peers: set media.sip.media_ip")
	}
	w.mediaIP = ip.Unmap()
	minPort, maxPort := conf.RTPPortMin, conf.RTPPortMax
	if minPort <= 0 {
		minPort = defaultRTPMin
	}
	if maxPort <= 0 {
		maxPort = defaultRTPMax
	}
	if maxPort <= minPort {
		return nil, errors.New("media.sip.rtp_port_max must be above media.sip.rtp_port_min")
	}
	w.ports = media.NewPorts(bindIP, minPort, maxPort)
	if (conf.TLSAddr != "") != (conf.TLSCertFile != "" && conf.TLSKeyFile != "") {
		return nil, errors.New("SIP over TLS needs media.sip.tls_addr, tls_cert_file and tls_key_file together")
	}
	w.guard = newGuard(conf.InvitesPerSecond, conf.InviteBurst, conf.BanSeconds, conf.BanAfter)
	w.digest = newDigestAuth(opts.Config.ControlPlane.MasterKey)
	w.reporter = &heartbeat.Reporter{
		Store: opts.Store, Log: w.log, NodeID: w.id, Kind: store.WorkerKindSIP, Region: opts.Config.Region, Address: "sip:" + w.host, MaxJobs: w.maxCalls,
		Active: func() int { return int(w.active.Load()) },
		Check:  w.checkHealth,
	}
	return w, nil
}

// checkHealth is what a call would need: a free media port, and the database.
func (w *Worker) checkHealth(ctx context.Context) error {
	s, err := w.ports.Open(w.log)
	if err != nil {
		return fmt.Errorf("no media ports: %w", err)
	}
	s.Close()
	return w.opts.Store.DB.Ping(ctx)
}

func (w *Worker) hasRoom() bool {
	return int(w.active.Load()) < w.maxCalls && !w.stopping.Load() && !w.reporter.Draining() && w.reporter.Healthy()
}

func (w *Worker) track(c *call) {
	w.active.Add(1)
	w.calls.Add(1)
	w.mu.Lock()
	w.running[c.id] = c
	w.mu.Unlock()
}

func (w *Worker) untrack(c *call) {
	w.mu.Lock()
	delete(w.running, c.id)
	if c.dialogID != "" {
		delete(w.byDialog, c.dialogID)
	}
	w.mu.Unlock()
	w.active.Add(-1)
	w.calls.Done()
}

func (w *Worker) registerDialog(id string, c *call) {
	w.mu.Lock()
	c.dialogID = id
	w.byDialog[id] = c
	w.mu.Unlock()
}

func (w *Worker) callForDialog(id string) *call {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.byDialog[id]
}

func (w *Worker) snapshot() []*call {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]*call, 0, len(w.running))
	for _, c := range w.running {
		out = append(out, c)
	}
	return out
}

// Run serves until ctx ends. Calls then get the time they are given to finish by themselves, and any left after that are hung up, so
// a stopping worker ends calls cleanly rather than dropping them.
func (w *Worker) Run(ctx context.Context) error {
	reportCtx, stopReport := context.WithCancel(context.Background())
	defer stopReport()
	listenCtx, stopListening := context.WithCancel(context.Background())
	defer stopListening()
	if err := w.startSignalling(listenCtx); err != nil {
		return err
	}
	defer func() { _ = w.ua.Close() }()

	var background sync.WaitGroup
	background.Add(3)
	go func() { defer background.Done(); w.reporter.Run(reportCtx) }()
	go func() { defer background.Done(); heartbeat.Reap(reportCtx, w.opts.Store, w.log) }()
	go func() {
		defer background.Done()
		w.guard.refreshKnown(reportCtx, w.opts.Store.SIPAllowedSources, func(err error) { w.log.Warn("could not load trunk allow-lists", "error", err) })
	}()
	w.log.Info("sip worker started", "worker", w.id, "udp", w.conf.UDPAddr, "tcp", w.conf.TCPAddr, "tls", w.conf.TLSAddr, "host", w.host, "maxCalls", w.maxCalls)

	claim := time.NewTicker(claimInterval)
	defer claim.Stop()
	lease := time.NewTicker(leaseEvery)
	defer lease.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-claim.C:
			w.claimCalls(ctx)
		case <-lease.C:
			w.renew(reportCtx)
		}
	}

	w.stopping.Store(true)
	w.reporter.Leave(reportCtx)
	w.log.Info("sip worker draining", "calls", w.active.Load(), "for", w.drain.String())
	finished := make(chan struct{})
	go func() { w.calls.Wait(); close(finished) }()
	timer := time.NewTimer(w.drain)
	defer timer.Stop()
	keepRenewing := time.NewTicker(leaseEvery)
	defer keepRenewing.Stop()
wait:
	for {
		select {
		case <-finished:
			break wait
		case <-keepRenewing.C:
			w.renew(reportCtx)
		case <-timer.C:
			for _, c := range w.snapshot() {
				c.hangup(endInfo{reason: "SERVER_SHUTDOWN", message: "The worker is shutting down."})
			}
			<-finished
			break wait
		}
	}
	stopListening()
	stopReport()
	background.Wait()
	w.log.Info("sip worker stopped")
	return nil
}

func (w *Worker) claimCalls(ctx context.Context) {
	for w.hasRoom() {
		row, req, err := w.opts.Store.ClaimSIPCall(ctx, w.id, w.lease)
		if err != nil {
			if ctx.Err() == nil {
				w.log.Warn("could not claim a call", "error", err)
			}
			if row != nil {
				// It was claimed, but its request could not be read.
				w.failUnstarted(row, "The call's request could not be read.")
			}
			return
		}
		if row == nil {
			return
		}
		c := newOutboundCall(w, row, req)
		w.track(c)
		go c.run()
	}
}

func (w *Worker) failUnstarted(row *store.SIPCall, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := w.opts.Store.UpdateSIPCall(ctx, row.ID, w.id, store.SIPCallUpdate{Status: store.SIPFailed, Error: message}); err != nil {
		w.log.Warn("could not record a failed call", "error", err, "call", row.ID)
	}
}

// renew holds this worker's leases, and picks up what the API has asked for. One statement covers all its calls, so this costs the
// database the same whatever the number of calls.
func (w *Worker) renew(ctx context.Context) {
	opCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	leases, err := w.opts.Store.RenewSIPCalls(opCtx, w.id, w.lease)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("could not renew call leases", "error", err)
		}
		return
	}
	held := make(map[string]bool, len(leases))
	for _, l := range leases {
		held[l.ID] = true
		if l.Stop {
			w.mu.Lock()
			c := w.running[l.ID]
			w.mu.Unlock()
			if c != nil {
				go c.hangup(endInfo{reason: "CLIENT_INITIATED"})
			}
		}
	}
	for _, c := range w.snapshot() {
		// A call that started after the statement ran is not missing. It has not been looked at yet.
		if !held[c.id] && c.age() > 2*leaseEvery && c.persisted() {
			w.log.Warn("this worker no longer owns a call, so it is ending it", "call", c.id)
			go c.hangup(endInfo{reason: "SERVER_SHUTDOWN", lost: true})
		}
	}
	transfers, err := w.opts.Store.TakeSIPTransfers(opCtx, w.id)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("could not check for transfers", "error", err)
		}
		return
	}
	for _, t := range transfers {
		w.mu.Lock()
		c := w.running[t.CallID]
		w.mu.Unlock()
		if c == nil {
			if err := w.opts.Store.FinishSIPTransfer(opCtx, t.ID, false, "STR_CALL_ENDED", 0, "", "The call ended before the transfer finished."); err != nil {
				w.log.Warn("could not record a transfer", "error", err)
			}
			continue
		}
		go c.transfer(t)
	}
}

// signingKey finds the key for a participant's access token.
func (w *Worker) signingKey(ctx context.Context, projectID string) (auth.Key, error) {
	for key, secret := range w.opts.StaticKeys {
		if projectID == "default" {
			return auth.Key{APIKey: key, Secret: secret, ProjectID: projectID}, nil
		}
	}
	return w.opts.Store.MediaSigningKey(ctx, projectID)
}

// contact is this worker's address for a transport.
func (w *Worker) contact(transport string) sipmsg.ContactHeader {
	port := w.listenPorts[transport]
	uri := sipmsg.Uri{Scheme: "sip", Host: w.host, Port: port}
	if transport != "udp" {
		uri.UriParams = sipmsg.HeaderParams{{K: "transport", V: transport}}
	}
	return sipmsg.ContactHeader{Address: uri}
}

// portOf reads the port from a listen address.
func portOf(addr net.Addr) int {
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(port)
	return n
}
