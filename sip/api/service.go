// Package sipapi is the control plane's side of SIP: trunks, dispatch rules, and asking a worker to place or transfer a call. The
// server runs it, and it talks to workers only through the database, the way recording and ingest do. It serves the Twirp API at
// /twirp/livekit.SIP/<Method> that server SDKs and CLIs for this protocol call, and the REST routes use the same operations.
package sipapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"mime"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"elnova-streamkit.local/api/middleware"
	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/ingress/source"
	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/config"
	"elnova-streamkit.local/server/participant"
	"elnova-streamkit.local/sip/rules"
)

const (
	pathPrefix      = "/twirp/livekit.SIP/"
	maxRequestBytes = 1 << 20

	defaultCallsPerProject = 50
	defaultOutboundPerMin  = 120

	// How long CreateSIPParticipant waits. Waiting for an answer takes as long as a phone can ring.
	joinWait   = 30 * time.Second
	answerWait = 80 * time.Second
	// transferWait applies when a request does not say.
	transferWait = 30 * time.Second
	pollEvery    = 100 * time.Millisecond
)

// Service runs the SIP operations. A nil store means there is no control plane, and every method says so.
type Service struct {
	cfg   *config.Config
	keys  auth.KeyProvider
	store *store.Store
	log   *slog.Logger
	dials *middleware.RateLimiter
}

// New creates a Service.
func New(cfg *config.Config, keys auth.KeyProvider, st *store.Store, log *slog.Logger) *Service {
	perMinute := cfg.Media.SIP.MaxOutboundPerMinute
	if perMinute <= 0 {
		perMinute = defaultOutboundPerMin
	}
	return &Service{cfg: cfg, keys: keys, store: st, log: log, dials: middleware.NewRateLimiter(float64(perMinute), perMinute)}
}

// Register adds the Twirp routes to mux.
func (s *Service) Register(mux *http.ServeMux) { mux.HandleFunc(pathPrefix, s.serve) }

// Error is a failure with its Twirp code.
type Error struct {
	Code string
	Msg  string
	Meta map[string]string
}

func (e *Error) Error() string { return e.Msg }

func newError(code, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

var (
	errDenied       = newError("unauthenticated", "permissions denied")
	errNotConnected = newError("internal", "sip not connected (database required)")
	errTrunkMissing = newError("not_found", "requested sip trunk does not exist")
	errRuleMissing  = newError("not_found", "requested sip dispatch rule does not exist")
)

var statusByCode = map[string]int{
	"canceled": 408, "unknown": 500, "invalid_argument": 400, "malformed": 400, "deadline_exceeded": 408,
	"not_found": 404, "bad_route": 404, "already_exists": 409, "permission_denied": 403, "resource_exhausted": 429,
	"failed_precondition": 412, "aborted": 409, "out_of_range": 400, "unimplemented": 501, "internal": 500,
	"unavailable": 503, "data_loss": 500, "unauthenticated": 401,
}

// HTTPStatus gives the status that goes with an error's code.
func (e *Error) HTTPStatus() int {
	if status, ok := statusByCode[e.Code]; ok {
		return status
	}
	return http.StatusInternalServerError
}

func writeError(w http.ResponseWriter, e *Error) {
	body, err := jsonError(e)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.HTTPStatus())
	_, _ = w.Write(body)
}

// sipStatusError turns a final SIP response into an API error, the way clients of this API expect to read one: a Twirp code they can
// branch on, and the SIP status in the error's metadata.
func sipStatusError(code int, status string) *Error {
	return &Error{
		Code: twirpCodeForSIP(code),
		Msg:  fmt.Sprintf("sip status %d: %s", code, status),
		Meta: map[string]string{"sip_status_code": strconv.Itoa(code), "sip_status": status},
	}
}

func twirpCodeForSIP(code int) string {
	switch code {
	case 301, 302, 404, 410, 481, 604:
		return "not_found"
	case 401, 402, 403, 433, 470, 603, 607, 608:
		return "permission_denied"
	case 407, 428, 436, 437, 438, 494:
		return "unauthenticated"
	case 408, 504:
		return "deadline_exceeded"
	case 487, 491:
		return "aborted"
	case 409, 412, 480, 482, 483, 486:
		return "failed_precondition"
	case 416:
		return "out_of_range"
	case 440, 600:
		return "resource_exhausted"
	}
	switch {
	case code >= 500 && code < 600:
		return "failed_precondition"
	case code >= 300 && code < 700:
		return "invalid_argument"
	}
	return "unknown"
}

type request struct {
	project string
	grants  *auth.Grants
}

func (r *request) needAdmin() *Error {
	if r.grants.SIP == nil || !r.grants.SIP.Admin {
		return errDenied
	}
	return nil
}

func (r *request) needCall() *Error {
	if r.grants.SIP == nil || !r.grants.SIP.Call {
		return errDenied
	}
	return nil
}

func method[Req, Res proto.Message](newReq func() Req, fn func(context.Context, *request, Req) (Res, *Error)) func(context.Context, *request, http.ResponseWriter, []byte, bool) {
	return func(ctx context.Context, r *request, w http.ResponseWriter, body []byte, isJSON bool) {
		req := newReq()
		if isJSON {
			if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(body, req); err != nil {
				writeError(w, newError("malformed", "the json request could not be decoded"))
				return
			}
		} else if err := proto.Unmarshal(body, req); err != nil {
			writeError(w, newError("malformed", "the protobuf request could not be decoded"))
			return
		}
		res, apiErr := fn(ctx, r, req)
		if apiErr != nil {
			writeError(w, apiErr)
			return
		}
		var data []byte
		var err error
		if isJSON {
			data, err = protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}.Marshal(res)
			w.Header().Set("Content-Type", "application/json")
		} else {
			data, err = proto.Marshal(res)
			w.Header().Set("Content-Type", "application/protobuf")
		}
		if err != nil {
			writeError(w, newError("internal", "could not encode the response"))
			return
		}
		_, _ = w.Write(data)
	}
}

