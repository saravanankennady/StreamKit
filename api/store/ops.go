package store

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// RoomRecord is one room session, live or finished.
type RoomRecord struct {
	SID               string     `json:"sid"`
	ProjectID         string     `json:"project_id"`
	Name              string     `json:"name"`
	NodeID            string     `json:"node_id"`
	Metadata          string     `json:"metadata"`
	MaxParticipants   int        `json:"max_participants"`
	PeakParticipants  int        `json:"peak_participants"`
	TotalParticipants int        `json:"total_participants"`
	CreatedAt         time.Time  `json:"created_at"`
	EndedAt           *time.Time `json:"ended_at"`
}

// RoomStarted records a new room session.
func (s *Store) RoomStarted(ctx context.Context, r RoomRecord) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO rooms (sid, project_id, name, node_id, metadata, max_participants, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (sid) DO NOTHING`,
		r.SID, r.ProjectID, r.Name, r.NodeID, r.Metadata, r.MaxParticipants, r.CreatedAt)
	return err
}

// RoomFinished closes a room session.
func (s *Store) RoomFinished(ctx context.Context, sid string, at time.Time) error {
	_, err := s.DB.Exec(ctx, `UPDATE rooms SET ended_at = $2 WHERE sid = $1`, sid, at)
	return err
}

// ListRoomHistory pages through a project's rooms, newest first.
func (s *Store) ListRoomHistory(ctx context.Context, projectID string, limit int, before *time.Time) ([]RoomRecord, error) {
	rows, err := s.DB.Query(ctx, `SELECT sid, project_id, name, node_id, metadata, max_participants, peak_participants, total_participants, created_at, ended_at
		FROM rooms WHERE project_id = $1 AND ($2::timestamptz IS NULL OR created_at < $2)
		ORDER BY created_at DESC LIMIT $3`, projectID, before, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[RoomRecord])
}

// ParticipantJoined records a participant session and updates its room's counts.
func (s *Store) ParticipantJoined(ctx context.Context, sid, roomSID, projectID, identity, name, kind string, at time.Time, current int) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO participant_sessions (sid, room_sid, project_id, identity, name, kind, joined_at)
			SELECT $1, $2, $3, $4, $5, $6, $7 WHERE EXISTS (SELECT 1 FROM rooms WHERE sid = $2) ON CONFLICT (sid) DO NOTHING`,
			sid, roomSID, projectID, identity, name, kind, at)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE rooms SET total_participants = total_participants + 1,
			peak_participants = GREATEST(peak_participants, $2) WHERE sid = $1`, roomSID, current)
		return err
	})
}

// ParticipantLeft closes a participant session.
func (s *Store) ParticipantLeft(ctx context.Context, sid, reason string, at time.Time) error {
	_, err := s.DB.Exec(ctx, `UPDATE participant_sessions SET left_at = $2, disconnect_reason = $3 WHERE sid = $1`, sid, at, reason)
	return err
}

// TrackPublished records a track session.
func (s *Store) TrackPublished(ctx context.Context, sid, participantSID, roomSID, projectID, trackType, source, mime string, at time.Time) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO track_sessions (sid, participant_sid, room_sid, project_id, type, source, mime_type, published_at)
		SELECT $1, $2, $3, $4, $5, $6, $7, $8 WHERE EXISTS (SELECT 1 FROM rooms WHERE sid = $3) ON CONFLICT (sid) DO NOTHING`,
		sid, participantSID, roomSID, projectID, trackType, source, mime, at)
	return err
}

// TrackUnpublished closes a track session.
func (s *Store) TrackUnpublished(ctx context.Context, sid string, at time.Time) error {
	_, err := s.DB.Exec(ctx, `UPDATE track_sessions SET unpublished_at = $2 WHERE sid = $1`, sid, at)
	return err
}

