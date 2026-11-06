package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/participant"
)

// Errors for media operations.
var (
	// ErrJobLost means this worker no longer owns a job: its lease ran out, or the job was deleted. The worker
	// must stop and must not write anything more about it.
	ErrJobLost = errors.New("this worker no longer owns the job")
	// ErrNotRunning means a job has already ended.
	ErrNotRunning = errors.New("the job is not running")
	// ErrStillRunning means a job must end before this can happen.
	ErrStillRunning = errors.New("the job is still running")
	// ErrIngressBusy means an ingress already has a publisher.
	ErrIngressBusy = errors.New("this ingress already has a publisher")
	// ErrIngressDisabled means an ingress has been turned off.
	ErrIngressDisabled = errors.New("this ingress is disabled")
	// ErrProjectAtLimit means a project already has as much running as it may.
	ErrProjectAtLimit = errors.New("this project is running as much as it may")
	// ErrNoCapacity means no worker has room for another job.
	ErrNoCapacity = errors.New("no worker has room for this job")
	// ErrProjectEnded means a project has been deleted or suspended, so nothing may run for it.
	ErrProjectEnded = errors.New("this project has been deleted or suspended")
)

// Egress statuses. See egressTransitions for how a job moves between them.
const (
	EgressStarting = "starting"
	EgressActive   = "active"
	EgressEnding   = "ending"
	EgressComplete = "complete"
	EgressFailed   = "failed"
	EgressAborted  = "aborted"
)

// egressTransitions is the only definition of which status can follow which. Failed means it broke, aborted
// means it ended before any media was recorded, and complete means the output is durable.
var egressTransitions = map[string][]string{
	EgressActive:   {EgressStarting},
	EgressEnding:   {EgressStarting, EgressActive},
	EgressComplete: {EgressActive, EgressEnding},
	EgressFailed:   {EgressStarting, EgressActive, EgressEnding},
	EgressAborted:  {EgressStarting, EgressActive, EgressEnding},
}

// CanTransition reports whether a job may move between two statuses.
func CanTransition(from, to string) bool {
	for _, f := range egressTransitions[to] {
		if f == from {
			return true
		}
	}
	return false
}

// EgressIsTerminal reports whether a status is final.
func EgressIsTerminal(status string) bool {
	return status == EgressComplete || status == EgressFailed || status == EgressAborted
}

// EgressOutput is a file output.
type EgressOutput struct {
	Kind   string `json:"kind"`
	Format string `json:"format"`
}

// EgressRequest is what a job records. Stream URLs live apart from it, because they hold stream keys.
type EgressRequest struct {
	Type        string        `json:"type"`
	Room        string        `json:"room"`
	Participant string        `json:"participant,omitempty"`
	Layout      string        `json:"layout,omitempty"`
	Output      *EgressOutput `json:"output,omitempty"`
	// MeetingRecording marks what a meeting's record button started. A room can run one of these at a time.
	MeetingRecording bool `json:"meeting_recording,omitempty"`
}

// Egress is a recording or streaming job.
type Egress struct {
	ID         string     `json:"id"`
	ProjectID  string     `json:"project_id"`
	RoomName   string     `json:"room_name"`
	RoomSID    string     `json:"room_sid"`
	Type       string     `json:"type"`
	Status     string     `json:"status"`
	Format     string     `json:"format"`
	OutputURL  string     `json:"output_url"`
	StreamURLs []string   `json:"stream_urls"`
	FileSize   int64      `json:"file_size"`
	DurationMs int64      `json:"duration_ms"`
	Error      string     `json:"error"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at"`
	RetryOf    *string    `json:"retry_of"`

	Request    EgressRequest `json:"-"`
	OutputPath string        `json:"-"`
	Storage    string        `json:"-"`
	WorkerID   *string       `json:"-"`
	CPUSeconds float64       `json:"-"`
	sealedURLs []byte
}

const egressColumns = `id, project_id, room_name, room_sid, type, status, format, output_url, stream_urls, file_size, duration_ms, error,
	created_at, started_at, ended_at, retry_of, request, output_path, storage, worker_id, cpu_seconds, stream_urls_sealed`

func scanEgress(row pgx.Row) (*Egress, error) {
	e := &Egress{}
	var request []byte
	err := row.Scan(&e.ID, &e.ProjectID, &e.RoomName, &e.RoomSID, &e.Type, &e.Status, &e.Format, &e.OutputURL, &e.StreamURLs, &e.FileSize, &e.DurationMs, &e.Error,
		&e.CreatedAt, &e.StartedAt, &e.EndedAt, &e.RetryOf, &request, &e.OutputPath, &e.Storage, &e.WorkerID, &e.CPUSeconds, &e.sealedURLs)
	if err != nil {
		return nil, notFound(err)
	}
	if err := json.Unmarshal(request, &e.Request); err != nil {
		return nil, err
	}
	if e.StreamURLs == nil {
		e.StreamURLs = []string{}
	}
	return e, nil
}

// RedactStreamURL hides the parts of a stream URL that act as a password: the stream key and any credentials.
func RedactStreamURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "{redacted}"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	path := strings.TrimRight(u.Path, "/")
	i := strings.LastIndex(path, "/")
	if i < 0 {
		return u.Scheme + "://" + u.Host
	}
	key := path[i+1:]
	shown := "***"
	if len(key) >= 12 {
		shown = key[:3] + "..." + key[len(key)-3:]
	}
	return u.Scheme + "://" + u.Host + path[:i+1] + "{" + shown + "}"
}

// RedactURLsIn does what RedactStreamURL does to every address in a piece of text, such as ffmpeg's output.
func RedactURLsIn(text string) string {
	fields := strings.Fields(text)
	for i, f := range fields {
		if strings.Contains(f, "://") {
			trimmed := strings.TrimRight(strings.TrimLeft(f, "'\"("), "'\":,.)")
			fields[i] = strings.Replace(f, trimmed, RedactStreamURL(trimmed), 1)
		}
	}
	return strings.Join(fields, " ")
}

