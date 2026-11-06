package streamkit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Egress types.
const (
	EgressRoomComposite = "room_composite"
	EgressParticipant   = "participant"
	EgressAudioOnly     = "audio_only"
	EgressVideoOnly     = "video_only"
)

// Egress statuses.
const (
	EgressStarting = "starting"
	EgressActive   = "active"
	EgressEnding   = "ending"
	EgressComplete = "complete"
	EgressFailed   = "failed"
	EgressAborted  = "aborted"
)

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
}

// Ended reports whether the job has finished, one way or another.
func (e *Egress) Ended() bool {
	return e.Status == EgressComplete || e.Status == EgressFailed || e.Status == EgressAborted
}

// EgressOutput says where a job sends its media. Give a Format to record a file, StreamURLs to stream, or both.
type EgressOutput struct {
	// Format of the file: mp4 or webm.
	Format string
	// StreamURLs are rtmp:// or rtmps:// addresses, at most five.
	StreamURLs []string
	// Layout of a composite: grid, the default, or speaker.
	Layout string
}

type startEgressBody struct {
	Type        string            `json:"type"`
	Room        string            `json:"room"`
	Participant string            `json:"participant,omitempty"`
	Layout      string            `json:"layout,omitempty"`
	Output      *egressFileOutput `json:"output,omitempty"`
	StreamURLs  []string          `json:"stream_urls,omitempty"`
}

type egressFileOutput struct {
	Kind   string `json:"kind"`
	Format string `json:"format,omitempty"`
}

// EgressPage is one page of jobs.
type EgressPage struct {
	Items []*Egress `json:"items"`
	// NextCursor fetches the next page. Empty means this is the last.
	NextCursor string `json:"next_cursor"`
}

// ListOptions pages through a list.
type ListOptions struct {
	Limit  int
	Cursor string
}

// EgressClient starts and manages recordings and stream outputs, over this server's REST API. The server needs a control
// plane and an egress worker.
type EgressClient struct{ *base }

// NewEgressClient creates a client.
func NewEgressClient(serverURL, apiKey, apiSecret string, opts ...Option) *EgressClient {
	return &EgressClient{newBase(serverURL, apiKey, apiSecret, opts)}
}

var recordGrant = &VideoGrant{RoomRecord: true}

func (c *EgressClient) start(ctx context.Context, kind, room, participant string, o EgressOutput) (*Egress, error) {
	body := startEgressBody{Type: kind, Room: room, Participant: participant, Layout: o.Layout, StreamURLs: o.StreamURLs}
	if o.Format != "" {
		body.Output = &egressFileOutput{Kind: "file", Format: o.Format}
	}
	out := &Egress{}
	if err := c.rest(ctx, http.MethodPost, "/egress", recordGrant, body, out); err != nil {
		return nil, err
	}
	return out, nil
}

// StartRoomCompositeEgress records or streams a whole room as one picture.
func (c *EgressClient) StartRoomCompositeEgress(ctx context.Context, room string, o EgressOutput) (*Egress, error) {
	return c.start(ctx, EgressRoomComposite, room, "", o)
}

// StartParticipantEgress records or streams one participant.
func (c *EgressClient) StartParticipantEgress(ctx context.Context, room, identity string, o EgressOutput) (*Egress, error) {
	return c.start(ctx, EgressParticipant, room, identity, o)
}

// StartAudioOnlyEgress records or streams a room's audio, or one participant's when identity is set.
func (c *EgressClient) StartAudioOnlyEgress(ctx context.Context, room, identity string, o EgressOutput) (*Egress, error) {
	return c.start(ctx, EgressAudioOnly, room, identity, o)
}

// StartVideoOnlyEgress records or streams a room's video, or one participant's when identity is set.
func (c *EgressClient) StartVideoOnlyEgress(ctx context.Context, room, identity string, o EgressOutput) (*Egress, error) {
	return c.start(ctx, EgressVideoOnly, room, identity, o)
}

func notSupported(what string) error {
	return fmt.Errorf("streamkit: %s: this server does not do this: %w", what, ErrNotSupported)
}

// StartWebEgress is not supported: this server only records rooms, not arbitrary pages.
func (c *EgressClient) StartWebEgress(context.Context) (*Egress, error) {
	return nil, notSupported("web egress")
}

