// Package agent is the server's job broker for AI agents. Workers hold a WebSocket open on /agent. When a room needs an
// agent, the broker offers a job to a worker, and an accepted job gets a token that joins the room as an ordinary
// participant. The agent's own work, voice and models included, happens in the worker, not here.
//
// It follows the reference broker's protocol. Two things differ on purpose:
//
//   - A dispatch that names an agent waits for a worker if none is registered, and starts when one registers. The
//     reference loses it. The implicit dispatch every room gets is still lost, as on the reference, so that starting a
//     worker does not put an agent into every room already running.
//   - A worker that reconnects and sends migrate_job takes its jobs back, so they can still be terminated. The reference
//     treats that message as a no-op.
//
// A broker sees only the rooms and workers on its own node. On a cluster, a worker must connect to each node.
package agent

import (
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/metrics"
	"elnova-streamkit.local/server/participant"
	"elnova-streamkit.local/server/room"
	"elnova-streamkit.local/server/roomapi"
)

const (
	// A worker has this long to register after connecting.
	defaultRegisterTimeout = 10 * time.Second
	// A worker has this long to answer a job offer.
	defaultAssignTimeout = 10 * time.Second
	// An agent removed by deleting its dispatch gets this long to leave by itself.
	agentLeaveTimeout = 3 * time.Second

	dispatchIDPrefix = "AD_"
	jobIDPrefix      = "AJ_"
	workerIDPrefix   = "AW_"

	maxDeploymentLength = 64
)

var (
	errWorkerNotAvailable = errors.New("worker not available")
	errWorkerClosed       = errors.New("worker closed")
	errDuplicateOffer     = errors.New("job is already offered to this worker")
	errOfferTimeout       = errors.New("worker did not answer the job offer in time")
)

var (
	workersGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "streamkit", Name: "agent_workers", Help: "Agent workers registered on this node.",
	})
	jobsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "streamkit", Name: "agent_jobs_total", Help: "Agent jobs, by outcome.",
	}, []string{"outcome"})
)

func init() { metrics.Registry.MustRegister(workersGauge, jobsCounter) }

// workerKey is what makes workers interchangeable. Rooms in one project never reach another project's workers.
type workerKey struct {
	project    string
	agentName  string
	namespace  string
	jobType    pb.JobType
	deployment string
}

// dispatch is one request for an agent in a room.
type dispatch struct {
	info *pb.AgentDispatch
	// implicit is the automatic dispatch a room gets when it names no agents.
	implicit bool
	// waiting is a room job with no capacity yet, which starts when a matching worker registers.
	waiting *pb.Job
	// deleted stops new launches, which is what makes it safe for finishDispatch to wait on launching.
	deleted   bool
	launching sync.WaitGroup
}

// jobRecord is an assigned job.
type jobRecord struct {
	job      *pb.Job
	worker   *Worker
	roomSID  string
	identity string
	ended    bool
	// left closes when the agent's session is gone.
	left     chan struct{}
	leftOnce sync.Once
}

// roomState is what the broker holds per room.
type roomState struct {
	project    string
	name       string
	sid        string
	dispatches map[string]*dispatch
	agentJobs  map[string]*jobRecord // by the agent's identity
	publishers map[string]bool       // sessions that have published, since a publisher job starts once
	started    bool
}

// Broker offers jobs to workers.
type Broker struct {
	rooms *room.Manager
	keys  auth.KeyProvider
	log   *slog.Logger

	registerTimeout time.Duration
	assignTimeout   time.Duration
	upgrader        websocket.Upgrader

	// mu guards everything below and job states. Nothing is sent to a worker while holding it.
	mu      sync.Mutex
	workers map[workerKey]map[string]*Worker
	byRoom  map[string]*roomState // by room SID
	jobs    map[string]*jobRecord
	closed  bool
}

// NewBroker creates a broker and starts it following room events.
func NewBroker(rooms *room.Manager, keys auth.KeyProvider, log *slog.Logger) *Broker {
	b := &Broker{
		rooms:           rooms,
		keys:            keys,
		log:             log.With("component", "agents"),
		registerTimeout: defaultRegisterTimeout,
		assignTimeout:   defaultAssignTimeout,
		upgrader: websocket.Upgrader{
			// Access tokens authenticate workers, not cookies, so the origin carries no meaning.
			CheckOrigin: func(*http.Request) bool { return true },
		},
		workers: map[workerKey]map[string]*Worker{},
		byRoom:  map[string]*roomState{},
		jobs:    map[string]*jobRecord{},
	}
	rooms.Subscribe(b.onRoomEvent)
	rooms.SetAgentLeft(b.onAgentLeft)
	return b
}