// queryer is what a pool and a transaction have in common.
type queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// CreateEgress queues a job. The caller has checked that a worker can take it.
func (s *Store) CreateEgress(ctx context.Context, e *Egress, streamURLs []string) (*Egress, error) {
	return s.insertEgress(ctx, s.DB, e, streamURLs)
}

func (s *Store) insertEgress(ctx context.Context, db queryer, e *Egress, streamURLs []string) (*Egress, error) {
	if e.ID == "" {
		e.ID = participant.NewID("EG_")
	}
	request, err := json.Marshal(e.Request)
	if err != nil {
		return nil, err
	}
	redacted := make([]string, 0, len(streamURLs))
	for _, u := range streamURLs {
		redacted = append(redacted, RedactStreamURL(u))
	}
	var sealed []byte
	if len(streamURLs) > 0 {
		plain, err := json.Marshal(streamURLs)
		if err != nil {
			return nil, err
		}
		if sealed, err = s.Sealer.Seal(string(plain)); err != nil {
			return nil, err
		}
	}
	outputURL := ""
	if e.Format != "" {
		outputURL = "/api/v1/recordings/" + e.ID + "/file"
	}
	return scanEgress(db.QueryRow(ctx, `INSERT INTO egresses (id, project_id, room_name, room_sid, type, status, request, format, output_url, stream_urls, stream_urls_sealed, retry_of)
		VALUES ($1, $2, $3, $4, $5, 'starting', $6, $7, $8, $9, $10, $11) RETURNING `+egressColumns,
		e.ID, e.ProjectID, e.RoomName, e.RoomSID, e.Type, request, e.Format, outputURL, redacted, sealed, e.RetryOf))
}

// projectLive is the condition, on a row of egresses or ingresses, that its project may still run things. A project with no
// row of its own is the one static API keys use.
func projectLive(table string) string {
	return `NOT EXISTS (SELECT 1 FROM projects p WHERE p.id::text = ` + table + `.project_id AND (p.deleted_at IS NOT NULL OR p.status <> 'active'))`
}

// EgressStreamURLs returns a job's real stream URLs, with their keys.
func (s *Store) EgressStreamURLs(e *Egress) ([]string, error) {
	if len(e.sealedURLs) == 0 {
		return nil, nil
	}
	plain, err := s.Sealer.Open(e.sealedURLs)
	if err != nil {
		return nil, err
	}
	var out []string
	return out, json.Unmarshal([]byte(plain), &out)
}

// EgressByID loads a job in a project.
func (s *Store) EgressByID(ctx context.Context, projectID, id string) (*Egress, error) {
	return scanEgress(s.DB.QueryRow(ctx, `SELECT `+egressColumns+` FROM egresses WHERE id = $1 AND project_id = $2`, id, projectID))
}

// EgressForWorker loads a job by ID alone. Workers serve every project.
func (s *Store) EgressForWorker(ctx context.Context, id string) (*Egress, error) {
	return scanEgress(s.DB.QueryRow(ctx, `SELECT `+egressColumns+` FROM egresses WHERE id = $1`, id))
}

// EgressCursor is where a page of jobs ended.
func egressCursor(e *Egress) string {
	return e.CreatedAt.UTC().Format(time.RFC3339Nano) + "," + e.ID
}

// ListEgress pages through a project's jobs, newest first. With onlyFiles it returns those with a file output.
func (s *Store) ListEgress(ctx context.Context, projectID string, onlyFiles bool, cursor string, limit int) ([]*Egress, string, error) {
	var before *time.Time
	var beforeID string
	if at, id, ok := strings.Cut(cursor, ","); ok {
		if t, err := time.Parse(time.RFC3339Nano, at); err == nil {
			before, beforeID = &t, id
		}
	}
	query := `SELECT ` + egressColumns + ` FROM egresses WHERE project_id = $1
		AND ($2::timestamptz IS NULL OR (created_at, id) < ($2, $3))`
	if onlyFiles {
		query += ` AND format <> ''`
	}
	rows, err := s.DB.Query(ctx, query+` ORDER BY created_at DESC, id DESC LIMIT $4`, projectID, before, beforeID, limit)
	if err != nil {
		return nil, "", err
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Egress, error) { return scanEgress(r) })
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(items) == limit {
		next = egressCursor(items[len(items)-1])
	}
	return items, next, nil
}

// WorkerSeenWithin is how recently a worker must have reported to count as alive. Workers report every 5s.
const WorkerSeenWithin = 15 * time.Second

// Worker kinds, in the stats they report to the servers table.
const (
	WorkerKindEgress  = "egress_worker"
	WorkerKindIngress = "ingress_worker"
)

// EgressCapacityAvailable reports whether a live, healthy worker has a free slot. Jobs are counted from the table, so
// jobs still in the queue use up slots, and a burst of requests cannot pile up behind a busy worker.
func (s *Store) EgressCapacityAvailable(ctx context.Context) (bool, error) {
	var capacity, running int
	err := s.DB.QueryRow(ctx, capacityQuery, WorkerKindEgress, WorkerSeenWithin.Seconds()).Scan(&capacity, &running)
	return running < capacity, err
}

const capacityQuery = `SELECT
	COALESCE((SELECT sum(COALESCE((stats ->> 'max_jobs')::int, 0)) FROM servers
		WHERE stats ->> 'kind' = $1 AND stats ->> 'healthy' = 'true' AND NOT draining AND last_seen_at > now() - make_interval(secs => $2)), 0),
	(SELECT count(*) FROM egresses WHERE status IN ('starting', 'active', 'ending'))`

// egressAdmission keeps requests arriving together from all seeing the same free slot.
const egressAdmission = 7302118

