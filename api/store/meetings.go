package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// Roles in a meeting, from most to least authority. A host link makes hosts, a guest link makes guests, and a
// host can make a guest a moderator.
const (
	RoleHost      = "host"
	RoleModerator = "moderator"
	RoleGuest     = "guest"
)

// States of a meeting member.
const (
	// MemberWaiting is someone in the waiting room. They hold a ticket, and no token.
	MemberWaiting = "waiting"
	// MemberMinted holds a token and has not connected yet, so a lock can still stop them.
	MemberMinted = "minted"
	// MemberAdmitted has connected, or was let in by a host. They can reconnect through a lock.
	MemberAdmitted = "admitted"
	// MemberDenied was turned away from the waiting room.
	MemberDenied = "denied"
	// MemberRemoved was removed by a host. Their token is refused from then on.
	MemberRemoved = "removed"
	// MemberEnded was part of a meeting that ended. Their token is refused, and the link works again if a host restarts it.
	MemberEnded = "ended"
)

// waitingSeenWithin is how recently someone in the waiting room must have checked in to still count as waiting.
const waitingSeenWithin = 15 * time.Second

// pgCharacterNotInRepertoire is what PostgreSQL says about a NUL in text.
const pgCharacterNotInRepertoire = "22021"

// Meeting is what hosts control about a room. A room with no row behaves like a meeting with everything off.
type Meeting struct {
	ProjectID   string     `json:"-"`
	Room        string     `json:"room"`
	Title       string     `json:"title"`
	Locked      bool       `json:"locked"`
	WaitingRoom bool       `json:"waiting_room"`
	CreatedAt   time.Time  `json:"created_at"`
	EndedAt     *time.Time `json:"ended_at"`
}

// MeetingMember is one identity a share link has handed out.
type MeetingMember struct {
	ProjectID string
	Room      string
	Identity  string
	Name      string
	Role      string
	State     string
	LinkID    *string
	// LinkRevoked says the link that handed out this identity has since been turned off.
	LinkRevoked bool
	// MeetingEnded says the meeting is over. Ending one marks its members, and this catches a row made as that happened.
	MeetingEnded bool
}

// CanAct reports whether this member still has a place in the meeting. Turning off a link stops anyone who only holds a
// token from it, and any host who came through it, since a host link is a key that may have leaked. A guest already in
// the meeting has done nothing wrong, and stays.
func (m *MeetingMember) CanAct() bool {
	if m.MeetingEnded {
		return false
	}
	switch m.State {
	case MemberMinted:
		return !m.LinkRevoked
	case MemberAdmitted:
		return !m.LinkRevoked || m.Role != RoleHost
	}
	return false
}

const meetingColumns = `project_id, room, title, locked, waiting_room, created_at, ended_at`

func scanMeeting(r pgx.Row) (Meeting, error) {
	var m Meeting
	err := r.Scan(&m.ProjectID, &m.Room, &m.Title, &m.Locked, &m.WaitingRoom, &m.CreatedAt, &m.EndedAt)
	return m, notFound(err)
}

// ErrTooManyMeetings means a project has all the meetings it may have open.
var ErrTooManyMeetings = errors.New("too many open meetings")

// CreateMeeting records a new meeting, if its project has fewer than maxOpen that can still be joined: not ended, with a
// link that still works. Zero sets no limit. Requests arriving together can go a little over.
func (s *Store) CreateMeeting(ctx context.Context, m Meeting, createdBy string, maxOpen int) (Meeting, error) {
	out, err := scanMeeting(s.DB.QueryRow(ctx, `INSERT INTO meetings (project_id, room, title, waiting_room, created_by)
		SELECT $1::uuid, $2::text, $3::text, $4::boolean, NULLIF($5::text, '')::uuid
		WHERE $6::int = 0 OR (SELECT count(*) FROM meetings g WHERE g.project_id = $1::uuid AND g.ended_at IS NULL AND EXISTS (
			SELECT 1 FROM join_links l WHERE l.project_id = g.project_id AND l.room = g.room AND l.revoked_at IS NULL AND l.expires_at > now())) < $6::int
		RETURNING `+meetingColumns,
		m.ProjectID, m.Room, m.Title, m.WaitingRoom, createdBy, maxOpen))
	if errors.Is(err, ErrNotFound) {
		return out, ErrTooManyMeetings
	}
	return out, err
}

