package sip

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo"
	sipmsg "github.com/emiago/sipgo/sip"
	"github.com/pion/webrtc/v4"

	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/sip/media"
	"elnova-streamkit.local/sip/rules"
)

const (
	tokenLife      = 24 * time.Hour
	byeTimeout     = 8 * time.Second
	dbTimeout      = 5 * time.Second
	transferGrace  = time.Second
	watchEvery     = time.Second
	attrRinging    = "ringing"
	attrDialing    = "dialing"
	attrActive     = "active"
	attrHangup     = "hangup"
	attrAutomation = "automation"
)

// endInfo says how a call ended.
type endInfo struct {
	reason  string // A DisconnectReason name.
	message string
	failed  bool
	code    int
	status  string
	// noBye means the other side already knows, so saying goodbye would only get an error back.
	noBye bool
	// lost means this worker no longer owns the call, and has nothing more to say about it.
	lost bool
	// headers go on the BYE.
	headers map[string]string
}

// call is one phone call in either direction.
type call struct {
	w         *Worker
	log       *slog.Logger
	id        string
	project   string
	direction string
	started   time.Time

	// For an inbound call and an outbound one, respectively.
	server *sipgo.DialogServerSession
	client *sipgo.DialogClientSession

	// dialogID is guarded by w.mu.
	dialogID string

	session *media.Session
	bridge  *media.Bridge
	room    *roomClient
	policy  media.Policy
	trusted netip.Addr
	// negotiated is what the call last agreed on. Only the handler for offers changes it, after the call starts.
	negotiated *media.Negotiated
	localSDP   atomic.Pointer[[]byte]

	identity       string
	roomName       string
	attrsToHeaders map[string]string
	remoteHeaders  map[string]string
	media          *pb.SIPMediaConfig
	saved          atomic.Bool
	established    atomic.Bool
	acked          atomic.Bool

	ready     chan struct{}
	readyOnce sync.Once

	stop     chan struct{}
	stopOnce sync.Once
	finished chan struct{}
	endOnce  sync.Once

	outbound *store.SIPOutboundRequest

	transferMu sync.Mutex
	notifyMu   sync.Mutex
	notify     chan transferNotice
}

func newCall(w *Worker, id, project, direction string) *call {
	return &call{
		w: w, log: w.log.With("call", id), id: id, project: project, direction: direction, started: time.Now(),
		ready: make(chan struct{}), stop: make(chan struct{}), finished: make(chan struct{}),
	}
}

func (c *call) age() time.Duration { return time.Since(c.started) }

// persisted reports whether this call has a row a lease could be on yet.
func (c *call) persisted() bool { return c.saved.Load() }

func (c *call) markReady() { c.readyOnce.Do(func() { close(c.ready) }) }

func dbContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), dbTimeout)
}

func (c *call) update(u store.SIPCallUpdate) {
	ctx, cancel := dbContext()
	defer cancel()
	if _, err := c.w.opts.Store.UpdateSIPCall(ctx, c.id, c.w.id, u); err != nil {
		if errors.Is(err, store.ErrJobLost) {
			c.log.Debug("this call is no longer this worker's to report on")
			return
		}
		c.log.Warn("could not record a call's progress", "error", err)
	}
}

func (c *call) setStatus(attr string) {
	if c.room != nil {
		c.room.SetAttributes(map[string]string{rules.AttrCallStatus: attr})
	}
}

// joinRoom makes the participant. Its attributes are in its token, so they are there from the moment anyone can see it.
func (c *call) joinRoom(ctx context.Context, name, displayName, metadata string, attrs map[string]string, roomConfig *pb.RoomConfiguration) error {
	key, err := c.w.signingKey(ctx, c.project)
	if err != nil {
		return fmt.Errorf("no key to sign with: %w", err)
	}
	yes := true
	token, err := auth.Mint(key, &auth.Grants{
		Identity: c.identity, Name: displayName, Kind: "sip", Metadata: metadata, Attributes: attrs, RoomConfig: roomConfig,
		Video: &auth.VideoGrant{RoomJoin: true, Room: name, CanPublish: &yes, CanSubscribe: &yes, CanPublishData: &yes, CanUpdateOwnMetadata: &yes},
	}, tokenLife)
	if err != nil {
		return err
	}
	c.room = newRoomClient(c.log, c.w.wsURL, token, c.w.opts.Config.RTC.IncludeLoopback, roomEvents{
		Track:    c.onTrack,
		OtherSIP: c.markReady,
		Digit:    c.onRoomDigit,
		RPC:      c.onRPC,
		Ended:    c.onRoomEnded,
	})
	c.roomName = name
	if err := c.room.join(ctx); err != nil {
		return err
	}
	return nil
}