// AdmitEgress queues a job if a worker has room for it and its project may have another. Everything happens under one lock, so
// a burst of requests cannot get past the limits between checking them and saving the job.
func (s *Store) AdmitEgress(ctx context.Context, e *Egress, streamURLs []string, perProject int) (*Egress, error) {
	var created *Egress
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, egressAdmission); err != nil {
			return err
		}
		var capacity, running, mine int
		err := tx.QueryRow(ctx, capacityQuery+`,
			(SELECT count(*) FROM egresses WHERE project_id = $3 AND status IN ('starting', 'active', 'ending'))`,
			WorkerKindEgress, WorkerSeenWithin.Seconds(), e.ProjectID).Scan(&capacity, &running, &mine)
		if err != nil {
			return err
		}
		if running >= capacity {
			return ErrNoCapacity
		}
		if perProject > 0 && mine >= perProject {
			return ErrProjectAtLimit
		}
		created, err = s.insertEgress(ctx, tx, e, streamURLs)
		return err
	})
	return created, err
}

// RequestEgressStop asks for a job to stop. A job no worker has claimed ends here, as nothing else would end
// it. A worker notices the request within about a second.
func (s *Store) RequestEgressStop(ctx context.Context, projectID, id string) (*Egress, error) {
	e, err := scanEgress(s.DB.QueryRow(ctx, `UPDATE egresses SET status = 'aborted', ended_at = now(), updated_at = now(), error = 'Stopped before it started.'
		WHERE id = $1 AND project_id = $2 AND status = 'starting' AND worker_id IS NULL RETURNING `+egressColumns, id, projectID))
	if err == nil {
		return e, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	e, err = scanEgress(s.DB.QueryRow(ctx, `UPDATE egresses SET stop_requested_at = COALESCE(stop_requested_at, now()), updated_at = now()
		WHERE id = $1 AND project_id = $2 AND status IN ('starting', 'active', 'ending') RETURNING `+egressColumns, id, projectID))
	if err == nil {
		return e, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if _, err := s.EgressByID(ctx, projectID, id); err != nil {
		return nil, err
	}
	return nil, ErrNotRunning
}

// DeleteEgress removes a finished job's record and returns it, so the caller can delete its file.
func (s *Store) DeleteEgress(ctx context.Context, projectID, id string) (*Egress, error) {
	e, err := scanEgress(s.DB.QueryRow(ctx, `DELETE FROM egresses WHERE id = $1 AND project_id = $2
		AND status NOT IN ('starting', 'active', 'ending') RETURNING `+egressColumns, id, projectID))
	if err == nil {
		return e, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if _, err := s.EgressByID(ctx, projectID, id); err != nil {
		return nil, err
	}
	return nil, ErrStillRunning
}

// ClaimEgress takes the oldest queued job, or returns nil. Workers competing for jobs never wait on each other.
func (s *Store) ClaimEgress(ctx context.Context, workerID string, lease time.Duration) (*Egress, error) {
	e, err := scanEgress(s.DB.QueryRow(ctx, `UPDATE egresses SET worker_id = $1, lease_expires_at = now() + make_interval(secs => $2), updated_at = now()
		WHERE id = (SELECT id FROM egresses WHERE status = 'starting' AND worker_id IS NULL AND `+projectLive("egresses")+` ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING `+egressColumns, workerID, lease.Seconds()))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return e, err
}

// RenewEgress extends a worker's lease on a job and reports whether someone has asked it to stop.
func (s *Store) RenewEgress(ctx context.Context, id, workerID string, lease time.Duration) (stop bool, err error) {
	err = s.DB.QueryRow(ctx, `UPDATE egresses SET lease_expires_at = now() + make_interval(secs => $3)
		WHERE id = $1 AND worker_id = $2 AND status IN ('starting', 'active', 'ending') RETURNING stop_requested_at IS NOT NULL`,
		id, workerID, lease.Seconds()).Scan(&stop)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrJobLost
	}
	return stop, err
}

// EgressResult is what a worker reports along with a status change. Zero values leave a field alone.
type EgressResult struct {
	Error      string
	OutputPath string
	Storage    string
	FileSize   int64
	DurationMs int64
	CPUSeconds float64
}

// TransitionEgress moves a job to a new status. The worker must still own the job, and the move must be legal:
// anything else returns ErrJobLost, and changes nothing.
func (s *Store) TransitionEgress(ctx context.Context, id, workerID, to string, r EgressResult) (*Egress, error) {
	from, ok := egressTransitions[to]
	if !ok {
		return nil, fmt.Errorf("no transitions lead to status %q", to)
	}
	e, err := scanEgress(s.DB.QueryRow(ctx, `UPDATE egresses SET status = $3, updated_at = now(),
			started_at = CASE WHEN $3 = 'active' THEN COALESCE(started_at, now()) ELSE started_at END,
			ended_at = CASE WHEN $3 IN ('complete', 'failed', 'aborted') THEN now() ELSE ended_at END,
			lease_expires_at = CASE WHEN $3 IN ('complete', 'failed', 'aborted') THEN NULL ELSE lease_expires_at END,
			error = CASE WHEN $5 <> '' THEN $5 ELSE error END,
			output_path = CASE WHEN $6 <> '' THEN $6 ELSE output_path END,
			storage = CASE WHEN $7 <> '' THEN $7 ELSE storage END,
			file_size = CASE WHEN $8 > 0 THEN $8 ELSE file_size END,
			duration_ms = CASE WHEN $9 > 0 THEN $9 ELSE duration_ms END,
			cpu_seconds = CASE WHEN $10 > 0 THEN $10 ELSE cpu_seconds END
		WHERE id = $1 AND worker_id = $2 AND status = ANY($4) RETURNING `+egressColumns,
		id, workerID, to, from, r.Error, r.OutputPath, r.Storage, r.FileSize, r.DurationMs, r.CPUSeconds))
	if errors.Is(err, ErrNotFound) {
		return nil, ErrJobLost
	}
	return e, err
}

// AttachSalvagedFile records a file recovered from a job that failed when its worker died. The job stays failed.
func (s *Store) AttachSalvagedFile(ctx context.Context, id string, r EgressResult) (*Egress, error) {
	return scanEgress(s.DB.QueryRow(ctx, `UPDATE egresses SET output_path = $2, storage = $3, file_size = $4, duration_ms = $5, error = $6, updated_at = now()
		WHERE id = $1 AND status = 'failed' AND output_path = '' RETURNING `+egressColumns, id, r.OutputPath, r.Storage, r.FileSize, r.DurationMs, r.Error))
}

// UnclaimedJobTimeout is how long a job waits for a worker. The API refuses jobs when no worker has room, so
// reaching this means the workers it counted on went away.
const UnclaimedJobTimeout = time.Minute

// ReapGrace is how long past the end of a lease a job is left alone. A machine that stops for a while stops everyone: when it
// starts again, the worker should get to renew before anyone decides from the clock that it has died. Failing a live
// recording cannot be undone, and waiting a little longer costs nothing.
const ReapGrace = 10 * time.Second

// ReapEgress fails jobs whose worker has stopped renewing, and queued jobs nobody took. It is safe to run
// anywhere, any number of times: each job fails once, and the caller gets it back to report.
func (s *Store) ReapEgress(ctx context.Context) ([]*Egress, error) {
	rows, err := s.DB.Query(ctx, `UPDATE egresses SET status = 'failed', ended_at = now(), updated_at = now(), lease_expires_at = NULL,
			error = CASE WHEN worker_id IS NULL THEN 'No worker took this job. Check that a streamkit-egress worker is running.'
				ELSE 'The worker running this job stopped responding.' END
		WHERE status IN ('starting', 'active', 'ending') AND (
			(worker_id IS NOT NULL AND lease_expires_at < now() - make_interval(secs => $2)) OR
			(worker_id IS NULL AND created_at < now() - make_interval(secs => $1)))
		RETURNING `+egressColumns, UnclaimedJobTimeout.Seconds(), ReapGrace.Seconds())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Egress, error) { return scanEgress(r) })
}

