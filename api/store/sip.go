package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/participant"
	"elnova-streamkit.local/sip/rules"
)

// Errors for SIP operations.
var (
	// ErrSIPCallLimit means a project is already on as many calls as it may have.
	ErrSIPCallLimit = errors.New("this project is on as many calls as it may have")
	// ErrSIPCallEnded means a call has already ended.
	ErrSIPCallEnded = errors.New("the call has ended")
	// ErrSIPTransferBusy means a call is already being transferred somewhere else.
	ErrSIPTransferBusy = errors.New("a transfer to another destination is already in progress")
)

// SIP call statuses.
const (
	SIPQueued  = "queued"
	SIPDialing = "dialing"
	SIPRinging = "ringing"
	SIPActive  = "active"
	SIPEnded   = "ended"
	SIPFailed  = "failed"
)

// SIP call directions.
const (
	SIPInbound  = "inbound"
	SIPOutbound = "outbound"
)

// SIP transfer statuses.
const (
	SIPTransferPending    = "pending"
	SIPTransferOngoing    = "ongoing"
	SIPTransferSuccessful = "successful"
	SIPTransferFailed     = "failed"
)

// sipSeenWithin is how long a queued call waits for a worker before it fails. A worker takes it within a second if one is alive.
const sipQueueTimeout = 30 * time.Second

// WorkerKindSIP is what SIP workers report as their kind.
const WorkerKindSIP = "sip_worker"

var sipJSON = protojson.MarshalOptions{UseProtoNames: true}
var sipJSONRead = protojson.UnmarshalOptions{DiscardUnknown: true}

type sipCredentials struct {
	Username string `json:"u"`
	Password string `json:"p"`
}

func (s *Store) sealCredentials(username, password string) ([]byte, error) {
	if username == "" && password == "" {
		return nil, nil
	}
	plain, err := json.Marshal(sipCredentials{username, password})
	if err != nil {
		return nil, err
	}
	return s.Sealer.Seal(string(plain))
}

func (s *Store) openCredentials(sealed []byte) (sipCredentials, error) {
	if len(sealed) == 0 {
		return sipCredentials{}, nil
	}
	plain, err := s.Sealer.Open(sealed)
	if err != nil {
		return sipCredentials{}, err
	}
	var c sipCredentials
	return c, json.Unmarshal([]byte(plain), &c)
}

// InboundTrunk is a trunk and who owns it. Info has its credentials filled in only when asked for: nothing that reaches an API
// should ask.
type InboundTrunk struct {
	ProjectID      string
	Info           *pb.SIPInboundTrunkInfo
	HasCredentials bool
}

// OutboundTrunk is a trunk and who owns it. See InboundTrunk about credentials.
type OutboundTrunk struct {
	ProjectID      string
	Info           *pb.SIPOutboundTrunkInfo
	HasCredentials bool
}

const sipTrunkColumns = `id, project_id, info, auth_sealed, created_at, updated_at`

func (s *Store) scanInbound(row pgx.Row, withSecrets bool) (*InboundTrunk, error) {
	var id, project string
	var info, sealed []byte
	var created, updated time.Time
	if err := row.Scan(&id, &project, &info, &sealed, &created, &updated); err != nil {
		return nil, notFound(err)
	}
	t := &pb.SIPInboundTrunkInfo{}
	if err := sipJSONRead.Unmarshal(info, t); err != nil {
		return nil, err
	}
	t.SipTrunkId, t.CreatedAt, t.UpdatedAt = id, timestamppb.New(created), timestamppb.New(updated)
	out := &InboundTrunk{ProjectID: project, Info: t, HasCredentials: len(sealed) > 0}
	if withSecrets {
		creds, err := s.openCredentials(sealed)
		if err != nil {
			return nil, err
		}
		t.AuthUsername, t.AuthPassword = creds.Username, creds.Password
	}
	return out, nil
}

func (s *Store) scanOutbound(row pgx.Row, withSecrets bool) (*OutboundTrunk, error) {
	var id, project string
	var info, sealed []byte
	var created, updated time.Time
	if err := row.Scan(&id, &project, &info, &sealed, &created, &updated); err != nil {
		return nil, notFound(err)
	}
	t := &pb.SIPOutboundTrunkInfo{}
	if err := sipJSONRead.Unmarshal(info, t); err != nil {
		return nil, err
	}
	t.SipTrunkId, t.CreatedAt, t.UpdatedAt = id, timestamppb.New(created), timestamppb.New(updated)
	out := &OutboundTrunk{ProjectID: project, Info: t, HasCredentials: len(sealed) > 0}
	if withSecrets {
		creds, err := s.openCredentials(sealed)
		if err != nil {
			return nil, err
		}
		t.AuthUsername, t.AuthPassword = creds.Username, creds.Password
	}
	return out, nil
}

// inboundTrunkLock makes checking for conflicts and then saving one step, across every project and every server.
const inboundTrunkLock = 7302118