// onTrack decodes one person's audio into the mix. A track arriving is also what an inbound call waits for: it means someone can
// hear and be heard.
func (c *call) onTrack(_ string, track *webrtc.TrackRemote) {
	if track.Codec().MimeType != webrtc.MimeTypeOpus {
		c.log.Warn("skipping a track in a codec this cannot decode", "codec", track.Codec().MimeType)
		return
	}
	c.markReady()
	dec, err := media.NewOpusDecoder()
	if err != nil {
		c.log.Warn("could not start decoding a track", "error", err)
		return
	}
	defer dec.Close()
	input := c.bridge.Mixer().Add()
	defer c.bridge.Mixer().Remove(input)
	readTrack(track, func(payload []byte) {
		pcm, err := dec.Decode(payload)
		if err != nil {
			return
		}
		input.Write(pcm)
	})
}

func (c *call) onRoomDigit(digit string, code uint32) {
	if c.session == nil {
		return
	}
	// The digit is what counts. The code field is how the old API said it, so it only fills in if there is no digit.
	if digit == "" {
		if d := media.CodeDigit(uint8(code)); d != 0 {
			digit = string(d)
		}
	}
	if digit == "" || media.DigitCode(digit[0]) < 0 {
		return
	}
	c.session.QueueDTMF(digit[:1])
}

func (c *call) onPhoneDigit(digit byte) {
	if c.room != nil {
		c.room.SendDigit(string(digit), uint32(media.DigitCode(digit)))
	}
}

func (c *call) onRoomEnded(reason pb.DisconnectReason, err error) {
	message := ""
	if err != nil {
		message = err.Error()
	}
	c.hangup(endInfo{reason: reason.String(), message: message})
}

type rpcEndCallRequest struct {
	Headers map[string]string `json:"headers"`
}

type rpcGetHeadersRequest struct {
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
}

