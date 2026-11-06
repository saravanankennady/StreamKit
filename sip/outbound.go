package sip

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/emiago/sipgo"
	sipmsg "github.com/emiago/sipgo/sip"
	"google.golang.org/protobuf/encoding/protojson"

	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/ingress/source"
	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/sip/media"
	"elnova-streamkit.local/sip/rules"
)

// cancelGrace is how long a trunk gets to answer a CANCEL before the call is reported over anyway.
const cancelGrace = 3 * time.Second

// newOutboundCall sets up a call a worker has claimed from the queue.
func newOutboundCall(w *Worker, row *store.SIPCall, req *store.SIPOutboundRequest) *call {
	c := newCall(w, row.ID, row.ProjectID, store.SIPOutbound)
	c.identity = row.ParticipantIdentity
	c.roomName = row.RoomName
	c.saved.Store(true)
	c.outbound = req
	return c
}

func transportName(t int32) string {
	switch pb.SIPTransport(t) {
	case pb.SIPTransport_SIP_TRANSPORT_TCP:
		return "tcp"
	case pb.SIPTransport_SIP_TRANSPORT_TLS:
		return "tls"
	}
	return "udp"
}

// run places the call. The participant joins first, and then the phone is dialled, so anyone in the room can see the call is coming
// and hear it ring. That is the order the reference uses, and what integrations watching sip.callStatus are written for.
func (c *call) run() {
	req := c.outbound
	c.media = &pb.SIPMediaConfig{}
	if len(req.MediaJSON) > 0 {
		if err := protojson.Unmarshal(req.MediaJSON, c.media); err != nil {
			c.hangup(endInfo{reason: "JOIN_FAILURE", failed: true, message: "The call's media settings could not be read."})
			return
		}
	}
	c.policy = policyFor(c.media)
	if len(c.policy.Codecs) == 0 {
		c.hangup(endInfo{reason: "JOIN_FAILURE", failed: true, message: "None of the codecs asked for are available.", code: 488, status: media.ErrNoCodec.Error()})
		return
	}
	limit := c.w.maxCall
	if req.MaxDurationMs > 0 {
		limit = min(limit, time.Duration(req.MaxDurationMs)*time.Millisecond)
	}
	ringing := c.w.ringing
	if req.RingingMs > 0 {
		ringing = time.Duration(req.RingingMs) * time.Millisecond
	}

	session, err := c.w.ports.Open(c.log)
	if err != nil {
		c.hangup(endInfo{reason: "JOIN_FAILURE", failed: true, message: "No media ports are free.", code: 503, status: "Service Unavailable"})
		return
	}
	c.session = session
	if c.bridge, err = media.NewBridge(session); err != nil {
		c.hangup(endInfo{reason: "JOIN_FAILURE", failed: true, message: "Could not set up media."})
		return
	}
	session.SetDigitHandler(c.onPhoneDigit)
	c.attrsToHeaders = req.AttributesToHeaders

	attrs := map[string]string{}
	for k, v := range req.Attributes {
		attrs[k] = v
	}
	attrs[rules.AttrCallID] = c.id
	attrs[rules.AttrCallStatus] = attrDialing
	ctx, cancel := context.WithTimeout(context.Background(), joinTimeout)
	joinErr := c.joinRoom(ctx, c.roomName, req.Name, req.Metadata, attrs, nil)
	cancel()
	if joinErr != nil {
		c.log.Warn("could not join the room", "error", joinErr)
		c.hangup(endInfo{reason: "JOIN_FAILURE", failed: true, message: "Could not join the room."})
		return
	}
	sid, roomSID := c.room.IDs()
	c.update(store.SIPCallUpdate{Status: store.SIPDialing, ParticipantSID: sid, RoomSID: roomSID})

	var stopTone func()
	if tone := media.NewDialtone(media.Dialtone(req.Dialtone), media.RoomRate); tone != nil {
		stopTone = c.bridge.PlayToRoom(tone, c.room.WriteOpus)
	}
	endTone := func() {
		if stopTone != nil {
			stopTone()
			stopTone = nil
		}
	}
	defer endTone()

	dialCtx, dialCancel := context.WithTimeout(context.Background(), ringing)
	defer dialCancel()
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-c.stop:
			dialCancel()
		case <-stopped:
		}
	}()
	dialog, offer, err := c.dial(dialCtx, req)
	if err != nil {
		c.endDialFailure(dialCtx, err)
		return
	}
	endTone()

	answer, err := media.ParseDescription(dialog.InviteResponse.Body())
	if err == nil {
		var negotiated *media.Negotiated
		if negotiated, err = offer.Accept(answer); err == nil {
			if err = session.Configure(answer, parseTrusted(dialog.InviteResponse.Source()), negotiated); err == nil {
				c.negotiated = negotiated
				c.update(store.SIPCallUpdate{AudioCodec: negotiated.Codec.Name, MediaEncryption: encryptionName(negotiated)})
			}
		}
	}
	// The dialog exists from the 200 OK on, whatever is in it, so it gets its ACK either way.
	ackCtx, ackCancel := context.WithTimeout(context.Background(), byeTimeout)
	ackErr := dialog.Ack(ackCtx)
	ackCancel()
	c.client = dialog
	c.acked.Store(true)
	c.w.registerDialog(dialog.ID, c)
	c.setEstablished()
	if err != nil {
		c.log.Warn("the answer's media could not be used", "error", err)
		c.hangup(endInfo{reason: "MEDIA_FAILURE", failed: true, message: "The answer's media could not be used.", code: 488, status: firstLine(err)})
		return
	}
	if ackErr != nil {
		c.log.Warn("could not send the ACK", "error", ackErr)
	}
	c.answered(dialog, req, limit)
}

