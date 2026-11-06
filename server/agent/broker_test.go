package agent

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/config"
	"elnova-streamkit.local/server/room"
	"elnova-streamkit.local/server/roomapi"
)

// These tests run the broker over real WebSockets, with a test worker speaking the protocol. They do not start agents
// or media, and tests/agents covers those against a real framework worker on both servers.

const testSecret = "a-long-enough-secret-for-testing-0001"

type testKeys map[string]auth.Key

func (k testKeys) Lookup(id string) (auth.Key, bool) { v, ok := k[id]; return v, ok }

var keys = testKeys{
	"k1": {APIKey: "k1", Secret: testSecret, ProjectID: "p1"},
	"k2": {APIKey: "k2", Secret: testSecret, ProjectID: "p2"},
}

type harness struct {
	t      *testing.T
	broker *Broker
	rooms  *room.Manager
	srv    *httptest.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	cfg := config.Default()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rooms := room.NewManager(ctx, cfg, nil, slog.New(slog.DiscardHandler))
	b := NewBroker(rooms, keys, slog.New(slog.DiscardHandler))
	t.Cleanup(b.Close)
	mux := http.NewServeMux()
	b.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &harness{t: t, broker: b, rooms: rooms, srv: srv}
}

func (h *harness) token(key string, agent bool) string {
	h.t.Helper()
	tok, err := auth.Mint(keys[key], &auth.Grants{Identity: "worker", Video: &auth.VideoGrant{Agent: agent}}, time.Minute)
	if err != nil {
		h.t.Fatal(err)
	}
	return tok
}

type testWorker struct {
	t  *testing.T
	ws *websocket.Conn
	id string
}

func (h *harness) dial(key string) *testWorker {
	h.t.Helper()
	url := "ws" + strings.TrimPrefix(h.srv.URL, "http") + "/agent"
	header := http.Header{"Authorization": {"Bearer " + h.token(key, true)}}
	ws, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = ws.Close() })
	return &testWorker{t: h.t, ws: ws}
}

// worker connects and registers.
func (h *harness) worker(key, agentName string, jobType pb.JobType) *testWorker {
	h.t.Helper()
	w := h.dial(key)
	w.send(&pb.WorkerMessage{Message: &pb.WorkerMessage_Register{Register: &pb.RegisterWorkerRequest{
		Type: jobType, AgentName: agentName, Version: "test",
	}}})
	reg := w.next().GetRegister()
	if reg == nil || reg.GetWorkerId() == "" || reg.GetServerInfo() == nil {
		h.t.Fatalf("no registration response: %v", reg)
	}
	w.id = reg.GetWorkerId()
	return w
}

func (w *testWorker) send(msg *pb.WorkerMessage) {
	w.t.Helper()
	data, err := proto.Marshal(msg)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := w.ws.WriteMessage(websocket.BinaryMessage, data); err != nil {
		w.t.Fatal(err)
	}
}

func (w *testWorker) nextWithin(d time.Duration) (*pb.ServerMessage, error) {
	_ = w.ws.SetReadDeadline(time.Now().Add(d))
	_, data, err := w.ws.ReadMessage()
	if err != nil {
		return nil, err
	}
	msg := &pb.ServerMessage{}
	return msg, proto.Unmarshal(data, msg)
}

func (w *testWorker) next() *pb.ServerMessage {
	w.t.Helper()
	msg, err := w.nextWithin(5 * time.Second)
	if err != nil {
		w.t.Fatalf("waiting for a server message: %v", err)
	}
	return msg
}

// expectNothing checks that the worker gets no message for a short while. Reading past a deadline spoils a gorilla
// connection, so a worker checked this way is finished with.
func (w *testWorker) expectNothing(d time.Duration) {
	w.t.Helper()
	if msg, err := w.nextWithin(d); err == nil {
		w.t.Fatalf("expected nothing, got %v", msg)
	}
}

