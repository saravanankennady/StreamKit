// Package schemas defines the REST API's request bodies and their validation.
package schemas

import (
	"net/mail"
	"net/url"
	"strings"
	"unicode"

	"elnova-streamkit.local/api/store"
)

// FieldErrors maps a field to what is wrong with it. An empty map means valid.
type FieldErrors map[string]string

const minPasswordLength = 10

func checkPassword(errs FieldErrors, field, password string) {
	if len(password) < minPasswordLength {
		errs[field] = "must be at least 10 characters"
	} else if len(password) > 256 {
		errs[field] = "must be at most 256 characters"
	}
}

func checkEmail(errs FieldErrors, field, email string) {
	if a, err := mail.ParseAddress(email); err != nil || a.Address != email {
		errs[field] = "must be a valid email address"
	}
}

func checkName(errs FieldErrors, field, value string, required bool, maxLen int) {
	switch {
	case required && strings.TrimSpace(value) == "":
		errs[field] = "is required"
	case len(value) > maxLen:
		errs[field] = "is too long"
	case strings.ContainsFunc(value, unicode.IsControl):
		// These end up in other people's pages and logs, and a NUL cannot be stored at all.
		errs[field] = "cannot contain control characters"
	}
}

// Register creates an account.
type Register struct {
	Email        string `json:"email"`
	Password     string `json:"password"`
	Name         string `json:"name"`
	Organization string `json:"organization"`
}

// Validate implements routes.Validator.
func (r *Register) Validate() FieldErrors {
	errs := FieldErrors{}
	r.Email = strings.TrimSpace(r.Email)
	checkEmail(errs, "email", r.Email)
	checkPassword(errs, "password", r.Password)
	checkName(errs, "name", r.Name, true, 200)
	checkName(errs, "organization", r.Organization, true, 200)
	return errs
}

// Login starts a session.
type Login struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// Validate implements routes.Validator.
func (r *Login) Validate() FieldErrors {
	errs := FieldErrors{}
	r.Email = strings.TrimSpace(r.Email)
	checkName(errs, "email", r.Email, true, 320)
	// Not a name: a password can hold whatever it was set to.
	if r.Password == "" {
		errs["password"] = "is required"
	} else if len(r.Password) > 256 {
		errs["password"] = "is too long"
	}
	return errs
}

// ForgotPassword asks for a reset link.
type ForgotPassword struct {
	Email string `json:"email"`
}

// Validate implements routes.Validator.
func (r *ForgotPassword) Validate() FieldErrors {
	errs := FieldErrors{}
	r.Email = strings.TrimSpace(r.Email)
	checkName(errs, "email", r.Email, true, 320)
	return errs
}

// ResetPassword completes a reset.
type ResetPassword struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// Validate implements routes.Validator.
func (r *ResetPassword) Validate() FieldErrors {
	errs := FieldErrors{}
	checkName(errs, "token", r.Token, true, 200)
	checkPassword(errs, "password", r.Password)
	return errs
}

// VerifyEmail completes verification.
type VerifyEmail struct {
	Token string `json:"token"`
}

// Validate implements routes.Validator.
func (r *VerifyEmail) Validate() FieldErrors {
	errs := FieldErrors{}
	checkName(errs, "token", r.Token, true, 200)
	return errs
}

func checkEnvironment(errs FieldErrors, env string) {
	switch env {
	case "", "development", "staging", "production":
	default:
		errs["environment"] = "must be development, staging or production"
	}
}