func (s *Service) handlers() map[string]func(context.Context, *request, http.ResponseWriter, []byte, bool) {
	return map[string]func(context.Context, *request, http.ResponseWriter, []byte, bool){
		"ListSIPTrunk":           method(func() *pb.ListSIPTrunkRequest { return &pb.ListSIPTrunkRequest{} }, s.listTrunk),
		"CreateSIPInboundTrunk":  method(func() *pb.CreateSIPInboundTrunkRequest { return &pb.CreateSIPInboundTrunkRequest{} }, s.createInbound),
		"CreateSIPOutboundTrunk": method(func() *pb.CreateSIPOutboundTrunkRequest { return &pb.CreateSIPOutboundTrunkRequest{} }, s.createOutbound),
		"UpdateSIPInboundTrunk":  method(func() *pb.UpdateSIPInboundTrunkRequest { return &pb.UpdateSIPInboundTrunkRequest{} }, s.updateInbound),
		"UpdateSIPOutboundTrunk": method(func() *pb.UpdateSIPOutboundTrunkRequest { return &pb.UpdateSIPOutboundTrunkRequest{} }, s.updateOutbound),
		"GetSIPInboundTrunk":     method(func() *pb.GetSIPInboundTrunkRequest { return &pb.GetSIPInboundTrunkRequest{} }, s.getInbound),
		"GetSIPOutboundTrunk":    method(func() *pb.GetSIPOutboundTrunkRequest { return &pb.GetSIPOutboundTrunkRequest{} }, s.getOutbound),
		"ListSIPInboundTrunk":    method(func() *pb.ListSIPInboundTrunkRequest { return &pb.ListSIPInboundTrunkRequest{} }, s.listInbound),
		"ListSIPOutboundTrunk":   method(func() *pb.ListSIPOutboundTrunkRequest { return &pb.ListSIPOutboundTrunkRequest{} }, s.listOutbound),
		"DeleteSIPTrunk":         method(func() *pb.DeleteSIPTrunkRequest { return &pb.DeleteSIPTrunkRequest{} }, s.deleteTrunk),
		"CreateSIPDispatchRule":  method(func() *pb.CreateSIPDispatchRuleRequest { return &pb.CreateSIPDispatchRuleRequest{} }, s.createRule),
		"UpdateSIPDispatchRule":  method(func() *pb.UpdateSIPDispatchRuleRequest { return &pb.UpdateSIPDispatchRuleRequest{} }, s.updateRule),
		"ListSIPDispatchRule":    method(func() *pb.ListSIPDispatchRuleRequest { return &pb.ListSIPDispatchRuleRequest{} }, s.listRules),
		"DeleteSIPDispatchRule":  method(func() *pb.DeleteSIPDispatchRuleRequest { return &pb.DeleteSIPDispatchRuleRequest{} }, s.deleteRule),
		"CreateSIPParticipant":   method(func() *pb.CreateSIPParticipantRequest { return &pb.CreateSIPParticipantRequest{} }, s.createParticipant),
		"TransferSIPParticipant": method(func() *pb.TransferSIPParticipantRequest { return &pb.TransferSIPParticipantRequest{} }, s.transferParticipant),
	}
}

func (s *Service) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, newError("bad_route", "unsupported method %q (only POST is allowed)", r.Method))
		return
	}
	handler, ok := s.handlers()[strings.TrimPrefix(r.URL.Path, pathPrefix)]
	if !ok {
		writeError(w, newError("bad_route", "no handler for path %q", r.URL.Path))
		return
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (contentType != "application/json" && contentType != "application/protobuf") {
		writeError(w, newError("bad_route", "unexpected Content-Type: %q", r.Header.Get("Content-Type")))
		return
	}
	token := auth.TokenFromHeader(r.Header.Get("Authorization"))
	verified, err := auth.Verify(token, s.keys)
	if err != nil {
		writeError(w, newError("unauthenticated", "%s", err.Error()))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		writeError(w, newError("malformed", "the request could not be read"))
		return
	}
	handler(r.Context(), &request{project: verified.Key.ProjectID, grants: verified.Grants}, w, body, contentType == "application/json")
}

// internal logs a failure, and returns one that says nothing about what went wrong: the details could include things a client
// should not see.
func (s *Service) internal(err error) *Error {
	if errors.Is(err, context.Canceled) {
		return newError("canceled", "the request was canceled")
	}
	s.log.Error("sip api request failed", "error", err)
	return newError("internal", "something went wrong on our side")
}

func (s *Service) requireUnauthenticatedCheck() bool {
	return !s.cfg.Media.SIP.AllowUnauthenticatedTrunks
}

// checkOutboundAddress refuses a trunk that points at a private network, so that a project cannot use the workers to reach inside
// yours. An address that does not resolve right now is let through for trunks, since it might later: a worker checks again when it
// dials.
func (s *Service) checkOutboundAddress(ctx context.Context, address string) *Error {
	host := address
	if h, _, err := net.SplitHostPort(address); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	err := source.CheckHost(ctx, host, s.cfg.Media.AllowPrivateAddresses)
	if errors.Is(err, source.ErrPrivate) {
		return newError("invalid_argument", "the trunk address is on a private network, which this installation does not allow")
	}
	return nil
}

// --- Trunks ---

