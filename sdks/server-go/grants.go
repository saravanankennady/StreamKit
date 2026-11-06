package streamkit

import (
	"encoding/json"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"

	"elnova-streamkit.local/pkg/pb"
)

// Bool returns a pointer to v, for the grant fields where unset and false mean different things.
func Bool(v bool) *bool { return &v }

// VideoGrant carries a token's room permissions.
type VideoGrant struct {
	RoomCreate bool `json:"roomCreate,omitempty"`
	RoomList   bool `json:"roomList,omitempty"`
	RoomRecord bool `json:"roomRecord,omitempty"`

	RoomAdmin bool   `json:"roomAdmin,omitempty"`
	RoomJoin  bool   `json:"roomJoin,omitempty"`
	Room      string `json:"room,omitempty"`

	// CanPublish and CanSubscribe are granted when unset.
	CanPublish   *bool `json:"canPublish,omitempty"`
	CanSubscribe *bool `json:"canSubscribe,omitempty"`
	// CanPublishData follows CanPublish when unset.
	CanPublishData *bool `json:"canPublishData,omitempty"`
	// CanPublishSources limits publishing to these sources: camera, microphone, screen_share, screen_share_audio.
	// Empty allows all of them. It cannot allow anything CanPublish denies.
	CanPublishSources []string `json:"canPublishSources,omitempty"`
	// CanUpdateOwnMetadata is denied when unset.
	CanUpdateOwnMetadata *bool `json:"canUpdateOwnMetadata,omitempty"`

	IngressAdmin bool `json:"ingressAdmin,omitempty"`

	Hidden   bool `json:"hidden,omitempty"`
	Recorder bool `json:"recorder,omitempty"`
	Agent    bool `json:"agent,omitempty"`

	CanSubscribeMetrics   *bool `json:"canSubscribeMetrics,omitempty"`
	CanManageAgentSession *bool `json:"canManageAgentSession,omitempty"`

	// DestinationRoom is the room this token may forward or move a participant to.
	DestinationRoom string `json:"destinationRoom,omitempty"`
}

// SetCanPublishSources names the sources this token may publish, using the protocol's enum.
func (v *VideoGrant) SetCanPublishSources(sources ...pb.TrackSource) {
	v.CanPublishSources = v.CanPublishSources[:0]
	for _, s := range sources {
		v.CanPublishSources = append(v.CanPublishSources, strings.ToLower(s.String()))
	}
}

// SIPGrant carries permission to manage SIP resources and place calls. This server has no SIP service, so it does not
// act on this grant yet. The reference server does.
type SIPGrant struct {
	Admin bool `json:"admin,omitempty"`
	Call  bool `json:"call,omitempty"`
}

// AgentGrant carries permission over hosted agents. This server does not act on it yet.
type AgentGrant struct {
	Admin           bool `json:"admin,omitempty"`
	SimulationAdmin bool `json:"simulationAdmin,omitempty"`
	DatabaseAdmin   bool `json:"databaseAdmin,omitempty"`
	DispatchAdmin   bool `json:"dispatchAdmin,omitempty"`
}

// InferenceGrant carries permission to call hosted inference. This server does not act on it.
type InferenceGrant struct {
	Perform bool `json:"perform,omitempty"`
}

// ObservabilityGrant carries permission to publish observability data. This server does not act on it.
type ObservabilityGrant struct {
	Write bool `json:"write,omitempty"`
}

// AgentEndpointGrant carries permission to call an agent's HTTP endpoints. This server does not act on it.
type AgentEndpointGrant struct {
	Call       bool   `json:"call,omitempty"`
	AgentName  string `json:"agentName,omitempty"`
	Deployment string `json:"deployment,omitempty"`
}

// AgentThreadGrant carries permission over agent threads. This server does not act on it.
type AgentThreadGrant struct {
	All       bool     `json:"all,omitempty"`
	Scope     string   `json:"scope,omitempty"`
	ThreadIDs []string `json:"threadIds,omitempty"`
	List      bool     `json:"list,omitempty"`
	Create    bool     `json:"create,omitempty"`
	Write     bool     `json:"write,omitempty"`
	Read      bool     `json:"read,omitempty"`
	Delete    bool     `json:"delete,omitempty"`
}

// ClaimGrants is everything a token says about its holder, apart from the JWT's own fields.
type ClaimGrants struct {
	Identity      string
	Name          string
	Kind          string
	KindDetails   []string
	Video         *VideoGrant
	SIP           *SIPGrant
	Agent         *AgentGrant
	Inference     *InferenceGrant
	Observability *ObservabilityGrant
	AgentEndpoint *AgentEndpointGrant
	AgentThread   *AgentThreadGrant
	// RoomConfig applies if this token's join creates the room.
	RoomConfig *pb.RoomConfiguration
	// RoomPreset names a stored room configuration.
	RoomPreset string
	Sha256     string
	Metadata   string
	Attributes map[string]string
}

// claimsJSON is ClaimGrants as the JWT writes it. Identity is left out: it travels in the sub claim.
type claimsJSON struct {
	Name          string              `json:"name,omitempty"`
	Kind          string              `json:"kind,omitempty"`
	KindDetails   []string            `json:"kindDetails,omitempty"`
	Video         *VideoGrant         `json:"video,omitempty"`
	SIP           *SIPGrant           `json:"sip,omitempty"`
	Agent         *AgentGrant         `json:"agent,omitempty"`
	Inference     *InferenceGrant     `json:"inference,omitempty"`
	Observability *ObservabilityGrant `json:"observability,omitempty"`
	AgentEndpoint *AgentEndpointGrant `json:"agentEndpoint,omitempty"`
	AgentThread   *AgentThreadGrant   `json:"agentThread,omitempty"`
	RoomConfig    json.RawMessage     `json:"roomConfig,omitempty"`
	RoomPreset    string              `json:"roomPreset,omitempty"`
	Sha256        string              `json:"sha256,omitempty"`
	Metadata      string              `json:"metadata,omitempty"`
	Attributes    map[string]string   `json:"attributes,omitempty"`
}

func (g *ClaimGrants) toJSON() (claimsJSON, error) {
	out := claimsJSON{
		Name: g.Name, Kind: g.Kind, KindDetails: g.KindDetails, Video: g.Video, SIP: g.SIP, Agent: g.Agent, Inference: g.Inference,
		Observability: g.Observability, AgentEndpoint: g.AgentEndpoint, AgentThread: g.AgentThread, RoomPreset: g.RoomPreset,
		Sha256: g.Sha256, Metadata: g.Metadata, Attributes: g.Attributes,
	}
	if g.RoomConfig != nil {
		data, err := protojson.Marshal(g.RoomConfig)
		if err != nil {
			return out, err
		}
		out.RoomConfig = data
	}
	return out, nil
}

func (c *claimsJSON) toGrants() (*ClaimGrants, error) {
	out := &ClaimGrants{
		Name: c.Name, Kind: c.Kind, KindDetails: c.KindDetails, Video: c.Video, SIP: c.SIP, Agent: c.Agent, Inference: c.Inference,
		Observability: c.Observability, AgentEndpoint: c.AgentEndpoint, AgentThread: c.AgentThread, RoomPreset: c.RoomPreset,
		Sha256: c.Sha256, Metadata: c.Metadata, Attributes: c.Attributes,
	}
	if len(c.RoomConfig) > 0 {
		out.RoomConfig = &pb.RoomConfiguration{}
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(c.RoomConfig, out.RoomConfig); err != nil {
			return nil, err
		}
	}
	return out, nil
}