func checkSettings(errs FieldErrors, s *store.ProjectSettings) {
	if s == nil {
		return
	}
	for _, origin := range s.AllowedOrigins {
		if origin == "*" {
			continue
		}
		if u, err := url.Parse(origin); err != nil || u.Scheme == "" || u.Host == "" || u.Path != "" {
			errs["settings.allowed_origins"] = "each origin must look like https://app.example.com"
		}
	}
	for _, server := range s.ICEServers {
		if len(server.URLs) == 0 {
			errs["settings.ice_servers"] = "each server needs at least one URL"
		}
		for _, u := range server.URLs {
			if !strings.HasPrefix(u, "stun:") && !strings.HasPrefix(u, "turn:") && !strings.HasPrefix(u, "turns:") {
				errs["settings.ice_servers"] = "URLs must start with stun:, turn: or turns:"
			}
		}
	}
	if s.TokenTTLSeconds < 0 || s.TokenTTLSeconds > 30*24*3600 {
		errs["settings.token_ttl_seconds"] = "must be between 0 and 30 days"
	}
	if s.EmptyTimeout > 7*24*3600 || s.DepartureTimeout > 7*24*3600 {
		errs["settings.empty_timeout"] = "must be at most 7 days"
	}
}

// CreateProject adds a project.
type CreateProject struct {
	OrgID       string                 `json:"org_id"`
	Name        string                 `json:"name"`
	Region      string                 `json:"region"`
	Environment string                 `json:"environment"`
	Settings    *store.ProjectSettings `json:"settings"`
}

// Validate implements routes.Validator.
func (r *CreateProject) Validate() FieldErrors {
	errs := FieldErrors{}
	checkName(errs, "name", r.Name, true, 200)
	checkName(errs, "region", r.Region, false, 100)
	checkEnvironment(errs, r.Environment)
	checkSettings(errs, r.Settings)
	return errs
}

// UpdateProject changes a project. Omitted fields stay as they are.
type UpdateProject struct {
	Name        *string                `json:"name"`
	Region      *string                `json:"region"`
	Environment *string                `json:"environment"`
	Status      *string                `json:"status"`
	Settings    *store.ProjectSettings `json:"settings"`
}

// Validate implements routes.Validator.
func (r *UpdateProject) Validate() FieldErrors {
	errs := FieldErrors{}
	if r.Name != nil {
		checkName(errs, "name", *r.Name, true, 200)
	}
	if r.Region != nil {
		checkName(errs, "region", *r.Region, true, 100)
	}
	if r.Environment != nil {
		checkEnvironment(errs, *r.Environment)
	}
	if r.Status != nil && *r.Status != "active" && *r.Status != "suspended" {
		errs["status"] = "must be active or suspended"
	}
	checkSettings(errs, r.Settings)
	return errs
}

// CreateKey adds an API key.
type CreateKey struct {
	Name string `json:"name"`
}

// Validate implements routes.Validator.
func (r *CreateKey) Validate() FieldErrors {
	errs := FieldErrors{}
	checkName(errs, "name", r.Name, false, 200)
	return errs
}

// RotateKey replaces an API key.
type RotateKey struct {
	// OverlapSeconds is how long the old key keeps working. Default one day.
	OverlapSeconds int `json:"overlap_seconds"`
}

// Validate implements routes.Validator.
func (r *RotateKey) Validate() FieldErrors {
	errs := FieldErrors{}
	if r.OverlapSeconds < 0 || r.OverlapSeconds > 30*24*3600 {
		errs["overlap_seconds"] = "must be between 0 and 30 days"
	}
	return errs
}

// CreateToken mints an access token.
type CreateToken struct {
	Identity   string            `json:"identity"`
	Name       string            `json:"name"`
	Room       string            `json:"room"`
	TTLSeconds int               `json:"ttl_seconds"`
	Metadata   string            `json:"metadata"`
	Attributes map[string]string `json:"attributes"`
	// Grants default to joining, publishing, subscribing and sending data.
	CanPublish           *bool    `json:"can_publish"`
	CanSubscribe         *bool    `json:"can_subscribe"`
	CanPublishData       *bool    `json:"can_publish_data"`
	CanPublishSources    []string `json:"can_publish_sources"`
	CanUpdateOwnMetadata *bool    `json:"can_update_own_metadata"`
	RoomAdmin            bool     `json:"room_admin"`
	RoomCreate           bool     `json:"room_create"`
	RoomList             bool     `json:"room_list"`
	RoomRecord           bool     `json:"room_record"`
	Hidden               bool     `json:"hidden"`
}

