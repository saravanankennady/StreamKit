package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// ProviderCredential describes a stored provider key. It has no field for the key itself, so no code path can serialize
// one by accident: ProviderSecret is the only way to get it.
type ProviderCredential struct {
	ID         string     `json:"id"`
	ProjectID  string     `json:"project_id"`
	Provider   string     `json:"provider"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

const providerCredentialColumns = `id, project_id, provider, name, created_at, updated_at, last_used_at`

func scanProviderCredential(r pgx.Row) (ProviderCredential, error) {
	var c ProviderCredential
	err := r.Scan(&c.ID, &c.ProjectID, &c.Provider, &c.Name, &c.CreatedAt, &c.UpdatedAt, &c.LastUsedAt)
	return c, err
}

// SaveProviderCredential stores a key sealed, replacing any key with the same provider and name.
func (s *Store) SaveProviderCredential(ctx context.Context, projectID, provider, name, secret, createdBy string) (ProviderCredential, error) {
	sealed, err := s.Sealer.Seal(secret)
	if err != nil {
		return ProviderCredential{}, err
	}
	return scanProviderCredential(s.DB.QueryRow(ctx, `INSERT INTO provider_credentials (project_id, provider, name, secret_sealed, created_by)
		VALUES ($1, $2, $3, $4, NULLIF($5, '')::uuid)
		ON CONFLICT (project_id, provider, name) DO UPDATE SET secret_sealed = EXCLUDED.secret_sealed, updated_at = now()
		RETURNING `+providerCredentialColumns, projectID, provider, name, sealed, createdBy))
}

// ListProviderCredentials returns what is stored, without the keys.
func (s *Store) ListProviderCredentials(ctx context.Context, projectID string) ([]ProviderCredential, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+providerCredentialColumns+` FROM provider_credentials WHERE project_id = $1 ORDER BY provider, name`, projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ProviderCredential, error) { return scanProviderCredential(r) })
}

// DeleteProviderCredential removes a key.
func (s *Store) DeleteProviderCredential(ctx context.Context, projectID, id string) error {
	tag, err := s.DB.Exec(ctx, `DELETE FROM provider_credentials WHERE project_id = $1 AND id = $2`, projectID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ProviderSecret opens a key for a worker, and notes that it was used.
func (s *Store) ProviderSecret(ctx context.Context, projectID, provider, name string) (string, error) {
	var sealed []byte
	err := s.DB.QueryRow(ctx, `UPDATE provider_credentials SET last_used_at = now()
		WHERE project_id = $1 AND provider = $2 AND name = $3 RETURNING secret_sealed`, projectID, provider, name).Scan(&sealed)
	if err != nil {
		return "", notFound(err)
	}
	return s.Sealer.Open(sealed)
}

// AgentSession is a session's summary.
type AgentSession struct {
	ID          string          `json:"id"`
	ProjectID   string          `json:"project_id"`
	SessionKey  string          `json:"session_key"`
	Room        string          `json:"room"`
	JobID       string          `json:"job_id"`
	AgentName   string          `json:"agent_name"`
	Pipeline    string          `json:"pipeline"`
	Models      json.RawMessage `json:"models"`
	StartedAt   time.Time       `json:"started_at"`
	EndedAt     *time.Time      `json:"ended_at"`
	TurnCount   int             `json:"turn_count"`
	Interrupted int             `json:"interrupted"`
	ErrorCount  int             `json:"error_count"`
	AvgEndToEnd *float64        `json:"avg_end_to_end"`
	CostUSD     *float64        `json:"cost_usd"`
	Body        json.RawMessage `json:"body,omitempty"`
}

const agentSessionColumns = `id, project_id, session_key, room, job_id, agent_name, pipeline, models, started_at, ended_at,
	turn_count, interrupted, error_count, avg_end_to_end, cost_usd`

func scanAgentSession(r pgx.Row, extra ...any) (AgentSession, error) {
	var a AgentSession
	dest := append([]any{&a.ID, &a.ProjectID, &a.SessionKey, &a.Room, &a.JobID, &a.AgentName, &a.Pipeline, &a.Models, &a.StartedAt, &a.EndedAt,
		&a.TurnCount, &a.Interrupted, &a.ErrorCount, &a.AvgEndToEnd, &a.CostUSD}, extra...)
	err := r.Scan(dest...)
	return a, err
}

// SaveAgentSession stores a report, replacing an earlier report of the same session.
func (s *Store) SaveAgentSession(ctx context.Context, a AgentSession) (AgentSession, error) {
	return scanAgentSession(s.DB.QueryRow(ctx, `INSERT INTO agent_sessions (project_id, session_key, room, job_id, agent_name, pipeline, models,
		started_at, ended_at, turn_count, interrupted, error_count, avg_end_to_end, cost_usd, body)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		ON CONFLICT (project_id, session_key) DO UPDATE SET ended_at = EXCLUDED.ended_at, models = EXCLUDED.models, pipeline = EXCLUDED.pipeline,
			turn_count = EXCLUDED.turn_count, interrupted = EXCLUDED.interrupted, error_count = EXCLUDED.error_count,
			avg_end_to_end = EXCLUDED.avg_end_to_end, cost_usd = EXCLUDED.cost_usd, body = EXCLUDED.body
		RETURNING `+agentSessionColumns,
		a.ProjectID, a.SessionKey, a.Room, a.JobID, a.AgentName, a.Pipeline, a.Models, a.StartedAt, a.EndedAt,
		a.TurnCount, a.Interrupted, a.ErrorCount, a.AvgEndToEnd, a.CostUSD, a.Body))
}

// ListAgentSessions returns summaries, newest first, optionally for one room.
func (s *Store) ListAgentSessions(ctx context.Context, projectID, room string, limit int) ([]AgentSession, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+agentSessionColumns+` FROM agent_sessions
		WHERE project_id = $1 AND ($2 = '' OR room = $2) ORDER BY started_at DESC LIMIT $3`, projectID, room, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AgentSession, error) { return scanAgentSession(r) })
}

// AgentSessionByID returns a session with its turns.
func (s *Store) AgentSessionByID(ctx context.Context, projectID, id string) (AgentSession, error) {
	var body json.RawMessage
	a, err := scanAgentSession(s.DB.QueryRow(ctx, `SELECT `+agentSessionColumns+`, body FROM agent_sessions WHERE project_id = $1 AND id = $2`, projectID, id), &body)
	if err != nil {
		return AgentSession{}, notFound(err)
	}
	a.Body = body
	return a, nil
}