// Register adds the worker endpoint to mux.
func (b *Broker) Register(mux *http.ServeMux) { mux.HandleFunc("/agent", b.serveWorker) }

// Close disconnects every worker.
func (b *Broker) Close() {
	b.mu.Lock()
	b.closed = true
	var all []*Worker
	for _, set := range b.workers {
		for _, w := range set {
			all = append(all, w)
		}
	}
	b.mu.Unlock()
	for _, w := range all {
		w.close()
	}
}

func (b *Broker) addWorker(w *Worker) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		w.close()
		return
	}
	set := b.workers[w.key]
	if set == nil {
		set = map[string]*Worker{}
		b.workers[w.key] = set
	}
	set[w.id] = w
	b.mu.Unlock()
	workersGauge.Inc()
	b.startWaiting(w)
}

func (b *Broker) removeWorker(w *Worker) {
	b.mu.Lock()
	if set := b.workers[w.key]; set != nil {
		delete(set, w.id)
		if len(set) == 0 {
			delete(b.workers, w.key)
		}
	}
	for _, rec := range b.jobs {
		if rec.worker == w {
			rec.worker = nil
		}
	}
	b.mu.Unlock()
	workersGauge.Dec()
}

// selectWorker picks among available workers at random, favouring the less loaded. It returns nil when none can take
// more work.
func (b *Broker) selectWorker(key workerKey, attempted map[*Worker]bool) *Worker {
	b.mu.Lock()
	candidates := make([]*Worker, 0, len(b.workers[key]))
	for _, w := range b.workers[key] {
		if !attempted[w] {
			candidates = append(candidates, w)
		}
	}
	b.mu.Unlock()
	return pickWorker(candidates, rand.Float64())
}

// pickWorker chooses by weight, where a worker's weight is the capacity it has left. r is in [0, 1).
func pickWorker(candidates []*Worker, r float64) *Worker {
	var total float64
	weights := make([]float64, len(candidates))
	for i, w := range candidates {
		weights[i] = w.weight()
		total += weights[i]
	}
	if total <= 0 {
		return nil
	}
	point := r * total
	for i, w := range candidates {
		if weights[i] <= 0 {
			continue
		}
		point -= weights[i]
		if point < 0 {
			return w
		}
	}
	for i := len(candidates) - 1; i >= 0; i-- {
		if weights[i] > 0 {
			return candidates[i]
		}
	}
	return nil
}

// namespacesFor lists the namespaces with workers for a dispatch and job type. Each gets its own job.
func (b *Broker) namespacesFor(project string, d *pb.AgentDispatch, jobType pb.JobType) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for key := range b.workers {
		if key.project == project && key.agentName == d.GetAgentName() && key.jobType == jobType && key.deployment == d.GetDeployment() {
			out = append(out, key.namespace)
		}
	}
	return out
}

// ensureRoom returns the broker's state for a room, creating its dispatches and starting them the first time.
func (b *Broker) ensureRoom(r *room.Room) *roomState {
	b.mu.Lock()
	rs := b.byRoom[r.SID()]
	if rs == nil {
		rs = &roomState{
			project:    r.ProjectID(),
			name:       r.Name(),
			sid:        r.SID(),
			dispatches: map[string]*dispatch{},
			agentJobs:  map[string]*jobRecord{},
			publishers: map[string]bool{},
		}
		b.byRoom[r.SID()] = rs
	}
	if rs.started {
		b.mu.Unlock()
		return rs
	}
	rs.started = true
	configured := r.ConfiguredAgents()
	implicit := len(configured) == 0
	if implicit {
		configured = []*pb.RoomAgentDispatch{{}}
	}
	var created []*dispatch
	for _, c := range configured {
		if validateDeployment(c.GetDeployment()) != nil {
			b.log.Warn("ignoring room dispatch with an invalid deployment", "room", r.Name())
			continue
		}
		d := newDispatch(r.Name(), c.GetAgentName(), c.GetMetadata(), c.GetDeployment(), c.GetAttributes())
		d.implicit = implicit
		rs.dispatches[d.info.GetId()] = d
		created = append(created, d)
	}
	b.mu.Unlock()
	for _, d := range created {
		b.launch(r, rs, d, pb.JobType_JT_ROOM, nil)
	}
	return rs
}