func (w *testWorker) offer() *pb.Job {
	w.t.Helper()
	job := w.next().GetAvailability().GetJob()
	if job == nil {
		w.t.Fatal("expected a job offer")
	}
	return job
}

func (w *testWorker) accept(job *pb.Job, identity string, attrs map[string]string) *pb.JobAssignment {
	w.t.Helper()
	w.send(&pb.WorkerMessage{Message: &pb.WorkerMessage_Availability{Availability: &pb.AvailabilityResponse{
		JobId: job.GetId(), Available: true, ParticipantIdentity: identity, ParticipantName: "bot",
		ParticipantMetadata: "meta", ParticipantAttributes: attrs,
	}}})
	a := w.next().GetAssignment()
	if a == nil || a.GetToken() == "" {
		w.t.Fatalf("expected an assignment, got %v", a)
	}
	return a
}

func (w *testWorker) decline(job *pb.Job) {
	w.t.Helper()
	w.send(&pb.WorkerMessage{Message: &pb.WorkerMessage_Availability{Availability: &pb.AvailabilityResponse{JobId: job.GetId()}}})
}

func (w *testWorker) status(status pb.WorkerStatus, load float32, seq uint64) {
	w.t.Helper()
	w.send(&pb.WorkerMessage{Message: &pb.WorkerMessage_UpdateWorker{UpdateWorker: &pb.UpdateWorkerStatus{
		Status: &status, Load: load, Seq: seq,
	}}})
}

// waitFor polls a condition on broker state. Status reports go through a read loop, so there is nothing else to wait on.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWorkerEndpointAuthentication(t *testing.T) {
	h := newHarness(t)
	url := "ws" + strings.TrimPrefix(h.srv.URL, "http") + "/agent"

	res, err := http.Get(h.srv.URL + "/agent")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("a plain request got %d, want 404", res.StatusCode)
	}

	for name, header := range map[string]http.Header{
		"no token":       nil,
		"no agent grant": {"Authorization": {"Bearer " + h.token("k1", false)}},
		"bad token":      {"Authorization": {"Bearer nonsense"}},
	} {
		_, res, err := websocket.DefaultDialer.Dial(url, header)
		if err == nil {
			t.Fatalf("%s: connection was accepted", name)
		}
		if res == nil || res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: got %v, want 401", name, res)
		}
	}
}

func TestFirstMessageMustRegister(t *testing.T) {
	h := newHarness(t)
	w := h.dial("k1")

	// A ping is the one thing allowed before registering.
	w.send(&pb.WorkerMessage{Message: &pb.WorkerMessage_Ping{Ping: &pb.WorkerPing{Timestamp: 42}}})
	if got := w.next().GetPong().GetLastTimestamp(); got != 42 {
		t.Fatalf("pong echoed %d", got)
	}

	w.send(&pb.WorkerMessage{Message: &pb.WorkerMessage_UpdateWorker{UpdateWorker: &pb.UpdateWorkerStatus{}}})
	if _, err := w.nextWithin(2 * time.Second); !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
		t.Fatalf("got %v, want a policy violation close", err)
	}
}

func TestRegisterDeadline(t *testing.T) {
	h := newHarness(t)
	h.broker.registerTimeout = 50 * time.Millisecond
	w := h.dial("k1")
	if _, err := w.nextWithin(2 * time.Second); err == nil || !strings.Contains(err.Error(), "close") && !strings.Contains(err.Error(), "EOF") {
		t.Fatalf("got %v, want the connection closed", err)
	}
}

func TestRegistrationRefused(t *testing.T) {
	h := newHarness(t)
	for name, req := range map[string]*pb.RegisterWorkerRequest{
		"unknown job type":   {Type: pb.JobType(99)},
		"reserved character": {Deployment: "a_b"},
		"whitespace":         {Deployment: "a b"},
		"deployment too big": {Deployment: strings.Repeat("x", maxDeploymentLength+1)},
	} {
		w := h.dial("k1")
		w.send(&pb.WorkerMessage{Message: &pb.WorkerMessage_Register{Register: req}})
		if _, err := w.nextWithin(2 * time.Second); !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
			t.Fatalf("%s: got %v, want a policy violation close", name, err)
		}
	}
}

