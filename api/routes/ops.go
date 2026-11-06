package routes

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"elnova-streamkit.local/api/recorder"
	"elnova-streamkit.local/api/schemas"
	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/api/webhook"
	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/room"
)

func (s *Server) listWebhooks(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	items, err := s.store.ListWebhooks(r.Context(), c.projectID())
	if err != nil {
		return err
	}
	if items == nil {
		items = []store.Webhook{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}

func (s *Server) webhookEvents(w http.ResponseWriter, _ *http.Request, _ *reqCtx) error {
	writeJSON(w, http.StatusOK, map[string]any{"items": schemas.WebhookEvents})
	return nil
}

func (s *Server) checkWebhookURL(raw string) error {
	if err := webhook.CheckURL(raw, s.cfg.ControlPlane.AllowPrivateWebhooks); err != nil {
		return &APIError{Status: http.StatusUnprocessableEntity, Code: "validation_failed", Msg: "Some fields need attention.",
			Fields: schemas.FieldErrors{"url": "cannot point at a private address"}}
	}
	return nil
}

func (s *Server) createWebhook(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	var in schemas.Webhook
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if err := s.checkWebhookURL(in.URL); err != nil {
		return err
	}
	enabled := in.Enabled == nil || *in.Enabled
	events := in.Events
	if events == nil {
		events = []string{}
	}
	hook, err := s.store.CreateWebhook(r.Context(), store.Webhook{
		ProjectID: c.project.ID, URL: in.URL, APIKeyID: in.APIKeyID, Events: events, Enabled: enabled,
	})
	if errors.Is(err, store.ErrNotFound) {
		return &APIError{Status: http.StatusUnprocessableEntity, Code: "validation_failed", Msg: "Some fields need attention.",
			Fields: schemas.FieldErrors{"api_key_id": "is not an active key in this project"}}
	}
	if err != nil {
		return err
	}
	s.audit(r, c, "webhook.created", hook.URL, nil)
	writeJSON(w, http.StatusCreated, hook)
	return nil
}

func (s *Server) getWebhook(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	hook, err := s.store.WebhookByID(r.Context(), c.projectID(), r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, hook)
	return nil
}

func (s *Server) updateWebhook(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	var in schemas.Webhook
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if err := s.checkWebhookURL(in.URL); err != nil {
		return err
	}
	enabled := in.Enabled == nil || *in.Enabled
	events := in.Events
	if events == nil {
		events = []string{}
	}
	hook, err := s.store.UpdateWebhook(r.Context(), store.Webhook{
		ID: r.PathValue("id"), ProjectID: c.project.ID, URL: in.URL, APIKeyID: in.APIKeyID, Events: events, Enabled: enabled,
	})
	if err != nil {
		return err
	}
	s.audit(r, c, "webhook.updated", hook.URL, nil)
	writeJSON(w, http.StatusOK, hook)
	return nil
}

func (s *Server) deleteWebhook(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := s.store.DeleteWebhook(r.Context(), c.project.ID, r.PathValue("id")); err != nil {
		return err
	}
	s.audit(r, c, "webhook.deleted", r.PathValue("id"), nil)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) listDeliveries(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	items, err := s.store.ListDeliveries(r.Context(), c.projectID(), r.PathValue("id"), queryInt(r, "limit", defaultPage, maxPage))
	if err != nil {
		return err
	}
	if items == nil {
		items = []store.Delivery{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}

func (s *Server) testWebhook(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	hook, err := s.store.WebhookByID(r.Context(), c.project.ID, r.PathValue("id"))
	if err != nil {
		return err
	}
	now := time.Now()
	body, err := recorder.EncodeEvent(room.Event{
		Type: room.EventRoomStarted, At: now, ProjectID: c.project.ID,
		Room: &pb.Room{Sid: "RM_test", Name: "test-delivery", CreationTime: now.Unix()},
	})
	if err != nil {
		return err
	}
	id, err := s.store.EnqueueDelivery(r.Context(), hook.ID, "test", body)
	if err != nil {
		return err
	}
	s.webhook.Notify()
	writeJSON(w, http.StatusAccepted, map[string]string{"delivery_id": id})
	return nil
}

func (s *Server) retryDelivery(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := s.store.RetryDelivery(r.Context(), c.project.ID, r.PathValue("id")); err != nil {
		return err
	}
	s.webhook.Notify()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
	return nil
}

func (s *Server) analyticsOverview(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needTokenGrant(c, grantList); err != nil {
		return err
	}
	from, to := parseWindow(r, 30*24*time.Hour)
	id := c.projectID()
	rooms, avgSeconds, err := s.store.RoomTotals(r.Context(), id, from, to)
	if err != nil {
		return err
	}
	points, err := s.store.UsageSeries(r.Context(), id, "day", from, to)
	if err != nil {
		return err
	}
	var total store.UsagePoint
	for _, p := range points {
		total.ParticipantMinutes += p.ParticipantMinutes
		total.RoomMinutes += p.RoomMinutes
		total.AudioMinutes += p.AudioMinutes
		total.VideoMinutes += p.VideoMinutes
		total.RecordingMinutes += p.RecordingMinutes
		total.BytesIn += p.BytesIn
		total.BytesOut += p.BytesOut
		total.PacketsIn += p.PacketsIn
		total.PacketsLost += p.PacketsLost
		total.ConnectionFailures += p.ConnectionFailures
		total.PeakParticipants = max(total.PeakParticipants, p.PeakParticipants)
		total.PeakRooms = max(total.PeakRooms, p.PeakRooms)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"from": from, "to": to, "total_rooms": rooms, "avg_room_seconds": avgSeconds, "totals": total,
	})
	return nil
}

func (s *Server) analyticsUsage(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := needTokenGrant(c, grantList); err != nil {
		return err
	}
	from, to := parseWindow(r, 30*24*time.Hour)
	interval := r.URL.Query().Get("interval")
	points, err := s.store.UsageSeries(r.Context(), c.projectID(), interval, from, to)
	if err != nil {
		return err
	}
	if points == nil {
		points = []store.UsagePoint{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": points})
	return nil
}

// monitorSnapshot names everyone connected across the project, along with the node, which no grant covers. A token that
// may list rooms still cannot ask who is in one, so this is for project members.
func (s *Server) monitorSnapshot(w http.ResponseWriter, _ *http.Request, c *reqCtx) error {
	writeJSON(w, http.StatusOK, s.monitor.View(c.projectID()))
	return nil
}

// liveUpgrader allows only pages from allowed origins. This endpoint authenticates by cookie, so unlike
// the token-authenticated signalling socket, any site could otherwise open it on a signed-in user's behalf.
func (s *Server) liveUpgrader() websocket.Upgrader {
	return websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			if origin == "" {
				return true
			}
			if d := s.cfg.ControlPlane.PublicDashboardURL; d != "" {
				return origin == d
			}
			// With no configured dashboard address, only the development default is trusted.
			return s.cfg.Development || origin == "http://localhost:3000"
		},
	}
}

func (s *Server) monitorLive(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	up := s.liveUpgrader()
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return nil // Upgrade already wrote the response.
	}
	defer func() { _ = conn.Close() }()

	done := make(chan struct{})
	var once sync.Once
	go func() {
		// Reading is how a closed connection is noticed. Nothing the client sends matters.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				once.Do(func() { close(done) })
				return
			}
		}
	}()
	ticks, stop := s.monitor.Subscribe()
	defer stop()
	id := c.projectID()
	for {
		select {
		case <-done:
			return nil
		case <-r.Context().Done():
			return nil
		case <-ticks:
			_ = conn.SetWriteDeadline(time.Now().Add(liveSendTimeout))
			if err := conn.WriteJSON(s.monitor.View(id)); err != nil {
				return nil
			}
		}
	}
}