func newDispatch(roomName, agentName, metadata, deployment string, attributes map[string]string) *dispatch {
	return &dispatch{info: &pb.AgentDispatch{
		Id:         participant.NewID(dispatchIDPrefix),
		AgentName:  agentName,
		Room:       roomName,
		Metadata:   metadata,
		Deployment: deployment,
		Attributes: attributes,
		State:      &pb.AgentDispatchState{CreatedAt: time.Now().UnixNano()},
	}}
}

// launch starts a dispatch's job of one type, on the workers registered for it. It returns before they answer.
func (b *Broker) launch(r *room.Room, rs *roomState, d *dispatch, jobType pb.JobType, target *pb.ParticipantInfo) {
	namespaces := b.namespacesFor(rs.project, d.info, jobType)
	if len(namespaces) == 0 {
		b.noCapacity(rs, d, jobType, target, "")
		return
	}
	b.mu.Lock()
	if d.deleted {
		b.mu.Unlock()
		return
	}
	d.launching.Add(len(namespaces))
	b.mu.Unlock()
	for _, ns := range namespaces {
		go func() {
			defer d.launching.Done()
			b.requestJob(r, rs, d, jobType, target, ns)
		}()
	}
}

// noCapacity records that nothing could take a job. A room job for a named agent waits for a worker to register.
func (b *Broker) noCapacity(rs *roomState, d *dispatch, jobType pb.JobType, target *pb.ParticipantInfo, namespace string) {
	jobsCounter.WithLabelValues("no_worker").Inc()
	b.log.Info("no worker available for agent job", "room", rs.name, "agentName", d.info.GetAgentName(),
		"jobType", jobType.String())
	if d.implicit || jobType != pb.JobType_JT_ROOM || target != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if d.waiting != nil {
		return
	}
	now := time.Now().UnixNano()
	d.waiting = &pb.Job{
		Id:         participant.NewID(jobIDPrefix),
		DispatchId: d.info.GetId(),
		Type:       jobType,
		Namespace:  namespace,
		AgentName:  d.info.GetAgentName(),
		Metadata:   d.info.GetMetadata(),
		Deployment: d.info.GetDeployment(),
		Attributes: d.info.GetAttributes(),
		State:      &pb.JobState{Status: pb.JobStatus_JS_PENDING, Reason: pb.JobStateReason_JSR_NO_CAPACITY, UpdatedAt: now},
	}
	d.info.State.Jobs = append(d.info.State.Jobs, d.waiting)
}

// startWaiting starts the room jobs that were waiting for a worker like this one.
func (b *Broker) startWaiting(w *Worker) {
	if w.key.jobType != pb.JobType_JT_ROOM {
		return
	}
	type start struct {
		rs *roomState
		d  *dispatch
	}
	var starts []start
	b.mu.Lock()
	for _, rs := range b.byRoom {
		if rs.project != w.key.project {
			continue
		}
		for _, d := range rs.dispatches {
			if d.waiting != nil && d.info.GetAgentName() == w.key.agentName && d.info.GetDeployment() == w.key.deployment {
				d.info.State.Jobs = removeJob(d.info.State.Jobs, d.waiting)
				d.waiting = nil
				starts = append(starts, start{rs, d})
			}
		}
	}
	b.mu.Unlock()
	for _, s := range starts {
		if r := b.rooms.Get(s.rs.project, s.rs.name); r != nil && r.SID() == s.rs.sid {
			b.launch(r, s.rs, s.d, pb.JobType_JT_ROOM, nil)
		}
	}
}

func removeJob(jobs []*pb.Job, job *pb.Job) []*pb.Job {
	out := jobs[:0]
	for _, j := range jobs {
		if j != job {
			out = append(out, j)
		}
	}
	return out
}