// Event is a row on the logs page.
type Event struct {
	ID                  int64           `json:"id"`
	ProjectID           *string         `json:"project_id"`
	Category            string          `json:"category"`
	Level               string          `json:"level"`
	Type                string          `json:"type"`
	Message             string          `json:"message"`
	RoomName            string          `json:"room_name"`
	RoomSID             string          `json:"room_sid"`
	ParticipantIdentity string          `json:"participant_identity"`
	TrackSID            string          `json:"track_sid"`
	Details             json.RawMessage `json:"details"`
	CreatedAt           time.Time       `json:"created_at"`
}

// Event categories on the logs page.
const (
	CategoryRoom        = "room"
	CategoryParticipant = "participant"
	CategoryTrack       = "track"
	CategorySignaling   = "signaling"
	CategoryWebRTC      = "webrtc"
	CategoryRecording   = "recording"
	CategoryAPI         = "api"
	CategoryApplication = "application"
)

// InsertEvents writes a batch of events.
func (s *Store) InsertEvents(ctx context.Context, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, e := range events {
		details := e.Details
		if len(details) == 0 {
			details = json.RawMessage(`{}`)
		}
		batch.Queue(`INSERT INTO events (project_id, category, level, type, message, room_name, room_sid, participant_identity, track_sid, details, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			e.ProjectID, e.Category, e.Level, e.Type, e.Message, e.RoomName, e.RoomSID, e.ParticipantIdentity, e.TrackSID, []byte(details), e.CreatedAt)
	}
	return s.DB.SendBatch(ctx, batch).Close()
}

// EventFilter narrows ListEvents.
type EventFilter struct {
	ProjectID string
	Category  string
	Level     string
	Search    string
	RoomName  string
	Before    int64
	Limit     int
}

// ListEvents pages through a project's events, newest first. Before is the last ID of the previous page.
func (s *Store) ListEvents(ctx context.Context, f EventFilter) ([]Event, error) {
	where := []string{"project_id = $1"}
	args := []any{f.ProjectID}
	add := func(clause string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(clause, "?", "$"+strconv.Itoa(len(args))))
	}
	if f.Category != "" {
		add("category = ?", f.Category)
	}
	if f.Level != "" {
		add("level = ?", f.Level)
	}
	if f.RoomName != "" {
		add("room_name = ?", f.RoomName)
	}
	if f.Search != "" {
		add(`to_tsvector('simple', message || ' ' || type || ' ' || room_name || ' ' || participant_identity) @@ plainto_tsquery('simple', ?)`, f.Search)
	}
	if f.Before > 0 {
		add("id < ?", f.Before)
	}
	args = append(args, f.Limit)
	rows, err := s.DB.Query(ctx, `SELECT id, project_id, category, level, type, message, room_name, room_sid, participant_identity, track_sid, details, created_at
		FROM events WHERE `+strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Event, error) {
		var e Event
		var details []byte
		err := r.Scan(&e.ID, &e.ProjectID, &e.Category, &e.Level, &e.Type, &e.Message, &e.RoomName, &e.RoomSID, &e.ParticipantIdentity, &e.TrackSID, &details, &e.CreatedAt)
		e.Details = details
		return e, err
	})
}

// AuditEntry is one security-relevant action.
type AuditEntry struct {
	ID        int64           `json:"id"`
	ProjectID *string         `json:"project_id"`
	UserID    *string         `json:"user_id"`
	UserEmail *string         `json:"user_email"`
	Action    string          `json:"action"`
	Target    string          `json:"target"`
	IP        string          `json:"ip"`
	Details   json.RawMessage `json:"details"`
	CreatedAt time.Time       `json:"created_at"`
}

// Audit records an action. Failures do not fail the request, since the action has already happened. They
// are returned so the caller can log them.
func (s *Store) Audit(ctx context.Context, projectID, userID, action, target, ip string, details any) error {
	data, err := json.Marshal(details)
	if err != nil || details == nil {
		data = []byte(`{}`)
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO audit_logs (project_id, user_id, action, target, ip, details) VALUES (NULLIF($1, ''), NULLIF($2, '')::uuid, $3, $4, $5, $6)`,
		projectID, userID, action, target, ip, data)
	return err
}

// ListAudit pages through a project's audit log, newest first.
func (s *Store) ListAudit(ctx context.Context, projectID string, before int64, limit int) ([]AuditEntry, error) {
	rows, err := s.DB.Query(ctx, `SELECT a.id, a.project_id, a.user_id::text, u.email::text, a.action, a.target, a.ip, a.details, a.created_at
		FROM audit_logs a LEFT JOIN users u ON u.id = a.user_id
		WHERE a.project_id = $1 AND ($2 = 0 OR a.id < $2) ORDER BY a.id DESC LIMIT $3`, projectID, before, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AuditEntry, error) {
		var e AuditEntry
		var details []byte
		err := r.Scan(&e.ID, &e.ProjectID, &e.UserID, &e.UserEmail, &e.Action, &e.Target, &e.IP, &details, &e.CreatedAt)
		e.Details = details
		return e, err
	})
}

