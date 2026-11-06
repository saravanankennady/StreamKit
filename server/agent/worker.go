package agent

import (
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/auth"
	"elnova-streamkit.local/server/participant"
)

const (
	maxWorkerMessageBytes = 1 << 20
	writeTimeout          = 10 * time.Second
	// The server pings a worker at this interval, and a worker silent for readTimeout is gone. Workers answer pings
	// at the WebSocket layer, so this needs nothing from them beyond a working connection.
	keepAliveInterval = 15 * time.Second
	readTimeout       = 60 * time.Second

	jobTokenTTL = time.Hour

	// Attributes the server adds to an agent's token.
	agentNameAttribute  = "lk.agent_name"
	jobAttemptAttribute = "lk.agent.job_attempt"
)

// Worker is one registered connection.
type Worker struct {
	id    string
	key   workerKey
	perms *pb.ParticipantPermission
	token auth.Key
	conn  *workerConn

	mu         sync.Mutex
	status     pb.WorkerStatus
	load       float32
	draining   bool
	seq        uint64
	offers     map[string]chan *pb.AvailabilityResponse
	closed     chan struct{}
	closedOnce sync.Once
}

func (w *Worker) send(msg *pb.ServerMessage) error { return w.conn.send(msg) }

func (w *Worker) close() {
	w.closedOnce.Do(func() { close(w.closed) })
	w.conn.close()
}

// weight is the worker's share in random selection: the capacity it has left, or zero if it is taking no work.
func (w *Worker) weight() float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status != pb.WorkerStatus_WS_AVAILABLE || w.draining {
		return 0
	}
	if w.load >= 1 {
		return 0
	}
	if w.load <= 0 {
		return 1
	}
	return float64(1 - w.load)
}

// offer asks the worker to take a job and waits for its answer.
func (w *Worker) offer(job *pb.Job, timeout time.Duration) (*pb.AvailabilityResponse, error) {
	answer := make(chan *pb.AvailabilityResponse, 1)
	id := job.GetId()
	w.mu.Lock()
	if _, dup := w.offers[id]; dup {
		w.mu.Unlock()
		return nil, errDuplicateOffer
	}
	w.offers[id] = answer
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.offers, id)
		w.mu.Unlock()
	}()
	if err := w.send(&pb.ServerMessage{Message: &pb.ServerMessage_Availability{
		Availability: &pb.AvailabilityRequest{Job: job},
	}}); err != nil {
		return nil, errWorkerClosed
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-answer:
		return res, nil
	case <-timer.C:
		return nil, errOfferTimeout
	case <-w.closed:
		return nil, errWorkerClosed
	}
}

// jobToken signs the agent's token for a job, with the key the worker authenticated with, for that one room.
func (w *Worker) jobToken(job *pb.Job, answer *pb.AvailabilityResponse, identity string) (string, error) {
	attributes := make(map[string]string, len(answer.GetParticipantAttributes())+2)
	for k, v := range answer.GetParticipantAttributes() {
		attributes[k] = v
	}
	attributes[agentNameAttribute] = w.key.agentName
	attributes[jobAttemptAttribute] = job.GetId() + ":" + strconv.FormatInt(job.GetState().GetStartedAt(), 10)

	p := w.perms
	return auth.Mint(w.token, &auth.Grants{
		Identity:   identity,
		Name:       answer.GetParticipantName(),
		Kind:       "agent",
		Metadata:   answer.GetParticipantMetadata(),
		Attributes: attributes,
		Video: &auth.VideoGrant{
			RoomJoin:             true,
			Agent:                true,
			Room:                 job.GetRoom().GetName(),
			CanSubscribe:         &p.CanSubscribe,
			CanPublish:           &p.CanPublish,
			CanPublishData:       &p.CanPublishData,
			Hidden:               p.Hidden,
			CanUpdateOwnMetadata: &p.CanUpdateMetadata,
			CanSubscribeMetrics:  &p.CanSubscribeMetrics,
		},
	}, jobTokenTTL)
}

