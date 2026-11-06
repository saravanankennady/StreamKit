package streamkit

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"

	"google.golang.org/protobuf/encoding/protojson"

	"elnova-streamkit.local/pkg/pb"
)

// maxWebhookBytes bounds what a receiver reads before it has checked the signature.
const maxWebhookBytes = 8 << 20

// Errors from checking a webhook.
var (
	ErrWebhookNoAuthorization = errors.New("streamkit: webhook has no Authorization header")
	ErrWebhookBadChecksum     = errors.New("streamkit: webhook body does not match its signature")
)

// WebhookReceiver checks that webhooks really came from a server holding one of these keys.
type WebhookReceiver struct {
	keys KeyProvider
}

// NewWebhookReceiver creates a receiver.
func NewWebhookReceiver(keys KeyProvider) *WebhookReceiver { return &WebhookReceiver{keys: keys} }

// ReceiveRequest reads and checks a webhook from an HTTP request. It closes the body.
func (w *WebhookReceiver) ReceiveRequest(r *http.Request) (*pb.WebhookEvent, error) {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxWebhookBytes {
		return nil, fmt.Errorf("streamkit: webhook body is over %d bytes", maxWebhookBytes)
	}
	return w.Receive(body, r.Header.Get("Authorization"))
}

// Receive checks a webhook's body against the value of its Authorization header, which is a token with no Bearer prefix.
// The token must be valid and carry the SHA-256 of these exact bytes. Fields this SDK does not know are ignored.
func (w *WebhookReceiver) Receive(body []byte, authorization string) (*pb.WebhookEvent, error) {
	if authorization == "" {
		return nil, ErrWebhookNoAuthorization
	}
	claims, err := VerifyToken(authorization, w.keys)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	want := base64.StdEncoding.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(claims.Sha256), []byte(want)) != 1 {
		return nil, ErrWebhookBadChecksum
	}
	event := &pb.WebhookEvent{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(body, event); err != nil {
		return nil, fmt.Errorf("streamkit: webhook body could not be read: %w", err)
	}
	return event, nil
}

// SignWebhook returns the Authorization header value for a webhook with this body. Servers do this themselves. It is
// here for testing a receiver.
func SignWebhook(body []byte, apiKey, apiSecret string) (string, error) {
	sum := sha256.Sum256(body)
	return NewAccessToken(apiKey, apiSecret).
		SetSha256(base64.StdEncoding.EncodeToString(sum[:])).
		SetValidFor(webhookTokenTTL).
		ToJWT()
}