func TestJSONFramesGetJSONReplies(t *testing.T) {
	h := newHarness(t)
	w := h.dial("k1")
	if err := w.ws.WriteMessage(websocket.TextMessage, []byte(`{"register":{"type":"JT_ROOM","agentName":"json"}}`)); err != nil {
		t.Fatal(err)
	}
	_ = w.ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	kind, data, err := w.ws.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if kind != websocket.TextMessage || !strings.Contains(string(data), "workerId") {
		t.Fatalf("got kind %d: %s", kind, data)
	}
}

func TestAutomaticDispatchGoesToUnnamedWorkers(t *testing.T) {
	h := newHarness(t)
	unnamed := h.worker("k1", "", pb.JobType_JT_ROOM)
	named := h.worker("k1", "named", pb.JobType_JT_ROOM)

	r, _ := h.rooms.GetOrCreate("p1", "auto", room.CreateOptions{})
	job := unnamed.offer()
	if job.GetType() != pb.JobType_JT_ROOM || job.GetRoom().GetName() != "auto" || job.GetRoom().GetSid() != r.SID() {
		t.Fatalf("wrong job: %v", job)
	}
	if job.GetState().GetStatus() != pb.JobStatus_JS_RUNNING || job.GetState().GetWorkerId() != unnamed.id {
		t.Fatalf("an offer should already carry the running state and worker: %v", job.GetState())
	}

	a := unnamed.accept(job, "agent-a", map[string]string{"custom": "yes"})
	assertJobToken(t, a.GetToken(), "auto", "agent-a", job.GetId(), "")
	named.expectNothing(150 * time.Millisecond)
}

func assertJobToken(t *testing.T, token, roomName, identity, jobID, agentName string) {
	t.Helper()
	v, err := auth.Verify(token, keys)
	if err != nil {
		t.Fatal(err)
	}
	g := v.Grants
	if g.Identity != identity || g.Name != "bot" || g.Metadata != "meta" || g.Kind != "agent" {
		t.Fatalf("wrong claims: %+v", g)
	}
	vg := g.Video
	if !vg.RoomJoin || !vg.Agent || vg.Room != roomName || vg.RoomAdmin || vg.RoomCreate {
		t.Fatalf("wrong grant: %+v", vg)
	}
	if !vg.GetCanPublish() || !vg.GetCanSubscribe() || !vg.GetCanPublishData() || !vg.GetCanUpdateOwnMetadata() {
		t.Fatalf("default permissions missing: %+v", vg)
	}
	if ttl := time.Until(v.ExpiresAt); ttl < 59*time.Minute || ttl > 61*time.Minute {
		t.Fatalf("token lifetime is %v, want an hour", ttl)
	}
	if g.Attributes["custom"] != "yes" || g.Attributes[agentNameAttribute] != agentName ||
		!strings.HasPrefix(g.Attributes[jobAttemptAttribute], jobID+":") {
		t.Fatalf("wrong attributes: %v", g.Attributes)
	}
}

func TestOffersStayWithinProject(t *testing.T) {
	h := newHarness(t)
	other := h.worker("k2", "", pb.JobType_JT_ROOM)
	mine := h.worker("k1", "", pb.JobType_JT_ROOM)
	h.rooms.GetOrCreate("p1", "tenant", room.CreateOptions{})
	mine.offer()
	other.expectNothing(150 * time.Millisecond)
}