// UsageDelta adds to one project's usage for a minute.
type UsageDelta struct {
	ProjectID          string
	Bucket             time.Time
	ParticipantSeconds int64
	RoomSeconds        int64
	AudioTrackSeconds  int64
	VideoTrackSeconds  int64
	RecordingSeconds   int64
	BytesIn            int64
	BytesOut           int64
	PacketsIn          int64
	PacketsOut         int64
	PacketsLost        int64
	ConnectionFailures int
	PeakParticipants   int
	PeakRooms          int
	AvgRTTMs           float32
}

// AddUsage accumulates into the minute bucket.
func (s *Store) AddUsage(ctx context.Context, d UsageDelta) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO usage_metrics (project_id, bucket, participant_seconds, room_seconds, audio_track_seconds, video_track_seconds,
		recording_seconds, bytes_in, bytes_out, packets_in, packets_out, packets_lost, connection_failures, peak_participants, peak_rooms, avg_rtt_ms)
		VALUES ($1, date_trunc('minute', $2::timestamptz), $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		ON CONFLICT (project_id, bucket) DO UPDATE SET
			participant_seconds = usage_metrics.participant_seconds + EXCLUDED.participant_seconds,
			room_seconds = usage_metrics.room_seconds + EXCLUDED.room_seconds,
			audio_track_seconds = usage_metrics.audio_track_seconds + EXCLUDED.audio_track_seconds,
			video_track_seconds = usage_metrics.video_track_seconds + EXCLUDED.video_track_seconds,
			recording_seconds = usage_metrics.recording_seconds + EXCLUDED.recording_seconds,
			bytes_in = usage_metrics.bytes_in + EXCLUDED.bytes_in,
			bytes_out = usage_metrics.bytes_out + EXCLUDED.bytes_out,
			packets_in = usage_metrics.packets_in + EXCLUDED.packets_in,
			packets_out = usage_metrics.packets_out + EXCLUDED.packets_out,
			packets_lost = usage_metrics.packets_lost + EXCLUDED.packets_lost,
			connection_failures = usage_metrics.connection_failures + EXCLUDED.connection_failures,
			peak_participants = GREATEST(usage_metrics.peak_participants, EXCLUDED.peak_participants),
			peak_rooms = GREATEST(usage_metrics.peak_rooms, EXCLUDED.peak_rooms),
			avg_rtt_ms = CASE WHEN EXCLUDED.avg_rtt_ms > 0 THEN EXCLUDED.avg_rtt_ms ELSE usage_metrics.avg_rtt_ms END`,
		d.ProjectID, d.Bucket, d.ParticipantSeconds, d.RoomSeconds, d.AudioTrackSeconds, d.VideoTrackSeconds, d.RecordingSeconds,
		d.BytesIn, d.BytesOut, d.PacketsIn, d.PacketsOut, d.PacketsLost, d.ConnectionFailures, d.PeakParticipants, d.PeakRooms, d.AvgRTTMs)
	return err
}

// UsagePoint is one bucket of a usage series.
type UsagePoint struct {
	Bucket             time.Time `json:"bucket"`
	ParticipantMinutes float64   `json:"participant_minutes"`
	RoomMinutes        float64   `json:"room_minutes"`
	AudioMinutes       float64   `json:"audio_minutes"`
	VideoMinutes       float64   `json:"video_minutes"`
	RecordingMinutes   float64   `json:"recording_minutes"`
	BytesIn            int64     `json:"bytes_in"`
	BytesOut           int64     `json:"bytes_out"`
	PacketsIn          int64     `json:"packets_in"`
	PacketsLost        int64     `json:"packets_lost"`
	ConnectionFailures int64     `json:"connection_failures"`
	PeakParticipants   int       `json:"peak_participants"`
	PeakRooms          int       `json:"peak_rooms"`
	AvgRTTMs           float64   `json:"avg_rtt_ms"`
}

// UsageSeries returns usage between two times, grouped by minute, hour, day or month.
func (s *Store) UsageSeries(ctx context.Context, projectID, interval string, from, to time.Time) ([]UsagePoint, error) {
	switch interval {
	case "minute", "hour", "day", "month":
	default:
		interval = "day"
	}
	// interval is checked against a fixed list above.
	rows, err := s.DB.Query(ctx, `SELECT date_trunc('`+interval+`', bucket) AS b,
		sum(participant_seconds)::float8 / 60, sum(room_seconds)::float8 / 60, sum(audio_track_seconds)::float8 / 60,
		sum(video_track_seconds)::float8 / 60, sum(recording_seconds)::float8 / 60,
		sum(bytes_in)::int8, sum(bytes_out)::int8, sum(packets_in)::int8, sum(packets_lost)::int8, sum(connection_failures)::int8,
		max(peak_participants), max(peak_rooms), COALESCE(avg(NULLIF(avg_rtt_ms, 0)), 0)::float8
		FROM usage_metrics WHERE project_id = $1 AND bucket >= $2 AND bucket < $3 GROUP BY b ORDER BY b`, projectID, from, to)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[UsagePoint])
}

// CountRooms returns how many room sessions a project has had in a window, and their average duration in
// seconds.
func (s *Store) RoomTotals(ctx context.Context, projectID string, from, to time.Time) (count int64, avgSeconds float64, err error) {
	err = s.DB.QueryRow(ctx, `SELECT count(*), COALESCE(avg(EXTRACT(EPOCH FROM (ended_at - created_at))) FILTER (WHERE ended_at IS NOT NULL), 0)::float8
		FROM rooms WHERE project_id = $1 AND created_at >= $2 AND created_at < $3`, projectID, from, to).Scan(&count, &avgSeconds)
	return count, avgSeconds, err
}

// Webhook is a configured endpoint.
type Webhook struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	URL       string    `json:"url"`
	APIKeyID  string    `json:"api_key_id"`
	APIKey    string    `json:"api_key"`
	Events    []string  `json:"events"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

const webhookSelect = `SELECT w.id, w.project_id, w.url, w.api_key_id, k.api_key, w.events, w.enabled, w.created_at, w.updated_at
	FROM webhooks w JOIN api_keys k ON k.id = w.api_key_id `

func scanWebhook(r pgx.Row) (Webhook, error) {
	var w Webhook
	err := r.Scan(&w.ID, &w.ProjectID, &w.URL, &w.APIKeyID, &w.APIKey, &w.Events, &w.Enabled, &w.CreatedAt, &w.UpdatedAt)
	return w, notFound(err)
}

// CreateWebhook adds an endpoint.
func (s *Store) CreateWebhook(ctx context.Context, w Webhook) (Webhook, error) {
	var id string
	err := s.DB.QueryRow(ctx, `INSERT INTO webhooks (project_id, url, api_key_id, events, enabled)
		SELECT $1, $2, k.id, $4, $5 FROM api_keys k WHERE k.id = $3 AND k.project_id = $1 AND k.revoked_at IS NULL RETURNING id`,
		w.ProjectID, w.URL, w.APIKeyID, w.Events, w.Enabled).Scan(&id)
	if err != nil {
		return Webhook{}, notFound(err)
	}
	return s.WebhookByID(ctx, w.ProjectID, id)
}

// UpdateWebhook saves an endpoint.
func (s *Store) UpdateWebhook(ctx context.Context, w Webhook) (Webhook, error) {
	tag, err := s.DB.Exec(ctx, `UPDATE webhooks SET url = $3, api_key_id = $4, events = $5, enabled = $6, updated_at = now()
		WHERE id = $1 AND project_id = $2 AND EXISTS (SELECT 1 FROM api_keys k WHERE k.id = $4 AND k.project_id = $2)`,
		w.ID, w.ProjectID, w.URL, w.APIKeyID, w.Events, w.Enabled)
	if err != nil {
		return Webhook{}, err
	}
	if tag.RowsAffected() == 0 {
		return Webhook{}, ErrNotFound
	}
	return s.WebhookByID(ctx, w.ProjectID, w.ID)
}

// DeleteWebhook removes an endpoint and its delivery history.
func (s *Store) DeleteWebhook(ctx context.Context, projectID, id string) error {
	tag, err := s.DB.Exec(ctx, `DELETE FROM webhooks WHERE id = $1 AND project_id = $2`, id, projectID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// WebhookByID loads an endpoint.
func (s *Store) WebhookByID(ctx context.Context, projectID, id string) (Webhook, error) {
	return scanWebhook(s.DB.QueryRow(ctx, webhookSelect+`WHERE w.id = $1 AND w.project_id = $2`, id, projectID))
}

// ListWebhooks returns a project's endpoints.
func (s *Store) ListWebhooks(ctx context.Context, projectID string) ([]Webhook, error) {
	rows, err := s.DB.Query(ctx, webhookSelect+`WHERE w.project_id = $1 ORDER BY w.created_at`, projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Webhook, error) { return scanWebhook(r) })
}

// WebhooksForEvent returns enabled endpoints in a project that want an event. No listed events means all.
func (s *Store) WebhooksForEvent(ctx context.Context, projectID, eventType string) ([]Webhook, error) {
	rows, err := s.DB.Query(ctx, webhookSelect+`WHERE w.project_id::text = $1 AND w.enabled AND (cardinality(w.events) = 0 OR $2 = ANY(w.events))`, projectID, eventType)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Webhook, error) { return scanWebhook(r) })
}