// stopProjectMedia is what a project being deleted or suspended does to its media. Queued jobs end here, and whatever is running hears about it the way it hears any stop
// request, on whichever worker or node it is. A running pull asks to start again, for a suspended project coming back.
func stopProjectMedia(ctx context.Context, tx pgx.Tx, projectID, why string) error {
	if _, err := tx.Exec(ctx, `UPDATE egresses SET status = 'aborted', ended_at = now(), updated_at = now(), error = $2
		WHERE project_id = $1 AND status = 'starting' AND worker_id IS NULL`, projectID, why); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE egresses SET stop_requested_at = COALESCE(stop_requested_at, now()), updated_at = now()
		WHERE project_id = $1 AND status IN ('starting', 'active', 'ending')`, projectID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE ingresses SET updated_at = now(),
			pull_requested = pull_requested OR (input_type = 'url' AND enabled AND worker_id IS NOT NULL),
			stop_requested_at = CASE WHEN worker_id IS NOT NULL THEN now() ELSE stop_requested_at END
		WHERE project_id = $1`, projectID)
	return err
}

// ReapMedia fails what dead media workers left behind, and reports it. Media workers run it too, so it touches only what
// their database role may: SIP calls are ReapCalls, which the server runs.
func (s *Store) ReapMedia(ctx context.Context) error {
	jobs, err := s.ReapEgress(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range jobs {
		errs = append(errs, s.RecordEgressEvent(ctx, e, "egress_ended", "error", "egress_failed", e.Error, nil))
	}
	streams, err := s.ReapIngress(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, in := range streams {
		errs = append(errs, s.RecordIngressEvent(ctx, in, "ingress_ended", "error", "ingress_failed", in.Error, nil))
	}
	return errors.Join(errs...)
}

// ReapCalls fails SIP calls whose worker has gone, and reports it.
func (s *Store) ReapCalls(ctx context.Context) error {
	calls, err := s.ReapSIP(ctx)
	errs := []error{err}
	for _, c := range calls {
		errs = append(errs, s.RecordSIPEvent(ctx, c, "error", "sip_call_failed", c.Error))
	}
	return errors.Join(errs...)
}

// reapFrozenAfter is how late a tick has to be to say this process did not run for a while.
const reapFrozenAfter = 3 * time.Second

// ReapLoop runs ReapMedia until ctx ends, and after each pass calls after, if given. A tick that comes late means this process
// has not been running, and quite likely nothing on the machine has. It skips that pass, so workers starting up again at the
// same moment get to renew their leases before anyone judges them by the clock.
func (s *Store) ReapLoop(ctx context.Context, interval time.Duration, log *slog.Logger, after func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		late := now.Sub(last) > interval+reapFrozenAfter
		last = now
		if late {
			log.Warn("this process did not run for a while, so it is leaving media leases alone for now")
			continue
		}
		if err := s.ReapMedia(ctx); err != nil && ctx.Err() == nil {
			log.Warn("could not check media leases", "error", err)
		}
		if after != nil {
			after()
		}
	}
}

// RecordingsToPurge finds finished jobs nobody can ask for any more: those of deleted projects, and, if retentionDays is set,
// those that ended longer ago than that. It returns a batch at a time.
func (s *Store) RecordingsToPurge(ctx context.Context, retentionDays int) ([]*Egress, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+egressColumns+` FROM egresses WHERE status NOT IN ('starting', 'active', 'ending') AND (
			EXISTS (SELECT 1 FROM projects p WHERE p.id::text = egresses.project_id AND p.deleted_at IS NOT NULL)
			OR ($1 > 0 AND ended_at < now() - make_interval(days => $1)))
		ORDER BY ended_at LIMIT 200`, retentionDays)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Egress, error) { return scanEgress(r) })
}