// CreateInbound creates a trunk. What it returns has no credentials in it.
func (s *Service) CreateInbound(ctx context.Context, project string, t *pb.SIPInboundTrunkInfo) (*store.InboundTrunk, *Error) {
	if s.store == nil {
		return nil, errNotConnected
	}
	if t == nil {
		return nil, newError("invalid_argument", "missing trunk")
	}
	if t.SipTrunkId != "" {
		return nil, newError("invalid_argument", "trunk id must not be set")
	}
	return s.saveInbound(ctx, project, t)
}

func (s *Service) saveInbound(ctx context.Context, project string, t *pb.SIPInboundTrunkInfo) (*store.InboundTrunk, *Error) {
	if err := rules.ValidateInbound(t, s.requireUnauthenticatedCheck()); err != nil {
		return nil, newError("invalid_argument", "%s", err.Error())
	}
	var conflict error
	saved, err := s.store.SaveInboundTrunk(ctx, project, t, func(others []*store.InboundTrunk) error {
		all := make([]*pb.SIPInboundTrunkInfo, 0, len(others)+1)
		for _, o := range others {
			all = append(all, o.Info)
		}
		conflict = rules.ValidateInboundTrunks(append(all, t))
		return conflict
	})
	if err != nil {
		if conflict != nil {
			return nil, newError("invalid_argument", "%s", conflict.Error())
		}
		if errors.Is(err, store.ErrNotFound) {
			return nil, errTrunkMissing
		}
		return nil, s.internal(err)
	}
	return saved, nil
}

// UpdateInbound changes a trunk. Credentials are never returned by this API, so a client that reads a trunk and sends it back has no
// way to include them. A replacement without credentials therefore keeps the ones the trunk has. Removing them takes an update
// that sets them empty.
func (s *Service) UpdateInbound(ctx context.Context, project string, r *pb.UpdateSIPInboundTrunkRequest) (*store.InboundTrunk, *Error) {
	if s.store == nil {
		return nil, errNotConnected
	}
	if r.SipTrunkId == "" {
		return nil, newError("invalid_argument", "trunk id must be set")
	}
	current, err := s.store.InboundTrunkByID(ctx, project, r.SipTrunkId, true)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errTrunkMissing
		}
		return nil, s.internal(err)
	}
	next := proto.Clone(current.Info).(*pb.SIPInboundTrunkInfo)
	switch a := r.Action.(type) {
	case *pb.UpdateSIPInboundTrunkRequest_Replace:
		if a.Replace == nil {
			return nil, newError("invalid_argument", "missing trunk")
		}
		if a.Replace.SipTrunkId != "" && a.Replace.SipTrunkId != r.SipTrunkId {
			return nil, newError("invalid_argument", "trunk id in the info must be empty or match the id in the update")
		}
		next = proto.Clone(a.Replace).(*pb.SIPInboundTrunkInfo)
		next.SipTrunkId = r.SipTrunkId
		if next.AuthUsername == "" && next.AuthPassword == "" {
			next.AuthUsername, next.AuthPassword = current.Info.AuthUsername, current.Info.AuthPassword
		}
	case *pb.UpdateSIPInboundTrunkRequest_Update:
		if a.Update == nil {
			return nil, newError("invalid_argument", "missing trunk update")
		}
		if err := rules.ApplyInboundUpdate(next, a.Update); err != nil {
			return nil, newError("invalid_argument", "%s", err.Error())
		}
	default:
		return nil, newError("invalid_argument", "missing or unsupported update action")
	}
	return s.saveInbound(ctx, project, next)
}

// CreateOutbound creates a trunk. What it returns has no credentials in it.
func (s *Service) CreateOutbound(ctx context.Context, project string, t *pb.SIPOutboundTrunkInfo) (*store.OutboundTrunk, *Error) {
	if s.store == nil {
		return nil, errNotConnected
	}
	if t == nil {
		return nil, newError("invalid_argument", "missing trunk")
	}
	if t.SipTrunkId != "" {
		return nil, newError("invalid_argument", "trunk id must not be set")
	}
	return s.saveOutbound(ctx, project, t)
}

func (s *Service) saveOutbound(ctx context.Context, project string, t *pb.SIPOutboundTrunkInfo) (*store.OutboundTrunk, *Error) {
	if err := rules.ValidateOutbound(t); err != nil {
		return nil, newError("invalid_argument", "%s", err.Error())
	}
	if e := s.checkOutboundAddress(ctx, t.Address); e != nil {
		return nil, e
	}
	saved, err := s.store.SaveOutboundTrunk(ctx, project, t)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errTrunkMissing
		}
		return nil, s.internal(err)
	}
	return saved, nil
}

// UpdateOutbound changes a trunk, with the same rule about credentials as UpdateInbound.
func (s *Service) UpdateOutbound(ctx context.Context, project string, r *pb.UpdateSIPOutboundTrunkRequest) (*store.OutboundTrunk, *Error) {
	if s.store == nil {
		return nil, errNotConnected
	}
	if r.SipTrunkId == "" {
		return nil, newError("invalid_argument", "trunk id must be set")
	}
	current, err := s.store.OutboundTrunkByID(ctx, project, r.SipTrunkId, true)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errTrunkMissing
		}
		return nil, s.internal(err)
	}
	next := proto.Clone(current.Info).(*pb.SIPOutboundTrunkInfo)
	switch a := r.Action.(type) {
	case *pb.UpdateSIPOutboundTrunkRequest_Replace:
		if a.Replace == nil {
			return nil, newError("invalid_argument", "missing trunk")
		}
		if a.Replace.SipTrunkId != "" && a.Replace.SipTrunkId != r.SipTrunkId {
			return nil, newError("invalid_argument", "trunk id in the info must be empty or match the id in the update")
		}
		next = proto.Clone(a.Replace).(*pb.SIPOutboundTrunkInfo)
		next.SipTrunkId = r.SipTrunkId
		if next.AuthUsername == "" && next.AuthPassword == "" {
			next.AuthUsername, next.AuthPassword = current.Info.AuthUsername, current.Info.AuthPassword
		}
	case *pb.UpdateSIPOutboundTrunkRequest_Update:
		if a.Update == nil {
			return nil, newError("invalid_argument", "missing trunk update")
		}
		if err := rules.ApplyOutboundUpdate(next, a.Update); err != nil {
			return nil, newError("invalid_argument", "%s", err.Error())
		}
	default:
		return nil, newError("invalid_argument", "missing or unsupported update action")
	}
	return s.saveOutbound(ctx, project, next)
}

