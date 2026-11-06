package sip

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/emiago/sipgo"
	sipmsg "github.com/emiago/sipgo/sip"

	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/participant"
	"elnova-streamkit.local/sip/media"
	"elnova-streamkit.local/sip/rules"
)

const (
	ringEvery      = time.Second
	pinTimeout     = 30 * time.Second
	pinMaxDigits   = 16
	lookupTimeout  = 5 * time.Second
	retryAfterSecs = "30"
)

// policyFor turns a media configuration into what negotiation allows. Only G.711 is available, so a configuration that lists other
// codecs, and only those, can match nothing, and the call is refused rather than quietly using something else.
func policyFor(m *pb.SIPMediaConfig) media.Policy {
	p := media.DefaultPolicy()
	switch rules.Encryption(m) {
	case pb.SIPMediaEncryption_SIP_MEDIA_ENCRYPT_ALLOW:
		p.Encryption = media.EncryptAllow
	case pb.SIPMediaEncryption_SIP_MEDIA_ENCRYPT_REQUIRE:
		p.Encryption = media.EncryptRequire
	}
	if m.GetOnlyListedCodecs() {
		var listed []string
		for _, want := range m.GetCodecs() {
			for _, have := range p.Codecs {
				if strings.EqualFold(want.GetName(), have) {
					listed = append(listed, have)
				}
			}
		}
		p.Codecs = listed
	}
	return p
}