// PurgeDeletedIngresses removes the ingresses of deleted projects, along with what is left of their stream keys.
func (s *Store) PurgeDeletedIngresses(ctx context.Context) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM ingresses WHERE worker_id IS NULL AND EXISTS (SELECT 1 FROM projects p WHERE p.id::text = ingresses.project_id AND p.deleted_at IS NOT NULL)`)
	return err
}

// EgressInfo is a job as webhook receivers expect it.
func EgressInfo(e *Egress) *pb.EgressInfo {
	info := &pb.EgressInfo{
		EgressId: e.ID, RoomId: e.RoomSID, RoomName: e.RoomName, Error: e.Error, UpdatedAt: time.Now().UnixNano(),
		Status: pb.EgressStatus(pb.EgressStatus_value["EGRESS_"+strings.ToUpper(e.Status)]),
	}
	if e.StartedAt != nil {
		info.StartedAt = e.StartedAt.UnixNano()
	}
	if e.EndedAt != nil {
		info.EndedAt = e.EndedAt.UnixNano()
	}
	if e.Format != "" && e.FileSize > 0 {
		info.FileResults = []*pb.FileInfo{{
			Filename: e.ID + "." + e.Format, Size: e.FileSize, Duration: e.DurationMs * int64(time.Millisecond), Location: e.OutputURL,
		}}
	}
	for _, u := range e.StreamURLs {
		info.StreamResults = append(info.StreamResults, &pb.StreamInfo{Url: u})
	}
	return info
}

// MediaEvent is one row for the logs page, and optionally a webhook.
type MediaEvent struct {
	ProjectID string
	RoomName  string
	RoomSID   string
	Identity  string
	Level     string
	Type      string
	Message   string
	Details   map[string]any
	// Webhook is the event to send, or empty. Payload fills in its subject.
	Webhook string
	Payload func(*pb.WebhookEvent)
}

// RecordMediaEvent writes a media event to the log and queues its webhooks. Failures are returned for logging:
// the thing the event describes has already happened. Nothing else is needed for delivery, because the server's
// dispatcher polls the queue, so workers can call this without a connection to the server.
func (s *Store) RecordMediaEvent(ctx context.Context, ev MediaEvent) error {
	details, err := json.Marshal(ev.Details)
	if err != nil || ev.Details == nil {
		details = []byte(`{}`)
	}
	level := ev.Level
	if level == "" {
		level = "info"
	}
	project := ev.ProjectID
	err = s.InsertEvents(ctx, []Event{{
		ProjectID: &project, Category: CategoryRecording, Level: level, Type: ev.Type, Message: ev.Message,
		RoomName: ev.RoomName, RoomSID: ev.RoomSID, ParticipantIdentity: ev.Identity, Details: details, CreatedAt: time.Now(),
	}})
	if ev.Webhook == "" {
		return err
	}
	return errors.Join(err, s.queueMediaWebhook(ctx, ev))
}

func (s *Store) queueMediaWebhook(ctx context.Context, ev MediaEvent) error {
	hooks, err := s.WebhooksForEvent(ctx, ev.ProjectID, ev.Webhook)
	if err != nil || len(hooks) == 0 {
		return err
	}
	event := &pb.WebhookEvent{Id: participant.NewID("EV_"), Event: ev.Webhook, CreatedAt: time.Now().Unix()}
	if ev.Payload != nil {
		ev.Payload(event)
	}
	body, err := protojson.Marshal(event)
	if err != nil {
		return err
	}
	var errs []error
	for _, hook := range hooks {
		if _, err := s.EnqueueDelivery(ctx, hook.ID, ev.Webhook, body); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// RecordEgressEvent logs a job's event, and sends the webhook for its status: egress_started when it was
// accepted, egress_updated while it runs, egress_ended when it has ended.
func (s *Store) RecordEgressEvent(ctx context.Context, e *Egress, webhook, level, eventType, message string, details map[string]any) error {
	if details == nil {
		details = map[string]any{}
	}
	details["egress_id"] = e.ID
	return s.RecordMediaEvent(ctx, MediaEvent{
		ProjectID: e.ProjectID, RoomName: e.RoomName, RoomSID: e.RoomSID, Level: level, Type: eventType, Message: message, Details: details,
		Webhook: webhook,
		Payload: func(ev *pb.WebhookEvent) { ev.EgressInfo = EgressInfo(e) },
	})
}

// SigningKeyFor finds the key to sign a media session's access token with. Static keys sign for the project that has no row
// in the database, and every other project uses MediaSigningKey.
func (s *Store) SigningKeyFor(ctx context.Context, projectID string, static auth.StaticKeys) (auth.Key, error) {
	if projectID == static.ProjectID {
		for key, secret := range static.Keys {
			return auth.Key{APIKey: key, Secret: secret, ProjectID: projectID}, nil
		}
	}
	return s.MediaSigningKey(ctx, projectID)
}

// MediaSigningKey returns a key to sign a media worker's access tokens with: the project's newest working key,
// or a new one if it has none, since nothing else requires a project to have a key before it records. A project that has
// been deleted or suspended gets neither: its keys were revoked for a reason.
func (s *Store) MediaSigningKey(ctx context.Context, projectID string) (auth.Key, error) {
	var live bool
	err := s.DB.QueryRow(ctx, `SELECT deleted_at IS NULL AND status = 'active' FROM projects WHERE id = $1`, projectID).Scan(&live)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !live {
		return auth.Key{}, ErrProjectEnded
	}
	if err != nil {
		return auth.Key{}, err
	}
	var sealed []byte
	var apiKey string
	err = s.DB.QueryRow(ctx, `SELECT secret_ciphertext, api_key FROM api_keys
		WHERE project_id = $1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now() + interval '1 hour')
		ORDER BY created_at DESC LIMIT 1`, projectID).Scan(&sealed, &apiKey)
	if errors.Is(err, pgx.ErrNoRows) {
		created, err := s.CreateAPIKey(ctx, projectID, "Media services (managed)", nil)
		if err != nil {
			return auth.Key{}, fmt.Errorf("the project has no API key, and one could not be created: %w", err)
		}
		return auth.Key{APIKey: created.APIKey, Secret: created.Secret, ProjectID: projectID}, nil
	}
	if err != nil {
		return auth.Key{}, err
	}
	secret, err := s.Sealer.Open(sealed)
	if err != nil {
		return auth.Key{}, err
	}
	return auth.Key{APIKey: apiKey, Secret: secret, ProjectID: projectID}, nil
}

// Ingress statuses.
const (
	IngressInactive   = "inactive"
	IngressBuffering  = "buffering"
	IngressPublishing = "publishing"
	IngressError      = "error"
)

// Ingress input types.
const (
	InputRTMP = "rtmp"
	InputWHIP = "whip"
	InputURL  = "url"
)

// IngressStats describes the stream coming in.
type IngressStats struct {
	BitrateIn  int64      `json:"bitrate_in"`
	Width      int        `json:"width"`
	Height     int        `json:"height"`
	FPS        float64    `json:"fps"`
	AudioCodec string     `json:"audio_codec"`
	VideoCodec string     `json:"video_codec"`
	StartedAt  *time.Time `json:"started_at"`
}

// Ingress is a way to bring outside media into a room.
type Ingress struct {
	ID                  string       `json:"id"`
	ProjectID           string       `json:"project_id"`
	Name                string       `json:"name"`
	InputType           string       `json:"input_type"`
	RoomName            string       `json:"room_name"`
	ParticipantIdentity string       `json:"participant_identity"`
	ParticipantName     string       `json:"participant_name"`
	SourceURL           string       `json:"source_url"`
	Enabled             bool         `json:"enabled"`
	PublishURL          string       `json:"publish_url"`
	StreamKey           string       `json:"stream_key,omitempty"`
	Status              string       `json:"status"`
	Error               string       `json:"error"`
	Stats               IngressStats `json:"stats"`
	CreatedAt           time.Time    `json:"created_at"`
	UpdatedAt           time.Time    `json:"updated_at"`

	sealedSource []byte
}

const ingressColumns = `id, project_id, name, input_type, room_name, participant_identity, participant_name, source_url, enabled, status, error, stats, created_at, updated_at, source_url_sealed`

func scanIngress(row pgx.Row) (*Ingress, error) {
	in := &Ingress{}
	var stats []byte
	err := row.Scan(&in.ID, &in.ProjectID, &in.Name, &in.InputType, &in.RoomName, &in.ParticipantIdentity, &in.ParticipantName, &in.SourceURL,
		&in.Enabled, &in.Status, &in.Error, &stats, &in.CreatedAt, &in.UpdatedAt, &in.sealedSource)
	if err != nil {
		return nil, notFound(err)
	}
	if err := json.Unmarshal(stats, &in.Stats); err != nil {
		return nil, err
	}
	return in, nil
}

// newStreamKey makes a key, its lookup hash, and the sealed copy. Keys are bearer secrets with 192 random bits,
// so a fast hash is the right one for finding them.
func (s *Store) newStreamKey() (key string, hash, sealed []byte, err error) {
	key = "sk_" + RandomToken(24)
	if sealed, err = s.Sealer.Seal(key); err != nil {
		return "", nil, nil, err
	}
	return key, HashToken(key), sealed, nil
}

// IngressSourceURL returns a URL input's real address, with its credentials. API responses only ever have a redacted copy.
func (s *Store) IngressSourceURL(in *Ingress) (string, error) {
	if len(in.sealedSource) == 0 {
		return in.SourceURL, nil
	}
	return s.Sealer.Open(in.sealedSource)
}

// SealOldSourceURLs seals the addresses of URL inputs saved before they were sealed.
func (s *Store) SealOldSourceURLs(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `SELECT `+ingressColumns+` FROM ingresses WHERE input_type = 'url' AND source_url_sealed IS NULL AND source_url <> ''`)
	if err != nil {
		return err
	}
	old, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Ingress, error) { return scanIngress(r) })
	if err != nil {
		return err
	}
	var errs []error
	for _, in := range old {
		sealed, err := s.Sealer.Seal(in.SourceURL)
		if err == nil {
			_, err = s.DB.Exec(ctx, `UPDATE ingresses SET source_url_sealed = $2, source_url = $3 WHERE id = $1`, in.ID, sealed, RedactStreamURL(in.SourceURL))
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// CreateIngress saves an ingress. A push ingress gets a stream key, set on the result: this is one of only two
// times it is ever readable. A URL ingress asks a worker to start pulling, and only a redacted copy of its address can be
// read back.
func (s *Store) CreateIngress(ctx context.Context, in *Ingress) (*Ingress, error) {
	id := participant.NewID("IN_")
	var key string
	var hash, sealed, sealedSource []byte
	var err error
	if in.InputType != InputURL {
		if key, hash, sealed, err = s.newStreamKey(); err != nil {
			return nil, err
		}
	}
	shownSource := in.SourceURL
	if in.SourceURL != "" {
		if sealedSource, err = s.Sealer.Seal(in.SourceURL); err != nil {
			return nil, err
		}
		shownSource = RedactStreamURL(in.SourceURL)
	}
	out, err := scanIngress(s.DB.QueryRow(ctx, `INSERT INTO ingresses (id, project_id, name, input_type, room_name, participant_identity, participant_name,
			stream_key_hash, stream_key_ciphertext, source_url, source_url_sealed, enabled, pull_requested)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING `+ingressColumns,
		id, in.ProjectID, in.Name, in.InputType, in.RoomName, in.ParticipantIdentity, in.ParticipantName,
		hash, sealed, shownSource, sealedSource, in.Enabled, in.InputType == InputURL && in.Enabled))
	if err != nil {
		return nil, err
	}
	out.StreamKey = key
	return out, nil
}

// IngressByID loads an ingress in a project.
func (s *Store) IngressByID(ctx context.Context, projectID, id string) (*Ingress, error) {
	return scanIngress(s.DB.QueryRow(ctx, `SELECT `+ingressColumns+` FROM ingresses WHERE id = $1 AND project_id = $2`, id, projectID))
}

// ListIngress returns a project's ingresses, newest first.
func (s *Store) ListIngress(ctx context.Context, projectID string) ([]*Ingress, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+ingressColumns+` FROM ingresses WHERE project_id = $1 ORDER BY created_at DESC, id DESC`, projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Ingress, error) { return scanIngress(r) })
}

// IngressPatch changes an ingress. Nil fields stay as they are.
type IngressPatch struct {
	Name                *string
	RoomName            *string
	ParticipantIdentity *string
	ParticipantName     *string
	Enabled             *bool
}

// UpdateIngress applies a patch. A change that a running stream cannot take on, such as a new room, ends that
// stream, and it applies when the publisher reconnects. Setting enabled on a URL ingress that is not running
// starts it again.
func (s *Store) UpdateIngress(ctx context.Context, projectID, id string, p IngressPatch) (*Ingress, error) {
	return scanIngress(s.DB.QueryRow(ctx, `UPDATE ingresses SET
			name = COALESCE($3, name),
			room_name = COALESCE($4, room_name),
			participant_identity = COALESCE($5, participant_identity),
			participant_name = COALESCE($6, participant_name),
			enabled = COALESCE($7, enabled),
			stop_requested_at = CASE WHEN worker_id IS NOT NULL AND (
				COALESCE($4, room_name) <> room_name OR COALESCE($5, participant_identity) <> participant_identity OR NOT COALESCE($7, enabled)
			) THEN now() ELSE stop_requested_at END,
			pull_requested = CASE
				WHEN NOT COALESCE($7, enabled) THEN false
				WHEN input_type = 'url' AND $7 AND worker_id IS NULL THEN true
				-- A running pull that has to stop for a new room has no publisher to come back, so it asks to start again.
				WHEN input_type = 'url' AND worker_id IS NOT NULL AND (COALESCE($4, room_name) <> room_name OR COALESCE($5, participant_identity) <> participant_identity) THEN true
				ELSE pull_requested END,
			updated_at = now()
		WHERE id = $1 AND project_id = $2 RETURNING `+ingressColumns,
		id, projectID, p.Name, p.RoomName, p.ParticipantIdentity, p.ParticipantName, p.Enabled))
}

// DeleteIngress removes an ingress. A worker running it notices the row is gone within about a second.
func (s *Store) DeleteIngress(ctx context.Context, projectID, id string) error {
	tag, err := s.DB.Exec(ctx, `DELETE FROM ingresses WHERE id = $1 AND project_id = $2`, id, projectID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RequestIngressStop asks for the running stream to stop. The ingress stays usable.
func (s *Store) RequestIngressStop(ctx context.Context, projectID, id string) (*Ingress, error) {
	return scanIngress(s.DB.QueryRow(ctx, `UPDATE ingresses SET pull_requested = false, updated_at = now(),
			stop_requested_at = CASE WHEN worker_id IS NOT NULL THEN now() ELSE NULL END
		WHERE id = $1 AND project_id = $2 RETURNING `+ingressColumns, id, projectID))
}

// RotateIngressKey issues a new stream key, set on the result, and ends any stream using the old one. Whoever
// holds a key that needed rotating should not stay on the air.
func (s *Store) RotateIngressKey(ctx context.Context, projectID, id string) (*Ingress, error) {
	key, hash, sealed, err := s.newStreamKey()
	if err != nil {
		return nil, err
	}
	in, err := scanIngress(s.DB.QueryRow(ctx, `UPDATE ingresses SET stream_key_hash = $3, stream_key_ciphertext = $4, updated_at = now(),
			stop_requested_at = CASE WHEN worker_id IS NOT NULL THEN now() ELSE stop_requested_at END
		WHERE id = $1 AND project_id = $2 AND input_type <> 'url' RETURNING `+ingressColumns, id, projectID, hash, sealed))
	if err != nil {
		return nil, err
	}
	in.StreamKey = key
	return in, nil
}

// IngressLogs returns an ingress's events, newest first.
func (s *Store) IngressLogs(ctx context.Context, projectID, id string, limit int) ([]Event, error) {
	rows, err := s.DB.Query(ctx, `SELECT id, project_id, category, level, type, message, room_name, room_sid, participant_identity, track_sid, details, created_at
		FROM events WHERE details ->> 'ingress_id' = $1 AND details ? 'ingress_id' AND project_id = $2 ORDER BY id DESC LIMIT $3`, id, projectID, limit)
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

// ClaimIngressByKey takes an ingress for a publisher that has presented a stream key. It tells apart the reasons
// to refuse: ErrNotFound for an unknown key, the wrong input type or a project that has been deleted or suspended,
// ErrIngressDisabled, and ErrIngressBusy for a second publisher on one key. Those last two return the ingress, so the
// refusal can be logged against it.
func (s *Store) ClaimIngressByKey(ctx context.Context, key, inputType, workerID string, lease time.Duration) (*Ingress, error) {
	hash := HashToken(key)
	in, err := scanIngress(s.DB.QueryRow(ctx, `UPDATE ingresses SET worker_id = $3, lease_expires_at = now() + make_interval(secs => $4), status = 'buffering', error = '',
			stop_requested_at = NULL, stats = '{}', updated_at = now()
		WHERE stream_key_hash = $1 AND input_type = $2 AND enabled AND (worker_id IS NULL OR lease_expires_at < now() - make_interval(secs => $5)) AND `+projectLive("ingresses")+`
			AND ($6 = 0 OR (SELECT count(*) FROM ingresses o WHERE o.project_id = ingresses.project_id AND o.worker_id IS NOT NULL AND o.id <> ingresses.id) < $6)
		RETURNING `+ingressColumns, hash, inputType, workerID, lease.Seconds(), ReapGrace.Seconds(), s.MaxIngressPerProject))
	if err == nil {
		return in, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	in, err = scanIngress(s.DB.QueryRow(ctx, `SELECT `+ingressColumns+` FROM ingresses WHERE stream_key_hash = $1 AND input_type = $2 AND `+projectLive("ingresses"), hash, inputType))
	if err != nil {
		return nil, err
	}
	if !in.Enabled {
		return in, ErrIngressDisabled
	}
	if s.MaxIngressPerProject > 0 {
		var running int
		if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM ingresses WHERE project_id = $1 AND worker_id IS NOT NULL AND id <> $2`, in.ProjectID, in.ID).Scan(&running); err != nil {
			return nil, err
		}
		if running >= s.MaxIngressPerProject {
			return in, ErrProjectAtLimit
		}
	}
	return in, ErrIngressBusy
}

