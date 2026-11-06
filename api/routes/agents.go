package routes

import (
	"encoding/json"
	"net/http"
	"time"

	"elnova-streamkit.local/api/schemas"
	"elnova-streamkit.local/api/store"
)

const (
	defaultAgentSessionLimit = 50
	maxAgentSessionLimit     = 200
	// A long session's report is big, but this is a limit on what workers can make the database hold.
	maxAgentSessionBytes = 2 << 20
)

// needWorker checks that a request comes from a worker: an access token with the agent grant and no room join, which only
// someone holding the API secret can sign, and no sign of a browser. A session cookie never gets this far, and neither does
// a page holding such a token, which browsers mark with an Origin header.
func needWorker(r *http.Request, c *reqCtx) error {
	if c.user != nil || c.token == nil {
		return errForbidden
	}
	if r.Header.Get("Origin") != "" {
		return errForbidden
	}
	grant := c.token.Grants.Video
	if !grant.Agent || grant.RoomJoin {
		return errForbidden
	}
	return nil
}

// The key goes in; it never comes back out through these routes.
func (s *Server) saveProviderCredential(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	var in schemas.SaveProviderCredential
	if err := decode(w, r, &in); err != nil {
		return err
	}
	name := in.Name
	if name == "" {
		name = "default"
	}
	saved, err := s.store.SaveProviderCredential(r.Context(), c.project.ID, in.Provider, name, in.Secret, c.userID())
	if err != nil {
		return err
	}
	s.audit(r, c, "provider_credential.saved", saved.ID, map[string]any{"provider": saved.Provider, "name": saved.Name})
	writeJSON(w, http.StatusOK, saved)
	return nil
}

func (s *Server) listProviderCredentials(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	items, err := s.store.ListProviderCredentials(r.Context(), c.project.ID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}

func (s *Server) deleteProviderCredential(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := s.store.DeleteProviderCredential(r.Context(), c.project.ID, r.PathValue("id")); err != nil {
		return err
	}
	s.audit(r, c, "provider_credential.deleted", r.PathValue("id"), nil)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) resolveProviderCredential(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needWorker(r, c); err != nil {
		return err
	}
	var in schemas.ResolveProviderCredential
	if err := decode(w, r, &in); err != nil {
		return err
	}
	name := in.Name
	if name == "" {
		name = "default"
	}
	secret, err := s.store.ProviderSecret(r.Context(), c.projectID(), in.Provider, name)
	if err != nil {
		return err
	}
	s.audit(r, c, "provider_credential.read", in.Provider+"/"+name, map[string]any{"api_key": c.token.Key.APIKey})
	writeJSON(w, http.StatusOK, map[string]any{"secret": secret})
	return nil
}

func unixTime(seconds float64) time.Time {
	return time.UnixMicro(int64(seconds * 1e6))
}

// reportAgentSession takes a worker's report of a session. Numbers come from the worker and are shown as reported. The
// summary columns are worked out here, so they cannot disagree with the turns.
func (s *Server) reportAgentSession(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needWorker(r, c); err != nil {
		return err
	}
	raw := make(json.RawMessage, 0, 1024)
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAgentSessionBytes)).Decode(&raw); err != nil {
		return errorf(http.StatusBadRequest, "bad_request", "The report is not valid JSON, or is too large.")
	}
	var in schemas.AgentSessionReport
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorf(http.StatusBadRequest, "bad_request", "The report has the wrong shape.")
	}
	if errs := in.Validate(); len(errs) > 0 {
		return &APIError{Status: http.StatusUnprocessableEntity, Code: "validation_failed", Msg: "Some fields need attention.", Fields: errs}
	}
	models, err := json.Marshal(in.Models)
	if err != nil {
		return err
	}
	session := store.AgentSession{
		ProjectID: c.projectID(), SessionKey: in.SessionID, Room: in.Room, JobID: in.JobID, AgentName: in.AgentName,
		Pipeline: in.Pipeline, Models: models, StartedAt: unixTime(in.StartedAt), CostUSD: in.CostUSD, Body: raw,
		TurnCount: len(in.Turns), ErrorCount: len(in.Errors),
	}
	if in.EndedAt != nil {
		ended := unixTime(*in.EndedAt)
		session.EndedAt = &ended
	}
	var sum float64
	var measured int
	for _, t := range in.Turns {
		if t.Interrupted {
			session.Interrupted++
		}
		if t.Latency.EndToEnd != nil {
			sum += *t.Latency.EndToEnd
			measured++
		}
	}
	if measured > 0 {
		avg := sum / float64(measured)
		session.AvgEndToEnd = &avg
	}
	saved, err := s.store.SaveAgentSession(r.Context(), session)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, saved)
	return nil
}

func (s *Server) listAgentSessions(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	limit := queryInt(r, "limit", defaultAgentSessionLimit, maxAgentSessionLimit)
	items, err := s.store.ListAgentSessions(r.Context(), c.projectID(), r.URL.Query().Get("room"), limit)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}

func (s *Server) getAgentSession(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	item, err := s.store.AgentSessionByID(r.Context(), c.projectID(), r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, item)
	return nil
}