func TestDeclinedJobGoesToTheNextWorker(t *testing.T) {
	h := newHarness(t)
	a := h.worker("k1", "", pb.JobType_JT_ROOM)
	b := h.worker("k1", "", pb.JobType_JT_ROOM)
	h.rooms.GetOrCreate("p1", "retry", room.CreateOptions{})

	first, second := make(chan *pb.Job, 1), make(chan *pb.Job, 1)
	read := func(w *testWorker, out chan *pb.Job) {
		if msg, err := w.nextWithin(3 * time.Second); err == nil {
			out <- msg.GetAvailability().GetJob()
		} else {
			out <- nil
		}
	}
	go read(a, first)
	go read(b, second)
	select {
	case job := <-first:
		a.decline(job)
		accepted := <-second
		if accepted == nil || accepted.GetId() != job.GetId() {
			t.Fatalf("second worker got %v", accepted)
		}
	case job := <-second:
		b.decline(job)
		accepted := <-first
		if accepted == nil || accepted.GetId() != job.GetId() {
			t.Fatalf("second worker got %v", accepted)
		}
	}
}

func TestTerminateAnswerEndsTheJob(t *testing.T) {
	h := newHarness(t)
	w := h.worker("k1", "term", pb.JobType_JT_ROOM)
	h.rooms.GetOrCreate("p1", "term", room.CreateOptions{Agents: []*pb.RoomAgentDispatch{{AgentName: "term"}}})
	job := w.offer()
	w.send(&pb.WorkerMessage{Message: &pb.WorkerMessage_Availability{Availability: &pb.AvailabilityResponse{JobId: job.GetId(), Terminate: true}}})

	waitFor(t, "the job to end", func() bool {
		list, err := h.broker.ListDispatch("p1", &pb.ListAgentDispatchRequest{Room: "term"})
		if err != nil || len(list) != 1 || len(list[0].GetState().GetJobs()) != 1 {
			return false
		}
		return list[0].GetState().GetJobs()[0].GetState().GetStatus() == pb.JobStatus_JS_SUCCESS
	})
	w.expectNothing(100 * time.Millisecond)
}

func TestTimedOutOfferIsNotRetried(t *testing.T) {
	h := newHarness(t)
	h.broker.assignTimeout = 100 * time.Millisecond
	a := h.worker("k1", "", pb.JobType_JT_ROOM)
	b := h.worker("k1", "", pb.JobType_JT_ROOM)
	h.rooms.GetOrCreate("p1", "slow", room.CreateOptions{})
	// One of them got the offer and says nothing. The other must not get it after the timeout.
	msgs := make(chan *pb.ServerMessage, 2)
	read := func(w *testWorker) {
		if msg, err := w.nextWithin(600 * time.Millisecond); err == nil {
			msgs <- msg
		}
	}
	go read(a)
	go read(b)
	time.Sleep(700 * time.Millisecond)
	if len(msgs) != 1 {
		t.Fatalf("%d workers got an offer, want exactly 1", len(msgs))
	}
}

func TestFullWorkerIsNotChosen(t *testing.T) {
	h := newHarness(t)
	full := h.worker("k1", "", pb.JobType_JT_ROOM)
	free := h.worker("k1", "", pb.JobType_JT_ROOM)
	full.status(pb.WorkerStatus_WS_FULL, 1, 1)
	waitFor(t, "the status report", func() bool {
		h.broker.mu.Lock()
		defer h.broker.mu.Unlock()
		for _, w := range h.broker.workers[workerKey{"p1", "", "", pb.JobType_JT_ROOM, ""}] {
			if w.id == full.id {
				return w.weight() == 0
			}
		}
		return false
	})
	for i := range 20 {
		name := "full-" + string(rune('a'+i))
		h.rooms.GetOrCreate("p1", name, room.CreateOptions{})
		if got := free.offer().GetRoom().GetName(); got != name {
			t.Fatalf("offer for %s, want %s", got, name)
		}
	}
	full.expectNothing(50 * time.Millisecond)
}

