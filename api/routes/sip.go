package routes

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/pkg/pb"
	sipapi "elnova-streamkit.local/sip/api"
)

// These are for the dashboard: a person signed in, or a token with the sip grant to read. The SDKs and CLIs use the Twirp API, which
// does the same things through the same code. Bodies are the API's own messages, in JSON. Nothing here ever returns a credential.

func needSIPAdmin(c *reqCtx) error {
	if c.token != nil && (c.token.Grants.SIP == nil || !c.token.Grants.SIP.Admin) {
		return errForbidden
	}
	return nil
}

func sipError(e *sipapi.Error) error {
	return errorf(e.HTTPStatus(), e.Code, e.Msg)
}

// decodeProto reads a body into an API message. A field it does not know is refused, because here it can only be a mistake.
func decodeProto(w http.ResponseWriter, r *http.Request, into proto.Message) error {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		return errorf(http.StatusBadRequest, "bad_request", "The request body could not be read.")
	}
	if err := protojson.Unmarshal(data, into); err != nil {
		return errorf(http.StatusBadRequest, "bad_request", "The request body is not valid: "+err.Error())
	}
	return nil
}

var protoJSON = protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}

// protoView turns a message into a map, so a field or two can be added for the dashboard.
func protoView(m proto.Message, extra map[string]any) (map[string]any, error) {
	data, err := protoJSON.Marshal(m)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	for k, v := range extra {
		out[k] = v
	}
	return out, nil
}

func inboundView(t *store.InboundTrunk) (map[string]any, error) {
	return protoView(t.Info, map[string]any{"has_credentials": t.HasCredentials})
}

func outboundView(t *store.OutboundTrunk) (map[string]any, error) {
	return protoView(t.Info, map[string]any{"has_credentials": t.HasCredentials})
}

func (s *Server) listSIPInbound(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needSIPAdmin(c); err != nil {
		return err
	}
	trunks, err := s.store.ListInboundTrunks(r.Context(), c.projectID())
	if err != nil {
		return err
	}
	items := make([]map[string]any, 0, len(trunks))
	for _, t := range trunks {
		view, err := inboundView(t)
		if err != nil {
			return err
		}
		items = append(items, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}

func (s *Server) getSIPInbound(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needSIPAdmin(c); err != nil {
		return err
	}
	t, err := s.store.InboundTrunkByID(r.Context(), c.projectID(), r.PathValue("id"), false)
	if err != nil {
		return err
	}
	view, err := inboundView(t)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, view)
	return nil
}

func (s *Server) createSIPInbound(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	in := &pb.SIPInboundTrunkInfo{}
	if err := decodeProto(w, r, in); err != nil {
		return err
	}
	t, e := s.sip.CreateInbound(r.Context(), c.projectID(), in)
	if e != nil {
		return sipError(e)
	}
	s.audit(r, c, "sip.inbound_trunk.create", t.Info.SipTrunkId, nil)
	view, err := inboundView(t)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, view)
	return nil
}

func (s *Server) updateSIPInbound(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	in := &pb.SIPInboundTrunkUpdate{}
	if err := decodeProto(w, r, in); err != nil {
		return err
	}
	id := r.PathValue("id")
	t, e := s.sip.UpdateInbound(r.Context(), c.projectID(), &pb.UpdateSIPInboundTrunkRequest{SipTrunkId: id, Action: &pb.UpdateSIPInboundTrunkRequest_Update{Update: in}})
	if e != nil {
		return sipError(e)
	}
	s.audit(r, c, "sip.inbound_trunk.update", id, nil)
	view, err := inboundView(t)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, view)
	return nil
}

func (s *Server) listSIPOutbound(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needSIPAdmin(c); err != nil {
		return err
	}
	trunks, err := s.store.ListOutboundTrunks(r.Context(), c.projectID())
	if err != nil {
		return err
	}
	items := make([]map[string]any, 0, len(trunks))
	for _, t := range trunks {
		view, err := outboundView(t)
		if err != nil {
			return err
		}
		items = append(items, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}

func (s *Server) getSIPOutbound(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needSIPAdmin(c); err != nil {
		return err
	}
	t, err := s.store.OutboundTrunkByID(r.Context(), c.projectID(), r.PathValue("id"), false)
	if err != nil {
		return err
	}
	view, err := outboundView(t)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, view)
	return nil
}

func (s *Server) createSIPOutbound(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	in := &pb.SIPOutboundTrunkInfo{}
	if err := decodeProto(w, r, in); err != nil {
		return err
	}
	t, e := s.sip.CreateOutbound(r.Context(), c.projectID(), in)
	if e != nil {
		return sipError(e)
	}
	s.audit(r, c, "sip.outbound_trunk.create", t.Info.SipTrunkId, nil)
	view, err := outboundView(t)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, view)
	return nil
}