// ClaimIngressPull takes the oldest URL ingress waiting to start, or returns nil.
func (s *Store) ClaimIngressPull(ctx context.Context, workerID string, lease time.Duration) (*Ingress, error) {
	in, err := scanIngress(s.DB.QueryRow(ctx, `UPDATE ingresses SET worker_id = $1, lease_expires_at = now() + make_interval(secs => $2), status = 'buffering', error = '',
			pull_requested = false, stop_requested_at = NULL, stats = '{}', updated_at = now()
		WHERE id = (SELECT id FROM ingresses WHERE pull_requested AND enabled AND worker_id IS NULL AND `+projectLive("ingresses")+`
				AND ($3 = 0 OR (SELECT count(*) FROM ingresses o WHERE o.project_id = ingresses.project_id AND o.worker_id IS NOT NULL) < $3)
			ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING `+ingressColumns, workerID, lease.Seconds(), s.MaxIngressPerProject))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return in, err
}

// RenewIngress extends a worker's lease on a running stream and reports whether it should stop.
func (s *Store) RenewIngress(ctx context.Context, id, workerID string, lease time.Duration) (stop bool, err error) {
	err = s.DB.QueryRow(ctx, `UPDATE ingresses SET lease_expires_at = now() + make_interval(secs => $3)
		WHERE id = $1 AND worker_id = $2 RETURNING stop_requested_at IS NOT NULL`, id, workerID, lease.Seconds()).Scan(&stop)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrJobLost
	}
	return stop, err
}