// SaveInboundTrunk creates a trunk, or changes one if it has an ID. The new form must have its credentials filled in. check sees
// every other inbound trunk in the installation that could conflict with it, with their real details, and can refuse the change.
func (s *Store) SaveInboundTrunk(ctx context.Context, projectID string, t *pb.SIPInboundTrunkInfo, check func(others []*InboundTrunk) error) (*InboundTrunk, error) {
	sealed, err := s.sealCredentials(t.AuthUsername, t.AuthPassword)
	if err != nil {
		return nil, err
	}
	clean := proto.Clone(t).(*pb.SIPInboundTrunkInfo)
	clean.AuthUsername, clean.AuthPassword, clean.CreatedAt, clean.UpdatedAt = "", "", nil, nil
	id := clean.SipTrunkId
	clean.SipTrunkId = ""
	info, err := sipJSON.Marshal(clean)
	if err != nil {
		return nil, err
	}
	numbers := rules.NormalizeNumbers(t.Numbers)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, inboundTrunkLock); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT `+sipTrunkColumns+` FROM sip_trunks
		WHERE direction = 'inbound' AND ($1::text = '' OR id <> $1) AND (numbers = '{}' OR numbers && $2 OR cardinality($2::text[]) = 0)`, id, numbers)
	if err != nil {
		return nil, err
	}
	others, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*InboundTrunk, error) { return s.scanInbound(r, false) })
	if err != nil {
		return nil, err
	}
	if err := check(others); err != nil {
		return nil, err
	}
	var saved *InboundTrunk
	if id == "" {
		saved, err = s.scanInbound(tx.QueryRow(ctx, `INSERT INTO sip_trunks (id, project_id, direction, info, numbers, auth_sealed)
			VALUES ($1, $2, 'inbound', $3, $4, $5) RETURNING `+sipTrunkColumns, participant.NewID("ST_"), projectID, info, numbers, sealed), false)
	} else {
		saved, err = s.scanInbound(tx.QueryRow(ctx, `UPDATE sip_trunks SET info = $3, numbers = $4, auth_sealed = $5, updated_at = now()
			WHERE id = $1 AND project_id = $2 AND direction = 'inbound' RETURNING `+sipTrunkColumns, id, projectID, info, numbers, sealed), false)
	}
	if err != nil {
		return nil, err
	}
	return saved, tx.Commit(ctx)
}

// SaveOutboundTrunk creates a trunk, or changes one if it has an ID. The new form must have its credentials filled in.
func (s *Store) SaveOutboundTrunk(ctx context.Context, projectID string, t *pb.SIPOutboundTrunkInfo) (*OutboundTrunk, error) {
	sealed, err := s.sealCredentials(t.AuthUsername, t.AuthPassword)
	if err != nil {
		return nil, err
	}
	clean := proto.Clone(t).(*pb.SIPOutboundTrunkInfo)
	clean.AuthUsername, clean.AuthPassword, clean.CreatedAt, clean.UpdatedAt = "", "", nil, nil
	id := clean.SipTrunkId
	clean.SipTrunkId = ""
	info, err := sipJSON.Marshal(clean)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return s.scanOutbound(s.DB.QueryRow(ctx, `INSERT INTO sip_trunks (id, project_id, direction, info, auth_sealed)
			VALUES ($1, $2, 'outbound', $3, $4) RETURNING `+sipTrunkColumns, participant.NewID("ST_"), projectID, info, sealed), false)
	}
	return s.scanOutbound(s.DB.QueryRow(ctx, `UPDATE sip_trunks SET info = $3, auth_sealed = $4, updated_at = now()
		WHERE id = $1 AND project_id = $2 AND direction = 'outbound' RETURNING `+sipTrunkColumns, id, projectID, info, sealed), false)
}

// InboundTrunkByID loads one trunk in a project.
func (s *Store) InboundTrunkByID(ctx context.Context, projectID, id string, withSecrets bool) (*InboundTrunk, error) {
	return s.scanInbound(s.DB.QueryRow(ctx, `SELECT `+sipTrunkColumns+` FROM sip_trunks WHERE id = $1 AND project_id = $2 AND direction = 'inbound'`, id, projectID), withSecrets)
}

// OutboundTrunkByID loads one trunk in a project.
func (s *Store) OutboundTrunkByID(ctx context.Context, projectID, id string, withSecrets bool) (*OutboundTrunk, error) {
	return s.scanOutbound(s.DB.QueryRow(ctx, `SELECT `+sipTrunkColumns+` FROM sip_trunks WHERE id = $1 AND project_id = $2 AND direction = 'outbound'`, id, projectID), withSecrets)
}

// ListInboundTrunks lists a project's trunks, oldest first.
func (s *Store) ListInboundTrunks(ctx context.Context, projectID string) ([]*InboundTrunk, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+sipTrunkColumns+` FROM sip_trunks WHERE project_id = $1 AND direction = 'inbound' ORDER BY created_at, id`, projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*InboundTrunk, error) { return s.scanInbound(r, false) })
}

// ListOutboundTrunks lists a project's trunks, oldest first.
func (s *Store) ListOutboundTrunks(ctx context.Context, projectID string) ([]*OutboundTrunk, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+sipTrunkColumns+` FROM sip_trunks WHERE project_id = $1 AND direction = 'outbound' ORDER BY created_at, id`, projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*OutboundTrunk, error) { return s.scanOutbound(r, false) })
}

// DeleteSIPTrunk deletes a trunk of either direction. It reports whether there was one.
func (s *Store) DeleteSIPTrunk(ctx context.Context, projectID, id string) (bool, error) {
	tag, err := s.DB.Exec(ctx, `DELETE FROM sip_trunks WHERE id = $1 AND project_id = $2`, id, projectID)
	return tag.RowsAffected() > 0, err
}

// InboundTrunksForNumber finds every trunk that could take a call to a number: those for that number, and those for any number,
// across all projects that can take calls, with their credentials. Workers need this and nothing else should.
func (s *Store) InboundTrunksForNumber(ctx context.Context, called string) ([]*InboundTrunk, error) {
	rows, err := s.DB.Query(ctx, `SELECT t.id, t.project_id, t.info, t.auth_sealed, t.created_at, t.updated_at FROM sip_trunks t
		LEFT JOIN projects p ON p.id::text = t.project_id
		WHERE t.direction = 'inbound' AND (t.numbers = '{}' OR t.numbers && $1)
		AND (p.id IS NULL OR (p.deleted_at IS NULL AND p.status = 'active'))`, []string{rules.NormalizeNumber(called)})
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*InboundTrunk, error) { return s.scanInbound(r, true) })
}

// SIPAllowedSources lists every allow-list entry on an inbound trunk, in any project. A worker uses this to recognise carriers.
func (s *Store) SIPAllowedSources(ctx context.Context) ([]string, error) {
	rows, err := s.DB.Query(ctx, `SELECT DISTINCT jsonb_array_elements_text(info -> 'allowed_addresses') FROM sip_trunks WHERE direction = 'inbound' AND info ? 'allowed_addresses'`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// DispatchRule is a rule and who owns it.
type DispatchRule struct {
	ProjectID string
	Info      *pb.SIPDispatchRuleInfo
}

const sipRuleColumns = `id, project_id, info, pin_sealed, created_at, updated_at`

// scanRule reads a rule. Without withPin, a rule that has a PIN shows rules.PinMask in its place.
func (s *Store) scanRule(row pgx.Row, withPin bool) (*DispatchRule, error) {
	var id, project string
	var info, sealed []byte
	var created, updated time.Time
	if err := row.Scan(&id, &project, &info, &sealed, &created, &updated); err != nil {
		return nil, notFound(err)
	}
	r := &pb.SIPDispatchRuleInfo{}
	if err := sipJSONRead.Unmarshal(info, r); err != nil {
		return nil, err
	}
	r.SipDispatchRuleId, r.CreatedAt, r.UpdatedAt = id, timestamppb.New(created), timestamppb.New(updated)
	if len(sealed) > 0 {
		pin := rules.PinMask
		if withPin {
			opened, err := s.Sealer.Open(sealed)
			if err != nil {
				return nil, err
			}
			pin = opened
		}
		rules.SetPin(r, pin)
	}
	return &DispatchRule{ProjectID: project, Info: r}, nil
}

// SaveDispatchRule creates a rule, or changes one if it has an ID. The new form must have its real PIN filled in. check sees the
// project's other rules, with their real PINs, and can refuse the change.
func (s *Store) SaveDispatchRule(ctx context.Context, projectID string, r *pb.SIPDispatchRuleInfo, check func(others []*pb.SIPDispatchRuleInfo) error) (*DispatchRule, error) {
	clean := proto.Clone(r).(*pb.SIPDispatchRuleInfo)
	_, pin, err := rules.PinAndRoom(clean)
	if err != nil {
		return nil, err
	}
	var sealed []byte
	if pin != "" {
		if sealed, err = s.Sealer.Seal(pin); err != nil {
			return nil, err
		}
	}
	rules.SetPin(clean, "")
	id := clean.SipDispatchRuleId
	clean.SipDispatchRuleId, clean.CreatedAt, clean.UpdatedAt = "", nil, nil
	info, err := sipJSON.Marshal(clean)
	if err != nil {
		return nil, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('sip-rules:' || $1::text, 0))`, projectID); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT `+sipRuleColumns+` FROM sip_dispatch_rules WHERE project_id = $1 AND ($2::text = '' OR id <> $2)`, projectID, id)
	if err != nil {
		return nil, err
	}
	others, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*pb.SIPDispatchRuleInfo, error) {
		d, err := s.scanRule(row, true)
		if err != nil {
			return nil, err
		}
		return d.Info, nil
	})
	if err != nil {
		return nil, err
	}
	if err := check(others); err != nil {
		return nil, err
	}
	var saved *DispatchRule
	if id == "" {
		saved, err = s.scanRule(tx.QueryRow(ctx, `INSERT INTO sip_dispatch_rules (id, project_id, info, pin_sealed) VALUES ($1, $2, $3, $4) RETURNING `+sipRuleColumns,
			participant.NewID("SDR_"), projectID, info, sealed), false)
	} else {
		saved, err = s.scanRule(tx.QueryRow(ctx, `UPDATE sip_dispatch_rules SET info = $3, pin_sealed = $4, updated_at = now() WHERE id = $1 AND project_id = $2 RETURNING `+sipRuleColumns,
			id, projectID, info, sealed), false)
	}
	if err != nil {
		return nil, err
	}
	return saved, tx.Commit(ctx)
}

// DispatchRuleByID loads one rule in a project.
func (s *Store) DispatchRuleByID(ctx context.Context, projectID, id string, withPin bool) (*DispatchRule, error) {
	return s.scanRule(s.DB.QueryRow(ctx, `SELECT `+sipRuleColumns+` FROM sip_dispatch_rules WHERE id = $1 AND project_id = $2`, id, projectID), withPin)
}

// ListDispatchRules lists a project's rules, oldest first. Only workers should ask for the PINs.
func (s *Store) ListDispatchRules(ctx context.Context, projectID string, withPins bool) ([]*pb.SIPDispatchRuleInfo, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+sipRuleColumns+` FROM sip_dispatch_rules WHERE project_id = $1 ORDER BY created_at, id`, projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*pb.SIPDispatchRuleInfo, error) {
		d, err := s.scanRule(row, withPins)
		if err != nil {
			return nil, err
		}
		return d.Info, nil
	})
}

// DeleteDispatchRule deletes a rule and returns it as it stood.
func (s *Store) DeleteDispatchRule(ctx context.Context, projectID, id string) (*DispatchRule, error) {
	return s.scanRule(s.DB.QueryRow(ctx, `DELETE FROM sip_dispatch_rules WHERE id = $1 AND project_id = $2 RETURNING `+sipRuleColumns, id, projectID), false)
}

// SIPCall is one call.
type SIPCall struct {
	ID                  string     `json:"id"`
	ProjectID           string     `json:"project_id"`
	Direction           string     `json:"direction"`
	Status              string     `json:"status"`
	TrunkID             string     `json:"trunk_id"`
	DispatchRuleID      string     `json:"dispatch_rule_id"`
	RoomName            string     `json:"room_name"`
	RoomSID             string     `json:"room_sid"`
	ParticipantIdentity string     `json:"participant_identity"`
	ParticipantSID      string     `json:"participant_sid"`
	FromUser            string     `json:"from_user"`
	FromHost            string     `json:"from_host"`
	ToUser              string     `json:"to_user"`
	ToHost              string     `json:"to_host"`
	SIPCallID           string     `json:"sip_call_id"`
	AudioCodec          string     `json:"audio_codec"`
	MediaEncryption     string     `json:"media_encryption"`
	SIPStatusCode       int        `json:"sip_status_code"`
	SIPStatus           string     `json:"sip_status"`
	DisconnectReason    string     `json:"disconnect_reason"`
	Error               string     `json:"error"`
	CreatedAt           time.Time  `json:"created_at"`
	AnsweredAt          *time.Time `json:"answered_at"`
	EndedAt             *time.Time `json:"ended_at"`

	WorkerID *string `json:"-"`
	sealed   []byte
}

const sipCallColumns = `id, project_id, direction, status, trunk_id, dispatch_rule_id, room_name, room_sid, participant_identity, participant_sid,
	from_user, from_host, to_user, to_host, sip_call_id, audio_codec, media_encryption, sip_status_code, sip_status, disconnect_reason, error,
	created_at, answered_at, ended_at, worker_id, request_sealed`

func scanSIPCall(row pgx.Row) (*SIPCall, error) {
	c := &SIPCall{}
	err := row.Scan(&c.ID, &c.ProjectID, &c.Direction, &c.Status, &c.TrunkID, &c.DispatchRuleID, &c.RoomName, &c.RoomSID, &c.ParticipantIdentity, &c.ParticipantSID,
		&c.FromUser, &c.FromHost, &c.ToUser, &c.ToHost, &c.SIPCallID, &c.AudioCodec, &c.MediaEncryption, &c.SIPStatusCode, &c.SIPStatus, &c.DisconnectReason, &c.Error,
		&c.CreatedAt, &c.AnsweredAt, &c.EndedAt, &c.WorkerID, &c.sealed)
	if err != nil {
		return nil, notFound(err)
	}
	return c, nil
}

// Open reports whether a call can still change.
func (c *SIPCall) Open() bool { return c.Status != SIPEnded && c.Status != SIPFailed }

// SIPOutboundRequest is what a worker needs to place a call. It can hold a trunk's credentials, so it only ever exists sealed.
type SIPOutboundRequest struct {
	Address             string            `json:"address"`
	Transport           int32             `json:"transport"`
	Number              string            `json:"number"`
	Username            string            `json:"username,omitempty"`
	Password            string            `json:"password,omitempty"`
	CallTo              string            `json:"call_to"`
	ToUserOverride      string            `json:"to_user_override,omitempty"`
	FromHost            string            `json:"from_host,omitempty"`
	DisplayName         *string           `json:"display_name,omitempty"`
	Name                string            `json:"name,omitempty"`
	Metadata            string            `json:"metadata,omitempty"`
	Attributes          map[string]string `json:"attributes,omitempty"`
	Dtmf                string            `json:"dtmf,omitempty"`
	Dialtone            int32             `json:"dialtone,omitempty"`
	Headers             map[string]string `json:"headers,omitempty"`
	HeadersToAttributes map[string]string `json:"headers_to_attributes,omitempty"`
	AttributesToHeaders map[string]string `json:"attributes_to_headers,omitempty"`
	IncludeHeaders      int32             `json:"include_headers,omitempty"`
	RingingMs           int64             `json:"ringing_ms,omitempty"`
	MaxDurationMs       int64             `json:"max_duration_ms,omitempty"`
	MediaJSON           json.RawMessage   `json:"media,omitempty"`
}

// countOpenCallsLocked takes the lock that makes counting a project's calls and adding one a single step, and returns the count.
func countOpenCallsLocked(ctx context.Context, tx pgx.Tx, projectID string) (int, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('sip-calls:' || $1::text, 0))`, projectID); err != nil {
		return 0, err
	}
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM sip_calls WHERE project_id = $1 AND status IN ('queued', 'dialing', 'ringing', 'active')`, projectID).Scan(&n)
	return n, err
}

// CreateInboundSIPCall records a call a worker has just authenticated. It fails with ErrSIPCallLimit if the project is at its limit.
func (s *Store) CreateInboundSIPCall(ctx context.Context, c *SIPCall, workerID string, lease time.Duration, limit int) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	n, err := countOpenCallsLocked(ctx, tx, c.ProjectID)
	if err != nil {
		return err
	}
	if n >= limit {
		return ErrSIPCallLimit
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sip_calls (id, project_id, direction, status, trunk_id, from_user, from_host, to_user, to_host, sip_call_id, worker_id, lease_expires_at)
		VALUES ($1, $2, 'inbound', 'ringing', $3, $4, $5, $6, $7, $8, $9, now() + make_interval(secs => $10))`,
		c.ID, c.ProjectID, c.TrunkID, c.FromUser, c.FromHost, c.ToUser, c.ToHost, c.SIPCallID, workerID, lease.Seconds()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// QueueOutboundSIPCall queues a call for a worker. It fails with ErrSIPCallLimit if the project is at its limit.
