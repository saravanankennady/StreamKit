package sip

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"

	"github.com/emiago/sipgo"
	sipmsg "github.com/emiago/sipgo/sip"
)

const allowedMethods = "INVITE, ACK, CANCEL, BYE, OPTIONS, NOTIFY, UPDATE, INFO"

// startSignalling opens the SIP ports. The guard reads every message before anything parses it.
func (w *Worker) startSignalling(ctx context.Context) error {
	quiet := slog.New(w.opts.Log.Handler()).With("component", "sipstack")
	filter := func(info sipmsg.TransportReadProps, data []byte) ([]byte, error) {
		ip := parseSource(info.RemoteAddr.String())
		if !ip.IsValid() || !w.guard.allow(ip, time.Now()) {
			return nil, nil
		}
		return data, nil
	}
	options := []sipgo.UserAgentOption{
		sipgo.WithUserAgent("StreamKit"),
		sipgo.WithUserAgentHostname(w.host),
		sipgo.WithUserAgentTransactionLayerOptions(sipmsg.WithTransactionLayerLogger(quiet)),
		sipgo.WithUserAgentTransportLayerOptions(sipmsg.WithTransportLayerLogger(quiet), sipmsg.WithTransportLayerReadFilter(filter)),
	}
	if w.conf.TLSCAFile != "" {
		pem, err := os.ReadFile(w.conf.TLSCAFile)
		if err != nil {
			return fmt.Errorf("media.sip.tls_ca_file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return errors.New("media.sip.tls_ca_file holds no certificates")
		}
		options = append(options, sipgo.WithUserAgenTLSConfig(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}))
	}
	ua, err := sipgo.NewUA(options...)
	if err != nil {
		return err
	}
	w.ua = ua
	if w.server, err = sipgo.NewServer(ua, sipgo.WithServerLogger(quiet)); err != nil {
		return err
	}
	if w.client, err = sipgo.NewClient(ua, sipgo.WithClientLogger(quiet), sipgo.WithClientHostname(w.host)); err != nil {
		return err
	}
	w.server.OnInvite(w.onInvite)
	w.server.OnAck(w.onAck)
	w.server.OnBye(w.onBye)
	w.server.OnOptions(w.onOptions)
	w.server.OnNotify(w.onNotify)
	w.server.OnUpdate(w.onUpdate)
	w.server.OnInfo(w.onInfo)
	w.server.OnCancel(w.onStrayCancel)
	w.server.OnNoRoute(w.onOther)

	errs := make(chan error, 3)
	started := false
	if w.enabled(w.conf.UDPAddr) {
		conn, err := net.ListenPacket("udp", w.conf.UDPAddr)
		if err != nil {
			return fmt.Errorf("SIP over UDP: %w", err)
		}
		w.listenPorts["udp"] = portOf(conn.LocalAddr())
		go func() { errs <- w.server.ServeUDP(conn) }()
		go func() { <-ctx.Done(); _ = conn.Close() }()
		started = true
	}
	if w.enabled(w.conf.TCPAddr) {
		l, err := net.Listen("tcp", w.conf.TCPAddr)
		if err != nil {
			return fmt.Errorf("SIP over TCP: %w", err)
		}
		w.listenPorts["tcp"] = portOf(l.Addr())
		go func() { errs <- w.server.ServeTCP(newLimitListener(l, w.guard)) }()
		go func() { <-ctx.Done(); _ = l.Close() }()
		started = true
	}
	if w.conf.TLSAddr != "" {
		cert, err := tls.LoadX509KeyPair(w.conf.TLSCertFile, w.conf.TLSKeyFile)
		if err != nil {
			return fmt.Errorf("SIP over TLS: %w", err)
		}
		l, err := net.Listen("tcp", w.conf.TLSAddr)
		if err != nil {
			return fmt.Errorf("SIP over TLS: %w", err)
		}
		w.listenPorts["tls"] = portOf(l.Addr())
		tl := tls.NewListener(newLimitListener(l, w.guard), &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
		go func() { errs <- w.server.ServeTLS(tl) }()
		go func() { <-ctx.Done(); _ = l.Close() }()
		started = true
	}
	if !started {
		return fmt.Errorf("no SIP transport is on: set media.sip.udp_addr, tcp_addr or tls_addr")
	}
	// A listener that fails right away is a real problem to report. One that stops later is ctx ending.
	go func() {
		for err := range errs {
			if ctx.Err() == nil && err != nil {
				w.log.Error("a SIP listener stopped", "error", err)
			}
		}
	}()
	return nil
}

// respond answers a request, logging only if that fails.
func (w *Worker) respond(req *sipmsg.Request, tx sipmsg.ServerTransaction, code int, reason string, headers ...sipmsg.Header) {
	res := sipmsg.NewResponseFromRequest(req, code, reason, nil)
	for _, h := range headers {
		res.AppendHeader(h)
	}
	if err := tx.Respond(res); err != nil {
		w.log.Debug("could not answer a request", "error", err, "code", code)
	}
}

// refuse turns away someone who has no business here. With silent_rejects on, they get no answer at all, and either way it counts
// towards getting them banned.
func (w *Worker) refuse(req *sipmsg.Request, tx sipmsg.ServerTransaction, code int, reason string) {
	ip := parseSource(req.Source())
	w.log.Debug("refused a request", "source", ip.String(), "method", req.Method.String(), "code", code)
	w.guard.failed(ip, time.Now())
	if w.conf.SilentRejects && !w.guard.isKnown(ip) {
		return
	}
	w.respond(req, tx, code, reason)
}

func userAgentOf(req *sipmsg.Request) string {
	if h := req.GetHeader("User-Agent"); h != nil {
		return h.Value()
	}
	return ""
}

func hasToTag(req *sipmsg.Request) bool {
	to := req.To()
	return to != nil && to.Params != nil && to.Params.Has("tag")
}

// dialogFor finds the call an in-dialog request belongs to.
func (w *Worker) dialogFor(req *sipmsg.Request) *call {
	if id, err := sipmsg.DialogIDFromRequestUAS(req); err == nil {
		if c := w.callForDialog(id); c != nil {
			return c
		}
	}
	if id, err := sipmsg.DialogIDFromRequestUAC(req); err == nil {
		if c := w.callForDialog(id); c != nil {
			return c
		}
	}
	return nil
}

func (w *Worker) onAck(req *sipmsg.Request, tx sipmsg.ServerTransaction) {
	if c := w.dialogFor(req); c != nil {
		c.gotAck(req, tx)
	}
}

func (w *Worker) onBye(req *sipmsg.Request, tx sipmsg.ServerTransaction) {
	c := w.dialogFor(req)
	if c == nil {
		w.refuse(req, tx, 481, "Call/Transaction Does Not Exist")
		return
	}
	c.gotBye(req, tx)
}

func (w *Worker) onOptions(req *sipmsg.Request, tx sipmsg.ServerTransaction) {
	if c := w.dialogFor(req); c == nil {
		if isScanner(userAgentOf(req)) {
			return
		}
		// A carrier's health check is the only reason for an OPTIONS from outside a call, and a scan is the most common one.
		if w.conf.SilentRejects && !w.guard.isKnown(parseSource(req.Source())) {
			return
		}
	}
	w.respond(req, tx, 200, "OK", sipmsg.NewHeader("Allow", allowedMethods), sipmsg.NewHeader("Accept", "application/sdp"))
}

func (w *Worker) onNotify(req *sipmsg.Request, tx sipmsg.ServerTransaction) {
	c := w.dialogFor(req)
	if c == nil {
		w.refuse(req, tx, 481, "Call/Transaction Does Not Exist")
		return
	}
	c.gotNotify(req, tx)
}

func (w *Worker) onUpdate(req *sipmsg.Request, tx sipmsg.ServerTransaction) {
	c := w.dialogFor(req)
	if c == nil {
		w.refuse(req, tx, 481, "Call/Transaction Does Not Exist")
		return
	}
	c.gotOffer(req, tx)
}

func (w *Worker) onInfo(req *sipmsg.Request, tx sipmsg.ServerTransaction) {
	c := w.dialogFor(req)
	if c == nil {
		w.refuse(req, tx, 481, "Call/Transaction Does Not Exist")
		return
	}
	c.gotInfo(req, tx)
}

// onStrayCancel handles a CANCEL with no INVITE to go with it. The transaction layer deals with the real ones.
func (w *Worker) onStrayCancel(req *sipmsg.Request, tx sipmsg.ServerTransaction) {
	w.refuse(req, tx, 481, "Call/Transaction Does Not Exist")
}

// onOther refuses everything else. There is no registrar here, and a REFER from a peer gets this too: it would ask this service to
// place a call for someone on the internet, and that is what an open relay is.
func (w *Worker) onOther(req *sipmsg.Request, tx sipmsg.ServerTransaction) {
	if tx == nil || isScanner(userAgentOf(req)) {
		return
	}
	ip := parseSource(req.Source())
	w.guard.failed(ip, time.Now())
	if w.conf.SilentRejects && !w.guard.isKnown(ip) {
		return
	}
	w.respond(req, tx, 405, "Method Not Allowed", sipmsg.NewHeader("Allow", allowedMethods))
}

// transportOf gives a request's transport in lower case.
func transportOf(req *sipmsg.Request) string { return strings.ToLower(req.Transport()) }
