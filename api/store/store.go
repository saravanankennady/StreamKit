// Package store is the control plane's PostgreSQL access layer.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"elnova-streamkit.local/database"
	"elnova-streamkit.local/server/auth"
)

// Errors callers branch on.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("already exists")
)

const pgUniqueViolation = "23505"

// Store wraps a connection pool.
type Store struct {
	DB     *pgxpool.Pool
	Sealer *Sealer
	// MaxIngressPerProject limits the streams one project can have running. Zero sets no limit.
	MaxIngressPerProject int
}

// Open connects and applies migrations.
func Open(ctx context.Context, url, masterKey string) (*Store, error) {
	sealer, err := NewSealer(masterKey)
	if err != nil {
		return nil, fmt.Errorf("STREAMKIT_MASTER_KEY: %w", err)
	}
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("database: %w", err)
	}
	if err := database.Migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{DB: db, Sealer: sealer}, nil
}

// workerSchemaWait is how long a worker waits for the server to bring the database up to date.
const workerSchemaWait = 2 * time.Minute

// OpenWorker connects as a media worker: it needs the schema to be ready, and never changes it, because it connects as a role
// that cannot. The server does the migrating, and may well be doing it now, so this waits for it. A role with no password
// yet is the same wait: the server sets it when it starts.
func OpenWorker(ctx context.Context, url, masterKey string) (*Store, error) {
	sealer, err := NewSealer(masterKey)
	if err != nil {
		return nil, fmt.Errorf("STREAMKIT_MASTER_KEY: %w", err)
	}
	latest, err := database.Latest()
	if err != nil {
		return nil, err
	}
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(workerSchemaWait)
	for {
		var ready bool
		err = db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, latest).Scan(&ready)
		if err == nil && ready {
			return &Store{DB: db, Sealer: sealer}, nil
		}
		if err == nil {
			err = fmt.Errorf("migration %s has not been applied", latest)
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			db.Close()
			return nil, fmt.Errorf("the server has not prepared the database: %w", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

// SetWorkerPassword lets media workers log in, as the role the migrations made for them.
func (s *Store) SetWorkerPassword(ctx context.Context, password string) error {
	var statement string
	if err := s.DB.QueryRow(ctx, `SELECT format('ALTER ROLE streamkit_worker LOGIN PASSWORD %L', $1::text)`, password).Scan(&statement); err != nil {
		return err
	}
	_, err := s.DB.Exec(ctx, statement)
	return err
}

// Close releases the pool.
func (s *Store) Close() { s.DB.Close() }

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func conflict(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		return ErrConflict
	}
	return err
}

// User is an account.
type User struct {
	ID              string     `json:"id"`
	Email           string     `json:"email"`
	Name            string     `json:"name"`
	IsPlatformAdmin bool       `json:"is_platform_admin"`
	EmailVerifiedAt *time.Time `json:"email_verified_at"`
	CreatedAt       time.Time  `json:"created_at"`
	passwordHash    string
}

// Organization is a tenant.
type Organization struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Role      string    `json:"role,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

const userColumns = `id, email, name, is_platform_admin, email_verified_at, created_at, password_hash`

func scanUser(row pgx.Row) (*User, error) {
	u := &User{}
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.IsPlatformAdmin, &u.EmailVerifiedAt, &u.CreatedAt, &u.passwordHash)
	if err != nil {
		return nil, notFound(err)
	}
	return u, nil
}

// ErrRegistrationClosed means this installation has its accounts, and is not taking more.
var ErrRegistrationClosed = errors.New("registration is closed")

// CreateAccount registers a user with their first organization. The first account on an installation is
// its platform administrator, and it can always be made. Others need open.
func (s *Store) CreateAccount(ctx context.Context, email, name, password, orgName string, open bool) (*User, *Organization, error) {
	// Hashing a password is slow on purpose, and a request that is going to be refused should not get to ask for it.
	if !open {
		var anyone bool
		if err := s.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users)`).Scan(&anyone); err != nil {
			return nil, nil, err
		}
		if anyone {
			return nil, nil, ErrRegistrationClosed
		}
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, nil, err
	}
	var user *User
	org := &Organization{Name: orgName, Role: "owner"}
	err = pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		// Serializes first-user detection across concurrent registrations.
		if _, err := tx.Exec(ctx, `LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return err
		}
		var existing int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&existing); err != nil {
			return err
		}
		if existing > 0 && !open {
			return ErrRegistrationClosed
		}
		user, err = scanUser(tx.QueryRow(ctx, `INSERT INTO users (email, name, password_hash, is_platform_admin)
			VALUES ($1, $2, $3, $4) RETURNING `+userColumns, email, name, hash, existing == 0))
		if err != nil {
			return conflict(err)
		}
		if err := tx.QueryRow(ctx, `INSERT INTO organizations (name) VALUES ($1) RETURNING id, created_at`, orgName).
			Scan(&org.ID, &org.CreatedAt); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`, org.ID, user.ID)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return user, org, nil
}