func (s *Server) listServers(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	items, err := s.store.ListServers(r.Context(), time.Minute)
	if err != nil {
		return err
	}
	if items == nil {
		items = []store.ServerNode{}
	}
	if !c.user.IsPlatformAdmin {
		for i := range items {
			items[i] = withoutMachineDetails(items[i])
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}

// publicNodeStats are the figures anyone signed in may see: enough to tell whether media services are up. An
// installation can host several organizations, and hostnames, addresses and overall load describe the operator's
// infrastructure and other tenants' activity.
var publicNodeStats = []string{"kind", "healthy", "draining", "uptime_seconds"}

func withoutMachineDetails(n store.ServerNode) store.ServerNode {
	n.Address = ""
	var stats map[string]json.RawMessage
	if err := json.Unmarshal(n.Stats, &stats); err != nil {
		n.Stats = json.RawMessage(`{}`)
		return n
	}
	kept := map[string]json.RawMessage{}
	for _, key := range publicNodeStats {
		if v, ok := stats[key]; ok {
			kept[key] = v
		}
	}
	data, err := json.Marshal(kept)
	if err != nil {
		data = []byte(`{}`)
	}
	n.Stats = data
	return n
}

func (s *Server) drainServer(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	var in schemas.Drain
	if err := decode(w, r, &in); err != nil {
		return err
	}
	id := r.PathValue("id")
	if err := s.store.SetServerDraining(r.Context(), id, in.Draining); err != nil {
		return err
	}
	if id == s.rooms.NodeID() {
		s.rooms.SetDraining(in.Draining)
	}
	s.audit(r, c, "server.drain", id, in)
	writeJSON(w, http.StatusOK, map[string]any{"node_id": id, "draining": in.Draining})
	return nil
}

func (s *Server) listLogs(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("cursor"), 10, 64)
	limit := queryInt(r, "limit", defaultPage, maxPage)
	items, err := s.store.ListEvents(r.Context(), store.EventFilter{
		ProjectID: c.projectID(), Category: q.Get("category"), Level: q.Get("level"),
		Search: q.Get("q"), RoomName: q.Get("room"), Before: before, Limit: limit,
	})
	if err != nil {
		return err
	}
	if items == nil {
		items = []store.Event{}
	}
	next := ""
	if len(items) == limit {
		next = strconv.FormatInt(items[len(items)-1].ID, 10)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
	return nil
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	before, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
	limit := queryInt(r, "limit", defaultPage, maxPage)
	items, err := s.store.ListAudit(r.Context(), c.project.ID, before, limit)
	if err != nil {
		return err
	}
	if items == nil {
		items = []store.AuditEntry{}
	}
	next := ""
	if len(items) == limit {
		next = strconv.FormatInt(items[len(items)-1].ID, 10)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
	return nil
}

//go:embed openapi.yaml
var openAPISpec []byte

func (s *Server) openAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write(openAPISpec)
}

// Project settings are read on every join, so they are cached. A change reaches other nodes when this expires.
const settingsTTL = 30 * time.Second

type cachedSettings struct {
	config  room.ProjectConfig
	expires time.Time
}

// ProjectConfig implements the room manager's settings lookup.
func (s *Server) ProjectConfig(projectID string) room.ProjectConfig {
	now := time.Now()
	s.settingsMu.Lock()
	entry, ok := s.settings[projectID]
	s.settingsMu.Unlock()
	if ok && now.Before(entry.expires) {
		return entry.config
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	project, err := s.store.ProjectByID(ctx, projectID)
	if err != nil {
		// Unknown projects, such as static keys, and outages both fall back to what we had, or to defaults.
		return entry.config
	}
	cfg := room.ProjectConfig{
		EmptyTimeout:     project.Settings.EmptyTimeout,
		DepartureTimeout: project.Settings.DepartureTimeout,
		MaxParticipants:  project.Settings.MaxParticipants,
		AutoCreate:       project.Settings.AutoCreateRooms,
		AllowedOrigins:   project.Settings.AllowedOrigins,
	}
	for _, server := range project.Settings.ICEServers {
		cfg.ICEServers = append(cfg.ICEServers, &pb.ICEServer{Urls: server.URLs, Username: server.Username, Credential: server.Credential})
	}
	s.settingsMu.Lock()
	if s.settings == nil {
		s.settings = map[string]cachedSettings{}
	}
	s.settings[projectID] = cachedSettings{cfg, now.Add(settingsTTL)}
	s.settingsMu.Unlock()
	return cfg
}

func (s *Server) invalidateProject(id string) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	delete(s.settings, id)
}