// onInvite is every INVITE. It does what is cheap first, and nothing that costs anything comes before knowing who this is.
func (w *Worker) onInvite(req *sipmsg.Request, tx sipmsg.ServerTransaction) {
	if hasToTag(req) {
		c := w.dialogFor(req)
		if c == nil {
			w.refuse(req, tx, 481, "Call/Transaction Does Not Exist")
			return
		}
		c.gotOffer(req, tx)
		return
	}
	if isScanner(userAgentOf(req)) {
		w.guard.failed(parseSource(req.Source()), time.Now())
		return
	}
	if !w.hasRoom() {
		w.respond(req, tx, 503, "Service Unavailable", sipmsg.NewHeader("Retry-After", retryAfterSecs))
		return
	}
	if mf := req.MaxForwards(); mf != nil && mf.Val() == 0 {
		w.respond(req, tx, 483, "Too Many Hops")
		return
	}
	if h := req.GetHeader("Require"); h != nil && strings.TrimSpace(h.Value()) != "" {
		w.respond(req, tx, 420, "Bad Extension", sipmsg.NewHeader("Unsupported", h.Value()))
		return
	}
	from, to := req.From(), req.To()
	if from == nil || to == nil || req.CallID() == nil || req.Contact() == nil {
		w.refuse(req, tx, 400, "Bad Request")
		return
	}
	source := parseSource(req.Source())
	info := rules.Call{FromUser: from.Address.User, FromHost: from.Address.Host, ToUser: to.Address.User, ToHost: to.Address.Host, SourceIP: source.String()}

	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	trunk, project, ok := w.findTrunk(ctx, req, tx, info)
	if !ok {
		return
	}
	if !w.authorize(req, tx, trunk) {
		return
	}

	info.ID = participant.NewID("SCL_")
	list, err := w.opts.Store.ListDispatchRules(ctx, project, true)
	if err != nil {
		w.log.Warn("could not read dispatch rules", "error", err)
		w.respond(req, tx, 503, "Service Unavailable", sipmsg.NewHeader("Retry-After", retryAfterSecs))
		return
	}
	rule, err := rules.MatchRule(trunk, list, info, "", false)
	if err != nil {
		if !errors.Is(err, rules.ErrNoRule) {
			w.log.Warn("this project's dispatch rules conflict", "project", project, "error", err)
		}
		w.refuse(req, tx, 404, "Not Found")
		return
	}
	dispatch, err := rules.Evaluate(trunk, rule, info, "")
	if err != nil {
		w.log.Warn("could not apply a dispatch rule", "error", err, "rule", rule.GetSipDispatchRuleId())
		w.respond(req, tx, 500, "Server Internal Error")
		return
	}

	body := req.Body()
	if ct := req.ContentType(); len(body) == 0 || ct == nil || !strings.HasPrefix(strings.ToLower(ct.Value()), "application/sdp") {
		// An INVITE with no offer is legal, but nothing here waits for an offer in the ACK.
		w.respond(req, tx, 488, media.ErrInvalidSDP.Error())
		return
	}
	offer, err := media.ParseDescription(body)
	if err != nil {
		w.respond(req, tx, 488, firstLine(err))
		return
	}
	policy := policyFor(dispatch.Media)
	negotiated, err := policy.Answer(offer)
	if err != nil {
		w.respond(req, tx, 488, err.Error())
		return
	}

	c := newCall(w, info.ID, project, store.SIPInbound)
	c.media, c.policy, c.trusted = dispatch.Media, policy, source
	session, err := w.ports.Open(c.log)
	if err != nil {
		w.log.Warn("no media ports for a call", "error", err)
		w.respond(req, tx, 503, "Service Unavailable", sipmsg.NewHeader("Retry-After", retryAfterSecs))
		return
	}
	c.session = session
	if c.bridge, err = media.NewBridge(session); err != nil {
		session.Close()
		w.log.Warn("could not set up media", "error", err)
		w.respond(req, tx, 503, "Service Unavailable")
		return
	}
	if err := session.Configure(offer, source, negotiated); err != nil {
		c.bridge.Close()
		session.Close()
		w.respond(req, tx, 488, media.ErrNoCrypto.Error())
		return
	}
	session.SetDigitHandler(c.onPhoneDigit)
	c.negotiated = negotiated
	answer := media.BuildAnswer(w.mediaIP, session.Port(), offer, negotiated, media.MirrorDirection(offer.Direction))
	c.localSDP.Store(&answer)

	row := &store.SIPCall{
		ID: c.id, ProjectID: project, TrunkID: dispatch.TrunkID, FromHost: info.FromHost, ToHost: info.ToHost,
		FromUser: dispatch.FromUser, ToUser: dispatch.ToUser, SIPCallID: string(*req.CallID()),
	}
	if err := w.opts.Store.CreateInboundSIPCall(ctx, row, w.id, w.lease, w.limit); err != nil {
		c.bridge.Close()
		session.Close()
		if !errors.Is(err, store.ErrSIPCallLimit) {
			w.log.Warn("could not record a call", "error", err)
		}
		w.respond(req, tx, 503, "Service Unavailable", sipmsg.NewHeader("Retry-After", retryAfterSecs))
		return
	}
	c.saved.Store(true)

	ua := sipgo.DialogUA{Client: w.client, ContactHDR: w.contact(transportOf(req)), RewriteContact: contactUnroutable(req, source)}
	dialog, err := ua.ReadInvite(req, tx)
	if err != nil {
		c.bridge.Close()
		session.Close()
		c.update(store.SIPCallUpdate{Status: store.SIPFailed, Error: "The INVITE could not be read."})
		w.respond(req, tx, 400, "Bad Request")
		return
	}
	c.server = dialog
	w.registerDialog(dialog.ID, c)
	w.track(c)
	c.update(store.SIPCallUpdate{AudioCodec: negotiated.Codec.Name, MediaEncryption: encryptionName(negotiated)})
	c.answerInbound(req, trunk, list, dispatch, info, answer)
}

func firstLine(err error) string {
	msg := err.Error()
	if i := strings.IndexAny(msg, ":\r\n"); i > 0 {
		return msg[:i]
	}
	return msg
}

func encryptionName(n *media.Negotiated) string {
	if n.Local != nil {
		return "SRTP " + n.Local.Suite.Name
	}
	return ""
}

// contactUnroutable reports a Contact that cannot be what it says, such as a private address from a peer on a public one. Requests in
// the call then go back to where this came from.
func contactUnroutable(req *sipmsg.Request, source interface {
	IsPrivate() bool
	IsLoopback() bool
}) bool {
	contact := req.Contact()
	if contact == nil || source.IsPrivate() || source.IsLoopback() {
		return false
	}
	ip := parseSource(contact.Address.Host)
	return ip.IsValid() && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast())
}