func TestSelectionFollowsLoad(t *testing.T) {
	light := &Worker{load: 0.2}
	heavy := &Worker{load: 0.6}
	full := &Worker{status: pb.WorkerStatus_WS_FULL}
	draining := &Worker{draining: true}
	saturated := &Worker{load: 1}
	candidates := []*Worker{full, light, draining, heavy, saturated}

	counts := map[*Worker]int{}
	const n = 10000
	for i := range n {
		counts[pickWorker(candidates, float64(i)/n)]++
	}
	if counts[full] != 0 || counts[draining] != 0 || counts[saturated] != 0 || counts[nil] != 0 {
		t.Fatalf("a worker with no capacity was chosen: %v", counts)
	}
	// Weights are 0.8 and 0.4.
	if got := float64(counts[light]) / n; got < 0.64 || got > 0.69 {
		t.Fatalf("the lighter worker got %.3f of jobs, want about 0.667", got)
	}
	if pickWorker([]*Worker{full, saturated}, 0.5) != nil {
		t.Fatal("no capacity should mean no worker")
	}
}

func TestStaleStatusIsIgnored(t *testing.T) {
	w := &Worker{}
	full := pb.WorkerStatus_WS_FULL
	available := pb.WorkerStatus_WS_AVAILABLE
	w.handleUpdate(&pb.UpdateWorkerStatus{Status: &full, Load: 0.9, Seq: 5})
	w.handleUpdate(&pb.UpdateWorkerStatus{Status: &available, Load: 0.1, Seq: 4})
	if w.status != full || w.load != 0.9 {
		t.Fatalf("a stale report took effect: %v %v", w.status, w.load)
	}
	// Workers that send no sequence number always apply.
	w.handleUpdate(&pb.UpdateWorkerStatus{Status: &available, Load: 0.1})
	if w.status != available {
		t.Fatal("an unsequenced report was ignored")
	}
}

func TestImplicitDispatchIsLostWithoutAWorker(t *testing.T) {
	h := newHarness(t)
	h.rooms.GetOrCreate("p1", "lost", room.CreateOptions{})
	waitFor(t, "the room's dispatch", func() bool {
		list, err := h.broker.ListDispatch("p1", &pb.ListAgentDispatchRequest{Room: "lost"})
		return err == nil && len(list) == 1
	})
	w := h.worker("k1", "", pb.JobType_JT_ROOM)
	w.expectNothing(200 * time.Millisecond)
}

func TestNamedDispatchWaitsForAWorker(t *testing.T) {
	h := newHarness(t)
	d, err := h.broker.CreateDispatch("p1", &pb.CreateAgentDispatchRequest{AgentName: "later", Room: "waiting", Metadata: "m"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to be pending", func() bool {
		list, _ := h.broker.ListDispatch("p1", &pb.ListAgentDispatchRequest{Room: "waiting", DispatchId: d.GetId()})
		if len(list) != 1 || len(list[0].GetState().GetJobs()) != 1 {
			return false
		}
		s := list[0].GetState().GetJobs()[0].GetState()
		return s.GetStatus() == pb.JobStatus_JS_PENDING && s.GetReason() == pb.JobStateReason_JSR_NO_CAPACITY
	})

	w := h.worker("k1", "later", pb.JobType_JT_ROOM)
	job := w.offer()
	if job.GetDispatchId() != d.GetId() || job.GetMetadata() != "m" {
		t.Fatalf("wrong job: %v", job)
	}
	w.accept(job, "agent-later", nil)
	list, _ := h.broker.ListDispatch("p1", &pb.ListAgentDispatchRequest{Room: "waiting", DispatchId: d.GetId()})
	if jobs := list[0].GetState().GetJobs(); len(jobs) != 1 || jobs[0].GetState().GetStatus() != pb.JobStatus_JS_RUNNING {
		t.Fatalf("wrong jobs after starting: %v", jobs)
	}
}

func TestRoomAgentsReplaceTheAutomaticDispatch(t *testing.T) {
	h := newHarness(t)
	unnamed := h.worker("k1", "", pb.JobType_JT_ROOM)
	named := h.worker("k1", "sales", pb.JobType_JT_ROOM)
	h.rooms.GetOrCreate("p1", "config", room.CreateOptions{Agents: []*pb.RoomAgentDispatch{{AgentName: "sales", Metadata: "hello"}}})
	job := named.offer()
	if job.GetAgentName() != "sales" || job.GetMetadata() != "hello" {
		t.Fatalf("wrong job: %v", job)
	}
	unnamed.expectNothing(150 * time.Millisecond)
}