// DummyPasswordCheck takes as long as a real check. Using it for unknown emails keeps login timing from
// revealing which addresses have accounts.
var dummyHash, _ = HashPassword("not a real password")

// Authenticate returns the user if the credentials match.
func (s *Store) Authenticate(ctx context.Context, email, password string) (*User, error) {
	u, err := scanUser(s.DB.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE email = $1`, email))
	if err != nil {
		CheckPassword(dummyHash, password)
		return nil, err
	}
	if !CheckPassword(u.passwordHash, password) {
		return nil, ErrNotFound
	}
	return u, nil
}

// UserByID loads a user.
func (s *Store) UserByID(ctx context.Context, id string) (*User, error) {
	return scanUser(s.DB.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id))
}

// UserByEmail loads a user.
func (s *Store) UserByEmail(ctx context.Context, email string) (*User, error) {
	return scanUser(s.DB.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE email = $1`, email))
}

// Session is a logged-in browser.
type Session struct {
	ID         string    `json:"id"`
	UserID     string    `json:"-"`
	UserAgent  string    `json:"user_agent"`
	IP         string    `json:"ip"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Current    bool      `json:"current"`
}

// SessionTTL is how long a login lasts without activity.
const SessionTTL = 14 * 24 * time.Hour

// CreateSession starts a session and returns the secret to place in a cookie.
func (s *Store) CreateSession(ctx context.Context, userID, userAgent, ip string) (string, error) {
	token := RandomToken(32)
	_, err := s.DB.Exec(ctx, `INSERT INTO sessions (user_id, token_hash, user_agent, ip, expires_at)
		VALUES ($1, $2, $3, $4, $5)`, userID, HashToken(token), truncate(userAgent, 512), ip, time.Now().Add(SessionTTL))
	if err != nil {
		return "", err
	}
	return token, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// UserBySession resolves a session cookie. It extends the session at most once a minute, to avoid a write
// on every request.
func (s *Store) UserBySession(ctx context.Context, token string) (*User, string, error) {
	var sessionID string
	var lastSeen time.Time
	u := &User{}
	err := s.DB.QueryRow(ctx, `SELECT s.id, s.last_seen_at, u.id, u.email, u.name, u.is_platform_admin, u.email_verified_at, u.created_at, u.password_hash
		FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token_hash = $1 AND s.expires_at > now()`, HashToken(token)).
		Scan(&sessionID, &lastSeen, &u.ID, &u.Email, &u.Name, &u.IsPlatformAdmin, &u.EmailVerifiedAt, &u.CreatedAt, &u.passwordHash)
	if err != nil {
		return nil, "", notFound(err)
	}
	if time.Since(lastSeen) > time.Minute {
		_, _ = s.DB.Exec(ctx, `UPDATE sessions SET last_seen_at = now(), expires_at = $2 WHERE id = $1`, sessionID, time.Now().Add(SessionTTL))
	}
	return u, sessionID, nil
}

// ListSessions returns a user's active sessions.
func (s *Store) ListSessions(ctx context.Context, userID, currentID string) ([]Session, error) {
	rows, err := s.DB.Query(ctx, `SELECT id, user_agent, ip, created_at, last_seen_at, expires_at FROM sessions
		WHERE user_id = $1 AND expires_at > now() ORDER BY last_seen_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Session, error) {
		var x Session
		err := r.Scan(&x.ID, &x.UserAgent, &x.IP, &x.CreatedAt, &x.LastSeenAt, &x.ExpiresAt)
		x.Current = x.ID == currentID
		return x, err
	})
}