func (s *Store) QueueOutboundSIPCall(ctx context.Context, c *SIPCall, req *SIPOutboundRequest, limit int) (*SIPCall, error) {
	plain, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	sealed, err := s.Sealer.Seal(string(plain))
	if err != nil {
		return nil, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	n, err := countOpenCallsLocked(ctx, tx, c.ProjectID)
	if err != nil {
		return nil, err
	}
	if n >= limit {
		return nil, ErrSIPCallLimit
	}
	saved, err := scanSIPCall(tx.QueryRow(ctx, `INSERT INTO sip_calls (id, project_id, direction, status, trunk_id, room_name, participant_identity, from_user, to_user, to_host, request_sealed)
		VALUES ($1, $2, 'outbound', 'queued', $3, $4, $5, $6, $7, $8, $9) RETURNING `+sipCallColumns,
		c.ID, c.ProjectID, c.TrunkID, c.RoomName, c.ParticipantIdentity, c.FromUser, c.ToUser, c.ToHost, sealed))
	if err != nil {
		return nil, err
	}
	return saved, tx.Commit(ctx)
}

// SIPCapacityAvailable reports whether a live, healthy worker has room for another call.
func (s *Store) SIPCapacityAvailable(ctx context.Context) (bool, error) {
	var capacity, running int
	err := s.DB.QueryRow(ctx, `SELECT
		COALESCE((SELECT sum(COALESCE((stats ->> 'max_jobs')::int, 0)) FROM servers
			WHERE stats ->> 'kind' = $1 AND stats ->> 'healthy' = 'true' AND NOT draining AND last_seen_at > now() - make_interval(secs => $2)), 0),
		(SELECT count(*) FROM sip_calls WHERE status IN ('queued', 'dialing', 'ringing', 'active'))`,
		WorkerKindSIP, WorkerSeenWithin.Seconds()).Scan(&capacity, &running)
	return running < capacity, err
}

// ClaimSIPCall takes the oldest queued outbound call, or returns nil, and opens its request.
func (s *Store) ClaimSIPCall(ctx context.Context, workerID string, lease time.Duration) (*SIPCall, *SIPOutboundRequest, error) {
	c, err := scanSIPCall(s.DB.QueryRow(ctx, `UPDATE sip_calls SET worker_id = $1, status = 'dialing', lease_expires_at = now() + make_interval(secs => $2), updated_at = now()
		WHERE id = (SELECT id FROM sip_calls WHERE status = 'queued' AND worker_id IS NULL ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING `+sipCallColumns, workerID, lease.Seconds()))
	if errors.Is(err, ErrNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	plain, err := s.Sealer.Open(c.sealed)
	if err != nil {
		return c, nil, err
	}
	var req SIPOutboundRequest
	return c, &req, json.Unmarshal([]byte(plain), &req)
}

// SIPCallLease says what a worker holds, and whether anyone has asked for a call to end.
type SIPCallLease struct {
	ID   string
	Stop bool
}

// RenewSIPCalls extends a worker's leases on all its calls with one statement, however many it has. A call missing from the answer
// is one this worker no longer owns.
func (s *Store) RenewSIPCalls(ctx context.Context, workerID string, lease time.Duration) ([]SIPCallLease, error) {
	rows, err := s.DB.Query(ctx, `UPDATE sip_calls SET lease_expires_at = now() + make_interval(secs => $2)
		WHERE worker_id = $1 AND status IN ('dialing', 'ringing', 'active') RETURNING id, stop_requested_at IS NOT NULL`, workerID, lease.Seconds())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (SIPCallLease, error) {
		var l SIPCallLease
		return l, r.Scan(&l.ID, &l.Stop)
	})
}

// SIPCallUpdate is what a worker reports. Zero values leave a field alone.
type SIPCallUpdate struct {
	Status          string
	DispatchRuleID  string
	RoomName        string
	RoomSID         string
	Identity        string
	ParticipantSID  string
	FromUser        string
	ToUser          string
	SIPCallID       string
	AudioCodec      string
	MediaEncryption string
	SIPStatusCode   int
	SIPStatus       string
	Reason          string
	Error           string
	Answered        bool
}

// UpdateSIPCall records what has happened to a call this worker owns. It fails with ErrJobLost if it no longer does, and a call
// that has ended stays ended.
func (s *Store) UpdateSIPCall(ctx context.Context, id, workerID string, u SIPCallUpdate) (*SIPCall, error) {
	c, err := scanSIPCall(s.DB.QueryRow(ctx, `UPDATE sip_calls SET
		status = COALESCE(NULLIF($3, ''), status),
		dispatch_rule_id = COALESCE(NULLIF($4, ''), dispatch_rule_id),
		room_name = COALESCE(NULLIF($5, ''), room_name),
		room_sid = COALESCE(NULLIF($6, ''), room_sid),
		participant_identity = COALESCE(NULLIF($7, ''), participant_identity),
		participant_sid = COALESCE(NULLIF($8, ''), participant_sid),
		from_user = COALESCE(NULLIF($9, ''), from_user),
		to_user = COALESCE(NULLIF($10, ''), to_user),
		sip_call_id = COALESCE(NULLIF($11, ''), sip_call_id),
		audio_codec = COALESCE(NULLIF($12, ''), audio_codec),
		media_encryption = COALESCE(NULLIF($13, ''), media_encryption),
		sip_status_code = CASE WHEN $14 > 0 THEN $14 ELSE sip_status_code END,
		sip_status = COALESCE(NULLIF($15, ''), sip_status),
		disconnect_reason = COALESCE(NULLIF($16, ''), disconnect_reason),
		error = COALESCE(NULLIF($17, ''), error),
		answered_at = CASE WHEN $18 THEN COALESCE(answered_at, now()) ELSE answered_at END,
		ended_at = CASE WHEN $3 IN ('ended', 'failed') THEN COALESCE(ended_at, now()) ELSE ended_at END,
		request_sealed = CASE WHEN $3 IN ('ended', 'failed') THEN NULL ELSE request_sealed END,
		updated_at = now()
		WHERE id = $1 AND worker_id = $2 AND status IN ('dialing', 'ringing', 'active') RETURNING `+sipCallColumns,
		id, workerID, u.Status, u.DispatchRuleID, u.RoomName, u.RoomSID, u.Identity, u.ParticipantSID, u.FromUser, u.ToUser, u.SIPCallID, u.AudioCodec, u.MediaEncryption,
		u.SIPStatusCode, u.SIPStatus, u.Reason, u.Error, u.Answered))
	if errors.Is(err, ErrNotFound) {
		return nil, ErrJobLost
	}
	return c, err
}

// SIPCallByID loads a call in a project.
func (s *Store) SIPCallByID(ctx context.Context, projectID, id string) (*SIPCall, error) {
	return scanSIPCall(s.DB.QueryRow(ctx, `SELECT `+sipCallColumns+` FROM sip_calls WHERE id = $1 AND project_id = $2`, id, projectID))
}

// OpenSIPCallInRoom finds the open call behind a participant.
func (s *Store) OpenSIPCallInRoom(ctx context.Context, projectID, room, identity string) (*SIPCall, error) {
	return scanSIPCall(s.DB.QueryRow(ctx, `SELECT `+sipCallColumns+` FROM sip_calls
		WHERE project_id = $1 AND room_name = $2 AND participant_identity = $3 AND status IN ('dialing', 'ringing', 'active') ORDER BY created_at DESC LIMIT 1`, projectID, room, identity))
}

// ListSIPCalls pages through a project's calls, newest first.
func (s *Store) ListSIPCalls(ctx context.Context, projectID, cursor string, limit int) ([]*SIPCall, string, error) {
	var before *time.Time
	var beforeID string
	if at, id, ok := strings.Cut(cursor, ","); ok {
		if t, err := time.Parse(time.RFC3339Nano, at); err == nil {
			before, beforeID = &t, id
		}
	}
	rows, err := s.DB.Query(ctx, `SELECT `+sipCallColumns+` FROM sip_calls WHERE project_id = $1
		AND ($2::timestamptz IS NULL OR (created_at, id) < ($2, $3)) ORDER BY created_at DESC, id DESC LIMIT $4`, projectID, before, beforeID, limit)
	if err != nil {
		return nil, "", err
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*SIPCall, error) { return scanSIPCall(r) })
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(items) == limit {
		last := items[len(items)-1]
		next = last.CreatedAt.UTC().Format(time.RFC3339Nano) + "," + last.ID
	}
	return items, next, nil
}

