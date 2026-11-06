package streamkit

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"elnova-streamkit.local/pkg/pb"
)

const (
	// DefaultTokenTTL is how long a token lasts when its creator does not say.
	DefaultTokenTTL = 6 * time.Hour
	// TokenLeeway tolerates clock differences between whoever signed a token and whoever checks it. Servers allow the
	// same.
	TokenLeeway = time.Minute
)

// Errors from creating and checking tokens.
var (
	ErrMissingCredentials = errors.New("streamkit: an API key and secret are required")
	ErrJoinNeedsIdentity  = errors.New("streamkit: a token that joins a room needs an identity and a room")
	ErrInvalidToken       = errors.New("streamkit: invalid token")
	ErrUnknownAPIKey      = errors.New("streamkit: unknown API key")
)

// AccessToken builds a signed token: an HS256 JWT with the API key as its issuer.
type AccessToken struct {
	apiKey, apiSecret string
	grant             ClaimGrants
	validFor          time.Duration
}

// NewAccessToken starts a token signed with this key and secret.
func NewAccessToken(apiKey, apiSecret string) *AccessToken {
	return &AccessToken{apiKey: apiKey, apiSecret: apiSecret}
}

// SetIdentity sets who holds the token. A token that joins a room needs one.
func (t *AccessToken) SetIdentity(identity string) *AccessToken {
	t.grant.Identity = identity
	return t
}

// SetValidFor sets how long the token lasts.
func (t *AccessToken) SetValidFor(d time.Duration) *AccessToken {
	t.validFor = d
	return t
}

// SetName sets the participant's display name.
func (t *AccessToken) SetName(name string) *AccessToken {
	t.grant.Name = name
	return t
}

// SetKind sets the kind of participant, such as an agent.
func (t *AccessToken) SetKind(kind pb.ParticipantInfo_Kind) *AccessToken {
	t.grant.Kind = strings.ToLower(kind.String())
	return t
}

// SetKindDetails adds detail to the kind of participant.
func (t *AccessToken) SetKindDetails(details ...pb.ParticipantInfo_KindDetail) *AccessToken {
	t.grant.KindDetails = nil
	for _, d := range details {
		t.grant.KindDetails = append(t.grant.KindDetails, strings.ToLower(d.String()))
	}
	return t
}

// SetVideoGrant sets the room permissions.
func (t *AccessToken) SetVideoGrant(g *VideoGrant) *AccessToken {
	t.grant.Video = g
	return t
}

// SetSIPGrant sets the SIP permissions.
func (t *AccessToken) SetSIPGrant(g *SIPGrant) *AccessToken {
	t.grant.SIP = g
	return t
}

// SetAgentGrant sets the hosted agent permissions.
func (t *AccessToken) SetAgentGrant(g *AgentGrant) *AccessToken {
	t.grant.Agent = g
	return t
}

// SetInferenceGrant sets the inference permissions.
func (t *AccessToken) SetInferenceGrant(g *InferenceGrant) *AccessToken {
	t.grant.Inference = g
	return t
}

// SetObservabilityGrant sets the observability permissions.
func (t *AccessToken) SetObservabilityGrant(g *ObservabilityGrant) *AccessToken {
	t.grant.Observability = g
	return t
}

// SetAgentEndpointGrant sets the agent endpoint permissions.
func (t *AccessToken) SetAgentEndpointGrant(g *AgentEndpointGrant) *AccessToken {
	t.grant.AgentEndpoint = g
	return t
}

// SetAgentThreadGrant sets the agent thread permissions.
func (t *AccessToken) SetAgentThreadGrant(g *AgentThreadGrant) *AccessToken {
	t.grant.AgentThread = g
	return t
}

// SetMetadata sets the participant's metadata.
func (t *AccessToken) SetMetadata(metadata string) *AccessToken {
	t.grant.Metadata = metadata
	return t
}

// SetAttributes sets the participant's attributes.
func (t *AccessToken) SetAttributes(attributes map[string]string) *AccessToken {
	t.grant.Attributes = attributes
	return t
}

// SetSha256 sets a body hash, as webhook signatures do.
func (t *AccessToken) SetSha256(sum string) *AccessToken {
	t.grant.Sha256 = sum
	return t
}

// SetRoomConfig sets the configuration a room gets if this token's join creates it.
func (t *AccessToken) SetRoomConfig(config *pb.RoomConfiguration) *AccessToken {
	t.grant.RoomConfig = config
	return t
}

// SetRoomPreset names a stored room configuration.
func (t *AccessToken) SetRoomPreset(preset string) *AccessToken {
	t.grant.RoomPreset = preset
	return t
}

// Grants returns the token's claims as they stand, for inspection.
func (t *AccessToken) Grants() *ClaimGrants { return &t.grant }

type signedClaims struct {
	jwt.RegisteredClaims
	claimsJSON
}

// ToJWT signs the token.
func (t *AccessToken) ToJWT() (string, error) {
	if t.apiKey == "" || t.apiSecret == "" {
		return "", ErrMissingCredentials
	}
	if v := t.grant.Video; v != nil && v.RoomJoin && (t.grant.Identity == "" || v.Room == "") {
		return "", ErrJoinNeedsIdentity
	}
	body, err := t.grant.toJSON()
	if err != nil {
		return "", err
	}
	validFor := t.validFor
	if validFor <= 0 {
		validFor = DefaultTokenTTL
	}
	now := time.Now()
	return jwt.NewWithClaims(jwt.SigningMethodHS256, signedClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    t.apiKey,
			Subject:   t.grant.Identity,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(validFor)),
		},
		claimsJSON: body,
	}).SignedString([]byte(t.apiSecret))
}

// KeyProvider finds the secret for an API key.
type KeyProvider interface {
	Secret(apiKey string) (string, bool)
}

// Keys is a KeyProvider over a fixed set of keys.
type Keys map[string]string

// Secret implements KeyProvider.
func (k Keys) Secret(apiKey string) (string, bool) {
	secret, ok := k[apiKey]
	return secret, ok
}

// Claims is a token that passed verification.
type Claims struct {
	ClaimGrants
	// APIKey is the key that signed it.
	APIKey    string
	ExpiresAt time.Time
	NotBefore time.Time
}

// VerifyToken checks a token's signature, issuer and validity window, the same checks a server makes, and returns its
// claims. It takes HS256 only, requires an expiry, and ignores claims it does not know.
func VerifyToken(raw string, keys KeyProvider) (*Claims, error) {
	if raw == "" {
		return nil, ErrInvalidToken
	}
	var unverified signedClaims
	// Read unverified only to learn which key to check against.
	if _, _, err := jwt.NewParser().ParseUnverified(raw, &unverified); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidToken, err)
	}
	secret, ok := keys.Secret(unverified.Issuer)
	if !ok {
		return nil, ErrUnknownAPIKey
	}
	var c signedClaims
	_, err := jwt.ParseWithClaims(raw, &c, func(*jwt.Token) (any, error) { return []byte(secret), nil },
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithIssuer(unverified.Issuer),
		jwt.WithLeeway(TokenLeeway),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidToken, err)
	}
	grants, err := c.toGrants()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidToken, err)
	}
	grants.Identity = c.Subject
	if grants.Identity == "" {
		grants.Identity = c.ID
	}
	out := &Claims{ClaimGrants: *grants, APIKey: c.Issuer, ExpiresAt: c.ExpiresAt.Time}
	if c.NotBefore != nil {
		out.NotBefore = c.NotBefore.Time
	}
	return out, nil
}
