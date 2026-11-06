// Package streamkit is the Go server SDK for Elnova StreamKit: access tokens, webhook checks, and clients for rooms,
// recording and ingest.
//
// The room client speaks the Twirp protocol that server SDKs of this wire protocol use, in protobuf, so it also works
// against the reference server. The egress and ingress clients use this server's REST API. See README.md for what is
// and is not supported.
package streamkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
)

const (
	// apiTokenTTL is how long the token signed for each API call lasts.
	apiTokenTTL     = 10 * time.Minute
	webhookTokenTTL = 5 * time.Minute
	defaultTimeout  = 30 * time.Second
	maxErrorBody    = 1 << 16
	userAgent       = "elnova-streamkit-server-go/" + Version
)

// Version is this SDK's version.
const Version = "0.1.0"

// ErrNotSupported is wrapped by errors for things this server does not do.
var ErrNotSupported = errors.New("not supported")

// Error is a failure the server reported.
type Error struct {
	// Code is the server's error code, such as not_found or unauthenticated.
	Code string
	// Message is the server's explanation.
	Message string
	// Status is the HTTP status.
	Status int
	// Meta holds extra Twirp detail, when the server gave any.
	Meta map[string]string
	// Fields names the fields that failed validation, on the REST API.
	Fields map[string]string
}

func (e *Error) Error() string {
	if len(e.Fields) > 0 {
		return fmt.Sprintf("streamkit: %s: %s %v (HTTP %d)", e.Code, e.Message, e.Fields, e.Status)
	}
	return fmt.Sprintf("streamkit: %s: %s (HTTP %d)", e.Code, e.Message, e.Status)
}

// Unwrap lets errors.Is(err, ErrNotSupported) find an unimplemented method.
func (e *Error) Unwrap() error {
	if e.Code == "unimplemented" {
		return ErrNotSupported
	}
	return nil
}