// DeleteSession logs out one of a user's sessions.
func (s *Store) DeleteSession(ctx context.Context, userID, sessionID string) error {
	tag, err := s.DB.Exec(ctx, `DELETE FROM sessions WHERE id = $1 AND user_id = $2`, sessionID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Email token purposes.
const (
	PurposeVerifyEmail   = "verify_email"
	PurposeResetPassword = "reset_password"
)

// CreateEmailToken issues a one-time token, replacing earlier unused ones for the same purpose.
func (s *Store) CreateEmailToken(ctx context.Context, userID, purpose string, ttl time.Duration) (string, error) {
	token := RandomToken(32)
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM email_tokens WHERE user_id = $1 AND purpose = $2 AND used_at IS NULL`, userID, purpose); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO email_tokens (user_id, purpose, token_hash, expires_at) VALUES ($1, $2, $3, $4)`,
			userID, purpose, HashToken(token), time.Now().Add(ttl))
		return err
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// ConsumeEmailToken marks a token used and returns its user. A token works once.
func (s *Store) ConsumeEmailToken(ctx context.Context, token, purpose string) (string, error) {
	var userID string
	err := s.DB.QueryRow(ctx, `UPDATE email_tokens SET used_at = now()
		WHERE token_hash = $1 AND purpose = $2 AND used_at IS NULL AND expires_at > now() RETURNING user_id`,
		HashToken(token), purpose).Scan(&userID)
	if err != nil {
		return "", notFound(err)
	}
	return userID, nil
}

// MarkEmailVerified records a verified address.
func (s *Store) MarkEmailVerified(ctx context.Context, userID string) error {
	_, err := s.DB.Exec(ctx, `UPDATE users SET email_verified_at = now(), updated_at = now() WHERE id = $1`, userID)
	return err
}

// SetPassword changes a password and ends every session: whoever knew the old one must not stay logged in.
func (s *Store) SetPassword(ctx context.Context, userID, password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1`, userID, hash); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID)
		return err
	})
}

// OrganizationsOf lists a user's organizations with their role.
func (s *Store) OrganizationsOf(ctx context.Context, userID string) ([]Organization, error) {
	rows, err := s.DB.Query(ctx, `SELECT o.id, o.name, m.role, o.created_at FROM organizations o
		JOIN memberships m ON m.org_id = o.id WHERE m.user_id = $1 ORDER BY o.created_at`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Organization])
}

// RoleIn returns a user's role in an organization, or ErrNotFound.
func (s *Store) RoleIn(ctx context.Context, userID, orgID string) (string, error) {
	var role string
	err := s.DB.QueryRow(ctx, `SELECT role FROM memberships WHERE user_id = $1 AND org_id = $2`, userID, orgID).Scan(&role)
	return role, notFound(err)
}

// ProjectSettings are the per-project options the server applies.
type ProjectSettings struct {
	EmptyTimeout     uint32      `json:"empty_timeout,omitempty"`
	DepartureTimeout uint32      `json:"departure_timeout,omitempty"`
	MaxParticipants  uint32      `json:"max_participants,omitempty"`
	AutoCreateRooms  *bool       `json:"auto_create_rooms,omitempty"`
	AllowedOrigins   []string    `json:"allowed_origins,omitempty"`
	TokenTTLSeconds  int         `json:"token_ttl_seconds,omitempty"`
	ICEServers       []ICEServer `json:"ice_servers,omitempty"`
}

// ICEServer is a STUN or TURN server for this project's clients.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// Project is a tenant's isolated space: rooms, keys and webhooks.
type Project struct {
	ID          string          `json:"id"`
	OrgID       string          `json:"org_id"`
	Name        string          `json:"name"`
	Region      string          `json:"region"`
	Environment string          `json:"environment"`
	Status      string          `json:"status"`
	Settings    ProjectSettings `json:"settings"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

const projectColumns = `id, org_id, name, region, environment, status, settings, created_at, updated_at`

func scanProject(row pgx.Row) (*Project, error) {
	p := &Project{}
	var settings []byte
	if err := row.Scan(&p.ID, &p.OrgID, &p.Name, &p.Region, &p.Environment, &p.Status, &settings, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, notFound(err)
	}
	if err := json.Unmarshal(settings, &p.Settings); err != nil {
		return nil, err
	}
	return p, nil
}

// CreateProject adds a project to an organization.
func (s *Store) CreateProject(ctx context.Context, orgID, name, region, environment string, settings ProjectSettings) (*Project, error) {
	data, err := json.Marshal(settings)
	if err != nil {
		return nil, err
	}
	return scanProject(s.DB.QueryRow(ctx, `INSERT INTO projects (org_id, name, region, environment, settings)
		VALUES ($1, $2, $3, $4, $5) RETURNING `+projectColumns, orgID, name, region, environment, data))
}

// ProjectByID loads a project that has not been deleted.
func (s *Store) ProjectByID(ctx context.Context, id string) (*Project, error) {
	return scanProject(s.DB.QueryRow(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = $1 AND deleted_at IS NULL`, id))
}

// ProjectsForUser lists the projects a user can reach.
func (s *Store) ProjectsForUser(ctx context.Context, userID string) ([]*Project, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+prefixed("p.", projectColumns)+` FROM projects p
		JOIN memberships m ON m.org_id = p.org_id
		WHERE m.user_id = $1 AND p.deleted_at IS NULL ORDER BY p.created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func prefixed(prefix, columns string) string {
	out := ""
	start := 0
	for i := 0; i <= len(columns); i++ {
		if i == len(columns) || columns[i] == ',' {
			col := columns[start:i]
			for len(col) > 0 && col[0] == ' ' {
				col = col[1:]
			}
			if out != "" {
				out += ", "
			}
			out += prefix + col
			start = i + 1
		}
	}
	return out
}

// UpdateProject saves a project's editable fields.
func (s *Store) UpdateProject(ctx context.Context, p *Project) (*Project, error) {
	data, err := json.Marshal(p.Settings)
	if err != nil {
		return nil, err
	}
	return scanProject(s.DB.QueryRow(ctx, `UPDATE projects SET name = $2, region = $3, environment = $4, status = $5, settings = $6, updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL RETURNING `+projectColumns, p.ID, p.Name, p.Region, p.Environment, p.Status, data))
}

// DeleteProject soft-deletes a project and revokes its keys, so nothing can connect to it afterward.
func (s *Store) DeleteProject(ctx context.Context, id string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE projects SET deleted_at = now() WHERE id = $1 AND deleted_at IS NULL`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if _, err = tx.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE project_id = $1 AND revoked_at IS NULL`, id); err != nil {
			return err
		}
		if err := stopProjectMedia(ctx, tx, id, "The project was deleted."); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE ingresses SET enabled = false, pull_requested = false WHERE project_id = $1`, id)
		return err
	})
}

// SuspendProjectMedia stops what is running for a project that has just been suspended. Ingresses stay as they are, so
// they work again when it comes back.
func (s *Store) SuspendProjectMedia(ctx context.Context, id string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		return stopProjectMedia(ctx, tx, id, "The project was suspended.")
	})
}

// APIKey is a project credential. The secret is only ever returned when created.
type APIKey struct {
	ID        string     `json:"id"`
	ProjectID string     `json:"project_id"`
	APIKey    string     `json:"api_key"`
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at"`
	// Secret is set only in the response that creates a key.
	Secret string `json:"secret,omitempty"`
}

// CreateAPIKey generates a new credential.
func (s *Store) CreateAPIKey(ctx context.Context, projectID, name string, expiresAt *time.Time) (*APIKey, error) {
	secret := RandomToken(32)
	sealed, err := s.Sealer.Seal(secret)
	if err != nil {
		return nil, err
	}
	k := &APIKey{ProjectID: projectID, Name: name, APIKey: "API" + RandomToken(12), ExpiresAt: expiresAt, Secret: secret}
	err = s.DB.QueryRow(ctx, `INSERT INTO api_keys (project_id, api_key, secret_ciphertext, name, expires_at)
		VALUES ($1, $2, $3, $4, $5) RETURNING id, created_at`, projectID, k.APIKey, sealed, name, expiresAt).Scan(&k.ID, &k.CreatedAt)
	if err != nil {
		return nil, err
	}
	return k, nil
}

// ListAPIKeys returns a project's keys, without secrets.
func (s *Store) ListAPIKeys(ctx context.Context, projectID string) ([]APIKey, error) {
	rows, err := s.DB.Query(ctx, `SELECT id, project_id, api_key, name, created_at, expires_at, revoked_at
		FROM api_keys WHERE project_id = $1 ORDER BY created_at DESC`, projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (APIKey, error) {
		var k APIKey
		err := r.Scan(&k.ID, &k.ProjectID, &k.APIKey, &k.Name, &k.CreatedAt, &k.ExpiresAt, &k.RevokedAt)
		return k, err
	})
}

// RevokeAPIKey disables a key for good.
func (s *Store) RevokeAPIKey(ctx context.Context, projectID, id string) error {
	tag, err := s.DB.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND project_id = $2 AND revoked_at IS NULL`, id, projectID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RotateAPIKey issues a replacement and makes the old key expire after overlap, so running services have
// time to switch without an outage.
func (s *Store) RotateAPIKey(ctx context.Context, projectID, id string, overlap time.Duration) (*APIKey, error) {
	var name string
	expires := time.Now().Add(overlap)
	err := s.DB.QueryRow(ctx, `UPDATE api_keys SET expires_at = LEAST(COALESCE(expires_at, $3), $3)
		WHERE id = $1 AND project_id = $2 AND revoked_at IS NULL RETURNING name`, id, projectID, expires).Scan(&name)
	if err != nil {
		return nil, notFound(err)
	}
	return s.CreateAPIKey(ctx, projectID, name, nil)
}

// LookupAPIKey resolves a key for token verification, decrypting its secret. Revoked, expired and
// deleted-project keys count as missing.
func (s *Store) LookupAPIKey(ctx context.Context, apiKey string) (auth.Key, error) {
	var sealed []byte
	var projectID string
	err := s.DB.QueryRow(ctx, `SELECT k.secret_ciphertext, k.project_id FROM api_keys k
		JOIN projects p ON p.id = k.project_id
		WHERE k.api_key = $1 AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at > now())
		AND p.deleted_at IS NULL AND p.status = 'active'`, apiKey).Scan(&sealed, &projectID)
	if err != nil {
		return auth.Key{}, notFound(err)
	}
	secret, err := s.Sealer.Open(sealed)
	if err != nil {
		return auth.Key{}, err
	}
	return auth.Key{APIKey: apiKey, Secret: secret, ProjectID: projectID}, nil
}

// KeyByID loads one project key with its secret, for signing.
func (s *Store) KeyByID(ctx context.Context, projectID, id string) (auth.Key, error) {
	var sealed []byte
	var apiKey string
	err := s.DB.QueryRow(ctx, `SELECT secret_ciphertext, api_key FROM api_keys
		WHERE id = $1 AND project_id = $2 AND revoked_at IS NULL`, id, projectID).Scan(&sealed, &apiKey)
	if err != nil {
		return auth.Key{}, notFound(err)
	}
	secret, err := s.Sealer.Open(sealed)
	if err != nil {
		return auth.Key{}, err
	}
	return auth.Key{APIKey: apiKey, Secret: secret, ProjectID: projectID}, nil
}

// How long key and settings lookups stay cached. Revoking a key takes at most this long to reach every
// node, and takes effect at once on the node that handled the request, through Invalidate.
const (
	keyCacheTTL      = 30 * time.Second
	negativeCacheTTL = 5 * time.Second
)

type cachedKey struct {
	key     auth.Key
	ok      bool
	expires time.Time
}

// KeyCache implements auth.KeyProvider over the database, with static keys from the config file taking
// precedence. It caches, because every token verification needs a lookup. It remembers misses briefly so
// tokens naming made-up keys cannot hammer the database.
type KeyCache struct {
	store  *Store
	static auth.KeyProvider

	mu      sync.Mutex
	entries map[string]cachedKey
}

// NewKeyCache creates a KeyCache. static may be nil.
func NewKeyCache(store *Store, static auth.KeyProvider) *KeyCache {
	return &KeyCache{store: store, static: static, entries: map[string]cachedKey{}}
}

// Lookup implements auth.KeyProvider.
func (c *KeyCache) Lookup(apiKey string) (auth.Key, bool) {
	if c.static != nil {
		if k, ok := c.static.Lookup(apiKey); ok {
			return k, true
		}
	}
	now := time.Now()
	c.mu.Lock()
	entry, hit := c.entries[apiKey]
	c.mu.Unlock()
	if hit && now.Before(entry.expires) {
		return entry.key, entry.ok
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	key, err := c.store.LookupAPIKey(ctx, apiKey)
	if err != nil && !errors.Is(err, ErrNotFound) && hit && entry.ok {
		// A database outage should not log everyone out: keep using what we knew.
		return entry.key, true
	}
	entry = cachedKey{key: key, ok: err == nil, expires: now.Add(keyCacheTTL)}
	if err != nil {
		entry.expires = now.Add(negativeCacheTTL)
	}
	c.mu.Lock()
	if len(c.entries) > 100_000 {
		c.entries = map[string]cachedKey{}
	}
	c.entries[apiKey] = entry
	c.mu.Unlock()
	return entry.key, entry.ok
}

// Invalidate forgets a cached key, after it is revoked or rotated.
func (c *KeyCache) Invalidate(apiKey string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, apiKey)
}

// InvalidateAll forgets every cached key.
func (c *KeyCache) InvalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = map[string]cachedKey{}
}