// EnsureMeeting records a meeting for a room that has none yet, and returns what is there.
func (s *Store) EnsureMeeting(ctx context.Context, projectID, room, title string) (Meeting, error) {
	return scanMeeting(s.DB.QueryRow(ctx, `INSERT INTO meetings (project_id, room, title) VALUES ($1, $2, $3)
		ON CONFLICT (project_id, room) DO UPDATE SET room = EXCLUDED.room RETURNING `+meetingColumns, projectID, room, title))
}

// MeetingByRoom returns a room's meeting, or ErrNotFound if it has none.
func (s *Store) MeetingByRoom(ctx context.Context, projectID, room string) (Meeting, error) {
	return scanMeeting(s.DB.QueryRow(ctx, `SELECT `+meetingColumns+` FROM meetings WHERE project_id = $1 AND room = $2`, projectID, room))
}

// UpdateMeeting changes what is given, for the members in it right now as well as anyone new. Locking sends away
// anyone still in the waiting room: nobody new means nobody new.
func (s *Store) UpdateMeeting(ctx context.Context, projectID, room string, title *string, locked, waitingRoom *bool) (Meeting, error) {
	var out Meeting
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var err error
		out, err = scanMeeting(tx.QueryRow(ctx, `INSERT INTO meetings (project_id, room, title, locked, waiting_room)
			VALUES ($1, $2, COALESCE($3, ''), COALESCE($4, false), COALESCE($5, false))
			ON CONFLICT (project_id, room) DO UPDATE SET
				title = COALESCE($3, meetings.title), locked = COALESCE($4, meetings.locked), waiting_room = COALESCE($5, meetings.waiting_room)
			RETURNING `+meetingColumns, projectID, room, title, locked, waitingRoom))
		if err != nil {
			return err
		}
		if out.Locked {
			// They keep their ticket, which is how they find out.
			_, err = tx.Exec(ctx, `UPDATE meeting_members SET state = $3
				WHERE project_id = $1 AND room = $2 AND state = $4`, projectID, room, MemberDenied, MemberWaiting)
		}
		return err
	})
	return out, err
}

// EndMeeting ends a meeting for everyone. Every token it has handed out stops working, and so does its guest link
// until a host starts it again.
func (s *Store) EndMeeting(ctx context.Context, projectID, room string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO meetings (project_id, room, ended_at) VALUES ($1, $2, now())
			ON CONFLICT (project_id, room) DO UPDATE SET ended_at = now()`, projectID, room); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE meeting_members SET state = $3
			WHERE project_id = $1 AND room = $2 AND state NOT IN ($4, $5)`, projectID, room, MemberEnded, MemberRemoved, MemberDenied)
		return err
	})
}

// OpenMeetingForHost readies a meeting for a host arriving: it starts one that ended, with its door back to how a new
// meeting starts, and makes one if the room has none.
func (s *Store) OpenMeetingForHost(ctx context.Context, projectID, room, title string) (Meeting, error) {
	return scanMeeting(s.DB.QueryRow(ctx, `INSERT INTO meetings (project_id, room, title) VALUES ($1, $2, $3)
		ON CONFLICT (project_id, room) DO UPDATE SET
			locked = CASE WHEN meetings.ended_at IS NULL THEN meetings.locked ELSE false END,
			waiting_room = CASE WHEN meetings.ended_at IS NULL THEN meetings.waiting_room ELSE false END,
			ended_at = NULL
		RETURNING `+meetingColumns, projectID, room, title))
}

// MemberLimits is how many identities a link and a meeting can hand out, counting rows still being kept. Each use of a link
// leaves a row, and nothing else stops someone with a guest link making them. Zero sets no limit.
type MemberLimits struct {
	PerLink    int
	PerMeeting int
}