// findTrunk works out which trunk, and so which project, a call belongs to. A call that belongs to none gets no further, however
// clever its dispatch rules would have been. The reference lets those through to the rules, and this does not.
func (w *Worker) findTrunk(ctx context.Context, req *sipmsg.Request, tx sipmsg.ServerTransaction, info rules.Call) (*pb.SIPInboundTrunkInfo, string, bool) {
	candidates, err := w.opts.Store.InboundTrunksForNumber(ctx, info.ToUser)
	if err != nil {
		w.log.Warn("could not read trunks", "error", err)
		w.respond(req, tx, 503, "Service Unavailable", sipmsg.NewHeader("Retry-After", retryAfterSecs))
		return nil, "", false
	}
	infos := make([]*pb.SIPInboundTrunkInfo, 0, len(candidates))
	projects := make(map[string]string, len(candidates))
	for _, t := range candidates {
		infos = append(infos, t.Info)
		projects[t.Info.SipTrunkId] = t.ProjectID
	}
	trunk, err := rules.MatchTrunk(infos, info)
	if err != nil {
		w.log.Warn("more than one trunk matches a call, so it was refused", "error", err)
		w.refuse(req, tx, 403, "Forbidden")
		return nil, "", false
	}
	if trunk == nil {
		w.refuse(req, tx, 403, "Forbidden")
		return nil, "", false
	}
	if !w.conf.AllowUnauthenticatedTrunks && !rules.IsAuthenticated(trunk) {
		w.log.Warn("refused a call to a trunk with neither an allow-list nor a password", "trunk", trunk.SipTrunkId)
		w.refuse(req, tx, 403, "Forbidden")
		return nil, "", false
	}
	return trunk, projects[trunk.SipTrunkId], true
}

// authorize checks the trunk's password, if it has one. An allow-list has already been checked by matching.
func (w *Worker) authorize(req *sipmsg.Request, tx sipmsg.ServerTransaction, trunk *pb.SIPInboundTrunkInfo) bool {
	if !rules.HasDigest(trunk) {
		return true
	}
	realm := trunk.AuthRealm
	if realm == "" {
		realm = defaultRealm
	}
	header := ""
	if h := req.GetHeader("Proxy-Authorization"); h != nil {
		header = h.Value()
	}
	callID := string(*req.CallID())
	now := time.Now()
	result := w.digest.check(now, header, "INVITE", realm, trunk.AuthUsername, trunk.AuthPassword, callID, trunk.SipTrunkId)
	switch result {
	case authOK:
		return true
	case authBad:
		w.guard.failed(parseSource(req.Source()), now)
		w.respond(req, tx, 403, "Forbidden")
		return false
	}
	// Asking costs nothing to keep: everything needed to check the answer is in the nonce.
	w.respond(req, tx, 407, "Proxy Authentication Required", sipmsg.NewHeader("Proxy-Authenticate", w.digest.challenge(now, realm, callID, trunk.SipTrunkId, result == authStale)))
	return false
}