// serveWorker authenticates a connection, runs the registration handshake, and serves the worker until it goes.
func (b *Broker) serveWorker(rw http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		rw.WriteHeader(http.StatusNotFound)
		return
	}
	raw := r.FormValue("access_token")
	if raw == "" {
		raw = auth.TokenFromHeader(r.Header.Get("Authorization"))
	}
	verified, err := auth.Verify(raw, b.keys)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusUnauthorized)
		return
	}
	if !verified.Grants.Video.Agent {
		http.Error(rw, "permissions denied", http.StatusUnauthorized)
		return
	}
	ws, err := b.upgrader.Upgrade(rw, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(maxWorkerMessageBytes)
	conn := &workerConn{ws: ws}
	defer conn.close()

	req := b.handshake(conn)
	if req == nil {
		return
	}
	w := &Worker{
		id: participant.NewID(workerIDPrefix),
		key: workerKey{
			project:    verified.Key.ProjectID,
			agentName:  req.GetAgentName(),
			namespace:  req.GetNamespace(),
			jobType:    req.GetType(),
			deployment: req.GetDeployment(),
		},
		perms:  req.GetAllowedPermissions(),
		token:  verified.Key,
		conn:   conn,
		offers: map[string]chan *pb.AvailabilityResponse{},
		closed: make(chan struct{}),
	}
	if w.perms == nil {
		w.perms = &pb.ParticipantPermission{CanSubscribe: true, CanPublish: true, CanPublishData: true, CanUpdateMetadata: true}
	}
	if err := w.send(&pb.ServerMessage{Message: &pb.ServerMessage_Register{Register: &pb.RegisterWorkerResponse{
		WorkerId:   w.id,
		ServerInfo: b.rooms.ServerInfo(),
	}}}); err != nil {
		return
	}
	b.log.Info("agent worker registered", "worker", w.id, "project", w.key.project, "agentName", w.key.agentName,
		"jobType", w.key.jobType.String(), "version", req.GetVersion())
	b.addWorker(w)
	defer func() {
		b.removeWorker(w)
		w.close()
		b.log.Info("agent worker disconnected", "worker", w.id)
	}()

	stop := make(chan struct{})
	defer close(stop)
	go conn.keepAlive(stop)
	b.readLoop(w)
}

// handshake waits for the registration. A ping may come first. Anything else closes the connection, as does a
// registration the server cannot honour.
func (b *Broker) handshake(c *workerConn) *pb.RegisterWorkerRequest {
	deadline := time.Now().Add(b.registerTimeout)
	for {
		_ = c.ws.SetReadDeadline(deadline)
		msg, err := c.read()
		if err != nil {
			b.log.Debug("worker gave up before registering", "error", err)
			return nil
		}
		switch m := msg.GetMessage().(type) {
		case *pb.WorkerMessage_Ping:
			if err := c.pong(m.Ping); err != nil {
				return nil
			}
		case *pb.WorkerMessage_Register:
			if err := validateRegistration(m.Register); err != nil {
				b.log.Info("refusing worker registration", "error", err)
				c.closeWith(websocket.ClosePolicyViolation, err.Error())
				return nil
			}
			return m.Register
		default:
			c.closeWith(websocket.ClosePolicyViolation, "the first message must be a registration")
			return nil
		}
	}
}

func validateRegistration(req *pb.RegisterWorkerRequest) error {
	if _, known := pb.JobType_name[int32(req.GetType())]; !known {
		return errors.New("unknown job type")
	}
	return validateDeployment(req.GetDeployment())
}

func (b *Broker) readLoop(w *Worker) {
	for {
		_ = w.conn.ws.SetReadDeadline(time.Now().Add(readTimeout))
		msg, err := w.conn.read()
		if err != nil {
			if errors.Is(err, errBadMessage) {
				b.log.Debug("bad worker message", "worker", w.id)
				continue
			}
			return
		}
		switch m := msg.GetMessage().(type) {
		case *pb.WorkerMessage_Availability:
			w.handleAvailability(m.Availability)
		case *pb.WorkerMessage_UpdateWorker:
			w.handleUpdate(m.UpdateWorker)
		case *pb.WorkerMessage_UpdateJob:
			b.updateJob(w, m.UpdateJob)
		case *pb.WorkerMessage_Ping:
			if err := w.conn.pong(m.Ping); err != nil {
				return
			}
		case *pb.WorkerMessage_SimulateJob:
			b.simulateJob(w, m.SimulateJob)
		case *pb.WorkerMessage_MigrateJob:
			b.migrateJobs(w, m.MigrateJob.GetJobIds())
		}
	}
}

