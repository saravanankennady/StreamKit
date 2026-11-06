package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/protobuf/encoding/protojson"

	"elnova-streamkit.local/pkg/pb"
)

// Leeway tolerates clock skew between the token minter and this server.
const Leeway = time.Minute

// DefaultTokenTTL applies when a caller does not choose a lifetime.
const DefaultTokenTTL = 6 * time.Hour

// Error values returned by Verify. Their text is sent to clients, which display it.
var (
	ErrInvalidToken  = errors.New("invalid authorization token")
	ErrInvalidAPIKey = errors.New("invalid API key")
)

type claims struct {
	jwt.RegisteredClaims
	Name       string            `json:"name,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	Video      *VideoGrant       `json:"video,omitempty"`
	SIP        *SIPGrant         `json:"sip,omitempty"`
	Metadata   string            `json:"metadata,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
	Sha256     string            `json:"sha256,omitempty"`
	RoomConfig json.RawMessage   `json:"roomConfig,omitempty"`
}

// Key is a resolved API credential.
type Key struct {
	APIKey    string
	Secret    string
	ProjectID string
}

// KeyProvider resolves an API key to its secret and owning project.
type KeyProvider interface {
	Lookup(apiKey string) (Key, bool)
}

// StaticProject is the project keys from the config file belong to. It has no row in the database.
const StaticProject = "default"

// StaticKeys is a KeyProvider over a fixed set of keys, all in one project.
type StaticKeys struct {
	ProjectID string
	Keys      map[string]string
}

// Lookup implements KeyProvider.
func (s StaticKeys) Lookup(apiKey string) (Key, bool) {
	secret, ok := s.Keys[apiKey]
	if !ok {
		return Key{}, false
	}
	return Key{APIKey: apiKey, Secret: secret, ProjectID: s.ProjectID}, true
}

// Verified is the outcome of a successful Verify.
type Verified struct {
	Key       Key
	Grants    *Grants
	ExpiresAt time.Time
}

// Verify checks a token's signature, issuer and validity window and returns its grants.
func Verify(raw string, keys KeyProvider) (*Verified, error) {
	if raw == "" {
		return nil, ErrInvalidToken
	}
	var parsed claims
	// Decode unverified only to learn which key to verify against.
	if _, _, err := jwt.NewParser().ParseUnverified(raw, &parsed); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidToken, err)
	}
	key, ok := keys.Lookup(parsed.Issuer)
	if !ok {
		return nil, ErrInvalidAPIKey
	}

	var c claims
	_, err := jwt.ParseWithClaims(raw, &c, func(*jwt.Token) (any, error) {
		return []byte(key.Secret), nil
	},
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithIssuer(key.APIKey),
		jwt.WithLeeway(Leeway),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidToken, err)
	}

	identity := c.Subject
	if identity == "" {
		identity = c.ID
	}
	v := &Verified{
		Key: key,
		Grants: &Grants{
			Identity:   identity,
			Name:       c.Name,
			Kind:       c.Kind,
			Video:      c.Video,
			SIP:        c.SIP,
			Metadata:   c.Metadata,
			Attributes: c.Attributes,
			Sha256:     c.Sha256,
		},
	}
	if c.ExpiresAt != nil {
		v.ExpiresAt = c.ExpiresAt.Time
	}
	if v.Grants.Video == nil {
		v.Grants.Video = &VideoGrant{}
	}
	if len(c.RoomConfig) > 0 {
		v.Grants.RoomConfig = &pb.RoomConfiguration{}
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(c.RoomConfig, v.Grants.RoomConfig); err != nil {
			return nil, fmt.Errorf("%w: %s", ErrInvalidToken, err)
		}
	}
	return v, nil
}

// TokenFromHeader extracts a bearer token from an Authorization header value.
func TokenFromHeader(h string) string {
	const prefix = "Bearer "
	if strings.HasPrefix(h, prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

// Mint creates a signed token. Used for token refresh and by the control plane.
func Mint(key Key, g *Grants, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = DefaultTokenTTL
	}
	now := time.Now()
	var roomConfig json.RawMessage
	if g.RoomConfig != nil {
		data, err := protojson.Marshal(g.RoomConfig)
		if err != nil {
			return "", err
		}
		roomConfig = data
	}
	c := claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    key.APIKey,
			Subject:   g.Identity,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
		Name:       g.Name,
		Kind:       g.Kind,
		Video:      g.Video,
		SIP:        g.SIP,
		Metadata:   g.Metadata,
		Attributes: g.Attributes,
		Sha256:     g.Sha256,
		RoomConfig: roomConfig,
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(key.Secret))
}

// Clone deep-copies grants so a holder can change them without affecting others.
func (g *Grants) Clone() *Grants {
	data, err := json.Marshal(struct {
		Name       string            `json:"name,omitempty"`
		Kind       string            `json:"kind,omitempty"`
		Video      *VideoGrant       `json:"video,omitempty"`
		SIP        *SIPGrant         `json:"sip,omitempty"`
		Metadata   string            `json:"metadata,omitempty"`
		Attributes map[string]string `json:"attributes,omitempty"`
		Sha256     string            `json:"sha256,omitempty"`
	}{g.Name, g.Kind, g.Video, g.SIP, g.Metadata, g.Attributes, g.Sha256})
	if err != nil {
		panic(err) // the struct contains only marshalable fields
	}
	// A room configuration is only read, so copies can share it.
	out := &Grants{Identity: g.Identity, RoomConfig: g.RoomConfig}
	if err := json.Unmarshal(data, out); err != nil {
		panic(err)
	}
	return out
}