// Validate implements routes.Validator.
func (r *CreateToken) Validate() FieldErrors {
	errs := FieldErrors{}
	checkName(errs, "identity", r.Identity, true, 256)
	checkName(errs, "room", r.Room, true, 256)
	checkName(errs, "name", r.Name, false, 256)
	if r.TTLSeconds < 0 || r.TTLSeconds > 30*24*3600 {
		errs["ttl_seconds"] = "must be between 0 and 30 days"
	}
	return errs
}

// CreateJoinLink makes a link that admits people to one room without an account.
type CreateJoinLink struct {
	Room  string `json:"room"`
	Label string `json:"label"`
	// ExpiresInSeconds of 0 uses the default of one day.
	ExpiresInSeconds int `json:"expires_in_seconds"`
	// CanPublish defaults to true. False makes the link watch-only.
	CanPublish *bool `json:"can_publish"`
	// Role is guest by default. A host link gives whoever opens it the running of the meeting.
	Role string `json:"role"`
}

// Validate implements routes.Validator.
func (r *CreateJoinLink) Validate() FieldErrors {
	errs := FieldErrors{}
	checkName(errs, "room", r.Room, true, 256)
	checkName(errs, "label", r.Label, false, 100)
	if r.ExpiresInSeconds != 0 && (r.ExpiresInSeconds < 60 || r.ExpiresInSeconds > 30*24*3600) {
		errs["expires_in_seconds"] = "must be between one minute and 30 days"
	}
	if r.Role != "" && r.Role != "guest" && r.Role != "host" {
		errs["role"] = "must be guest or host"
	}
	return errs
}

// CreateMeeting starts a meeting from scratch: a room name the server picks, and a host link and guest link for it.
type CreateMeeting struct {
	Title string `json:"title"`
	// ExpiresInSeconds of 0 uses the default of one day.
	ExpiresInSeconds int `json:"expires_in_seconds"`
	// WaitingRoom makes guests wait for a host to let them in, from the start.
	WaitingRoom bool `json:"waiting_room"`
}

// Validate implements routes.Validator.
func (r *CreateMeeting) Validate() FieldErrors {
	errs := FieldErrors{}
	checkName(errs, "title", r.Title, false, 100)
	if r.ExpiresInSeconds != 0 && (r.ExpiresInSeconds < 60 || r.ExpiresInSeconds > 30*24*3600) {
		errs["expires_in_seconds"] = "must be between one minute and 30 days"
	}
	return errs
}

// UpdateMeeting changes a meeting. Omitted fields stay as they are.
type UpdateMeeting struct {
	Title       *string `json:"title"`
	Locked      *bool   `json:"locked"`
	WaitingRoom *bool   `json:"waiting_room"`
}

// Validate implements routes.Validator.
func (r *UpdateMeeting) Validate() FieldErrors {
	errs := FieldErrors{}
	if r.Title != nil {
		checkName(errs, "title", *r.Title, false, 100)
	}
	return errs
}

// SetHand raises or lowers the caller's own hand.
type SetHand struct {
	Raised bool `json:"raised"`
}

// Validate implements routes.Validator.
func (r *SetHand) Validate() FieldErrors { return FieldErrors{} }

// SetMeetingRole makes someone a moderator, or a guest again.
type SetMeetingRole struct {
	Role string `json:"role"`
}

// Validate implements routes.Validator.
func (r *SetMeetingRole) Validate() FieldErrors {
	errs := FieldErrors{}
	if r.Role != "moderator" && r.Role != "guest" {
		errs["role"] = "must be moderator or guest"
	}
	return errs
}

// AnswerWaiting lets someone in from the waiting room, or turns them away.
type AnswerWaiting struct {
	Admit bool `json:"admit"`
}

// Validate implements routes.Validator.
func (r *AnswerWaiting) Validate() FieldErrors { return FieldErrors{} }

// WaitingTicket names a place in the waiting room. Cancel gives it up.
type WaitingTicket struct {
	Ticket string `json:"ticket"`
	Cancel bool   `json:"cancel"`
}