func (w *Worker) handleAvailability(res *pb.AvailabilityResponse) {
	w.mu.Lock()
	answer := w.offers[res.GetJobId()]
	w.mu.Unlock()
	if answer == nil {
		return
	}
	select {
	case answer <- res:
	default:
	}
}

func (w *Worker) handleUpdate(u *pb.UpdateWorkerStatus) {
	w.mu.Lock()
	defer w.mu.Unlock()
	// Status reports can arrive out of order, and a late one must not undo a newer one.
	if seq := u.GetSeq(); seq != 0 {
		if seq <= w.seq {
			return
		}
		w.seq = seq
	}
	if u.Status != nil {
		w.status = u.GetStatus()
	}
	w.load = u.GetLoad()
	w.draining = u.GetDraining()
}

// simulateJob offers the worker a job it asked for, for testing an agent without a real dispatch.
func (b *Broker) simulateJob(w *Worker, s *pb.SimulateJobRequest) {
	jobType := pb.JobType_JT_ROOM
	if s.GetParticipant() != nil {
		jobType = pb.JobType_JT_PUBLISHER
	}
	job := &pb.Job{
		Id:          participant.NewID(jobIDPrefix),
		Type:        jobType,
		Room:        s.GetRoom(),
		Participant: s.GetParticipant(),
		Namespace:   w.key.namespace,
		AgentName:   w.key.agentName,
		State:       &pb.JobState{},
	}
	go func() {
		if err := b.assign(w, job, nil, nil); err != nil {
			b.log.Info("could not simulate job", "worker", w.id, "error", err)
		}
	}()
}

var errBadMessage = errors.New("bad worker message")

// workerConn sends and receives worker protocol messages. Workers send binary protobuf. A worker that sends a text
// frame gets JSON back from then on, as with the signalling protocol.
type workerConn struct {
	ws      *websocket.Conn
	mu      sync.Mutex
	useJSON bool
}

func (c *workerConn) read() (*pb.WorkerMessage, error) {
	kind, data, err := c.ws.ReadMessage()
	if err != nil {
		return nil, err
	}
	msg := &pb.WorkerMessage{}
	switch kind {
	case websocket.BinaryMessage:
		c.mu.Lock()
		c.useJSON = false
		c.mu.Unlock()
		err = proto.Unmarshal(data, msg)
	case websocket.TextMessage:
		c.mu.Lock()
		c.useJSON = true
		c.mu.Unlock()
		err = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(data, msg)
	default:
		return nil, errBadMessage
	}
	if err != nil {
		return nil, errBadMessage
	}
	return msg, nil
}

func (c *workerConn) send(msg *pb.ServerMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var (
		data []byte
		err  error
		kind = websocket.BinaryMessage
	)
	if c.useJSON {
		kind = websocket.TextMessage
		data, err = protojson.Marshal(msg)
	} else {
		data, err = proto.Marshal(msg)
	}
	if err != nil {
		return err
	}
	_ = c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
	return c.ws.WriteMessage(kind, data)
}

func (c *workerConn) pong(ping *pb.WorkerPing) error {
	return c.send(&pb.ServerMessage{Message: &pb.ServerMessage_Pong{Pong: &pb.WorkerPong{
		LastTimestamp: ping.GetTimestamp(),
		Timestamp:     time.Now().UnixMilli(),
	}}})
}

func (c *workerConn) keepAlive(stop <-chan struct{}) {
	t := time.NewTicker(keepAliveInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout)); err != nil {
				return
			}
		}
	}
}

func (c *workerConn) closeWith(code int, reason string) {
	_ = c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
}

func (c *workerConn) close() { _ = c.ws.Close() }