// Delivery is one attempt series to send one event to one endpoint.
type Delivery struct {
	ID             string          `json:"id"`
	WebhookID      string          `json:"webhook_id"`
	EventType      string          `json:"event_type"`
	Payload        json.RawMessage `json:"payload"`
	Status         string          `json:"status"`
	Attempts       int             `json:"attempts"`
	ResponseStatus *int            `json:"response_status"`
	LastError      string          `json:"last_error"`
	CreatedAt      time.Time       `json:"created_at"`
	NextAttemptAt  time.Time       `json:"next_attempt_at"`
	DeliveredAt    *time.Time      `json:"delivered_at"`
}

const deliveryColumns = `id, webhook_id, event_type, payload, status, attempts, response_status, last_error, created_at, next_attempt_at, delivered_at`

func collectDeliveries(rows pgx.Rows, err error) ([]Delivery, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Delivery, error) {
		var d Delivery
		var payload []byte
		err := r.Scan(&d.ID, &d.WebhookID, &d.EventType, &payload, &d.Status, &d.Attempts, &d.ResponseStatus, &d.LastError, &d.CreatedAt, &d.NextAttemptAt, &d.DeliveredAt)
		d.Payload = payload
		return d, err
	})
}

// EnqueueDelivery queues an event for sending.
func (s *Store) EnqueueDelivery(ctx context.Context, webhookID, eventType string, payload []byte) (string, error) {
	var id string
	err := s.DB.QueryRow(ctx, `INSERT INTO webhook_deliveries (webhook_id, event_type, payload) VALUES ($1, $2, $3) RETURNING id`,
		webhookID, eventType, payload).Scan(&id)
	return id, err
}