// answerInbound takes an accepted INVITE as far as a running call. It runs on the handler for the INVITE, whose transaction it needs.
func (c *call) answerInbound(req *sipmsg.Request, trunk *pb.SIPInboundTrunkInfo, list []*pb.SIPDispatchRuleInfo, d *rules.Dispatch, info rules.Call, answer []byte) {
	limit := c.w.maxCall
	if t := d.MaxCallDuration; t != nil && t.AsDuration() > 0 && t.AsDuration() < limit {
		limit = t.AsDuration()
	}
	c.session.Start()
	if d.NeedPin {
		// Asking for a PIN takes a voice path, so this call is answered before there is a room to put it in.
		if !c.respondAnswer(answer, d) {
			return
		}
		c.setEstablished()
		go c.watch(limit)
		c.bridge.Start()
		final, ok := c.collectPin(trunk, list, info)
		if !ok {
			return
		}
		d = final
		c.attrsToHeaders = d.AttributesToHeaders
		if !c.enterRoom(req, d, attrActive) {
			c.hangup(endInfo{reason: "JOIN_FAILURE", failed: true, message: "Could not join the room."})
			return
		}
		c.bridge.PlayToPhone(nil)
		c.bridge.SetRoom(c.room.WriteOpus)
		c.update(store.SIPCallUpdate{Status: store.SIPActive, Answered: true})
		c.record("sip_call_started", "The call was answered.")
		return
	}

	c.attrsToHeaders = d.AttributesToHeaders
	if !c.enterRoom(req, d, attrRinging) {
		c.releaseUnanswered(endInfo{reason: "JOIN_FAILURE", failed: true, code: 503, status: "Service Unavailable", message: "Could not join the room."}, sipmsg.NewHeader("Retry-After", retryAfterSecs))
		return
	}
	// It stays ringing until someone in the room can hear this caller. Answering sooner would start the caller's call charges
	// with nobody there. A second phone in the room also counts, or two calls into an empty room would wait for each other.
	ringing := c.w.ringing
	if t := d.RingingTimeout; t != nil && t.AsDuration() > 0 {
		ringing = t.AsDuration()
	}
	if err := c.server.Respond(180, "Ringing", nil); err != nil {
		c.endFromCaller()
		return
	}
	timeout := time.NewTimer(ringing)
	defer timeout.Stop()
	ring := time.NewTicker(ringEvery)
	defer ring.Stop()
wait:
	for {
		select {
		case <-c.ready:
			break wait
		case <-c.server.Context().Done():
			c.endFromCaller()
			return
		case <-c.stop:
			c.releaseUnanswered(endInfo{reason: "USER_UNAVAILABLE", code: 480, status: "Temporarily Unavailable"})
			return
		case <-timeout.C:
			code, reason := 486, "Busy Here"
			if s := d.RingingTimeoutStatus; s != pb.SIPStatusCode_SIP_STATUS_UNKNOWN {
				code, reason = int(s), strings.TrimPrefix(s.String(), "SIP_STATUS_")
			}
			c.releaseUnanswered(endInfo{reason: "USER_UNAVAILABLE", code: code, status: reason, message: "Nobody in the room was ready to take the call."})
			return
		case <-ring.C:
			if err := c.server.Respond(180, "Ringing", nil); err != nil {
				c.endFromCaller()
				return
			}
		}
	}
	if !c.respondAnswer(answer, d) {
		return
	}
	c.setEstablished()
	c.bridge.SetRoom(c.room.WriteOpus)
	c.bridge.Start()
	go c.watch(limit)
	c.setStatus(attrActive)
	c.update(store.SIPCallUpdate{Status: store.SIPActive, Answered: true, SIPStatusCode: 200, SIPStatus: "OK"})
	c.record("sip_call_started", "The call was answered.")
}

func (c *call) setEstablished() { c.established.Store(true) }

func (c *call) record(event, message string) {
	ctx, cancel := dbContext()
	defer cancel()
	row, err := c.w.opts.Store.SIPCallByID(ctx, c.project, c.id)
	if err != nil {
		return
	}
	if err := c.w.opts.Store.RecordSIPEvent(ctx, row, "info", event, message); err != nil {
		c.log.Warn("could not record a sip event", "error", err)
	}
}

// respondAnswer sends the 200 OK, and comes back at once. Waiting for the ACK happens on the side: an ACK can be lost, for instance by
// a load balancer, and that is no reason to hold up a call that is otherwise working. The watch shortens how long it may stay silent
// instead.
func (c *call) respondAnswer(answer []byte, d *rules.Dispatch) bool {
	res := sipmsg.NewSDPResponseFromRequest(c.server.InviteRequest, answer)
	for k, v := range d.Headers {
		res.AppendHeader(sipmsg.NewHeader(k, v))
	}
	res.AppendHeader(sipmsg.NewHeader("Allow", allowedMethods))
	done := make(chan error, 1)
	// Give the first send a moment to fail, for a call the caller has just cancelled.
	go func() {
		if err := c.server.WriteResponse(res); err != nil {
			done <- err
			return
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			c.log.Debug("the answer did not go through", "error", err)
			if !c.acked.Load() {
				c.endFromCaller()
				return false
			}
		}
	case <-time.After(50 * time.Millisecond):
	}
	return true
}

// endFromCaller ends a call the caller gave up on before it was answered.
func (c *call) endFromCaller() {
	c.hangup(endInfo{reason: "CLIENT_INITIATED", noBye: true, code: 487, status: "Request Terminated"})
}