// ErrorCode returns the server's code for err, or "" if err did not come from a server.
func ErrorCode(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// Option configures a client.
type Option func(*base)

// WithHTTPClient uses this HTTP client.
func WithHTTPClient(c *http.Client) Option { return func(b *base) { b.http = c } }

// WithToken sends this token on every call, instead of signing one per call with only the grants that call needs. The
// caller is then responsible for its grants.
func WithToken(token string) Option { return func(b *base) { b.token = token } }

// base is what every client shares.
type base struct {
	url               string
	apiKey, apiSecret string
	token             string
	http              *http.Client
}

func newBase(serverURL, apiKey, apiSecret string, opts []Option) *base {
	b := &base{url: httpURL(serverURL), apiKey: apiKey, apiSecret: apiSecret, http: &http.Client{Timeout: defaultTimeout}}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// httpURL accepts the ws:// address clients connect to.
func httpURL(u string) string {
	u = strings.TrimRight(u, "/")
	switch {
	case strings.HasPrefix(u, "ws://"):
		return "http://" + u[len("ws://"):]
	case strings.HasPrefix(u, "wss://"):
		return "https://" + u[len("wss://"):]
	}
	return u
}

func (b *base) authorization(grant *VideoGrant) (string, error) {
	if b.token != "" {
		return "Bearer " + b.token, nil
	}
	if b.apiKey == "" || b.apiSecret == "" {
		return "", ErrMissingCredentials
	}
	token, err := NewAccessToken(b.apiKey, b.apiSecret).SetVideoGrant(grant).SetValidFor(apiTokenTTL).ToJWT()
	if err != nil {
		return "", err
	}
	return "Bearer " + token, nil
}

// serverError reads a failure. Twirp errors are {"code","msg"}, REST errors are {"error":{"code","message"}}, and
// authentication failures and proxies give plain text.
func serverError(res *http.Response) *Error {
	data, _ := io.ReadAll(io.LimitReader(res.Body, maxErrorBody))
	out := &Error{Status: res.StatusCode}
	var body struct {
		Code  string            `json:"code"`
		Msg   string            `json:"msg"`
		Meta  map[string]string `json:"meta"`
		Error *struct {
			Code    string            `json:"code"`
			Message string            `json:"message"`
			Fields  map[string]string `json:"fields"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &body) == nil {
		switch {
		case body.Error != nil:
			out.Code, out.Message, out.Fields = body.Error.Code, body.Error.Message, body.Error.Fields
		case body.Code != "":
			out.Code, out.Message, out.Meta = body.Code, body.Msg, body.Meta
		}
	}
	if out.Code == "" {
		out.Code = codeForStatus(res.StatusCode)
		out.Message = strings.TrimSpace(string(data))
	}
	return out
}

func codeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_argument"
	case http.StatusUnauthorized:
		return "unauthenticated"
	case http.StatusForbidden:
		return "permission_denied"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusTooManyRequests:
		return "resource_exhausted"
	case http.StatusNotImplemented:
		return "unimplemented"
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return "unavailable"
	}
	return "unknown"
}

// twirp calls one method. A route that does not exist means the server does not have this service, and the error says so.
func (b *base) twirp(ctx context.Context, service, method string, grant *VideoGrant, in, out proto.Message) error {
	body, err := proto.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.url+"/twirp/livekit."+service+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	auth, err := b.authorization(grant)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/protobuf")
	req.Header.Set("Accept", "application/protobuf")
	req.Header.Set("User-Agent", userAgent)
	res, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		e := serverError(res)
		if res.StatusCode == http.StatusNotFound && e.Code == "not_found" && !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
			return fmt.Errorf("streamkit: %s: this server has no %s service: %w", method, service, ErrNotSupported)
		}
		return e
	}
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	return proto.Unmarshal(data, out)
}

// rest calls the management API. A nil out discards the response.
func (b *base) rest(ctx context.Context, verb, path string, grant *VideoGrant, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	res, err := b.restRequest(ctx, verb, path, grant, body)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, res.Body)
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// restRequest sends a request and returns a successful response, whose body the caller must close.
func (b *base) restRequest(ctx context.Context, verb, path string, grant *VideoGrant, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, verb, b.url+"/api/v1"+path, body)
	if err != nil {
		return nil, err
	}
	auth, err := b.authorization(grant)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	res, err := b.http.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 {
		defer res.Body.Close()
		e := serverError(res)
		if res.StatusCode == http.StatusNotFound && !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
			return nil, fmt.Errorf("streamkit: %s has no management API at /api/v1. It needs an Elnova StreamKit server with a control plane (control_plane.database_url): %w", b.url, ErrNotSupported)
		}
		return nil, e
	}
	return res, nil
}

func pageQuery(limit int, cursor string) string {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

// Client bundles every service client for one server.
type Client struct {
	Room          *RoomClient
	Egress        *EgressClient
	Ingress       *IngressClient
	AgentDispatch *AgentDispatchClient
}

// NewClient creates clients for all the services of one server. serverURL may be the ws:// address clients connect to.
func NewClient(serverURL, apiKey, apiSecret string, opts ...Option) *Client {
	return &Client{
		Room:          NewRoomClient(serverURL, apiKey, apiSecret, opts...),
		Egress:        NewEgressClient(serverURL, apiKey, apiSecret, opts...),
		Ingress:       NewIngressClient(serverURL, apiKey, apiSecret, opts...),
		AgentDispatch: NewAgentDispatchClient(serverURL, apiKey, apiSecret, opts...),
	}
}

// NewClientFromEnv reads STREAMKIT_URL, STREAMKIT_API_KEY and STREAMKIT_API_SECRET.
func NewClientFromEnv(opts ...Option) (*Client, error) {
	serverURL := os.Getenv("STREAMKIT_URL")
	if serverURL == "" {
		return nil, errors.New("streamkit: STREAMKIT_URL is not set")
	}
	return NewClient(serverURL, os.Getenv("STREAMKIT_API_KEY"), os.Getenv("STREAMKIT_API_SECRET"), opts...), nil
}