// ClaimDueDeliveries takes deliveries ready to send. Claiming pushes next_attempt_at forward, so another
// node, or a crash, cannot send the same attempt twice at once.
func (s *Store) ClaimDueDeliveries(ctx context.Context, limit int, lease time.Duration) ([]Delivery, error) {
	// An UPDATE returns rows in whatever order it reached them, whatever order the rows it was given were in. Events about one thing
	// have to go out in the order they happened, so a receiver does not hear that a job has ended and then that it is ending.
	rows, err := s.DB.Query(ctx, `WITH claimed AS (UPDATE webhook_deliveries SET next_attempt_at = $2, attempts = attempts + 1
		WHERE id IN (SELECT id FROM webhook_deliveries WHERE status = 'pending' AND next_attempt_at <= now()
			ORDER BY next_attempt_at LIMIT $1 FOR UPDATE SKIP LOCKED) RETURNING `+deliveryColumns+`)
		SELECT `+deliveryColumns+` FROM claimed ORDER BY created_at, id`, limit, time.Now().Add(lease))
	return collectDeliveries(rows, err)
}

// FinishDelivery records a delivery attempt's outcome. A pending status with nextAttempt schedules a retry.
func (s *Store) FinishDelivery(ctx context.Context, id, status string, responseStatus *int, lastError string, nextAttempt time.Time) error {
	_, err := s.DB.Exec(ctx, `UPDATE webhook_deliveries SET status = $2, response_status = $3, last_error = $4, next_attempt_at = $5,
		delivered_at = CASE WHEN $2 = 'delivered' THEN now() ELSE delivered_at END WHERE id = $1`, id, status, responseStatus, lastError, nextAttempt)
	return err
}

