package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// joinCodeBytes gives a code that cannot be guessed, and is still short enough to paste into a chat.
const joinCodeBytes = 16

// JoinLink lets anyone holding its code join one room without an account.
type JoinLink struct {
	ID         string     `json:"id"`
	ProjectID  string     `json:"project_id"`
	Room       string     `json:"room"`
	Label      string     `json:"label"`
	CanPublish bool       `json:"can_publish"`
	Role       string     `json:"role"`
	Uses       int64      `json:"uses"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	RevokedAt  *time.Time `json:"-"`
	Code       string     `json:"-"`
}

const joinLinkColumns = `id, project_id, room, label, can_publish, role, uses, created_at, expires_at, revoked_at`

func scanJoinLink(r pgx.Row, extra ...any) (JoinLink, error) {
	var l JoinLink
	dest := append([]any{&l.ID, &l.ProjectID, &l.Room, &l.Label, &l.CanPublish, &l.Role, &l.Uses, &l.CreatedAt, &l.ExpiresAt, &l.RevokedAt}, extra...)
	err := r.Scan(dest...)
	return l, err
}

// CreateJoinLink makes a link with a new code. With no role it is a guest link.
func (s *Store) CreateJoinLink(ctx context.Context, l JoinLink, createdBy string) (JoinLink, error) {
	code := RandomToken(joinCodeBytes)
	sealed, err := s.Sealer.Seal(code)
	if err != nil {
		return JoinLink{}, err
	}
	if l.Role == "" {
		l.Role = RoleGuest
	}
	created, err := scanJoinLink(s.DB.QueryRow(ctx, `INSERT INTO join_links
		(project_id, room, label, code_hash, code_sealed, can_publish, role, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING `+joinLinkColumns,
		l.ProjectID, l.Room, l.Label, HashToken(code), sealed, l.CanPublish, l.Role, createdBy, l.ExpiresAt))
	if err != nil {
		return JoinLink{}, err
	}
	created.Code = code
	return created, nil
}

// ListJoinLinks returns the links that still work, newest first, optionally for one room.
func (s *Store) ListJoinLinks(ctx context.Context, projectID, room string) ([]JoinLink, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+joinLinkColumns+`, code_sealed FROM join_links
		WHERE project_id = $1 AND ($2 = '' OR room = $2) AND revoked_at IS NULL AND expires_at > now()
		ORDER BY created_at DESC, id DESC LIMIT 200`, projectID, room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := []JoinLink{}
	for rows.Next() {
		var sealed []byte
		l, err := scanJoinLink(rows, &sealed)
		if err != nil {
			return nil, err
		}
		if l.Code, err = s.Sealer.Open(sealed); err != nil {
			return nil, err
		}
		links = append(links, l)
	}
	return links, rows.Err()
}

// RevokeJoinLink stops a link from working. It returns ErrNotFound if the link has already stopped.
func (s *Store) RevokeJoinLink(ctx context.Context, projectID, id string) error {
	var revoked string
	err := s.DB.QueryRow(ctx, `UPDATE join_links SET revoked_at = now()
		WHERE project_id = $1 AND id::text = $2 AND revoked_at IS NULL AND expires_at > now() RETURNING id`, projectID, id).Scan(&revoked)
	return notFound(err)
}

// JoinLinkByCode finds a link, including one that has stopped working, so the caller can say why.
func (s *Store) JoinLinkByCode(ctx context.Context, code string) (JoinLink, error) {
	l, err := scanJoinLink(s.DB.QueryRow(ctx, `SELECT `+joinLinkColumns+` FROM join_links WHERE code_hash = $1`, HashToken(code)))
	return l, notFound(err)
}

// JoinLinkByID finds a link by its row, including one that has stopped working.
func (s *Store) JoinLinkByID(ctx context.Context, id string) (JoinLink, error) {
	l, err := scanJoinLink(s.DB.QueryRow(ctx, `SELECT `+joinLinkColumns+` FROM join_links WHERE id = $1`, id))
	return l, notFound(err)
}

// UseJoinLink counts a use, in one statement so a link revoked at this moment cannot still admit someone.
// It returns ErrNotFound if the link has stopped working.
func (s *Store) UseJoinLink(ctx context.Context, code string) (JoinLink, error) {
	l, err := scanJoinLink(s.DB.QueryRow(ctx, `UPDATE join_links SET uses = uses + 1
		WHERE code_hash = $1 AND revoked_at IS NULL AND expires_at > now() RETURNING `+joinLinkColumns, HashToken(code)))
	if errors.Is(err, pgx.ErrNoRows) {
		return JoinLink{}, ErrNotFound
	}
	return l, err
}