// Validate implements routes.Validator.
func (r *WaitingTicket) Validate() FieldErrors {
	errs := FieldErrors{}
	checkName(errs, "ticket", r.Ticket, true, 64)
	return errs
}

// JoinLinkCode names a link by its code.
type JoinLinkCode struct {
	Code string `json:"code"`
}

// Validate implements routes.Validator.
func (r *JoinLinkCode) Validate() FieldErrors {
	errs := FieldErrors{}
	checkName(errs, "code", r.Code, true, 64)
	return errs
}

// JoinWithLink joins a link's room as a guest.
type JoinWithLink struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// Validate implements routes.Validator.
func (r *JoinWithLink) Validate() FieldErrors {
	errs := FieldErrors{}
	checkName(errs, "code", r.Code, true, 64)
	checkName(errs, "name", r.Name, false, 100)
	return errs
}

// CreateRoom opens a room.
type CreateRoom struct {
	Name             string `json:"name"`
	Metadata         string `json:"metadata"`
	EmptyTimeout     uint32 `json:"empty_timeout"`
	DepartureTimeout uint32 `json:"departure_timeout"`
	MaxParticipants  uint32 `json:"max_participants"`
}

// Validate implements routes.Validator.
func (r *CreateRoom) Validate() FieldErrors {
	errs := FieldErrors{}
	checkName(errs, "name", r.Name, true, 256)
	return errs
}

// UpdateRoom changes a room's metadata.
type UpdateRoom struct {
	Metadata string `json:"metadata"`
}

// Validate implements routes.Validator.
func (r *UpdateRoom) Validate() FieldErrors { return FieldErrors{} }

// UpdateParticipant changes a participant. Omitted fields stay as they are.
type UpdateParticipant struct {
	Metadata   *string           `json:"metadata"`
	Name       *string           `json:"name"`
	Attributes map[string]string `json:"attributes"`
	Permission *Permission       `json:"permission"`
}

// Permission is what a participant may do.
type Permission struct {
	CanPublish        bool     `json:"can_publish"`
	CanSubscribe      bool     `json:"can_subscribe"`
	CanPublishData    bool     `json:"can_publish_data"`
	CanUpdateMetadata bool     `json:"can_update_metadata"`
	CanPublishSources []string `json:"can_publish_sources"`
	Hidden            bool     `json:"hidden"`
}

// Validate implements routes.Validator.
func (r *UpdateParticipant) Validate() FieldErrors { return FieldErrors{} }

// MuteTrack mutes or unmutes a published track.
type MuteTrack struct {
	Muted bool `json:"muted"`
}

// Validate implements routes.Validator.
func (r *MuteTrack) Validate() FieldErrors { return FieldErrors{} }

// Webhook creates or changes an endpoint.
type Webhook struct {
	URL      string   `json:"url"`
	APIKeyID string   `json:"api_key_id"`
	Events   []string `json:"events"`
	Enabled  *bool    `json:"enabled"`
}

// WebhookEvents lists the events an endpoint can subscribe to.
var WebhookEvents = []string{
	"room_started", "room_finished", "participant_joined", "participant_left", "participant_connection_aborted",
	"track_published", "track_unpublished", "egress_started", "egress_updated", "egress_ended",
	"ingress_started", "ingress_ended",
}

// Validate implements routes.Validator.
func (r *Webhook) Validate() FieldErrors {
	errs := FieldErrors{}
	u, err := url.Parse(r.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		errs["url"] = "must be an http or https URL"
	}
	checkName(errs, "api_key_id", r.APIKeyID, true, 64)
	for _, e := range r.Events {
		known := false
		for _, k := range WebhookEvents {
			known = known || k == e
		}
		if !known {
			errs["events"] = "unknown event: " + e
		}
	}
	return errs
}

// Drain asks a node to stop or resume taking new sessions.
type Drain struct {
	Draining bool `json:"draining"`
}

// Validate implements routes.Validator.
func (r *Drain) Validate() FieldErrors { return FieldErrors{} }