// DeleteTrunk deletes a trunk. Deleting one that does not exist is not an error, which is also how the reference behaves.
func (s *Service) DeleteTrunk(ctx context.Context, project, id string) *Error {
	if s.store == nil {
		return errNotConnected
	}
	if id == "" {
		return newError("invalid_argument", "trunk ID is required")
	}
	if _, err := s.store.DeleteSIPTrunk(ctx, project, id); err != nil {
		return s.internal(err)
	}
	return nil
}

func (s *Service) createInbound(ctx context.Context, r *request, req *pb.CreateSIPInboundTrunkRequest) (*pb.SIPInboundTrunkInfo, *Error) {
	if e := r.needAdmin(); e != nil {
		return nil, e
	}
	t, e := s.CreateInbound(ctx, r.project, req.Trunk)
	if e != nil {
		return nil, e
	}
	return t.Info, nil
}

func (s *Service) createOutbound(ctx context.Context, r *request, req *pb.CreateSIPOutboundTrunkRequest) (*pb.SIPOutboundTrunkInfo, *Error) {
	if e := r.needAdmin(); e != nil {
		return nil, e
	}
	t, e := s.CreateOutbound(ctx, r.project, req.Trunk)
	if e != nil {
		return nil, e
	}
	return t.Info, nil
}

func (s *Service) updateInbound(ctx context.Context, r *request, req *pb.UpdateSIPInboundTrunkRequest) (*pb.SIPInboundTrunkInfo, *Error) {
	if e := r.needAdmin(); e != nil {
		return nil, e
	}
	t, e := s.UpdateInbound(ctx, r.project, req)
	if e != nil {
		return nil, e
	}
	return t.Info, nil
}

func (s *Service) updateOutbound(ctx context.Context, r *request, req *pb.UpdateSIPOutboundTrunkRequest) (*pb.SIPOutboundTrunkInfo, *Error) {
	if e := r.needAdmin(); e != nil {
		return nil, e
	}
	t, e := s.UpdateOutbound(ctx, r.project, req)
	if e != nil {
		return nil, e
	}
	return t.Info, nil
}

func (s *Service) getInbound(ctx context.Context, r *request, req *pb.GetSIPInboundTrunkRequest) (*pb.GetSIPInboundTrunkResponse, *Error) {
	if e := r.needAdmin(); e != nil {
		return nil, e
	}
	if s.store == nil {
		return nil, errNotConnected
	}
	if req.SipTrunkId == "" {
		return nil, newError("invalid_argument", "trunk ID is required")
	}
	t, err := s.store.InboundTrunkByID(ctx, r.project, req.SipTrunkId, false)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errTrunkMissing
		}
		return nil, s.internal(err)
	}
	return &pb.GetSIPInboundTrunkResponse{Trunk: t.Info}, nil
}

func (s *Service) getOutbound(ctx context.Context, r *request, req *pb.GetSIPOutboundTrunkRequest) (*pb.GetSIPOutboundTrunkResponse, *Error) {
	if e := r.needAdmin(); e != nil {
		return nil, e
	}
	if s.store == nil {
		return nil, errNotConnected
	}
	if req.SipTrunkId == "" {
		return nil, newError("invalid_argument", "trunk ID is required")
	}
	t, err := s.store.OutboundTrunkByID(ctx, r.project, req.SipTrunkId, false)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errTrunkMissing
		}
		return nil, s.internal(err)
	}
	return &pb.GetSIPOutboundTrunkResponse{Trunk: t.Info}, nil
}

// inOrder applies a list's filter by IDs: the answer has the same order, with an empty item for any that is missing.
func inOrder[T proto.Message](items []T, ids []string, id func(T) string, empty func() T) []T {
	if len(ids) == 0 {
		return items
	}
	out := make([]T, len(ids))
	for i, want := range ids {
		out[i] = empty()
		for _, item := range items {
			if id(item) == want {
				out[i] = item
				break
			}
		}
	}
	return out
}

func numberMatches(have, want []string) bool {
	if len(have) == 0 || len(want) == 0 {
		return true
	}
	wanted := rules.NormalizeNumbers(want)
	for _, n := range rules.NormalizeNumbers(have) {
		if slices.Contains(wanted, n) {
			return true
		}
	}
	return false
}

func (s *Service) listInbound(ctx context.Context, r *request, req *pb.ListSIPInboundTrunkRequest) (*pb.ListSIPInboundTrunkResponse, *Error) {
	if e := r.needAdmin(); e != nil {
		return nil, e
	}
	if s.store == nil {
		return nil, errNotConnected
	}
	trunks, err := s.store.ListInboundTrunks(ctx, r.project)
	if err != nil {
		return nil, s.internal(err)
	}
	items := make([]*pb.SIPInboundTrunkInfo, 0, len(trunks))
	for _, t := range trunks {
		if numberMatches(t.Info.Numbers, req.Numbers) {
			items = append(items, t.Info)
		}
	}
	items = inOrder(items, req.TrunkIds, func(t *pb.SIPInboundTrunkInfo) string { return t.SipTrunkId }, func() *pb.SIPInboundTrunkInfo { return &pb.SIPInboundTrunkInfo{} })
	return &pb.ListSIPInboundTrunkResponse{Items: items}, nil
}

