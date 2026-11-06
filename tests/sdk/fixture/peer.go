package fixture

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Event is one line of `streamkit room join --json`: something a real participant saw.
type Event struct {
	Event      string            `json:"event"`
	Room       string            `json:"room"`
	Metadata   *string           `json:"metadata"`
	Identity   string            `json:"identity"`
	SID        string            `json:"sid"`
	Name       string            `json:"name"`
	Attributes map[string]string `json:"attributes"`
	CanPublish *bool             `json:"can_publish"`
	Kind       string            `json:"kind"`
	Source     string            `json:"source"`
	Muted      *bool             `json:"muted"`
	Packets    int64             `json:"packets"`
	Bytes      int64             `json:"bytes"`
	Topic      string            `json:"topic"`
	Channel    string            `json:"channel"`
	From       string            `json:"from"`
	Data       string            `json:"data_base64"`
	Reason     string            `json:"reason"`
	Error      string            `json:"error"`
	Tracks     []string          `json:"tracks"`
}

// Peer is a real participant, run as the streamkit CLI: a real WebRTC session with real media. It is a separate process
// because that is also how the Node and Python tests get one.
type Peer struct {
	Identity string

	cmd    *exec.Cmd
	mu     sync.Mutex
	cond   *sync.Cond
	events []Event
	closed bool
	stderr *lockedBuffer
}

// PeerOptions says how a peer joins.
type PeerOptions struct {
	Identity string
	// Publish sends the CLI's test pattern.
	Publish bool
	// NoAutoSubscribe stays unsubscribed until told otherwise.
	NoAutoSubscribe bool
	// Token joins with this token rather than one the CLI signs.
	Token string
	// Extra is more arguments for the CLI.
	Extra []string
}

// Join starts a peer. Stop it with Close.
func (f *Fixture) Join(ctx context.Context, s *Server, room string, o PeerOptions) (*Peer, error) {
	args := []string{"room", "join", room, "--json", "--url", s.URL, "--identity", o.Identity}
	if o.Token != "" {
		args = append(args, "--token", o.Token)
	} else {
		args = append(args, "--api-key", s.APIKey, "--api-secret", s.APISecret)
	}
	if o.Publish {
		args = append(args, "--publish-test-pattern")
	}
	if o.NoAutoSubscribe {
		args = append(args, "--no-auto-subscribe")
	}
	args = append(args, o.Extra...)
	cmd := exec.CommandContext(ctx, f.Info.CLI, args...)
	cmd.Dir = f.dir
	cmd.Env = append(cmd.Environ(), "STREAMKIT_CONFIG_DIR="+f.dir+"/peer-config")
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	p := &Peer{Identity: o.Identity, cmd: cmd, stderr: &lockedBuffer{}}
	cmd.Stderr = p.stderr
	p.cond = sync.NewCond(&p.mu)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go p.read(stdout)
	return p, nil
}

func (p *Peer) read(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		var e Event
		if json.Unmarshal(scanner.Bytes(), &e) != nil {
			continue
		}
		p.mu.Lock()
		p.events = append(p.events, e)
		p.cond.Broadcast()
		p.mu.Unlock()
	}
	p.mu.Lock()
	p.closed = true
	p.cond.Broadcast()
	p.mu.Unlock()
}

// Wait returns the first event since the start of the session that check accepts. It fails if none comes in time.
func (p *Peer) Wait(timeout time.Duration, what string, check func(Event) bool) (Event, error) {
	timer := time.AfterFunc(timeout, func() {
		p.mu.Lock()
		p.cond.Broadcast()
		p.mu.Unlock()
	})
	defer timer.Stop()
	deadline := time.Now().Add(timeout)
	p.mu.Lock()
	defer p.mu.Unlock()
	next := 0
	for {
		for ; next < len(p.events); next++ {
			if check(p.events[next]) {
				return p.events[next], nil
			}
		}
		if p.closed {
			return Event{}, fmt.Errorf("%s left without %s. Saw %v. Errors: %s", p.Identity, what, summarize(p.events), p.stderr.String())
		}
		if time.Now().After(deadline) {
			return Event{}, fmt.Errorf("%s: timed out waiting for %s. Saw %v. Errors: %s", p.Identity, what, summarize(p.events), p.stderr.String())
		}
		p.cond.Wait()
	}
}

// Events returns everything seen so far.
func (p *Peer) Events() []Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Event(nil), p.events...)
}

func summarize(events []Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		if e.Event != "media" {
			out = append(out, e.Event)
		}
	}
	return out
}

// WaitConnected waits until the peer has joined and its subscriber connection is up.
func (p *Peer) WaitConnected() error {
	_, err := p.Wait(30*time.Second, "a connection", func(e Event) bool { return e.Event == "connected" })
	return err
}

// WaitPublished waits until the server has accepted n tracks, and returns them.
func (p *Peer) WaitPublished(n int) ([]Event, error) {
	var out []Event
	for len(out) < n {
		seen := len(out)
		count := 0
		e, err := p.Wait(30*time.Second, fmt.Sprintf("%d published tracks", n), func(e Event) bool {
			if e.Event != "track_published" {
				return false
			}
			count++
			return count > seen
		})
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// WaitDataChannels waits until the peer can receive data on both channels.
func (p *Peer) WaitDataChannels() error {
	for _, label := range []string{"_reliable", "_lossy"} {
		if _, err := p.Wait(30*time.Second, "data channel "+label, func(e Event) bool {
			return e.Event == "data_channel_open" && e.Channel == label
		}); err != nil {
			return err
		}
	}
	return nil
}

// Close asks the peer to leave and waits for it.
func (p *Peer) Close() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
	}
	done := make(chan struct{})
	go func() {
		// Wait closes the pipe, so the reader goes first to not lose the last lines.
		p.mu.Lock()
		for !p.closed {
			p.cond.Wait()
		}
		p.mu.Unlock()
		_ = p.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = p.cmd.Process.Kill()
		<-done
	}
}

type lockedBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}