// releaseUnanswered turns down a call that never got answered, and cleans up after it.
func (c *call) releaseUnanswered(info endInfo, headers ...sipmsg.Header) {
	if err := c.server.Respond(info.code, info.status, nil, headers...); err != nil {
		c.log.Debug("could not turn down a call", "error", err)
	}
	info.noBye = true
	c.hangup(info)
}

// enterRoom joins the room with the attributes the call's trunk and rule give it. It reports whether that worked.
func (c *call) enterRoom(req *sipmsg.Request, d *rules.Dispatch, status string) bool {
	c.identity = d.Identity
	attrs := d.Attributes
	if attrs == nil {
		attrs = map[string]string{}
	}
	c.remoteHeaders = mapHeaders(req, attrs, d.HeadersToAttributes, d.IncludeHeaders)
	attrs[rules.AttrCallIDFull] = string(*req.CallID())
	if from := req.From(); from != nil {
		if tag, ok := from.Params.Get("tag"); ok {
			attrs[rules.AttrCallTag] = tag
		}
	}
	attrs[rules.AttrInviteTime] = itoa(c.started.UnixMilli())
	attrs[rules.AttrCallStatus] = status
	ctx, cancel := context.WithTimeout(context.Background(), joinTimeout)
	defer cancel()
	if err := c.joinRoom(ctx, d.Room, d.Name, d.Metadata, attrs, d.RoomConfig); err != nil {
		c.log.Warn("could not join the room", "error", err)
		return false
	}
	sid, roomSID := c.room.IDs()
	c.update(store.SIPCallUpdate{
		DispatchRuleID: d.RuleID, RoomName: d.Room, RoomSID: roomSID, Identity: d.Identity, ParticipantSID: sid, FromUser: d.FromUser, ToUser: d.ToUser,
	})
	return true
}

// Tones for entering a PIN. There are no recorded prompts, because recording voices is its own decision for whoever runs this, so
// these only say that something is wanted, or wrong.
func promptTone() *media.Tone { return media.NewTone(media.PhoneRate, 150, 2850, 880) }
func wrongTone() *media.Tone  { return media.NewTone(media.PhoneRate, 150, 150, 300) }

// collectPin plays a prompt and reads digits until #, and looks for a rule that matches. An empty # asks for a rule with no PIN at all.
// One wrong try ends the call, as it does on the reference, and counts against the source too, so guessing is slow as well as noisy.
func (c *call) collectPin(trunk *pb.SIPInboundTrunkInfo, list []*pb.SIPDispatchRuleInfo, info rules.Call) (*rules.Dispatch, bool) {
	digits := make(chan byte, 32)
	c.session.SetDigitHandler(func(d byte) {
		select {
		case digits <- d:
		default:
		}
	})
	defer c.session.SetDigitHandler(c.onPhoneDigit)
	c.bridge.PlayToPhone(promptTone())
	entered, ok := c.readPin(digits)
	if !ok {
		return nil, false
	}
	rule, err := rules.MatchRule(trunk, list, info, entered, entered == "")
	if err == nil {
		d, err := rules.Evaluate(trunk, rule, info, entered)
		if err == nil && !d.NeedPin {
			return d, true
		}
	}
	c.w.guard.failed(c.trusted, time.Now())
	c.bridge.PlayToPhone(wrongTone())
	select {
	case <-time.After(time.Second):
	case <-c.stop:
		return nil, false
	}
	c.hangup(endInfo{reason: "USER_REJECTED", message: "The caller did not enter a correct PIN."})
	return nil, false
}

func (c *call) readPin(digits <-chan byte) (string, bool) {
	var b strings.Builder
	timeout := time.NewTimer(pinTimeout)
	defer timeout.Stop()
	for {
		select {
		case <-c.stop:
			return "", false
		case <-timeout.C:
			c.hangup(endInfo{reason: "USER_REJECTED", message: "The caller did not enter a PIN in time."})
			return "", false
		case d := <-digits:
			if d == '#' {
				return b.String(), true
			}
			if b.Len() < pinMaxDigits {
				b.WriteByte(d)
			}
		}
	}
}