func (s *Service) listOutbound(ctx context.Context, r *request, req *pb.ListSIPOutboundTrunkRequest) (*pb.ListSIPOutboundTrunkResponse, *Error) {
	if e := r.needAdmin(); e != nil {
		return nil, e
	}
	if s.store == nil {
		return nil, errNotConnected
	}
	trunks, err := s.store.ListOutboundTrunks(ctx, r.project)
	if err != nil {
		return nil, s.internal(err)
	}
	items := make([]*pb.SIPOutboundTrunkInfo, 0, len(trunks))
	for _, t := range trunks {
		if numberMatches(t.Info.Numbers, req.Numbers) {
			items = append(items, t.Info)
		}
	}
	items = inOrder(items, req.TrunkIds, func(t *pb.SIPOutboundTrunkInfo) string { return t.SipTrunkId }, func() *pb.SIPOutboundTrunkInfo { return &pb.SIPOutboundTrunkInfo{} })
	return &pb.ListSIPOutboundTrunkResponse{Items: items}, nil
}

// listTrunk is the old way to list trunks, kept because the method is still on the wire. New clients use the two lists above.
func (s *Service) listTrunk(ctx context.Context, r *request, _ *pb.ListSIPTrunkRequest) (*pb.ListSIPTrunkResponse, *Error) {
	if e := r.needAdmin(); e != nil {
		return nil, e
	}
	if s.store == nil {
		return nil, errNotConnected
	}
	in, err := s.store.ListInboundTrunks(ctx, r.project)
	if err != nil {
		return nil, s.internal(err)
	}
	out, err := s.store.ListOutboundTrunks(ctx, r.project)
	if err != nil {
		return nil, s.internal(err)
	}
	first := func(list []string) string {
		if len(list) > 0 {
			return list[0]
		}
		return ""
	}
	items := make([]*pb.SIPTrunkInfo, 0, len(in)+len(out))
	for _, t := range in {
		items = append(items, &pb.SIPTrunkInfo{
			SipTrunkId: t.Info.SipTrunkId, Kind: pb.SIPTrunkInfo_TRUNK_INBOUND, Name: t.Info.Name, Metadata: t.Info.Metadata,
			OutboundNumber: first(t.Info.Numbers), InboundAddresses: t.Info.AllowedAddresses, InboundNumbers: t.Info.AllowedNumbers,
		})
	}
	for _, t := range out {
		items = append(items, &pb.SIPTrunkInfo{
			SipTrunkId: t.Info.SipTrunkId, Kind: pb.SIPTrunkInfo_TRUNK_OUTBOUND, Name: t.Info.Name, Metadata: t.Info.Metadata,
			OutboundAddress: t.Info.Address, Transport: t.Info.Transport, OutboundNumber: first(t.Info.Numbers),
		})
	}
	return &pb.ListSIPTrunkResponse{Items: items}, nil
}

func (s *Service) deleteTrunk(ctx context.Context, r *request, req *pb.DeleteSIPTrunkRequest) (*pb.SIPTrunkInfo, *Error) {
	if e := r.needAdmin(); e != nil {
		return nil, e
	}
	if e := s.DeleteTrunk(ctx, r.project, req.SipTrunkId); e != nil {
		return nil, e
	}
	return &pb.SIPTrunkInfo{SipTrunkId: req.SipTrunkId}, nil
}

// --- Dispatch rules ---

// CreateRule creates a dispatch rule. What it returns shows a PIN only as a mask.
func (s *Service) CreateRule(ctx context.Context, project string, info *pb.SIPDispatchRuleInfo) (*store.DispatchRule, *Error) {
	if s.store == nil {
		return nil, errNotConnected
	}
	if info == nil {
		return nil, newError("invalid_argument", "missing rule")
	}
	if info.SipDispatchRuleId != "" {
		return nil, newError("invalid_argument", "rule id must not be set")
	}
	if _, pin, err := rules.PinAndRoom(info); err == nil && pin == rules.PinMask {
		return nil, newError("invalid_argument", "a PIN can only hold digits")
	}
	return s.saveRule(ctx, project, info)
}

func (s *Service) saveRule(ctx context.Context, project string, info *pb.SIPDispatchRuleInfo) (*store.DispatchRule, *Error) {
	if err := rules.ValidateRule(info); err != nil {
		return nil, newError("invalid_argument", "%s", err.Error())
	}
	var conflict error
	saved, err := s.store.SaveDispatchRule(ctx, project, info, func(others []*pb.SIPDispatchRuleInfo) error {
		conflict = rules.ValidateRules(append(others, info))
		return conflict
	})
	if err != nil {
		if conflict != nil {
			return nil, newError("invalid_argument", "%s", conflict.Error())
		}
		if errors.Is(err, store.ErrNotFound) {
			return nil, errRuleMissing
		}
		return nil, s.internal(err)
	}
	return saved, nil
}