// requestJob offers a job to workers until one takes it or none is left.
func (b *Broker) requestJob(r *room.Room, rs *roomState, d *dispatch, jobType pb.JobType, target *pb.ParticipantInfo, namespace string) {
	job := &pb.Job{
		Id:          participant.NewID(jobIDPrefix),
		DispatchId:  d.info.GetId(),
		Type:        jobType,
		Room:        r.ToProto(),
		Participant: target,
		Namespace:   namespace,
		AgentName:   d.info.GetAgentName(),
		Metadata:    d.info.GetMetadata(),
		Deployment:  d.info.GetDeployment(),
		Attributes:  d.info.GetAttributes(),
		State:       &pb.JobState{},
	}
	key := workerKey{rs.project, d.info.GetAgentName(), namespace, jobType, d.info.GetDeployment()}
	attempted := map[*Worker]bool{}
	for {
		w := b.selectWorker(key, attempted)
		if w == nil {
			b.noCapacity(rs, d, jobType, target, namespace)
			return
		}
		attempted[w] = true
		err := b.assign(w, job, rs, d)
		if err == nil {
			return
		}
		b.log.Info("could not assign agent job", "job", job.GetId(), "worker", w.id, "error", err)
		if !errors.Is(err, errWorkerNotAvailable) && !errors.Is(err, errWorkerClosed) {
			jobsCounter.WithLabelValues("failed").Inc()
			return
		}
	}
}

// assign takes a job through the handshake with one worker: the offer, its answer, then the assignment with a token.
func (b *Broker) assign(w *Worker, job *pb.Job, rs *roomState, d *dispatch) error {
	job = proto.Clone(job).(*pb.Job)
	now := time.Now().UnixNano()
	job.State.WorkerId = w.id
	job.State.Status = pb.JobStatus_JS_RUNNING
	job.State.StartedAt = now
	job.State.UpdatedAt = now

	answer, err := w.offer(job, b.assignTimeout)
	if err != nil {
		return err
	}
	if answer.GetTerminate() {
		job.State.Status = pb.JobStatus_JS_SUCCESS
		job.State.EndedAt = time.Now().UnixNano()
		jobsCounter.WithLabelValues("declined_by_terminate").Inc()
		if d != nil {
			b.mu.Lock()
			d.info.State.Jobs = append(d.info.State.Jobs, job)
			b.mu.Unlock()
		}
		return nil
	}
	if !answer.GetAvailable() {
		return errWorkerNotAvailable
	}

	identity := answer.GetParticipantIdentity()
	if identity == "" {
		identity = "agent-" + job.GetId()
	}
	job.State.ParticipantIdentity = identity
	token, err := w.jobToken(job, answer, identity)
	if err != nil {
		return err
	}

	// Record the job before the worker hears about it, so a status update cannot arrive for an unknown job.
	rec := &jobRecord{job: job, worker: w, identity: identity, left: make(chan struct{})}
	b.mu.Lock()
	b.jobs[job.GetId()] = rec
	if rs != nil {
		rec.roomSID = rs.sid
		rs.agentJobs[identity] = rec
	}
	if d != nil {
		d.info.State.Jobs = append(d.info.State.Jobs, job)
	}
	assignment := proto.Clone(job).(*pb.Job)
	b.mu.Unlock()

	err = w.send(&pb.ServerMessage{Message: &pb.ServerMessage_Assignment{
		Assignment: &pb.JobAssignment{Job: assignment, Token: token},
	}})
	if err != nil {
		b.mu.Lock()
		delete(b.jobs, job.GetId())
		if rs != nil {
			delete(rs.agentJobs, identity)
		}
		if d != nil {
			d.info.State.Jobs = removeJob(d.info.State.Jobs, job)
		}
		b.mu.Unlock()
		return errWorkerClosed
	}
	jobsCounter.WithLabelValues("assigned").Inc()
	return nil
}

// updateJob applies a status report from the worker holding the job.
func (b *Broker) updateJob(w *Worker, u *pb.UpdateJobStatus) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.jobs[u.GetJobId()]
	if rec == nil || rec.worker != w || rec.ended {
		return
	}
	b.setJobStateLocked(rec, u.GetStatus(), u.GetError())
}