// ErrTooManyMembers means a link or a meeting has handed out all the identities it may.
var ErrTooManyMembers = errors.New("too many identities handed out")

// AddMeetingMember records an identity a link has handed out. A ticket is stored only as a hash. Requests arriving together
// can go a little over the limits, by no more than there are requests in flight.
func (s *Store) AddMeetingMember(ctx context.Context, m MeetingMember, ticket string, limits MemberLimits) error {
	var hash []byte
	if ticket != "" {
		hash = HashToken(ticket)
	}
	tag, err := s.DB.Exec(ctx, `INSERT INTO meeting_members (project_id, room, identity, name, role, state, link_id, ticket_hash)
		SELECT $1::uuid, $2::text, $3::text, $4::text, $5::text, $6::text, $7::uuid, $8::bytea
		WHERE ($9::int = 0 OR (SELECT count(*) FROM meeting_members WHERE link_id = $7::uuid) < $9::int)
			AND ($10::int = 0 OR (SELECT count(*) FROM meeting_members WHERE project_id = $1::uuid AND room = $2::text) < $10::int)`,
		m.ProjectID, m.Room, m.Identity, m.Name, m.Role, m.State, m.LinkID, hash, limits.PerLink, limits.PerMeeting)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrTooManyMembers
	}
	return nil
}

// memberLinkRevoked is whether a member's link has been turned off, for a row named m. This looks at the link when asked,
// rather than marking its members when it is turned off, so a join in flight at that moment cannot slip past it.
const memberLinkRevoked = `COALESCE((SELECT l.revoked_at IS NOT NULL FROM join_links l WHERE l.id = m.link_id), false)`

// memberMeetingEnded is whether a member's meeting is over, for a row named m.
const memberMeetingEnded = `COALESCE((SELECT g.ended_at IS NOT NULL FROM meetings g WHERE g.project_id = m.project_id AND g.room = m.room), false)`

const meetingMemberColumns = `m.project_id, m.room, m.identity, m.name, m.role, m.state, m.link_id, ` + memberLinkRevoked + `, ` + memberMeetingEnded

func scanMeetingMember(r pgx.Row) (*MeetingMember, error) {
	m := &MeetingMember{}
	err := r.Scan(&m.ProjectID, &m.Room, &m.Identity, &m.Name, &m.Role, &m.State, &m.LinkID, &m.LinkRevoked, &m.MeetingEnded)
	if err != nil {
		return nil, notFound(err)
	}
	return m, nil
}

// MeetingMemberByIdentity finds a member, in any state.
func (s *Store) MeetingMemberByIdentity(ctx context.Context, projectID, room, identity string) (*MeetingMember, error) {
	return scanMeetingMember(s.DB.QueryRow(ctx, `SELECT `+meetingMemberColumns+` FROM meeting_members m
		WHERE m.project_id = $1 AND m.room = $2 AND m.identity = $3`, projectID, room, identity))
}

// Refusals from the door. The caller says these in its own words.
var (
	ErrMemberLocked  = errors.New("the meeting is locked")
	ErrMemberWaiting = errors.New("the waiting room is on")
	ErrMemberRemoved = errors.New("removed from the meeting")
	ErrMemberDenied  = errors.New("turned away from the waiting room")
	ErrMemberEnded   = errors.New("the meeting has ended")
	ErrMemberRevoked = errors.New("the link they came through has been turned off")
)