// UpdateIngressRunning records a running stream's status and health.
func (s *Store) UpdateIngressRunning(ctx context.Context, id, workerID, status string, stats IngressStats) (*Ingress, error) {
	data, err := json.Marshal(stats)
	if err != nil {
		return nil, err
	}
	in, err := scanIngress(s.DB.QueryRow(ctx, `UPDATE ingresses SET status = $3, stats = $4, updated_at = now()
		WHERE id = $1 AND worker_id = $2 RETURNING `+ingressColumns, id, workerID, status, data))
	if errors.Is(err, ErrNotFound) {
		return nil, ErrJobLost
	}
	return in, err
}

// ReleaseIngress ends a stream: the ingress goes back to waiting, or to error with a reason.
func (s *Store) ReleaseIngress(ctx context.Context, id, workerID, status, message string) (*Ingress, error) {
	in, err := scanIngress(s.DB.QueryRow(ctx, `UPDATE ingresses SET status = $3, error = $4, worker_id = NULL, lease_expires_at = NULL, stop_requested_at = NULL, updated_at = now()
		WHERE id = $1 AND worker_id = $2 RETURNING `+ingressColumns, id, workerID, status, message))
	if errors.Is(err, ErrNotFound) {
		return nil, ErrJobLost
	}
	return in, err
}