// RequestSIPCallStop asks for a call to end. A call no worker has claimed ends here, since nothing else would end it.
func (s *Store) RequestSIPCallStop(ctx context.Context, projectID, id string) (*SIPCall, error) {
	c, err := scanSIPCall(s.DB.QueryRow(ctx, `UPDATE sip_calls SET status = 'ended', ended_at = now(), updated_at = now(), disconnect_reason = 'CLIENT_INITIATED', request_sealed = NULL
		WHERE id = $1 AND project_id = $2 AND status = 'queued' AND worker_id IS NULL RETURNING `+sipCallColumns, id, projectID))
	if err == nil {
		return c, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	c, err = scanSIPCall(s.DB.QueryRow(ctx, `UPDATE sip_calls SET stop_requested_at = COALESCE(stop_requested_at, now()), updated_at = now()
		WHERE id = $1 AND project_id = $2 AND status IN ('dialing', 'ringing', 'active') RETURNING `+sipCallColumns, id, projectID))
	if err == nil {
		return c, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if _, err := s.SIPCallByID(ctx, projectID, id); err != nil {
		return nil, err
	}
	return nil, ErrSIPCallEnded
}

// ReapSIP fails calls whose workers have stopped renewing, and queued calls no worker has taken.
func (s *Store) ReapSIP(ctx context.Context) ([]*SIPCall, error) {
	rows, err := s.DB.Query(ctx, `UPDATE sip_calls SET status = 'failed', ended_at = now(), updated_at = now(), request_sealed = NULL,
		disconnect_reason = 'SIP_TRUNK_FAILURE',
		error = CASE WHEN worker_id IS NULL THEN 'No SIP worker took this call.' ELSE 'The worker handling this call stopped responding.' END,
		sip_status_code = CASE WHEN worker_id IS NULL THEN 503 ELSE sip_status_code END
		WHERE (worker_id IS NOT NULL AND status IN ('dialing', 'ringing', 'active') AND lease_expires_at < now())
		OR (worker_id IS NULL AND status = 'queued' AND created_at < now() - make_interval(secs => $1))
		RETURNING `+sipCallColumns, sipQueueTimeout.Seconds())
	if err != nil {
		return nil, err
	}
	calls, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*SIPCall, error) { return scanSIPCall(r) })
	if err != nil {
		return nil, err
	}
	if len(calls) > 0 {
		ids := make([]string, 0, len(calls))
		for _, c := range calls {
			ids = append(ids, c.ID)
		}
		// A transfer cannot finish once its call has gone.
		if _, err := s.DB.Exec(ctx, `UPDATE sip_transfers SET status = 'failed', reason = 'STR_CALL_ENDED', error = 'The call ended before the transfer finished.', updated_at = now()
			WHERE call_id = ANY($1) AND status IN ('pending', 'ongoing')`, ids); err != nil {
			return calls, err
		}
	}
	return calls, nil
}