func (s *Server) updateSIPOutbound(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	in := &pb.SIPOutboundTrunkUpdate{}
	if err := decodeProto(w, r, in); err != nil {
		return err
	}
	id := r.PathValue("id")
	t, e := s.sip.UpdateOutbound(r.Context(), c.projectID(), &pb.UpdateSIPOutboundTrunkRequest{SipTrunkId: id, Action: &pb.UpdateSIPOutboundTrunkRequest_Update{Update: in}})
	if e != nil {
		return sipError(e)
	}
	s.audit(r, c, "sip.outbound_trunk.update", id, nil)
	view, err := outboundView(t)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, view)
	return nil
}

func (s *Server) deleteSIPTrunk(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	id := r.PathValue("id")
	removed, err := s.store.DeleteSIPTrunk(r.Context(), c.projectID(), id)
	if err != nil {
		return err
	}
	if !removed {
		return errNotFound
	}
	s.audit(r, c, "sip.trunk.delete", id, nil)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) listSIPRules(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needSIPAdmin(c); err != nil {
		return err
	}
	list, err := s.store.ListDispatchRules(r.Context(), c.projectID(), false)
	if err != nil {
		return err
	}
	items := make([]map[string]any, 0, len(list))
	for _, info := range list {
		view, err := protoView(info, nil)
		if err != nil {
			return err
		}
		items = append(items, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}

func (s *Server) createSIPRule(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	in := &pb.SIPDispatchRuleInfo{}
	if err := decodeProto(w, r, in); err != nil {
		return err
	}
	saved, e := s.sip.CreateRule(r.Context(), c.projectID(), in)
	if e != nil {
		return sipError(e)
	}
	s.audit(r, c, "sip.dispatch_rule.create", saved.Info.SipDispatchRuleId, nil)
	view, err := protoView(saved.Info, nil)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, view)
	return nil
}

func (s *Server) updateSIPRule(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	in := &pb.SIPDispatchRuleUpdate{}
	if err := decodeProto(w, r, in); err != nil {
		return err
	}
	id := r.PathValue("id")
	saved, e := s.sip.UpdateRule(r.Context(), c.projectID(), &pb.UpdateSIPDispatchRuleRequest{SipDispatchRuleId: id, Action: &pb.UpdateSIPDispatchRuleRequest_Update{Update: in}})
	if e != nil {
		return sipError(e)
	}
	s.audit(r, c, "sip.dispatch_rule.update", id, nil)
	view, err := protoView(saved.Info, nil)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, view)
	return nil
}

func (s *Server) deleteSIPRule(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	id := r.PathValue("id")
	if _, err := s.store.DeleteDispatchRule(r.Context(), c.projectID(), id); err != nil {
		return err
	}
	s.audit(r, c, "sip.dispatch_rule.delete", id, nil)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) listSIPCalls(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needSIPAdmin(c); err != nil {
		return err
	}
	calls, next, err := s.store.ListSIPCalls(r.Context(), c.projectID(), r.URL.Query().Get("cursor"), queryInt(r, "limit", defaultPage, maxPage))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": calls, "next_cursor": next})
	return nil
}

func (s *Server) getSIPCall(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needSIPAdmin(c); err != nil {
		return err
	}
	call, err := s.store.SIPCallByID(r.Context(), c.projectID(), r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, call)
	return nil
}

func (s *Server) createSIPCall(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	in := &pb.CreateSIPParticipantRequest{}
	if err := decodeProto(w, r, in); err != nil {
		return err
	}
	call, e := s.sip.PlaceCall(r.Context(), c.projectID(), in)
	if e != nil {
		return sipError(e)
	}
	s.audit(r, c, "sip.call.create", call.ID, nil)
	writeJSON(w, http.StatusCreated, call)
	return nil
}

func (s *Server) hangupSIPCall(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	call, err := s.store.RequestSIPCallStop(r.Context(), c.projectID(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrSIPCallEnded) {
			return errorf(http.StatusConflict, "sip_call_ended", "That call has already ended.")
		}
		return err
	}
	s.audit(r, c, "sip.call.hangup", call.ID, nil)
	writeJSON(w, http.StatusOK, call)
	return nil
}