// onRPC answers the two methods a SIP participant has. Anyone in the room may call them, which is the same trust that lets them talk
// to the caller. Headers named by a caller still go through the same checks as any others.
func (c *call) onRPC(method, payload string) (string, string, uint32) {
	const unsupported, badRequest = 1400, 1500
	switch method {
	case rpcEndCall:
		var req rpcEndCallRequest
		if payload != "" {
			if err := json.Unmarshal([]byte(payload), &req); err != nil {
				return "", "the request could not be read", badRequest
			}
		}
		if err := rules.ValidateHeaders(req.Headers); err != nil {
			return "", err.Error(), badRequest
		}
		go c.hangup(endInfo{reason: "CLIENT_INITIATED", headers: req.Headers})
		return "{}", "", 0
	case rpcGetHeaders:
		var req rpcGetHeadersRequest
		if payload != "" {
			if err := json.Unmarshal([]byte(payload), &req); err != nil {
				return "", "the request could not be read", badRequest
			}
		}
		out := map[string]string{}
		for name, value := range c.remoteHeaders {
			if len(req.Include) > 0 && !containsFold(req.Include, name) {
				continue
			}
			if containsFold(req.Exclude, name) {
				continue
			}
			out[name] = value
		}
		data, err := json.Marshal(map[string]any{"headers": out})
		if err != nil {
			return "", "the answer could not be written", badRequest
		}
		return string(data), "", 0
	}
	return "", "Method not supported at destination", unsupported
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// watch ends a call that has gone quiet, or gone on too long. Either could otherwise hold a port and a project's slot for ever.
func (c *call) watch(limit time.Duration) {
	idleLimit := c.w.idle
	if t := c.media.GetMediaTimeout(); t != nil && t.AsDuration() > 0 {
		idleLimit = t.AsDuration()
	}
	initial := max(initialMediaIdle, idleLimit)
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	tick := time.NewTicker(watchEvery)
	defer tick.Stop()
	answered := time.Now()
	for {
		select {
		case <-c.finished:
			return
		case <-deadline.C:
			c.log.Info("the call reached its time limit")
			c.hangup(endInfo{reason: "USER_UNAVAILABLE", message: "The call reached its time limit."})
			return
		case <-tick.C:
			idle, expected := c.session.IdleFor()
			if !expected {
				continue
			}
			allowed := idleLimit
			if !c.session.GotPacket() {
				allowed = initial
				if !c.acked.Load() {
					// No ACK and no audio: an answer that was lost, most likely. Waiting the full time helps nobody.
					allowed = idleLimit
				}
				idle = time.Since(answered)
			}
			if idle > allowed {
				c.log.Info("no media from the phone, so ending the call", "idle", idle.String())
				c.hangup(endInfo{reason: "MEDIA_FAILURE", failed: true, message: "No media arrived from the phone."})
				return
			}
		}
	}
}

// hangup ends the call. The first call to this decides how it ended, and it does not come back until the call is cleaned up.
func (c *call) hangup(info endInfo) {
	c.stopOnce.Do(func() { close(c.stop) })
	c.endOnce.Do(func() { c.cleanup(info) })
	<-c.finished
}

func (c *call) cleanup(info endInfo) {
	defer close(c.finished)
	defer c.w.untrack(c)
	c.setStatus(attrHangup)
	if c.established.Load() && !info.noBye && !info.lost {
		// Before leaving the room, because attributes can name headers to send.
		c.sendBye(info.headers)
	}
	if c.bridge != nil {
		c.bridge.Close()
	}
	if c.session != nil {
		c.session.Close()
	}
	if c.room != nil {
		c.room.close()
	}
	if c.server != nil {
		_ = c.server.Close()
	}
	if c.client != nil {
		_ = c.client.Close()
	}
	if info.lost || !c.saved.Load() {
		return
	}
	status := store.SIPEnded
	level, event := "info", "sip_call_ended"
	if info.failed {
		status = store.SIPFailed
		level, event = "error", "sip_call_failed"
	}
	u := store.SIPCallUpdate{Status: status, Reason: info.reason, Error: info.message, SIPStatusCode: info.code, SIPStatus: info.status}
	ctx, cancel := dbContext()
	defer cancel()
	row, err := c.w.opts.Store.UpdateSIPCall(ctx, c.id, c.w.id, u)
	if err != nil {
		if !errors.Is(err, store.ErrJobLost) {
			c.log.Warn("could not record the end of a call", "error", err)
		}
	} else if err := c.w.opts.Store.RecordSIPEvent(ctx, row, level, event, firstOf(info.message, "The call ended.")); err != nil {
		c.log.Warn("could not record a sip event", "error", err)
	}
	if err := c.w.opts.Store.FailSIPTransfersOfCall(ctx, c.id); err != nil {
		c.log.Warn("could not close a call's transfers", "error", err)
	}
	c.log.Info("call ended", "reason", info.reason, "failed", info.failed, "code", info.code)
}

func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (c *call) sendBye(extra map[string]string) {
	ctx, cancel := context.WithTimeout(context.Background(), byeTimeout)
	defer cancel()
	var err error
	switch {
	case c.server != nil:
		contact := c.server.InviteRequest.Contact()
		if contact == nil {
			return
		}
		bye := sipmsg.NewRequest(sipmsg.BYE, *contact.Address.Clone())
		bye.SetTransport(c.server.InviteRequest.Transport())
		c.addCallHeaders(bye, extra)
		err = c.server.WriteBye(ctx, bye)
	case c.client != nil:
		res := c.client.InviteResponse
		req := c.client.InviteRequest
		if res == nil {
			return
		}
		target := req.Recipient
		if contact := res.Contact(); contact != nil {
			target = contact.Address
		}
		bye := sipmsg.NewRequest(sipmsg.BYE, *target.Clone())
		bye.SetTransport(req.Transport())
		c.addCallHeaders(bye, extra)
		err = c.client.WriteBye(ctx, bye)
	}
	if err != nil {
		c.log.Debug("the BYE did not get a clean answer", "error", err)
	}
}

// addCallHeaders puts on a request the headers that attributes map to, and any others given.
func (c *call) addCallHeaders(req *sipmsg.Request, extra map[string]string) {
	if c.room != nil && len(c.attrsToHeaders) > 0 {
		attrs := c.room.Attributes()
		for attr, header := range c.attrsToHeaders {
			if v, ok := attrs[attr]; ok && rules.ValidateHeaderValue(header, v) == nil {
				req.AppendHeader(sipmsg.NewHeader(header, v))
			}
		}
	}
	for k, v := range extra {
		req.AppendHeader(sipmsg.NewHeader(k, v))
	}
}

func (c *call) gotAck(req *sipmsg.Request, tx sipmsg.ServerTransaction) {
	if c.server == nil {
		return
	}
	if err := c.server.ReadAck(req, tx); err != nil {
		c.log.Debug("an ACK did not match", "error", err)
		return
	}
	c.acked.Store(true)
}

// gotBye is the phone hanging up.
func (c *call) gotBye(req *sipmsg.Request, tx sipmsg.ServerTransaction) {
	var err error
	switch {
	case c.server != nil:
		err = c.server.ReadBye(req, tx)
	case c.client != nil:
		err = c.client.ReadBye(req, tx)
	}
	if err != nil {
		c.log.Debug("a BYE could not be answered", "error", err)
	}
	c.established.Store(false)
	go c.hangup(endInfo{reason: "CLIENT_INITIATED", noBye: true})
}

// gotOffer handles a re-INVITE or an UPDATE: new media details for the call, such as going on hold. The answer always says the same
// codec, since switching in mid-call has no benefit here.
func (c *call) gotOffer(req *sipmsg.Request, tx sipmsg.ServerTransaction) {
	body := req.Body()
	if len(body) == 0 {
		// A session refresh, with nothing to change. A re-INVITE must still carry something to answer with.
		if req.IsInvite() {
			if sdp := c.localSDP.Load(); sdp != nil {
				c.respondOffer(req, tx, *sdp)
				return
			}
		}
		c.w.respond(req, tx, 200, "OK")
		return
	}
	offer, err := media.ParseDescription(body)
	if err != nil {
		c.w.respond(req, tx, 488, media.ErrInvalidSDP.Error())
		return
	}
	policy := c.policy
	policy.Codecs = []string{c.session.Codec().Name}
	negotiated, err := policy.Answer(offer)
	if err != nil {
		c.w.respond(req, tx, 488, err.Error())
		return
	}
	// Going on hold is not a change of keys. A peer that keeps its key expects the same stream to carry on, with its counters,
	// and it gets the same key back from this side.
	negotiated.KeepKeys(c.negotiated)
	c.negotiated = negotiated
	if err := c.session.Configure(offer, c.trusted, negotiated); err != nil {
		c.w.respond(req, tx, 488, media.ErrNoCrypto.Error())
		return
	}
	answer := media.BuildAnswer(c.w.mediaIP, c.session.Port(), offer, negotiated, media.MirrorDirection(offer.Direction))
	c.localSDP.Store(&answer)
	c.respondOffer(req, tx, answer)
}

func (c *call) respondOffer(req *sipmsg.Request, tx sipmsg.ServerTransaction, sdp []byte) {
	res := sipmsg.NewSDPResponseFromRequest(req, sdp)
	contact := c.w.contact(transportOf(req))
	res.AppendHeader(&contact)
	if err := tx.Respond(res); err != nil {
		c.log.Debug("could not answer an offer", "error", err)
	}
}

// gotInfo takes a digit sent as SIP INFO, which some equipment does instead of RFC 4733.
func (c *call) gotInfo(req *sipmsg.Request, tx sipmsg.ServerTransaction) {
	c.w.respond(req, tx, 200, "OK")
	ct := req.ContentType()
	if ct == nil || !strings.EqualFold(ct.Value(), "application/dtmf-relay") {
		return
	}
	for _, line := range strings.Split(string(req.Body()), "\n") {
		name, value, ok := strings.Cut(line, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "signal") {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) == 1 && media.DigitCode(value[0]) >= 0 {
			c.onPhoneDigit(value[0])
		}
		return
	}
}

// parseTrusted reads the address signalling came from.
func parseTrusted(source string) netip.Addr { return parseSource(source) }

func statusFromResponse(res *sipmsg.Response) (int, string) {
	if res == nil {
		return 0, ""
	}
	return int(res.StatusCode), res.Reason
}

// itoa is short for what it says, for the attribute with the time of the INVITE.
func itoa(n int64) string { return strconv.FormatInt(n, 10) }