// ListDeliveries pages through an endpoint's deliveries, newest first.
func (s *Store) ListDeliveries(ctx context.Context, projectID, webhookID string, limit int) ([]Delivery, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+prefixed("d.", deliveryColumns)+` FROM webhook_deliveries d JOIN webhooks w ON w.id = d.webhook_id
		WHERE d.webhook_id = $1 AND w.project_id = $2 ORDER BY d.created_at DESC LIMIT $3`, webhookID, projectID, limit)
	return collectDeliveries(rows, err)
}

// RetryDelivery queues a finished delivery again.
func (s *Store) RetryDelivery(ctx context.Context, projectID, id string) error {
	tag, err := s.DB.Exec(ctx, `UPDATE webhook_deliveries d SET status = 'pending', next_attempt_at = now(), attempts = 0
		FROM webhooks w WHERE d.id = $1 AND d.webhook_id = w.id AND w.project_id = $2`, id, projectID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ServerNode is a media server, as last reported.
type ServerNode struct {
	NodeID     string          `json:"node_id"`
	Region     string          `json:"region"`
	Version    string          `json:"version"`
	Address    string          `json:"address"`
	StartedAt  time.Time       `json:"started_at"`
	LastSeenAt time.Time       `json:"last_seen_at"`
	Draining   bool            `json:"draining"`
	Stats      json.RawMessage `json:"stats"`
}

// HeartbeatServer updates a node's row and returns whether an operator asked it to drain.
func (s *Store) HeartbeatServer(ctx context.Context, n ServerNode) (bool, error) {
	var draining bool
	err := s.DB.QueryRow(ctx, `INSERT INTO servers (node_id, region, version, address, started_at, stats)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (node_id) DO UPDATE SET region = EXCLUDED.region, version = EXCLUDED.version, address = EXCLUDED.address,
			last_seen_at = now(), stats = EXCLUDED.stats RETURNING draining`,
		n.NodeID, n.Region, n.Version, n.Address, n.StartedAt, []byte(n.Stats)).Scan(&draining)
	return draining, err
}

// ListServers returns nodes seen within a window.
func (s *Store) ListServers(ctx context.Context, seenWithin time.Duration) ([]ServerNode, error) {
	rows, err := s.DB.Query(ctx, `SELECT node_id, region, version, address, started_at, last_seen_at, draining, stats FROM servers
		WHERE last_seen_at > $1 ORDER BY started_at`, time.Now().Add(-seenWithin))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ServerNode, error) {
		var n ServerNode
		var stats []byte
		err := r.Scan(&n.NodeID, &n.Region, &n.Version, &n.Address, &n.StartedAt, &n.LastSeenAt, &n.Draining, &stats)
		n.Stats = stats
		return n, err
	})
}