// RecordSIPEvent logs what happened to a call. Phone numbers stay out of it: a call's row says who, and follows its rule about
// hiding them.
func (s *Store) RecordSIPEvent(ctx context.Context, c *SIPCall, level, eventType, message string) error {
	return s.RecordMediaEvent(ctx, MediaEvent{
		ProjectID: c.ProjectID, RoomName: c.RoomName, RoomSID: c.RoomSID, Identity: c.ParticipantIdentity, Level: level, Type: eventType, Message: message,
		Details: map[string]any{"sip_call_id": c.ID, "direction": c.Direction, "sip_status_code": c.SIPStatusCode},
	})
}

// SIPTransfer is one attempt to move a call elsewhere.
type SIPTransfer struct {
	ID            string
	CallID        string
	ProjectID     string
	Status        string
	TransferTo    string
	Request       SIPTransferRequest
	Reason        string
	SIPStatusCode int
	SIPStatus     string
	Error         string
}

// SIPTransferRequest is the rest of what a transfer asks for.
type SIPTransferRequest struct {
	Headers   map[string]string `json:"headers,omitempty"`
	Dialtone  int32             `json:"dialtone,omitempty"`
	TimeoutMs int64             `json:"timeout_ms,omitempty"`
}

const sipTransferColumns = `id, call_id, project_id, status, transfer_to, request, reason, sip_status_code, sip_status, error`