func TestDeploymentsAreSeparate(t *testing.T) {
	h := newHarness(t)
	w := h.dial("k1")
	w.send(&pb.WorkerMessage{Message: &pb.WorkerMessage_Register{Register: &pb.RegisterWorkerRequest{AgentName: "a", Deployment: "blue"}}})
	w.next()
	other := h.worker("k1", "a", pb.JobType_JT_ROOM)
	if _, err := h.broker.CreateDispatch("p1", &pb.CreateAgentDispatchRequest{AgentName: "a", Room: "dep", Deployment: "blue"}); err != nil {
		t.Fatal(err)
	}
	if got := w.offer().GetDeployment(); got != "blue" {
		t.Fatalf("deployment %q", got)
	}
	other.expectNothing(100 * time.Millisecond)
	if _, err := h.broker.CreateDispatch("p1", &pb.CreateAgentDispatchRequest{AgentName: "a", Room: "dep", Deployment: "a_b"}); err == nil {
		t.Fatal("an invalid deployment was accepted")
	}
}

func TestJobStatusUpdates(t *testing.T) {
	h := newHarness(t)
	w := h.worker("k1", "up", pb.JobType_JT_ROOM)
	intruder := h.worker("k1", "other", pb.JobType_JT_ROOM)
	d, err := h.broker.CreateDispatch("p1", &pb.CreateAgentDispatchRequest{AgentName: "up", Room: "status"})
	if err != nil {
		t.Fatal(err)
	}
	job := w.offer()
	w.accept(job, "agent-up", nil)

	jobState := func() *pb.JobState {
		list, _ := h.broker.ListDispatch("p1", &pb.ListAgentDispatchRequest{Room: "status", DispatchId: d.GetId()})
		if len(list) != 1 || len(list[0].GetState().GetJobs()) != 1 {
			return nil
		}
		return list[0].GetState().GetJobs()[0].GetState()
	}

	// Only the worker holding a job may report on it.
	intruder.send(&pb.WorkerMessage{Message: &pb.WorkerMessage_UpdateJob{UpdateJob: &pb.UpdateJobStatus{JobId: job.GetId(), Status: pb.JobStatus_JS_FAILED}}})
	ping := func(tw *testWorker) {
		tw.send(&pb.WorkerMessage{Message: &pb.WorkerMessage_Ping{Ping: &pb.WorkerPing{Timestamp: 1}}})
		tw.next()
	}
	ping(intruder)
	if s := jobState(); s.GetStatus() != pb.JobStatus_JS_RUNNING {
		t.Fatalf("another worker changed the job: %v", s)
	}

	w.send(&pb.WorkerMessage{Message: &pb.WorkerMessage_UpdateJob{UpdateJob: &pb.UpdateJobStatus{JobId: job.GetId(), Status: pb.JobStatus_JS_FAILED, Error: "boom"}}})
	ping(w)
	s := jobState()
	if s.GetStatus() != pb.JobStatus_JS_FAILED || s.GetError() != "boom" || s.GetEndedAt() == 0 {
		t.Fatalf("wrong state: %v", s)
	}
}