func (b *Broker) setJobStateLocked(rec *jobRecord, status pb.JobStatus, errText string) {
	now := time.Now().UnixNano()
	state := rec.job.GetState()
	state.Status = status
	state.Error = errText
	state.UpdatedAt = now
	if status == pb.JobStatus_JS_SUCCESS || status == pb.JobStatus_JS_FAILED || status == pb.JobStatus_JS_CANCELLED {
		state.EndedAt = now
		rec.ended = true
		delete(b.jobs, rec.job.GetId())
		jobsCounter.WithLabelValues(status.String()).Inc()
	}
}

// terminate ends a job and tells its worker.
func (b *Broker) terminate(rec *jobRecord, status pb.JobStatus, errText string) {
	b.mu.Lock()
	if rec.ended {
		b.mu.Unlock()
		return
	}
	w := rec.worker
	b.setJobStateLocked(rec, status, errText)
	jobID := rec.job.GetId()
	b.mu.Unlock()
	if w != nil {
		_ = w.send(&pb.ServerMessage{Message: &pb.ServerMessage_Termination{Termination: &pb.JobTermination{JobId: jobID}}})
	}
}

// migrateJobs gives a reconnected worker its jobs back.
func (b *Broker) migrateJobs(w *Worker, jobIDs []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, id := range jobIDs {
		rec := b.jobs[id]
		if rec == nil {
			continue
		}
		if rs := b.byRoom[rec.roomSID]; rs != nil && rs.project != w.key.project {
			continue
		}
		rec.worker = w
		rec.job.State.WorkerId = w.id
	}
}

func (b *Broker) onRoomEvent(e room.Event) {
	switch e.Type {
	case room.EventRoomStarted:
		if r := b.rooms.Get(e.ProjectID, e.Room.GetName()); r != nil {
			b.ensureRoom(r)
		}
	case room.EventRoomFinished:
		b.onRoomFinished(e.Room.GetSid())
	case room.EventParticipantJoined:
		b.launchTarget(e, pb.JobType_JT_PARTICIPANT)
	case room.EventTrackPublished:
		b.launchTarget(e, pb.JobType_JT_PUBLISHER)
	}
}

// launchTarget starts per-participant jobs. Agents and recorders do not attract agents of their own.
func (b *Broker) launchTarget(e room.Event, jobType pb.JobType) {
	r := b.rooms.Get(e.ProjectID, e.Room.GetName())
	if r == nil || e.Participant == nil {
		return
	}
	p := r.Participant(e.Participant.GetIdentity())
	if p == nil || p.Dependent() {
		return
	}
	rs := b.ensureRoom(r)
	b.mu.Lock()
	if jobType == pb.JobType_JT_PUBLISHER {
		if rs.publishers[e.Participant.GetSid()] {
			b.mu.Unlock()
			return
		}
		rs.publishers[e.Participant.GetSid()] = true
	}
	list := make([]*dispatch, 0, len(rs.dispatches))
	for _, d := range rs.dispatches {
		list = append(list, d)
	}
	b.mu.Unlock()
	for _, d := range list {
		b.launch(r, rs, d, jobType, e.Participant)
	}
}

func (b *Broker) onRoomFinished(sid string) {
	b.mu.Lock()
	rs := b.byRoom[sid]
	delete(b.byRoom, sid)
	var open []*jobRecord
	if rs != nil {
		for _, rec := range rs.agentJobs {
			if !rec.ended {
				open = append(open, rec)
			}
		}
	}
	b.mu.Unlock()
	for _, rec := range open {
		b.terminate(rec, pb.JobStatus_JS_SUCCESS, "")
	}
}

// onAgentLeft ends the job of an agent whose session ended. The job failed unless something already ended it, since a
// healthy job's agent stays until the job is over.
func (b *Broker) onAgentLeft(r *room.Room, p *participant.Participant, _ pb.DisconnectReason) {
	// A newer session with this identity means the agent replaced itself and has not left.
	if current := r.Participant(p.Identity()); current != nil && current != p {
		return
	}
	b.mu.Lock()
	rs := b.byRoom[r.SID()]
	var rec *jobRecord
	if rs != nil {
		rec = rs.agentJobs[p.Identity()]
		delete(rs.agentJobs, p.Identity())
	}
	b.mu.Unlock()
	if rec == nil {
		return
	}
	rec.leftOnce.Do(func() { close(rec.left) })
	b.terminate(rec, pb.JobStatus_JS_FAILED, "agent worker left the room")
}