// answered turns an answered call into a running one.
func (c *call) answered(dialog *sipgo.DialogClientSession, req *store.SIPOutboundRequest, limit time.Duration) {
	session := c.session
	attrs := map[string]string{rules.AttrCallStatus: attrActive}
	c.remoteHeaders = mapHeaders(dialog.InviteResponse, attrs, req.HeadersToAttributes, pb.SIPHeaderOptions(req.IncludeHeaders))
	code, reason := statusFromResponse(dialog.InviteResponse)
	session.Start()
	c.bridge.SetRoom(c.room.WriteOpus)
	c.bridge.Start()
	go c.watch(limit)
	if req.Dtmf != "" {
		session.QueueDTMF(req.Dtmf)
		attrs[rules.AttrCallStatus] = attrAutomation
		go func() {
			t := time.NewTicker(100 * time.Millisecond)
			defer t.Stop()
			for session.DTMFPending() {
				select {
				case <-c.stop:
					return
				case <-t.C:
				}
			}
			c.setStatus(attrActive)
		}()
	}
	c.room.SetAttributes(attrs)
	c.update(store.SIPCallUpdate{Status: store.SIPActive, Answered: true, SIPStatusCode: code, SIPStatus: reason})
	c.record("sip_call_started", "The call was answered.")
}

// dialError carries the final response to an INVITE that was not answered.
type dialError struct {
	code   int
	status string
}

func (e *dialError) Error() string { return fmt.Sprintf("sip status %d: %s", e.code, e.status) }

