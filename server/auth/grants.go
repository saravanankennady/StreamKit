// Package auth implements access tokens: HS256 JWTs signed with a project's API secret.
//
// The claim layout matches the LiveKit access token format, so tokens minted by LiveKit server SDKs
// work here unchanged.
package auth

import (
	"strings"

	"elnova-streamkit.local/pkg/pb"
)

// VideoGrant carries a token's room permissions.
type VideoGrant struct {
	RoomCreate bool `json:"roomCreate,omitempty"`
	RoomList   bool `json:"roomList,omitempty"`
	RoomRecord bool `json:"roomRecord,omitempty"`

	RoomAdmin bool   `json:"roomAdmin,omitempty"`
	RoomJoin  bool   `json:"roomJoin,omitempty"`
	Room      string `json:"room,omitempty"`

	// Publish, subscribe and data permissions default to true when absent.
	CanPublish     *bool `json:"canPublish,omitempty"`
	CanSubscribe   *bool `json:"canSubscribe,omitempty"`
	CanPublishData *bool `json:"canPublishData,omitempty"`

	CanPublishSources    []string `json:"canPublishSources,omitempty"`
	CanUpdateOwnMetadata *bool    `json:"canUpdateOwnMetadata,omitempty"`

	IngressAdmin bool `json:"ingressAdmin,omitempty"`

	Hidden   bool `json:"hidden,omitempty"`
	Recorder bool `json:"recorder,omitempty"`
	Agent    bool `json:"agent,omitempty"`

	CanSubscribeMetrics *bool `json:"canSubscribeMetrics,omitempty"`
}

// SIPGrant carries a token's telephony permissions.
type SIPGrant struct {
	// Admin manages trunks and dispatch rules.
	Admin bool `json:"admin,omitempty"`
	// Call makes and transfers calls.
	Call bool `json:"call,omitempty"`
}

// Grants is the verified payload of an access token.
type Grants struct {
	Identity   string
	Name       string            `json:"name,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	Video      *VideoGrant       `json:"video,omitempty"`
	SIP        *SIPGrant         `json:"sip,omitempty"`
	Metadata   string            `json:"metadata,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
	Sha256     string            `json:"sha256,omitempty"`
	// RoomConfig applies if this token's join creates the room, and is how a token asks for agents.
	RoomConfig *pb.RoomConfiguration `json:"-"`
}

func boolOr(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

// GetCanPublish reports whether the token may publish media.
func (v *VideoGrant) GetCanPublish() bool { return boolOr(v.CanPublish, true) }

// GetCanSubscribe reports whether the token may subscribe to media.
func (v *VideoGrant) GetCanSubscribe() bool { return boolOr(v.CanSubscribe, true) }

// GetCanPublishData reports whether the token may send data packets. Unset follows CanPublish.
func (v *VideoGrant) GetCanPublishData() bool { return boolOr(v.CanPublishData, v.GetCanPublish()) }

// GetCanUpdateOwnMetadata reports whether the participant may change its own metadata.
func (v *VideoGrant) GetCanUpdateOwnMetadata() bool { return boolOr(v.CanUpdateOwnMetadata, false) }

// CanPublishSource reports whether a track source, named as in the proto enum in lowercase
// ("camera", "microphone", "screen_share", "screen_share_audio"), may be published.
func (v *VideoGrant) CanPublishSource(source string) bool {
	if !v.GetCanPublish() {
		return false
	}
	if len(v.CanPublishSources) == 0 {
		return true
	}
	for _, s := range v.CanPublishSources {
		if strings.EqualFold(s, source) {
			return true
		}
	}
	return false
}
