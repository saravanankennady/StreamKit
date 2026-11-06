package sip

import (
	"context"
	"strconv"
	"strings"
	"time"

	sipmsg "github.com/emiago/sipgo/sip"

	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/sip/media"
)

const (
	defaultTransferWait = 30 * time.Second
	referTimeout        = 10 * time.Second
)

// transferNotice is what a NOTIFY said about a transfer.
type transferNotice struct {
	code       int
	status     string
	terminated bool
}

// transfer asks the phone's side to take the call elsewhere: a cold transfer, with REFER. It waits for the NOTIFY that says how it
// went, then ends this leg.
func (c *call) transfer(t *store.SIPTransfer) {
	c.transferMu.Lock()
	defer c.transferMu.Unlock()
	finish := func(ok bool, reason string, code int, status, message string) {
		ctx, cancel := dbContext()
		defer cancel()
		if err := c.w.opts.Store.FinishSIPTransfer(ctx, t.ID, ok, reason, code, status, message); err != nil {
			c.log.Warn("could not record a transfer", "error", err)
		}
	}
	if !c.established.Load() {
		finish(false, "STR_CALL_ENDED", 0, "", "The call has not been answered.")
		return
	}
	if tone := media.NewDialtone(media.Dialtone(t.Request.Dialtone), media.PhoneRate); tone != nil {
		c.bridge.PlayToPhone(tone)
		defer c.bridge.PlayToPhone(nil)
	}
	wait := defaultTransferWait
	if t.Request.TimeoutMs > 0 {
		wait = time.Duration(t.Request.TimeoutMs) * time.Millisecond
	}
	notices := make(chan transferNotice, 8)
	c.setNotify(notices)
	defer c.setNotify(nil)

	if code, status, err := c.sendRefer(t); err != nil {
		c.log.Warn("could not send a REFER", "error", err)
		finish(false, "STR_REJECTED", 0, "", "The REFER could not be sent.")
		return
	} else if code != 200 && code != 202 {
		finish(false, "STR_REJECTED", code, status, status)
		return
	}
	timeout := time.NewTimer(wait)
	defer timeout.Stop()
	for {
		select {
		case <-c.stop:
			finish(false, "STR_CALL_ENDED", 0, "", "The call ended before the transfer finished.")
			return
		case <-timeout.C:
			finish(false, "STR_RINGING_TIMEOUT", 0, "", "The transfer did not finish in time.")
			return
		case n := <-notices:
			switch {
			case n.code >= 200 && n.code < 300:
				finish(true, "STR_COMPLETED", n.code, n.status, "")
				// The other side usually hangs up by itself now. If it does not, this leg has nothing left to do.
				select {
				case <-c.stop:
				case <-time.After(transferGrace):
					c.hangup(endInfo{reason: "CLIENT_INITIATED", message: "The call was transferred."})
				}
				return
			case n.code >= 300:
				finish(false, "STR_REJECTED", n.code, n.status, n.status)
				return
			case n.terminated:
				finish(false, "STR_SUBSCRIPTION_TERMINATED", 0, "", "REFER subscription terminated")
				return
			}
		}
	}
}

// sendRefer sends the REFER, and returns its final response.
func (c *call) sendRefer(t *store.SIPTransfer) (int, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), referTimeout)
	defer cancel()
	var target sipmsg.Uri
	var transport string
	switch {
	case c.server != nil:
		contact := c.server.InviteRequest.Contact()
		target, transport = contact.Address, c.server.InviteRequest.Transport()
	default:
		target, transport = c.client.InviteRequest.Recipient, c.client.InviteRequest.Transport()
		if contact := c.client.InviteResponse.Contact(); contact != nil {
			target = contact.Address
		}
	}
	req := sipmsg.NewRequest(sipmsg.REFER, *target.Clone())
	req.SetTransport(transport)
	req.AppendHeader(sipmsg.NewHeader("Refer-To", t.TransferTo))
	contact := c.w.contact(strings.ToLower(transport))
	req.AppendHeader(sipmsg.NewHeader("Referred-By", "<"+contact.Address.String()+">"))
	c.addCallHeaders(req, t.Request.Headers)
	var res *sipmsg.Response
	var err error
	if c.server != nil {
		res, err = c.server.Do(ctx, req)
	} else {
		res, err = c.client.Do(ctx, req)
	}
	if err != nil {
		return 0, "", err
	}
	return int(res.StatusCode), res.Reason, nil
}

// gotNotify is the phone's side reporting on a transfer. Only the status line in it matters here.
func (c *call) gotNotify(req *sipmsg.Request, tx sipmsg.ServerTransaction) {
	c.w.respond(req, tx, 200, "OK")
	if event := req.GetHeader("Event"); event == nil || !strings.HasPrefix(strings.ToLower(event.Value()), "refer") {
		return
	}
	n := transferNotice{}
	if state := req.GetHeader("Subscription-State"); state != nil {
		n.terminated = strings.HasPrefix(strings.ToLower(state.Value()), "terminated")
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(req.Body())), "\n")
	fields := strings.SplitN(strings.TrimSpace(line), " ", 3)
	if len(fields) >= 2 && strings.HasPrefix(fields[0], "SIP/") {
		n.code, _ = strconv.Atoi(fields[1])
		if len(fields) == 3 {
			n.status = strings.TrimSpace(fields[2])
		}
	}
	c.notifyMu.Lock()
	ch := c.notify
	c.notifyMu.Unlock()
	if ch != nil {
		select {
		case ch <- n:
		default:
		}
	}
}

func (c *call) setNotify(ch chan transferNotice) {
	c.notifyMu.Lock()
	c.notify = ch
	c.notifyMu.Unlock()
}