// destinationFor works out where to send the INVITE: an address that was checked just now, so a name cannot resolve to one thing for
// the check and something else for the call. TLS keeps the name, because the certificate has to match it.
func (c *call) destinationFor(ctx context.Context, address, transport string) (host string, port int, pinned string, err error) {
	host = address
	if h, p, splitErr := net.SplitHostPort(address); splitErr == nil {
		host = h
		if port, err = strconv.Atoi(p); err != nil {
			return "", 0, "", fmt.Errorf("bad port in the trunk address: %w", err)
		}
	}
	host = strings.Trim(host, "[]")
	dport := port
	if dport == 0 {
		dport = 5060
		if transport == "tls" {
			dport = 5061
		}
	}
	if c.w.opts.Config.Media.AllowPrivateAddresses {
		if transport == "tls" {
			return host, port, "", nil
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(ips) == 0 {
			return "", 0, "", fmt.Errorf("the trunk address does not resolve: %w", source.ErrResolve)
		}
		return host, port, net.JoinHostPort(ips[0].IP.String(), strconv.Itoa(dport)), nil
	}
	ip, err := source.PublicAddress(ctx, host)
	if err != nil {
		return "", 0, "", err
	}
	if transport == "tls" {
		return host, port, "", nil
	}
	return host, port, net.JoinHostPort(ip.String(), strconv.Itoa(dport)), nil
}

func (c *call) dial(ctx context.Context, req *store.SIPOutboundRequest) (*sipgo.DialogClientSession, *media.Offer, error) {
	transport := transportName(req.Transport)
	host, port, pinned, err := c.destinationFor(ctx, req.Address, transport)
	if err != nil {
		return nil, nil, &dialError{code: 503, status: "The trunk address is not usable"}
	}
	offer, err := media.BuildOffer(c.w.mediaIP, c.session.Port(), c.policy)
	if err != nil {
		return nil, nil, err
	}
	uri := sipmsg.Uri{Scheme: "sip", User: req.CallTo, Host: host, Port: port}
	if transport != "udp" {
		uri.UriParams = sipmsg.HeaderParams{{K: "transport", V: transport}}
	}
	invite := sipmsg.NewRequest(sipmsg.INVITE, uri)
	invite.SetTransport(strings.ToUpper(transport))
	if pinned != "" {
		invite.SetDestination(pinned)
	}
	fromHost := req.FromHost
	if fromHost == "" {
		fromHost = c.w.host
	}
	display := req.Number
	if req.DisplayName != nil {
		display = *req.DisplayName
	}
	from := &sipmsg.FromHeader{DisplayName: display, Address: sipmsg.Uri{Scheme: "sip", User: req.Number, Host: fromHost}, Params: sipmsg.HeaderParams{{K: "tag", V: sipmsg.GenerateTagN(16)}}}
	toUser := req.CallTo
	if req.ToUserOverride != "" {
		toUser = req.ToUserOverride
	}
	to := &sipmsg.ToHeader{Address: sipmsg.Uri{Scheme: "sip", User: toUser, Host: host, Port: port}}
	invite.AppendHeader(from)
	invite.AppendHeader(to)
	invite.AppendHeader(sipmsg.NewHeader("Allow", allowedMethods))
	invite.AppendHeader(sipmsg.NewHeader("Content-Type", "application/sdp"))
	for k, v := range req.Headers {
		invite.AppendHeader(sipmsg.NewHeader(k, v))
	}
	invite.SetBody(offer.SDP)

	ua := sipgo.DialogUA{Client: c.w.client, ContactHDR: c.w.contact(transport)}
	dialog, err := ua.WriteInvite(ctx, invite)
	if err != nil {
		return nil, nil, err
	}
	c.update(store.SIPCallUpdate{SIPCallID: string(*invite.CallID())})
	ringingSent := false
	waited := make(chan error, 1)
	go func() {
		waited <- dialog.WaitAnswer(ctx, sipgo.AnswerOptions{
			Username: req.Username, Password: req.Password,
			OnResponse: func(res *sipmsg.Response) error {
				if res.IsProvisional() && res.StatusCode != 100 && !ringingSent {
					ringingSent = true
					c.setStatus(attrRinging)
					c.update(store.SIPCallUpdate{Status: store.SIPRinging})
				}
				return nil
			},
		})
	}()
	select {
	case err = <-waited:
	case <-ctx.Done():
		// That sends a CANCEL, and then waits for the answer to it, for half a minute if a trunk is slow. Nobody needs to wait
		// that long to be told a phone did not answer.
		select {
		case err = <-waited:
		case <-time.After(cancelGrace):
			go func() {
				<-waited
				c.tidyAfterCancel(dialog)
			}()
			return nil, nil, ctx.Err()
		}
	}
	if err != nil {
		c.tidyAfterCancel(dialog)
		var rejected *sipgo.ErrDialogResponse
		if errors.As(err, &rejected) {
			return nil, nil, &dialError{code: int(rejected.Res.StatusCode), status: rejected.Res.Reason}
		}
		return nil, nil, err
	}
	return dialog, offer, nil
}

// tidyAfterCancel handles a phone that answered just as this side gave up. The call is over, but that phone has a dialog, and it is owed
// an ACK and a BYE.
func (c *call) tidyAfterCancel(dialog *sipgo.DialogClientSession) {
	res := dialog.InviteResponse
	if res == nil || !res.IsSuccess() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), byeTimeout)
	defer cancel()
	if err := dialog.Ack(ctx); err != nil {
		c.log.Debug("could not ACK an answer that came too late", "error", err)
		return
	}
	if err := dialog.Bye(ctx); err != nil {
		c.log.Debug("could not hang up an answer that came too late", "error", err)
	}
}

// endDialFailure records why a phone was never reached, in the terms a caller of the API gets it in.
func (c *call) endDialFailure(ctx context.Context, err error) {
	select {
	case <-c.stop:
		c.hangup(endInfo{reason: "CLIENT_INITIATED", noBye: true, code: 487, status: "Request Terminated"})
		return
	default:
	}
	var rejected *dialError
	switch {
	case errors.As(err, &rejected):
		reason := "USER_UNAVAILABLE"
		if rejected.code == 486 || rejected.code == 603 {
			reason = "USER_REJECTED"
		}
		c.hangup(endInfo{reason: reason, failed: true, noBye: true, code: rejected.code, status: rejected.status, message: rejected.Error()})
	case ctx.Err() != nil:
		c.hangup(endInfo{reason: "USER_UNAVAILABLE", failed: true, noBye: true, code: 408, status: "Request Timeout", message: "The phone did not answer in time."})
	default:
		c.log.Warn("could not reach the trunk", "error", err)
		c.hangup(endInfo{reason: "SIP_TRUNK_FAILURE", failed: true, noBye: true, code: 503, status: "Service Unavailable", message: "Could not reach the trunk."})
	}
}