// StartTrackCompositeEgress is not supported. Use StartAudioOnlyEgress or StartVideoOnlyEgress for one kind of media.
func (c *EgressClient) StartTrackCompositeEgress(context.Context) (*Egress, error) {
	return nil, notSupported("track composite egress")
}

// StartTrackEgress is not supported.
func (c *EgressClient) StartTrackEgress(context.Context) (*Egress, error) {
	return nil, notSupported("track egress")
}

// UpdateLayout is not supported: a running job keeps the layout it started with.
func (c *EgressClient) UpdateLayout(context.Context, string, string) (*Egress, error) {
	return nil, notSupported("changing a running job's layout")
}

// UpdateStream is not supported: a running job keeps the stream outputs it started with.
func (c *EgressClient) UpdateStream(context.Context, string, []string, []string) (*Egress, error) {
	return nil, notSupported("changing a running job's stream outputs")
}

// ListEgress lists jobs, newest first.
func (c *EgressClient) ListEgress(ctx context.Context, o ListOptions) (*EgressPage, error) {
	out := &EgressPage{}
	if err := c.rest(ctx, http.MethodGet, "/egress"+pageQuery(o.Limit, o.Cursor), recordGrant, nil, out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetEgress returns one job.
func (c *EgressClient) GetEgress(ctx context.Context, id string) (*Egress, error) {
	out := &Egress{}
	if err := c.rest(ctx, http.MethodGet, "/egress/"+url.PathEscape(id), recordGrant, nil, out); err != nil {
		return nil, err
	}
	return out, nil
}

// StopEgress stops a running job. The file, if any, is complete once the job's status says so.
func (c *EgressClient) StopEgress(ctx context.Context, id string) (*Egress, error) {
	out := &Egress{}
	if err := c.rest(ctx, http.MethodPost, "/egress/"+url.PathEscape(id)+"/stop", recordGrant, nil, out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListRecordings lists jobs with a file output.
func (c *EgressClient) ListRecordings(ctx context.Context, o ListOptions) (*EgressPage, error) {
	out := &EgressPage{}
	if err := c.rest(ctx, http.MethodGet, "/recordings"+pageQuery(o.Limit, o.Cursor), recordGrant, nil, out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetRecording returns one job with a file output.
func (c *EgressClient) GetRecording(ctx context.Context, id string) (*Egress, error) {
	out := &Egress{}
	if err := c.rest(ctx, http.MethodGet, "/recordings/"+url.PathEscape(id), recordGrant, nil, out); err != nil {
		return nil, err
	}
	return out, nil
}

// DownloadRecording writes a recording's file to w and returns how many bytes it wrote. A recording can take far longer
// to fetch than an API call should, so the client's timeout does not apply: use ctx to limit it.
func (c *EgressClient) DownloadRecording(ctx context.Context, id string, w io.Writer) (int64, error) {
	unlimited := *c.http
	unlimited.Timeout = 0
	download := *c.base
	download.http = &unlimited
	res, err := download.restRequest(ctx, http.MethodGet, "/recordings/"+url.PathEscape(id)+"/file", recordGrant, nil)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	return io.Copy(w, res.Body)
}

// RetryRecording starts a new job with the same request as a finished one.
func (c *EgressClient) RetryRecording(ctx context.Context, id string) (*Egress, error) {
	out := &Egress{}
	if err := c.rest(ctx, http.MethodPost, "/recordings/"+url.PathEscape(id)+"/retry", recordGrant, nil, out); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteRecording deletes a finished job and its file.
func (c *EgressClient) DeleteRecording(ctx context.Context, id string) error {
	return c.rest(ctx, http.MethodDelete, "/recordings/"+url.PathEscape(id), recordGrant, nil, nil)
}

// WaitForEgress polls a job until check accepts it, or ctx ends.
func (c *EgressClient) WaitForEgress(ctx context.Context, id string, check func(*Egress) bool) (*Egress, error) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		e, err := c.GetEgress(ctx, id)
		if err != nil {
			return nil, err
		}
		if check(e) {
			return e, nil
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), fmt.Errorf("streamkit: egress %s was %s when the wait ended (%s)", id, e.Status, strings.TrimSpace(e.Error)))
		case <-t.C:
		}
	}
}