// UpdateRule changes a dispatch rule. A rule sent back with the PIN mask in it keeps the PIN it has.
func (s *Service) UpdateRule(ctx context.Context, project string, r *pb.UpdateSIPDispatchRuleRequest) (*store.DispatchRule, *Error) {
	if s.store == nil {
		return nil, errNotConnected
	}
	if r.SipDispatchRuleId == "" {
		return nil, newError("invalid_argument", "rule id must be set")
	}
	current, err := s.store.DispatchRuleByID(ctx, project, r.SipDispatchRuleId, true)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errRuleMissing
		}
		return nil, s.internal(err)
	}
	next := proto.Clone(current.Info).(*pb.SIPDispatchRuleInfo)
	switch a := r.Action.(type) {
	case *pb.UpdateSIPDispatchRuleRequest_Replace:
		if a.Replace == nil {
			return nil, newError("invalid_argument", "missing dispatch rule")
		}
		if a.Replace.SipDispatchRuleId != "" && a.Replace.SipDispatchRuleId != r.SipDispatchRuleId {
			return nil, newError("invalid_argument", "rule id in the info must be empty or match the id in the update")
		}
		next = proto.Clone(a.Replace).(*pb.SIPDispatchRuleInfo)
		next.SipDispatchRuleId = r.SipDispatchRuleId
	case *pb.UpdateSIPDispatchRuleRequest_Update:
		if a.Update == nil {
			return nil, newError("invalid_argument", "missing dispatch rule update")
		}
		if err := rules.ApplyRuleUpdate(next, a.Update); err != nil {
			return nil, newError("invalid_argument", "%s", err.Error())
		}
	default:
		return nil, newError("invalid_argument", "missing or unsupported update action")
	}
	if _, pin, err := rules.PinAndRoom(next); err == nil && pin == rules.PinMask {
		_, realPin, _ := rules.PinAndRoom(current.Info)
		rules.SetPin(next, realPin)
	}
	return s.saveRule(ctx, project, next)
}

func (s *Service) createRule(ctx context.Context, r *request, req *pb.CreateSIPDispatchRuleRequest) (*pb.SIPDispatchRuleInfo, *Error) {
	if e := r.needAdmin(); e != nil {
		return nil, e
	}
	info := req.DispatchRule
	if info == nil {
		// The old shape of this request.
		info = &pb.SIPDispatchRuleInfo{
			Rule: req.Rule, TrunkIds: req.TrunkIds, InboundNumbers: req.InboundNumbers, HidePhoneNumber: req.HidePhoneNumber,
			Name: req.Name, Metadata: req.Metadata, Attributes: req.Attributes, RoomPreset: req.RoomPreset, RoomConfig: req.RoomConfig,
		}
	}
	saved, e := s.CreateRule(ctx, r.project, info)
	if e != nil {
		return nil, e
	}
	return saved.Info, nil
}

func (s *Service) updateRule(ctx context.Context, r *request, req *pb.UpdateSIPDispatchRuleRequest) (*pb.SIPDispatchRuleInfo, *Error) {
	if e := r.needAdmin(); e != nil {
		return nil, e
	}
	saved, e := s.UpdateRule(ctx, r.project, req)
	if e != nil {
		return nil, e
	}
	return saved.Info, nil
}

func (s *Service) listRules(ctx context.Context, r *request, req *pb.ListSIPDispatchRuleRequest) (*pb.ListSIPDispatchRuleResponse, *Error) {
	if e := r.needAdmin(); e != nil {
		return nil, e
	}
	if s.store == nil {
		return nil, errNotConnected
	}
	list, err := s.store.ListDispatchRules(ctx, r.project, false)
	if err != nil {
		return nil, s.internal(err)
	}
	items := make([]*pb.SIPDispatchRuleInfo, 0, len(list))
	for _, info := range list {
		if len(req.TrunkIds) != 0 && len(info.TrunkIds) != 0 && !slices.ContainsFunc(info.TrunkIds, func(id string) bool { return slices.Contains(req.TrunkIds, id) }) {
			continue
		}
		items = append(items, info)
	}
	items = inOrder(items, req.DispatchRuleIds, func(i *pb.SIPDispatchRuleInfo) string { return i.SipDispatchRuleId }, func() *pb.SIPDispatchRuleInfo { return &pb.SIPDispatchRuleInfo{} })
	return &pb.ListSIPDispatchRuleResponse{Items: items}, nil
}

func (s *Service) deleteRule(ctx context.Context, r *request, req *pb.DeleteSIPDispatchRuleRequest) (*pb.SIPDispatchRuleInfo, *Error) {
	if e := r.needAdmin(); e != nil {
		return nil, e
	}
	if s.store == nil {
		return nil, errNotConnected
	}
	if req.SipDispatchRuleId == "" {
		return nil, newError("invalid_argument", "dispatch rule ID is required")
	}
	saved, err := s.store.DeleteDispatchRule(ctx, r.project, req.SipDispatchRuleId)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errRuleMissing
		}
		return nil, s.internal(err)
	}
	return saved.Info, nil
}

// --- Calls ---

func (s *Service) callLimit() int {
	if n := s.cfg.Media.SIP.MaxCallsPerProject; n > 0 {
		return n
	}
	return defaultCallsPerProject
}