func scanSIPTransfer(row pgx.Row) (*SIPTransfer, error) {
	t := &SIPTransfer{}
	var request []byte
	err := row.Scan(&t.ID, &t.CallID, &t.ProjectID, &t.Status, &t.TransferTo, &request, &t.Reason, &t.SIPStatusCode, &t.SIPStatus, &t.Error)
	if err != nil {
		return nil, notFound(err)
	}
	return t, json.Unmarshal(request, &t.Request)
}

// CreateSIPTransfer asks a call's worker to transfer it. Asking again for the same target joins the transfer already under way.
func (s *Store) CreateSIPTransfer(ctx context.Context, c *SIPCall, to string, req SIPTransferRequest) (*SIPTransfer, error) {
	request, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('sip-transfer:' || $1::text, 0))`, c.ID); err != nil {
		return nil, err
	}
	current, err := scanSIPTransfer(tx.QueryRow(ctx, `SELECT `+sipTransferColumns+` FROM sip_transfers WHERE call_id = $1 AND status IN ('pending', 'ongoing') LIMIT 1`, c.ID))
	if err == nil {
		if current.TransferTo != to {
			return nil, ErrSIPTransferBusy
		}
		return current, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	t, err := scanSIPTransfer(tx.QueryRow(ctx, `INSERT INTO sip_transfers (id, call_id, project_id, status, transfer_to, request) VALUES ($1, $2, $3, 'pending', $4, $5) RETURNING `+sipTransferColumns,
		participant.NewID("STR_"), c.ID, c.ProjectID, to, request))
	if err != nil {
		return nil, err
	}
	return t, tx.Commit(ctx)
}

// SIPTransferByID loads a transfer in a project.
func (s *Store) SIPTransferByID(ctx context.Context, projectID, id string) (*SIPTransfer, error) {
	return scanSIPTransfer(s.DB.QueryRow(ctx, `SELECT `+sipTransferColumns+` FROM sip_transfers WHERE id = $1 AND project_id = $2`, id, projectID))
}

// TakeSIPTransfers moves the pending transfers of this worker's calls to ongoing, and returns them.
func (s *Store) TakeSIPTransfers(ctx context.Context, workerID string) ([]*SIPTransfer, error) {
	rows, err := s.DB.Query(ctx, `UPDATE sip_transfers t SET status = 'ongoing', updated_at = now() FROM sip_calls c
		WHERE t.call_id = c.id AND c.worker_id = $1 AND t.status = 'pending' AND c.status IN ('ringing', 'active')
		RETURNING t.id, t.call_id, t.project_id, t.status, t.transfer_to, t.request, t.reason, t.sip_status_code, t.sip_status, t.error`, workerID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*SIPTransfer, error) { return scanSIPTransfer(r) })
}

// FinishSIPTransfer records how a transfer ended.
func (s *Store) FinishSIPTransfer(ctx context.Context, id string, ok bool, reason string, code int, status, message string) error {
	result := SIPTransferFailed
	if ok {
		result = SIPTransferSuccessful
	}
	_, err := s.DB.Exec(ctx, `UPDATE sip_transfers SET status = $2, reason = $3, sip_status_code = $4, sip_status = $5, error = $6, updated_at = now() WHERE id = $1`,
		id, result, reason, code, status, message)
	return err
}

// FailSIPTransfersOfCall ends any transfer still open on a call that has ended.
func (s *Store) FailSIPTransfersOfCall(ctx context.Context, callID string) error {
	_, err := s.DB.Exec(ctx, `UPDATE sip_transfers SET status = 'failed', reason = 'STR_CALL_ENDED', error = 'The call ended before the transfer finished.', updated_at = now()
		WHERE call_id = $1 AND status IN ('pending', 'ongoing')`, callID)
	return err
}
