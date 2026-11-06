package streamkit

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// Ingress input types.
const (
	IngressRTMP = "rtmp"
	IngressWHIP = "whip"
	IngressURL  = "url"
)

// Ingress statuses.
const (
	IngressInactive   = "inactive"
	IngressBuffering  = "buffering"
	IngressPublishing = "publishing"
	IngressError      = "error"
)

// IngressStats describes a running stream.
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
	ID                  string `json:"id"`
	ProjectID           string `json:"project_id"`
	Name                string `json:"name"`
	InputType           string `json:"input_type"`
	RoomName            string `json:"room_name"`
	ParticipantIdentity string `json:"participant_identity"`
	ParticipantName     string `json:"participant_name"`
	SourceURL           string `json:"source_url"`
	Enabled             bool   `json:"enabled"`
	// PublishURL is where a publisher sends its stream.
	PublishURL string `json:"publish_url"`
	// StreamKey is only in the responses to CreateIngress and RotateIngressKey. Keep it secret.
	StreamKey string       `json:"stream_key,omitempty"`
	Status    string       `json:"status"`
	Error     string       `json:"error"`
	Stats     IngressStats `json:"stats"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
}

// CreateIngressRequest describes a new ingress.
type CreateIngressRequest struct {
	Name string `json:"name,omitempty"`
	// InputType is rtmp, whip or url.
	InputType           string `json:"input_type"`
	Room                string `json:"room"`
	ParticipantIdentity string `json:"participant_identity"`
	ParticipantName     string `json:"participant_name,omitempty"`
	// SourceURL is what a url input pulls from: http, https, rtmp or srt.
	SourceURL string `json:"source_url,omitempty"`
}

// UpdateIngressRequest changes an ingress. Fields left nil stay as they are. Changing the room or identity, or disabling
// it, ends a running stream.
type UpdateIngressRequest struct {
	Name                *string `json:"name,omitempty"`
	Room                *string `json:"room,omitempty"`
	ParticipantIdentity *string `json:"participant_identity,omitempty"`
	ParticipantName     *string `json:"participant_name,omitempty"`
	Enabled             *bool   `json:"enabled,omitempty"`
}

// IngressEvent is one entry in an ingress's log.
type IngressEvent struct {
	Level     string    `json:"level"`
	Type      string    `json:"type"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

// IngressClient manages ingresses, over this server's REST API. The server needs a control plane and, for a stream to
// actually arrive, an ingress worker.
type IngressClient struct{ *base }

// NewIngressClient creates a client.
func NewIngressClient(serverURL, apiKey, apiSecret string, opts ...Option) *IngressClient {
	return &IngressClient{newBase(serverURL, apiKey, apiSecret, opts)}
}

var ingressGrant = &VideoGrant{IngressAdmin: true}

// CreateIngress creates an ingress. The response holds the stream key, which is never shown again.
func (c *IngressClient) CreateIngress(ctx context.Context, in *CreateIngressRequest) (*Ingress, error) {
	out := &Ingress{}
	if err := c.rest(ctx, http.MethodPost, "/ingress", ingressGrant, in, out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListIngress lists ingresses.
func (c *IngressClient) ListIngress(ctx context.Context) ([]*Ingress, error) {
	var out struct {
		Items []*Ingress `json:"items"`
	}
	if err := c.rest(ctx, http.MethodGet, "/ingress", ingressGrant, nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// GetIngress returns one ingress.
func (c *IngressClient) GetIngress(ctx context.Context, id string) (*Ingress, error) {
	out := &Ingress{}
	if err := c.rest(ctx, http.MethodGet, "/ingress/"+url.PathEscape(id), ingressGrant, nil, out); err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateIngress changes an ingress.
func (c *IngressClient) UpdateIngress(ctx context.Context, id string, in *UpdateIngressRequest) (*Ingress, error) {
	out := &Ingress{}
	if err := c.rest(ctx, http.MethodPatch, "/ingress/"+url.PathEscape(id), ingressGrant, in, out); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteIngress deletes an ingress and stops any running stream.
func (c *IngressClient) DeleteIngress(ctx context.Context, id string) error {
	return c.rest(ctx, http.MethodDelete, "/ingress/"+url.PathEscape(id), ingressGrant, nil, nil)
}

// StopIngress stops the running stream. The ingress can be used again.
func (c *IngressClient) StopIngress(ctx context.Context, id string) (*Ingress, error) {
	out := &Ingress{}
	if err := c.rest(ctx, http.MethodPost, "/ingress/"+url.PathEscape(id)+"/stop", ingressGrant, nil, out); err != nil {
		return nil, err
	}
	return out, nil
}

// RotateIngressKey replaces the stream key, and returns the new one once. The old one stops working and its stream ends.
func (c *IngressClient) RotateIngressKey(ctx context.Context, id string) (*Ingress, error) {
	out := &Ingress{}
	if err := c.rest(ctx, http.MethodPost, "/ingress/"+url.PathEscape(id)+"/rotate-key", ingressGrant, nil, out); err != nil {
		return nil, err
	}
	return out, nil
}

// IngressLogs returns an ingress's events, newest first.
func (c *IngressClient) IngressLogs(ctx context.Context, id string, limit int) ([]*IngressEvent, error) {
	var out struct {
		Items []*IngressEvent `json:"items"`
	}
	if err := c.rest(ctx, http.MethodGet, "/ingress/"+url.PathEscape(id)+"/logs"+pageQuery(limit, ""), ingressGrant, nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}