// PlaceCall queues an outbound call and waits for a worker to get as far as the request asked: the participant joining the room,
// or with wait_until_answered, the phone being answered.
func (s *Service) PlaceCall(ctx context.Context, project string, req *pb.CreateSIPParticipantRequest) (*store.SIPCall, *Error) {
	if s.store == nil {
		return nil, errNotConnected
	}
	if err := rules.ValidateCreateParticipant(req); err != nil {
		return nil, newError("invalid_argument", "%s", err.Error())
	}
	job, e := s.buildOutbound(ctx, project, req)
	if e != nil {
		return nil, e
	}
	if !s.dials.Allow(project) {
		return nil, newError("resource_exhausted", "this project has started too many calls recently")
	}
	available, err := s.store.SIPCapacityAvailable(ctx)
	if err != nil {
		return nil, s.internal(err)
	}
	if !available {
		return nil, newError("unavailable", "no SIP worker is available")
	}
	call := &store.SIPCall{
		ID: participant.NewID("SCL_"), ProjectID: project, TrunkID: req.SipTrunkId, RoomName: req.RoomName, ToHost: job.Address,
		ParticipantIdentity: req.ParticipantIdentity,
	}
	if call.ParticipantIdentity == "" {
		call.ParticipantIdentity = "sip_" + req.SipCallTo
		if req.HidePhoneNumber {
			call.ParticipantIdentity, _ = rules.HideNumber(req.SipCallTo)
		}
	}
	if !req.HidePhoneNumber {
		call.FromUser, call.ToUser = job.Number, req.SipCallTo
	}
	saved, err := s.store.QueueOutboundSIPCall(ctx, call, job, s.callLimit())
	if err != nil {
		if errors.Is(err, store.ErrSIPCallLimit) {
			return nil, newError("resource_exhausted", "%s", err.Error())
		}
		return nil, s.internal(err)
	}
	if err := s.store.RecordSIPEvent(ctx, saved, "info", "sip_call_requested", "An outbound call was requested."); err != nil {
		s.log.Warn("could not record a sip event", "error", err)
	}
	wait := joinWait
	if req.WaitUntilAnswered {
		wait = answerWait
	}
	if deadline, ok := ctx.Deadline(); ok {
		wait = min(wait, time.Until(deadline))
	}
	return s.waitForCall(ctx, saved, req.WaitUntilAnswered, wait)
}

func (s *Service) waitForCall(ctx context.Context, call *store.SIPCall, untilAnswered bool, wait time.Duration) (*store.SIPCall, *Error) {
	deadline := time.Now().Add(wait)
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		current, err := s.store.SIPCallByID(ctx, call.ProjectID, call.ID)
		if err != nil {
			return nil, s.internal(err)
		}
		switch {
		case !current.Open():
			return nil, callFailure(current)
		case untilAnswered && current.Status == store.SIPActive:
			return current, nil
		case !untilAnswered && current.ParticipantSID != "":
			return current, nil
		}
		if time.Now().After(deadline) {
			// Nobody is waiting for this any more, and nobody could tell it to stop if it were left running.
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			_, _ = s.store.RequestSIPCallStop(stopCtx, call.ProjectID, call.ID)
			cancel()
			return nil, newError("deadline_exceeded", "the call did not get far enough in time")
		}
		select {
		case <-ctx.Done():
			return nil, newError("canceled", "the request was canceled")
		case <-t.C:
		}
	}
}

func callFailure(c *store.SIPCall) *Error {
	if c.SIPStatusCode >= 300 {
		return sipStatusError(c.SIPStatusCode, c.SIPStatus)
	}
	if c.Error != "" {
		return newError("unavailable", "%s", c.Error)
	}
	return newError("aborted", "the call ended before it was answered")
}

// buildOutbound puts together what a worker needs from the request and its trunk. Credentials come from the trunk here, or from the
// request itself, and from here on only exist sealed.
func (s *Service) buildOutbound(ctx context.Context, project string, req *pb.CreateSIPParticipantRequest) (*store.SIPOutboundRequest, *Error) {
	job := &store.SIPOutboundRequest{
		CallTo: req.SipCallTo, ToUserOverride: req.ToUserOverride, DisplayName: req.DisplayName, Name: req.ParticipantName, Metadata: req.ParticipantMetadata,
		Dtmf: req.Dtmf, Number: req.SipNumber,
	}
	media := rules.UpgradeMedia(req.Media, req.MediaEncryption)
	var trunkHeaders map[string]string
	switch {
	case req.SipTrunkId != "":
		t, err := s.store.OutboundTrunkByID(ctx, project, req.SipTrunkId, true)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, errTrunkMissing
			}
			return nil, s.internal(err)
		}
		job.Address, job.Transport, job.Username, job.Password = t.Info.Address, int32(t.Info.Transport), t.Info.AuthUsername, t.Info.AuthPassword
		job.FromHost = t.Info.FromHost
		job.HeadersToAttributes, job.AttributesToHeaders = t.Info.HeadersToAttributes, t.Info.AttributesToHeaders
		job.IncludeHeaders = int32(t.Info.IncludeHeaders)
		trunkHeaders = t.Info.Headers
		media = rules.MergeMedia(rules.UpgradeMedia(t.Info.Media, t.Info.MediaEncryption), media)
		if job.Number == "" {
			if len(t.Info.Numbers) == 0 {
				return nil, newError("failed_precondition", "no numbers on outbound trunk")
			}
			job.Number = t.Info.Numbers[rand.IntN(len(t.Info.Numbers))]
		}
	case req.Trunk != nil:
		t := req.Trunk
		job.Address, job.Transport, job.Username, job.Password = t.Hostname, int32(t.Transport), t.AuthUsername, t.AuthPassword
		job.FromHost = t.FromHost
		job.HeadersToAttributes, job.AttributesToHeaders = t.HeadersToAttributes, t.AttributesToHeaders
		if job.Number == "" {
			return nil, newError("failed_precondition", "no numbers on outbound trunk")
		}
	}
	if e := s.checkOutboundAddress(ctx, job.Address); e != nil {
		return nil, e
	}
	if len(req.Headers) != 0 || len(trunkHeaders) != 0 {
		job.Headers = map[string]string{}
		for k, v := range trunkHeaders {
			job.Headers[k] = v
		}
		for k, v := range req.Headers {
			job.Headers[k] = v
		}
	}
	if req.IncludeHeaders != pb.SIPHeaderOptions_SIP_NO_HEADERS {
		job.IncludeHeaders = int32(req.IncludeHeaders)
	}
	data, err := protojson.Marshal(media)
	if err != nil {
		return nil, s.internal(err)
	}
	job.MediaJSON = data
	job.Dialtone = int32(req.Dialtone)
	if req.Dialtone == pb.SIPDialtone_SIP_DIALTONE_DISABLED && (req.PlayDialtone || req.PlayRingtone) {
		job.Dialtone = int32(pb.SIPDialtone_SIP_DIALTONE_ETSI)
	}
	if req.RingingTimeout != nil {
		job.RingingMs = req.RingingTimeout.AsDuration().Milliseconds()
	}
	if req.MaxCallDuration != nil {
		job.MaxDurationMs = req.MaxCallDuration.AsDuration().Milliseconds()
	}
	job.Attributes = map[string]string{}
	for k, v := range req.ParticipantAttributes {
		job.Attributes[k] = v
	}
	job.Attributes[rules.AttrTrunkID] = req.SipTrunkId
	if !req.HidePhoneNumber {
		job.Attributes[rules.AttrPhoneNumber] = req.SipCallTo
		job.Attributes[rules.AttrHostname] = job.Address
		job.Attributes[rules.AttrTrunkNumber] = job.Number
	}
	return job, nil
}