// CreateDispatch implements roomapi.AgentDispatcher.
func (b *Broker) CreateDispatch(project string, in *pb.CreateAgentDispatchRequest) (*pb.AgentDispatch, error) {
	if err := validateDeployment(in.GetDeployment()); err != nil {
		return nil, err
	}
	r := b.rooms.Get(project, in.GetRoom())
	if r == nil {
		if !b.rooms.AutoCreateEnabled(project) {
			return nil, roomapi.ErrRoomNotFound
		}
		r, _ = b.rooms.GetOrCreate(project, in.GetRoom(), room.CreateOptions{})
	}
	rs := b.ensureRoom(r)
	d := newDispatch(in.GetRoom(), in.GetAgentName(), in.GetMetadata(), in.GetDeployment(), in.GetAttributes())
	b.mu.Lock()
	rs.dispatches[d.info.GetId()] = d
	out := proto.Clone(d.info).(*pb.AgentDispatch)
	b.mu.Unlock()

	b.launch(r, rs, d, pb.JobType_JT_ROOM, nil)
	for _, p := range r.Participants() {
		if p.Dependent() {
			continue
		}
		info := p.ToProto()
		if info.GetIsPublisher() {
			b.launch(r, rs, d, pb.JobType_JT_PUBLISHER, info)
		}
		b.launch(r, rs, d, pb.JobType_JT_PARTICIPANT, info)
	}
	return out, nil
}

// DeleteDispatch implements roomapi.AgentDispatcher. The dispatch's jobs end, and any agent still there is removed
// shortly after.
func (b *Broker) DeleteDispatch(project string, in *pb.DeleteAgentDispatchRequest) (*pb.AgentDispatch, error) {
	r := b.rooms.Get(project, in.GetRoom())
	if r == nil {
		return nil, roomapi.ErrRoomNotFound
	}
	rs := b.ensureRoom(r)
	b.mu.Lock()
	d := rs.dispatches[in.GetDispatchId()]
	if d == nil {
		b.mu.Unlock()
		return nil, roomapi.ErrDispatchNotFound
	}
	delete(rs.dispatches, in.GetDispatchId())
	d.deleted = true
	d.info.State.DeletedAt = time.Now().UnixNano()
	out := proto.Clone(d.info).(*pb.AgentDispatch)
	b.mu.Unlock()

	go b.finishDispatch(r, rs, d)
	return out, nil
}

func (b *Broker) finishDispatch(r *room.Room, rs *roomState, d *dispatch) {
	d.launching.Wait()
	b.mu.Lock()
	var recs []*jobRecord
	for _, rec := range rs.agentJobs {
		if rec.job.GetDispatchId() == d.info.GetId() {
			recs = append(recs, rec)
		}
	}
	b.mu.Unlock()
	for _, rec := range recs {
		b.terminate(rec, pb.JobStatus_JS_SUCCESS, "")
		select {
		case <-rec.left:
			continue
		case <-time.After(agentLeaveTimeout):
		}
		if p := r.Participant(rec.identity); p != nil {
			p.Close(true, pb.DisconnectReason_PARTICIPANT_REMOVED)
		}
	}
}

// ListDispatch implements roomapi.AgentDispatcher.
func (b *Broker) ListDispatch(project string, in *pb.ListAgentDispatchRequest) ([]*pb.AgentDispatch, error) {
	r := b.rooms.Get(project, in.GetRoom())
	if r == nil {
		return nil, roomapi.ErrRoomNotFound
	}
	rs := b.ensureRoom(r)
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*pb.AgentDispatch, 0, len(rs.dispatches))
	for id, d := range rs.dispatches {
		if in.GetDispatchId() == "" || in.GetDispatchId() == id {
			out = append(out, proto.Clone(d.info).(*pb.AgentDispatch))
		}
	}
	return out, nil
}

func validateDeployment(deployment string) error {
	if len(deployment) > maxDeploymentLength {
		return errors.New("deployment is too long")
	}
	for i := 0; i < len(deployment); i++ {
		c := deployment[i]
		if c == '_' || c <= ' ' || c == 0x7f {
			return errors.New("deployment contains a reserved character")
		}
	}
	return nil
}