// ReleaseIngressForRestart ends a stream because its worker is going away. A URL input goes back in the queue for the next
// worker, unless someone has asked it to stop: nobody chose for it to end, and it has no publisher to come back by itself.
func (s *Store) ReleaseIngressForRestart(ctx context.Context, id, workerID, status, message string) (*Ingress, error) {
	in, err := scanIngress(s.DB.QueryRow(ctx, `UPDATE ingresses SET status = $3, error = $4, worker_id = NULL, lease_expires_at = NULL, stop_requested_at = NULL, updated_at = now(),
			pull_requested = (input_type = 'url' AND enabled AND stop_requested_at IS NULL)
		WHERE id = $1 AND worker_id = $2 RETURNING `+ingressColumns, id, workerID, status, message))
	if errors.Is(err, ErrNotFound) {
		return nil, ErrJobLost
	}
	return in, err
}

// ReapIngress releases streams whose worker has stopped renewing, so the key can publish again, and a URL input can be
// taken up by another worker.
func (s *Store) ReapIngress(ctx context.Context) ([]*Ingress, error) {
	rows, err := s.DB.Query(ctx, `UPDATE ingresses SET status = 'error', error = 'The worker running this stream stopped responding.',
			pull_requested = (input_type = 'url' AND enabled AND stop_requested_at IS NULL),
			worker_id = NULL, lease_expires_at = NULL, stop_requested_at = NULL, updated_at = now()
		WHERE worker_id IS NOT NULL AND lease_expires_at < now() - make_interval(secs => $1) RETURNING `+ingressColumns, ReapGrace.Seconds())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Ingress, error) { return scanIngress(r) })
}

// IngressInfo is an ingress as webhook receivers expect it.
func IngressInfo(in *Ingress) *pb.IngressInfo {
	enabled := in.Enabled
	status := pb.IngressState_ENDPOINT_INACTIVE
	switch in.Status {
	case IngressBuffering:
		status = pb.IngressState_ENDPOINT_BUFFERING
	case IngressPublishing:
		status = pb.IngressState_ENDPOINT_PUBLISHING
	case IngressError:
		status = pb.IngressState_ENDPOINT_ERROR
	}
	input := pb.IngressInput_RTMP_INPUT
	switch in.InputType {
	case InputWHIP:
		input = pb.IngressInput_WHIP_INPUT
	case InputURL:
		input = pb.IngressInput_URL_INPUT
	}
	return &pb.IngressInfo{
		IngressId: in.ID, Name: in.Name, InputType: input, RoomName: in.RoomName, ParticipantIdentity: in.ParticipantIdentity,
		ParticipantName: in.ParticipantName, Enabled: &enabled, Reusable: in.InputType != InputURL,
		State: &pb.IngressState{Status: status, Error: in.Error, UpdatedAt: time.Now().UnixNano()},
	}
}

// RecordIngressEvent logs an ingress's event, optionally with its webhook: ingress_started or ingress_ended.
func (s *Store) RecordIngressEvent(ctx context.Context, in *Ingress, webhook, level, eventType, message string, details map[string]any) error {
	if details == nil {
		details = map[string]any{}
	}
	details["ingress_id"] = in.ID
	return s.RecordMediaEvent(ctx, MediaEvent{
		ProjectID: in.ProjectID, RoomName: in.RoomName, Identity: in.ParticipantIdentity, Level: level, Type: eventType, Message: message, Details: details,
		Webhook: webhook,
		Payload: func(ev *pb.WebhookEvent) { ev.IngressInfo = IngressInfo(in) },
	})
}