func TestDeleteDispatchTerminatesItsJobs(t *testing.T) {
	h := newHarness(t)
	w := h.worker("k1", "del", pb.JobType_JT_ROOM)
	d, err := h.broker.CreateDispatch("p1", &pb.CreateAgentDispatchRequest{AgentName: "del", Room: "gone"})
	if err != nil {
		t.Fatal(err)
	}
	job := w.offer()
	w.accept(job, "agent-del", nil)

	if _, err := h.broker.DeleteDispatch("p1", &pb.DeleteAgentDispatchRequest{DispatchId: "nope", Room: "gone"}); err != roomapi.ErrDispatchNotFound {
		t.Fatalf("got %v", err)
	}
	if _, err := h.broker.DeleteDispatch("p1", &pb.DeleteAgentDispatchRequest{DispatchId: d.GetId(), Room: "gone"}); err != nil {
		t.Fatal(err)
	}
	if got := w.next().GetTermination().GetJobId(); got != job.GetId() {
		t.Fatalf("terminated %q, want %q", got, job.GetId())
	}
	list, err := h.broker.ListDispatch("p1", &pb.ListAgentDispatchRequest{Room: "gone"})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range list {
		if x.GetId() == d.GetId() {
			t.Fatal("the deleted dispatch is still listed")
		}
	}
}

func TestDispatchNeedsARoom(t *testing.T) {
	h := newHarness(t)
	if _, err := h.broker.ListDispatch("p1", &pb.ListAgentDispatchRequest{Room: "missing"}); err != roomapi.ErrRoomNotFound {
		t.Fatalf("list: %v", err)
	}
	if _, err := h.broker.DeleteDispatch("p1", &pb.DeleteAgentDispatchRequest{Room: "missing", DispatchId: "x"}); err != roomapi.ErrRoomNotFound {
		t.Fatalf("delete: %v", err)
	}
	// Room names are per project: this project's dispatch does not make a room for another project.
	if _, err := h.broker.CreateDispatch("p1", &pb.CreateAgentDispatchRequest{AgentName: "a", Room: "shared"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.broker.ListDispatch("p2", &pb.ListAgentDispatchRequest{Room: "shared"}); err != roomapi.ErrRoomNotFound {
		t.Fatalf("other project: %v", err)
	}
}

func TestSimulateJobOffersItBack(t *testing.T) {
	h := newHarness(t)
	w := h.worker("k1", "sim", pb.JobType_JT_ROOM)
	w.send(&pb.WorkerMessage{Message: &pb.WorkerMessage_SimulateJob{SimulateJob: &pb.SimulateJobRequest{
		Type: pb.JobType_JT_ROOM, Room: &pb.Room{Name: "simulated"},
	}}})
	job := w.offer()
	if job.GetRoom().GetName() != "simulated" || job.GetAgentName() != "sim" {
		t.Fatalf("wrong job: %v", job)
	}
	a := w.accept(job, "agent-sim", map[string]string{"custom": "yes"})
	assertJobToken(t, a.GetToken(), "simulated", "agent-sim", job.GetId(), "sim")
}

func TestReconnectedWorkerTakesItsJobsBack(t *testing.T) {
	h := newHarness(t)
	first := h.worker("k1", "mig", pb.JobType_JT_ROOM)
	if _, err := h.broker.CreateDispatch("p1", &pb.CreateAgentDispatchRequest{AgentName: "mig", Room: "moved"}); err != nil {
		t.Fatal(err)
	}
	job := first.offer()
	first.accept(job, "agent-mig", nil)
	_ = first.ws.Close()

	second := h.worker("k1", "mig", pb.JobType_JT_ROOM)
	second.send(&pb.WorkerMessage{Message: &pb.WorkerMessage_MigrateJob{MigrateJob: &pb.MigrateJobRequest{JobIds: []string{job.GetId()}}}})
	second.send(&pb.WorkerMessage{Message: &pb.WorkerMessage_Ping{Ping: &pb.WorkerPing{Timestamp: 1}}})
	second.next()

	h.broker.mu.Lock()
	rec := h.broker.jobs[job.GetId()]
	var owner string
	if rec != nil && rec.worker != nil {
		owner = rec.worker.id
	}
	h.broker.mu.Unlock()
	if owner != second.id {
		t.Fatalf("job belongs to %q, want %q", owner, second.id)
	}
	h.broker.terminate(rec, pb.JobStatus_JS_SUCCESS, "")
	if got := second.next().GetTermination().GetJobId(); got != job.GetId() {
		t.Fatalf("terminated %q", got)
	}
}