// SetServerDraining asks a node to stop taking new rooms, or to resume.
func (s *Store) SetServerDraining(ctx context.Context, nodeID string, draining bool) error {
	tag, err := s.DB.Exec(ctx, `UPDATE servers SET draining = $2 WHERE node_id = $1`, nodeID, draining)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Retention limits for the maintenance pass.
const (
	eventRetention    = 30 * 24 * time.Hour
	deliveryRetention = 30 * 24 * time.Hour
	usageRetention    = 400 * 24 * time.Hour
	serverRetention   = 7 * 24 * time.Hour
	auditRetention    = 365 * 24 * time.Hour

	meetingMemberRetention = 30 * 24 * time.Hour
	// Someone who never got in holds a token good for minutes, or a ticket good for seconds, so these go sooner. This is
	// what a flood of joins leaves behind.
	meetingMemberNeverInRetention = 24 * time.Hour
	// How long a link is kept after it stops working, which lets its page say so, and a meeting after everything about it
	// has gone. Neither goes while a member's row points at it: that row is how a turned-off link still stops whoever used it.
	joinLinkRetention = 30 * 24 * time.Hour
	meetingRetention  = 30 * 24 * time.Hour
)

// PurgeOld removes rows past their retention.
func (s *Store) PurgeOld(ctx context.Context) error {
	now := time.Now()
	statements := []struct {
		sql   string
		older time.Duration
	}{
		{`DELETE FROM events WHERE created_at < $1`, eventRetention},
		{`DELETE FROM webhook_deliveries WHERE created_at < $1 AND status <> 'pending'`, deliveryRetention},
		{`DELETE FROM usage_metrics WHERE bucket < $1`, usageRetention},
		{`DELETE FROM servers WHERE last_seen_at < $1`, serverRetention},
		{`DELETE FROM sessions WHERE expires_at < $1`, 0},
		{`DELETE FROM email_tokens WHERE expires_at < $1`, 0},
		{`DELETE FROM audit_logs WHERE created_at < $1`, auditRetention},
		// These must outlive the longest token, and hold people's names, so they do not stay forever. A row's age says little:
		// what counts is how long since anyone used it. A token that has no row at all is refused, so this cannot let anyone in.
		{`DELETE FROM meeting_members WHERE seen_at < $1`, meetingMemberRetention},
		{`DELETE FROM meeting_members WHERE state IN ('waiting', 'minted') AND seen_at < $1`, meetingMemberNeverInRetention},
		{`DELETE FROM join_links l WHERE COALESCE(l.revoked_at, l.expires_at) < $1
			AND NOT EXISTS (SELECT 1 FROM meeting_members m WHERE m.link_id = l.id)`, joinLinkRetention},
		// With no row, a room behaves like a meeting with everything off, so one still open keeps its row. A room that old
		// with no end recorded was left behind by a server that stopped.
		{`DELETE FROM meetings g WHERE g.created_at < $1
			AND NOT EXISTS (SELECT 1 FROM join_links l WHERE l.project_id = g.project_id AND l.room = g.room)
			AND NOT EXISTS (SELECT 1 FROM meeting_members m WHERE m.project_id = g.project_id AND m.room = g.room)
			AND NOT EXISTS (SELECT 1 FROM rooms r WHERE r.project_id = g.project_id::text AND r.name = g.room AND r.ended_at IS NULL AND r.created_at >= $1)`, meetingRetention},
	}
	for _, st := range statements {
		if _, err := s.DB.Exec(ctx, st.sql, now.Add(-st.older)); err != nil {
			return err
		}
	}
	return nil
}