// CheckMeetingMember decides whether a member may start a session, and returns their role. With commit it also records
// that they are in, so they can reconnect later through a lock. The decision and the record are one statement, which
// is what stops a token being let in by a door that has just closed. It returns ErrNotFound for an identity no link
// handed out.
func (s *Store) CheckMeetingMember(ctx context.Context, projectID, room, identity string, commit bool) (string, error) {
	// A token held by someone who has not connected is only good while the door is open. Everyone else who is still a
	// member has already been through it.
	const allowed = `(NOT ` + memberMeetingEnded + ` AND ((m.state = 'admitted' AND (m.role <> 'host' OR NOT ` + memberLinkRevoked + `)) OR (m.state = 'minted' AND NOT ` + memberLinkRevoked + ` AND NOT COALESCE(
		(SELECT g.locked OR g.waiting_room FROM meetings g WHERE g.project_id = m.project_id AND g.room = m.room), false))))`
	var role string
	var err error
	if commit {
		err = s.DB.QueryRow(ctx, `UPDATE meeting_members m SET state = 'admitted', joined_at = COALESCE(m.joined_at, now()), seen_at = now()
			WHERE m.project_id = $1 AND m.room = $2 AND m.identity = $3 AND `+allowed+` RETURNING m.role`, projectID, room, identity).Scan(&role)
	} else {
		err = s.DB.QueryRow(ctx, `SELECT m.role FROM meeting_members m
			WHERE m.project_id = $1 AND m.room = $2 AND m.identity = $3 AND `+allowed, projectID, room, identity).Scan(&role)
	}
	if err == nil {
		return role, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	// Refused, or not ours. A second look says which, and why.
	var state string
	var locked, revoked, ended bool
	err = s.DB.QueryRow(ctx, `SELECT m.state, COALESCE((SELECT g.locked FROM meetings g WHERE g.project_id = m.project_id AND g.room = m.room), false), `+memberLinkRevoked+`, `+memberMeetingEnded+`
		FROM meeting_members m WHERE m.project_id = $1 AND m.room = $2 AND m.identity = $3`, projectID, room, identity).Scan(&state, &locked, &revoked, &ended)
	if err != nil {
		return "", notFound(err)
	}
	switch state {
	case MemberRemoved:
		return "", ErrMemberRemoved
	case MemberDenied:
		return "", ErrMemberDenied
	case MemberEnded:
		return "", ErrMemberEnded
	}
	if ended {
		return "", ErrMemberEnded
	}
	if revoked {
		return "", ErrMemberRevoked
	}
	if locked {
		return "", ErrMemberLocked
	}
	return "", ErrMemberWaiting
}

// RemoveMeetingMember refuses a member from now on, whether they are connected or not, and reports whether there was one.
// It does nothing for an identity no link handed out, including anyone in the project static API keys belong to, which
// has no row.
func (s *Store) RemoveMeetingMember(ctx context.Context, projectID, room, identity string) (bool, error) {
	var id pgtype.UUID
	if id.Scan(projectID) != nil {
		return false, nil
	}
	tag, err := s.DB.Exec(ctx, `UPDATE meeting_members SET state = $4, ticket_hash = NULL
		WHERE project_id = $1 AND room = $2 AND identity = $3`, id, room, identity, MemberRemoved)
	return tag.RowsAffected() > 0, err
}

// SetMeetingMemberRole changes a member's role.
func (s *Store) SetMeetingMemberRole(ctx context.Context, projectID, room, identity, role string) error {
	var changed string
	err := s.DB.QueryRow(ctx, `UPDATE meeting_members SET role = $4 WHERE project_id = $1 AND room = $2 AND identity = $3 RETURNING identity`,
		projectID, room, identity, role).Scan(&changed)
	return notFound(err)
}

// WaitingMember is someone in the waiting room, as a host sees them.
type WaitingMember struct {
	Identity string `json:"identity"`
	Name     string `json:"name"`
}

// ListWaiting returns who is waiting and still there, longest first, up to limit of them, and how many there are in all.
func (s *Store) ListWaiting(ctx context.Context, projectID, room string, limit int) ([]WaitingMember, int, error) {
	var total int
	err := s.DB.QueryRow(ctx, `SELECT count(*) FROM meeting_members
		WHERE project_id = $1 AND room = $2 AND state = $3 AND seen_at > now() - make_interval(secs => $4)`,
		projectID, room, MemberWaiting, waitingSeenWithin.Seconds()).Scan(&total)
	if err != nil {
		return nil, 0, err
	}
	rows, err := s.DB.Query(ctx, `SELECT identity, name FROM meeting_members
		WHERE project_id = $1 AND room = $2 AND state = $3 AND seen_at > now() - make_interval(secs => $4)
		ORDER BY created_at, identity LIMIT $5`, projectID, room, MemberWaiting, waitingSeenWithin.Seconds(), limit)
	if err != nil {
		return nil, 0, err
	}
	waiting, err := pgx.CollectRows(rows, pgx.RowToStructByPos[WaitingMember])
	return waiting, total, err
}

// TurnAwayWaiting turns away everyone in the waiting room, and says how many that was. They keep their tickets, which is
// how they find out.
func (s *Store) TurnAwayWaiting(ctx context.Context, projectID, room string) (int64, error) {
	tag, err := s.DB.Exec(ctx, `UPDATE meeting_members SET state = $3 WHERE project_id = $1 AND room = $2 AND state = $4`,
		projectID, room, MemberDenied, MemberWaiting)
	return tag.RowsAffected(), err
}

// AnswerWaiting lets someone in or turns them away. It returns ErrNotFound if they are not waiting any more.
func (s *Store) AnswerWaiting(ctx context.Context, projectID, room, identity string, admit bool) error {
	state := MemberDenied
	if admit {
		state = MemberAdmitted
	}
	var changed string
	err := s.DB.QueryRow(ctx, `UPDATE meeting_members SET state = $4 WHERE project_id = $1 AND room = $2 AND identity = $3 AND state = $5 RETURNING identity`,
		projectID, room, identity, state, MemberWaiting).Scan(&changed)
	return notFound(err)
}

// MemberByTicket finds whoever holds a waiting room ticket, and notes that they are still there. A place in the waiting room
// lapses if nobody has asked about it lately. An answer does not: someone who has been let in or turned away can still find
// out, however late they ask.
func (s *Store) MemberByTicket(ctx context.Context, ticket string) (*MeetingMember, error) {
	return scanMeetingMember(s.DB.QueryRow(ctx, `UPDATE meeting_members m SET seen_at = now()
		WHERE m.ticket_hash = $1 AND (m.state <> $2 OR m.seen_at > now() - make_interval(secs => $3)) RETURNING `+meetingMemberColumns,
		HashToken(ticket), MemberWaiting, waitingSeenWithin.Seconds()))
}

// LeaveWaiting takes someone out of the waiting room at their own request.
func (s *Store) LeaveWaiting(ctx context.Context, ticket string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM meeting_members WHERE ticket_hash = $1 AND state = $2`, HashToken(ticket), MemberWaiting)
	return err
}

// SpendTicket admits a ticket's holder and uses the ticket up, so a token comes out of it once. It decides again, in one
// statement, whether they may have one: a host can turn them away, or a meeting end, after the caller last looked. Someone
// let in can spend it through a lock, and someone still waiting only with the door open.
func (s *Store) SpendTicket(ctx context.Context, ticket string) error {
	var identity string
	err := s.DB.QueryRow(ctx, `UPDATE meeting_members m SET state = $2, ticket_hash = NULL
		WHERE m.ticket_hash = $1 AND NOT `+memberMeetingEnded+` AND (m.state = $2 OR (m.state = $3 AND NOT COALESCE(
			(SELECT g.locked OR g.waiting_room FROM meetings g WHERE g.project_id = m.project_id AND g.room = m.room), false)))
		RETURNING m.identity`, HashToken(ticket), MemberAdmitted, MemberWaiting).Scan(&identity)
	return notFound(err)
}

// ActiveRecording returns the running file recording of a room, or ErrNotFound.
func (s *Store) ActiveRecording(ctx context.Context, projectID, room string) (*Egress, error) {
	return scanEgress(s.DB.QueryRow(ctx, `SELECT `+egressColumns+` FROM egresses
		WHERE project_id = $1 AND room_name = $2 AND format <> '' AND status IN ($3, $4, $5)
		ORDER BY created_at DESC, id DESC LIMIT 1`, projectID, room, EgressStarting, EgressActive, EgressEnding))
}

// ActiveEgressForRoom returns everything still running or queued for a room: recordings, stream outputs, and jobs for one
// person in it.
func (s *Store) ActiveEgressForRoom(ctx context.Context, projectID, room string) ([]*Egress, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+egressColumns+` FROM egresses
		WHERE project_id = $1 AND room_name = $2 AND status IN ($3, $4, $5) ORDER BY created_at, id`, projectID, room, EgressStarting, EgressActive, EgressEnding)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Egress, error) { return scanEgress(r) })
}

// IsUnstorableText reports whether a query failed because of text PostgreSQL cannot hold, which means a NUL.
func IsUnstorableText(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgCharacterNotInRepertoire
}

// IsMeetingRecordingRunning reports whether saving a job failed because its room's record button already has one going.
func IsMeetingRecordingRunning(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == "egresses_one_meeting_recording"
}

// SummaryPerson is one session in a meeting's summary.
type SummaryPerson struct {
	Name     string     `json:"name"`
	Role     string     `json:"role"`
	JoinedAt time.Time  `json:"joined_at"`
	LeftAt   *time.Time `json:"left_at"`
}

// SummaryRecording is one recording in a meeting's summary.
type SummaryRecording struct {
	Status     string `json:"status"`
	DurationMs int64  `json:"duration_ms"`
	FileSize   int64  `json:"file_size"`
}

// MeetingSummary is what the server knows about a session of a room. Nothing in it is inferred.
type MeetingSummary struct {
	Title            string             `json:"title"`
	StartedAt        time.Time          `json:"started_at"`
	EndedAt          *time.Time         `json:"ended_at"`
	PeakParticipants int                `json:"peak_participants"`
	People           []SummaryPerson    `json:"people"`
	Recordings       []SummaryRecording `json:"recordings"`
}

// SummarizeMeeting reads from its history the latest session of a room that this identity was in, which is all they get to
// see of who came. It returns ErrNotFound if there is none, as for someone holding a token they never used.
func (s *Store) SummarizeMeeting(ctx context.Context, projectID, room, identity string) (*MeetingSummary, error) {
	out := &MeetingSummary{}
	var sid string
	err := s.DB.QueryRow(ctx, `SELECT r.sid, r.created_at, r.ended_at, r.peak_participants,
			COALESCE((SELECT g.title FROM meetings g WHERE g.project_id::text = r.project_id AND g.room = r.name), '')
		FROM rooms r WHERE r.project_id = $1 AND r.name = $2
			AND EXISTS (SELECT 1 FROM participant_sessions p WHERE p.room_sid = r.sid AND p.identity = $3)
		ORDER BY r.created_at DESC LIMIT 1`, projectID, room, identity).
		Scan(&sid, &out.StartedAt, &out.EndedAt, &out.PeakParticipants, &out.Title)
	if err != nil {
		return nil, notFound(err)
	}
	rows, err := s.DB.Query(ctx, `SELECT p.name, COALESCE(m.role, ''), p.joined_at, p.left_at
		FROM participant_sessions p LEFT JOIN meeting_members m ON m.project_id::text = p.project_id AND m.room = $2 AND m.identity = p.identity
		WHERE p.room_sid = $1 ORDER BY p.joined_at, p.sid`, sid, room)
	if err != nil {
		return nil, err
	}
	if out.People, err = pgx.CollectRows(rows, pgx.RowToStructByPos[SummaryPerson]); err != nil {
		return nil, err
	}
	rows, err = s.DB.Query(ctx, `SELECT status, duration_ms, file_size FROM egresses
		WHERE project_id = $1 AND room_sid = $2 AND format <> '' ORDER BY created_at, id`, projectID, sid)
	if err != nil {
		return nil, err
	}
	if out.Recordings, err = pgx.CollectRows(rows, pgx.RowToStructByPos[SummaryRecording]); err != nil {
		return nil, err
	}
	return out, nil
}