func (s *Service) createParticipant(ctx context.Context, r *request, req *pb.CreateSIPParticipantRequest) (*pb.SIPParticipantInfo, *Error) {
	if e := r.needCall(); e != nil {
		return nil, e
	}
	call, e := s.PlaceCall(ctx, r.project, req)
	if e != nil {
		return nil, e
	}
	return &pb.SIPParticipantInfo{
		ParticipantId: call.ParticipantSID, ParticipantIdentity: call.ParticipantIdentity, RoomName: call.RoomName, SipCallId: call.ID,
	}, nil
}

func (s *Service) transferParticipant(ctx context.Context, r *request, req *pb.TransferSIPParticipantRequest) (*pb.TransferSIPParticipantResponse, *Error) {
	if req.RoomName == "" {
		return nil, newError("invalid_argument", "Missing room name")
	}
	if req.ParticipantIdentity == "" {
		return nil, newError("invalid_argument", "Missing participant identity")
	}
	if e := r.needCall(); e != nil {
		return nil, e
	}
	if !r.grants.Video.RoomAdmin || r.grants.Video.Room != req.RoomName {
		return nil, errDenied
	}
	if s.store == nil {
		return nil, errNotConnected
	}
	if req.TransferTo == "" {
		return nil, newError("invalid_argument", "missing transfer to")
	}
	to, err := rules.NormalizeTransferTo(req.TransferTo)
	if err != nil {
		return nil, newError("invalid_argument", "%s", err.Error())
	}
	if err := rules.ValidateHeaders(req.Headers); err != nil {
		return nil, newError("invalid_argument", "%s", err.Error())
	}
	call, err := s.store.OpenSIPCallInRoom(ctx, r.project, req.RoomName, req.ParticipantIdentity)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, newError("invalid_argument", "no SIP session associated with participant")
		}
		return nil, s.internal(err)
	}
	wait := transferWait
	if req.RingingTimeout != nil {
		wait = req.RingingTimeout.AsDuration()
	}
	if deadline, ok := ctx.Deadline(); ok {
		wait = min(wait, time.Until(deadline))
	}
	dialtone := int32(req.Dialtone)
	if req.Dialtone == pb.SIPDialtone_SIP_DIALTONE_DISABLED && req.PlayDialtone {
		dialtone = int32(pb.SIPDialtone_SIP_DIALTONE_ETSI)
	}
	transfer, err := s.store.CreateSIPTransfer(ctx, call, to, store.SIPTransferRequest{Headers: req.Headers, Dialtone: dialtone, TimeoutMs: wait.Milliseconds()})
	if err != nil {
		if errors.Is(err, store.ErrSIPTransferBusy) {
			return nil, newError("failed_precondition", "%s", err.Error())
		}
		return nil, s.internal(err)
	}
	deadline := time.Now().Add(wait)
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		current, err := s.store.SIPTransferByID(ctx, r.project, transfer.ID)
		if err != nil {
			return nil, s.internal(err)
		}
		if current.Status == store.SIPTransferSuccessful {
			res := &pb.TransferSIPParticipantResponse{TransferId: current.ID, Status: pb.SIPTransferStatus_STS_TRANSFER_SUCCESSFUL, Reason: pb.SIPTransferReason_STR_COMPLETED}
			if current.SIPStatusCode != 0 {
				res.SipStatus = &pb.SIPStatus{Code: pb.SIPStatusCode(current.SIPStatusCode), Status: current.SIPStatus}
			}
			return res, nil
		}
		if current.Status == store.SIPTransferFailed {
			return nil, transferFailure(current)
		}
		if time.Now().After(deadline) {
			return nil, newError("deadline_exceeded", "the transfer did not finish in time")
		}
		select {
		case <-ctx.Done():
			return nil, newError("canceled", "the request was canceled")
		case <-t.C:
		}
	}
}

func transferFailure(t *store.SIPTransfer) *Error {
	e := &Error{Code: "failed_precondition", Msg: "sip transfer failed: " + t.Reason, Meta: map[string]string{"sip_transfer_id": t.ID, "sip_transfer_reason": t.Reason}}
	if t.SIPStatusCode != 0 {
		e.Code = twirpCodeForSIP(t.SIPStatusCode)
		e.Msg = fmt.Sprintf("sip transfer failed: %s: sip status %d: %s", t.Reason, t.SIPStatusCode, t.SIPStatus)
		e.Meta["sip_status_code"] = strconv.Itoa(t.SIPStatusCode)
		e.Meta["sip_status"] = t.SIPStatus
	} else if t.Error != "" {
		e.Msg = "sip transfer failed: " + t.Error
	}
	return e
}
