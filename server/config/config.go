// Package config loads server configuration from a YAML file and the environment.
package config

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Version identifies this server build.
const Version = "0.1.0"

// WireCompatVersion is what the server reports as its version on the wire. Clients of this protocol
// turn features on by comparing that number against the reference server's releases, so it states which
// release's client-visible behaviour this server matches. It is not the product version: RPC needs at
// least 1.8.0, and SVC simulcast, which this server does not implement, needs more than it says.
const WireCompatVersion = "1.9.0"

// ICEServer is a STUN or TURN server handed to clients at join time.
type ICEServer struct {
	URLs       []string `yaml:"urls"`
	Username   string   `yaml:"username"`
	Credential string   `yaml:"credential"`
}

// RTC configures media transport.
type RTC struct {
	UDPPort int `yaml:"udp_port"`
	TCPPort int `yaml:"tcp_port"`
	// NodeIP, when set, is the only address advertised in ICE candidates. Set it to the public IP of a
	// host behind 1:1 NAT.
	NodeIP string `yaml:"node_ip"`
	// IncludeLoopback advertises 127.0.0.1. For local development and tests only.
	IncludeLoopback bool `yaml:"include_loopback"`
	EnableIPv6      bool `yaml:"enable_ipv6"`
	// ScreenShareMaxPlayoutDelayMs limits how long a viewer's browser may hold frames of a shared screen before
	// showing them. Left to itself, Chrome holds them for around 100 ms even on a clean network. A network with more
	// jitter than this shows it as uneven motion. Zero sets no limit.
	ScreenShareMaxPlayoutDelayMs int `yaml:"screen_share_max_playout_delay_ms"`
	// MaxBitrate caps what one subscriber is sent, in bits per second.
	MaxBitrate int `yaml:"max_bitrate"`
	// SinglePeerConnection serves /rtc/v1, where a client has one connection carrying media both ways and makes every offer.
	// It is on by default. Off, those routes return 404, and clients fall back to /rtc with two connections.
	SinglePeerConnection bool        `yaml:"single_peer_connection"`
	ICEServers           []ICEServer `yaml:"ice_servers"`
	// TURN mints short-lived credentials for a TURN server sharing this secret.
	TURN TURN `yaml:"turn"`
}

// TURN configures a TURN server with time-limited credentials. On coturn: --use-auth-secret and
// --static-auth-secret with the same value.
type TURN struct {
	URLs   []string `yaml:"urls"`
	Secret string   `yaml:"secret"`
}

// Room holds defaults for newly created rooms.
type Room struct {
	AutoCreate       bool   `yaml:"auto_create"`
	EmptyTimeout     uint32 `yaml:"empty_timeout"`
	DepartureTimeout uint32 `yaml:"departure_timeout"`
	MaxParticipants  uint32 `yaml:"max_participants"`
	// EnableRemoteUnmute lets the room API unmute a participant's track, not only mute it. Off by default:
	// turning on someone's microphone from afar is a privacy decision to make deliberately.
	EnableRemoteUnmute bool `yaml:"enable_remote_unmute"`
	// SyncStreams puts a publisher's camera and microphone in one MediaStream for subscribers, so that browsers line them
	// up. A browser does that by delaying whichever arrives first, so this trades latency for lip sync. It also stops
	// m-sections being reused, and rooms can ask for it one at a time when they are created. Off by default, as on the
	// reference.
	SyncStreams bool `yaml:"sync_streams"`
	// MeetingMaxParticipants limits a room opened through a share link, unless max_participants or the project's own
	// settings say otherwise. It is there so a leaked link cannot bring in people without end. It says nothing about how
	// many your machines can carry, which is yours to measure. Zero sets no limit.
	MeetingMaxParticipants uint32 `yaml:"meeting_max_participants"`
}

// Limits bounds client-controlled input.
type Limits struct {
	MaxSignalMessageBytes int64 `yaml:"max_signal_message_bytes"`
	MaxDataPacketBytes    int   `yaml:"max_data_packet_bytes"`
	MaxMetadataBytes      int   `yaml:"max_metadata_bytes"`
	// RTCConnectsPerMinute limits how fast one client address can start signalling sessions, with bursts up to
	// RTCConnectBurst, which has to cover a whole office joining at once. Reconnecting to a session is not counted against
	// the address. Zero uses the defaults, or no limit with --dev, where every client is on one address. Negative sets no
	// limit.
	RTCConnectsPerMinute int `yaml:"rtc_connects_per_minute"`
	RTCConnectBurst      int `yaml:"rtc_connect_burst"`
	// MaxDataMessagesPerSecond and MaxDataBytesPerSecond limit what one participant can send to a room over data channels.
	// Past that, messages are dropped. A short burst can go to twice the messages and four times the bytes. The defaults leave
	// room for a data stream going as fast as it usefully can, so turn them up only for something that really sends this much.
	// Zero sets no limit.
	MaxDataMessagesPerSecond int `yaml:"max_data_messages_per_second"`
	MaxDataBytesPerSecond    int `yaml:"max_data_bytes_per_second"`
}

// The defaults for Limits.RTCConnectsPerMinute and Limits.RTCConnectBurst.
const (
	defaultRTCConnectsPerMinute = 600
	defaultRTCConnectBurst      = 300
)

// RTCConnectLimit says how fast one client address can start signalling sessions. Zero means no limit.
func (c *Config) RTCConnectLimit() (perMinute, burst int) {
	perMinute, burst = c.Limits.RTCConnectsPerMinute, c.Limits.RTCConnectBurst
	if perMinute < 0 || (perMinute == 0 && c.Development) {
		return 0, 0
	}
	if perMinute == 0 {
		perMinute = defaultRTCConnectsPerMinute
	}
	if burst <= 0 {
		burst = defaultRTCConnectBurst
	}
	return perMinute, burst
}

// Config is the complete server configuration.
type Config struct {
	Port        int               `yaml:"port"`
	BindAddress string            `yaml:"bind_address"`
	Region      string            `yaml:"region"`
	LogLevel    string            `yaml:"log_level"`
	Keys        map[string]string `yaml:"keys"`
	// AllowedOrigins restricts browser origins for the HTTP APIs. Empty allows any origin, which is
	// safe for the token-authenticated RTC endpoints but should be narrowed for dashboards.
	AllowedOrigins []string `yaml:"allowed_origins"`
	RTC            RTC      `yaml:"rtc"`
	Room           Room     `yaml:"room"`
	Limits         Limits   `yaml:"limits"`
	Development    bool     `yaml:"development"`

	// OTLPEndpoint sends OpenTelemetry traces to a collector, such as http://otel-collector:4318.
	OTLPEndpoint string `yaml:"otlp_endpoint"`

	// MetricsToken protects /metrics. Without one, /metrics is only open in development mode: it reveals room
	// and usage activity.
	MetricsToken string `yaml:"metrics_token"`

	// Redis enables running several nodes as a cluster. Empty means this is the only node.
	Redis Redis `yaml:"redis"`

	// ControlPlane enables accounts, projects, the REST API and history. It requires a database.
	ControlPlane ControlPlane `yaml:"control_plane"`

	// Media configures recording and ingest. The server and its workers read the same file.
	Media Media `yaml:"media"`
}

// Media configures recording and ingest.
type Media struct {
	// WSURL is how workers reach rooms, such as ws://127.0.0.1:7880. Empty uses control_plane.public_ws_url.
	WSURL string `yaml:"ws_url"`
	// PublicRTMPURL is shown to publishers, such as rtmp://live.example.com:1935/live. Empty uses the host of
	// the API request on port 1935.
	PublicRTMPURL string `yaml:"public_rtmp_url"`
	// PublicWHIPURL is shown to publishers, such as https://rtc.example.com/whip/v1. Empty uses the address of
	// the API request.
	PublicWHIPURL string `yaml:"public_whip_url"`
	// AllowPrivateAddresses lets URL inputs and stream outputs reach private networks. Off by default, because
	// otherwise anyone with a project could probe your internal network through the workers.
	AllowPrivateAddresses bool         `yaml:"allow_private_addresses"`
	Storage               MediaStorage `yaml:"storage"`
	Egress                EgressWorker `yaml:"egress"`
	Ingress               IngressConf  `yaml:"ingress"`
	SIP                   SIPConf      `yaml:"sip"`
}

// SIPConf configures streamkit-sip, and the limits the API enforces for it. See docs/deployment/sip.md.
type SIPConf struct {
	// UDPAddr and TCPAddr are where SIP is served. Each defaults to :5060, and "off" turns one off.
	UDPAddr string `yaml:"udp_addr"`
	TCPAddr string `yaml:"tcp_addr"`
	// TLSAddr serves SIP over TLS, such as :5061. It needs TLSCertFile and TLSKeyFile. Empty leaves TLS off.
	TLSAddr     string `yaml:"tls_addr"`
	TLSCertFile string `yaml:"tls_cert_file"`
	TLSKeyFile  string `yaml:"tls_key_file"`
	// TLSCAFile adds certificate authorities to trust when calling out over TLS, for a carrier with a private one. A carrier's
	// certificate is always checked against its name.
	TLSCAFile string `yaml:"tls_ca_file"`
	// PublicHost is the name or address peers reach this worker at, used in Contact headers. It has to reach this exact
	// worker, because requests within a call come straight back to it. Required unless it can be worked out from a listen
	// address that names a specific IP.
	PublicHost string `yaml:"public_host"`
	// MediaIP is the address put in SDP, which RTP is sent to. Empty uses PublicHost if that is an IP.
	MediaIP string `yaml:"media_ip"`
	// RTPPortMin and RTPPortMax bound the UDP ports for media. A call takes two: RTP on an even port, and RTCP on the next.
	// The default is 10000 to 20000.
	RTPPortMin int `yaml:"rtp_port_min"`
	RTPPortMax int `yaml:"rtp_port_max"`
	// MaxCalls is how many calls one worker carries at once. Zero uses 25 per CPU.
	MaxCalls int `yaml:"max_calls"`
	// MaxCallsPerProject is how many calls one project may have at once, across all workers, in both directions. Zero uses 50.
	MaxCallsPerProject int `yaml:"max_calls_per_project"`
	// MaxOutboundPerMinute is how many outbound calls one project may start in a minute. Zero uses 120.
	MaxOutboundPerMinute int `yaml:"max_outbound_per_minute"`
	// InvitesPerSecond and InviteBurst limit the requests one source address can make. Zero uses 20 and 40. Set them with your
	// carrier in mind: all of a carrier's calls may come from one address.
	InvitesPerSecond float64 `yaml:"invites_per_second"`
	InviteBurst      int     `yaml:"invite_burst"`
	// BanSeconds is how long a source is ignored after too many refused calls. Zero uses 600, and a negative value turns this off.
	BanSeconds int `yaml:"ban_seconds"`
	// BanAfter is how many refused requests in a minute get a source banned. Zero uses 20.
	BanAfter int `yaml:"ban_after"`
	// SilentRejects drops calls that match no trunk without answering, so a scan of the internet learns nothing from this port.
	SilentRejects bool `yaml:"silent_rejects"`
	// AllowUnauthenticatedTrunks permits inbound trunks with neither an IP allow-list nor digest credentials. Off by default,
	// because anyone on the internet can call such a trunk. Turn it on only behind something else that does this checking.
	AllowUnauthenticatedTrunks bool `yaml:"allow_unauthenticated_trunks"`
	// LeaseSeconds is how long a silent worker keeps a call before anyone may mark it failed. Zero uses 30.
	LeaseSeconds int `yaml:"lease_seconds"`
	// MaxCallSeconds ends any call that lasts this long. Zero uses a day.
	MaxCallSeconds int `yaml:"max_call_seconds"`
	// RingingSeconds is how long a call rings before giving up, unless a trunk or request says otherwise. Zero uses 180.
	RingingSeconds int `yaml:"ringing_seconds"`
	// MediaTimeoutSeconds ends a call that has sent no RTP for this long. Zero uses 15, and 30 before the first packet.
	MediaTimeoutSeconds int `yaml:"media_timeout_seconds"`
	// DrainSeconds is how long a stopping worker waits for calls to end by themselves. Zero uses 60.
	DrainSeconds int `yaml:"drain_seconds"`
}

// MediaStorage says where recordings are kept.
type MediaStorage struct {
	// Kind is local, the default, or s3.
	Kind string `yaml:"kind"`
	// Dir is the directory for local storage.
	Dir string `yaml:"dir"`
	S3  S3     `yaml:"s3"`
	// RetentionDays removes recordings this long after they ended. Zero keeps them until someone deletes them.
	RetentionDays int `yaml:"retention_days"`
}

// S3 configures S3-compatible storage. STREAMKIT_S3_ACCESS_KEY and STREAMKIT_S3_SECRET_KEY override the file.
type S3 struct {
	Endpoint  string `yaml:"endpoint"`
	Region    string `yaml:"region"`
	Bucket    string `yaml:"bucket"`
	Prefix    string `yaml:"prefix"`
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	UseTLS    bool   `yaml:"use_tls"`
}

// EgressWorker configures streamkit-egress.
type EgressWorker struct {
	// MaxJobs is how many recordings one worker runs at once. Zero uses NumCPU / 3, which leaves room for busier
	// pictures than the 0.7 of a core a two-person composite measured. See docs/api/media-services.md.
	MaxJobs int `yaml:"max_jobs"`
	// ChromePath defaults to google-chrome on PATH.
	ChromePath string `yaml:"chrome_path"`
	// ChromeNoSandbox turns off Chrome's sandbox. Hosts without unprivileged user namespaces, such as most
	// containers, need it.
	ChromeNoSandbox bool `yaml:"chrome_no_sandbox"`
	// WorkDir holds recordings in progress. It must have room for the longest recording.
	WorkDir string `yaml:"work_dir"`
	// MaxJobsPerProject stops one project from taking every slot there is. Zero sets no limit, which suits an installation
	// with one project. Set it if you host several.
	MaxJobsPerProject int `yaml:"max_jobs_per_project"`
	// WaitForMediaSeconds is how long a job waits for something to record before it gives up, and frees its slot. Zero uses
	// 900.
	WaitForMediaSeconds int `yaml:"wait_for_media_seconds"`
	// MaxDurationSeconds ends a recording that has gone on this long, as if someone had stopped it. Zero uses 86400.
	MaxDurationSeconds int `yaml:"max_duration_seconds"`
	// LeaseSeconds is how long a silent worker keeps a job before anyone may mark it failed. The default is 30. Too short
	// fails good recordings when a busy machine stalls, and too long leaves a dead worker's job looking alive.
	LeaseSeconds int `yaml:"lease_seconds"`
}

// IngressConf configures streamkit-ingress.
type IngressConf struct {
	// RTMPAddr is the RTMP listen address. Empty uses :1935.
	RTMPAddr string `yaml:"rtmp_addr"`
	// MaxSessions is how many streams one worker transcodes at once. Zero uses NumCPU / 2.
	MaxSessions int `yaml:"max_sessions"`
	// MaxSessionsPerProject stops one project from taking every slot there is. Zero sets no limit.
	MaxSessionsPerProject int `yaml:"max_sessions_per_project"`
	// LeaseSeconds is how long a silent worker keeps a session before anyone may mark it failed. The default is 30.
	LeaseSeconds int `yaml:"lease_seconds"`
	// StallSeconds is how long a stream can produce nothing before it counts as dead. The default is 60.
	StallSeconds int `yaml:"stall_seconds"`
}

// Redis configures clustering.
type Redis struct {
	URL string `yaml:"url"`
	// NodeURL is how other nodes reach this one over HTTP, such as http://10.0.0.5:7880. It must not go
	// through a load balancer, because it has to reach this exact node.
	NodeURL string `yaml:"node_url"`
}

// ControlPlane configures the management API.
type ControlPlane struct {
	// DatabaseURL is a PostgreSQL connection string. Empty leaves the control plane off.
	DatabaseURL string `yaml:"database_url"`
	// MasterKey encrypts API secrets at rest: 32 bytes as hex or base64. Set it in the environment as
	// STREAMKIT_MASTER_KEY rather than in a file, and back it up: losing it makes every API secret unreadable.
	MasterKey string `yaml:"master_key"`
	// WorkerDBPassword lets media workers log in to the database as streamkit_worker, a role with only what they need.
	// The server sets it when it starts. Set it in the environment as STREAMKIT_WORKER_DB_PASSWORD.
	WorkerDBPassword string `yaml:"worker_db_password"`
	// PublicWSURL is the address clients connect to, shown in the dashboard, such as wss://rtc.example.com.
	PublicWSURL string `yaml:"public_ws_url"`
	// PublicDashboardURL is used to build links in emails.
	PublicDashboardURL string `yaml:"public_dashboard_url"`
	// SecureCookies sets the Secure flag on session cookies. Turn off only for plain HTTP development.
	SecureCookies bool `yaml:"secure_cookies"`
	// TrustProxyHeaders believes X-Forwarded-For. Turn on only behind a proxy that overwrites it, or clients
	// can dodge rate limits.
	TrustProxyHeaders bool `yaml:"trust_proxy_headers"`
	// AllowPrivateWebhooks permits webhook URLs on private networks. Off by default, because otherwise
	// anyone with a project could probe your internal network through the server.
	AllowPrivateWebhooks bool `yaml:"allow_private_webhooks"`
	// RequireEmailVerification blocks login until the address is verified.
	RequireEmailVerification bool `yaml:"require_email_verification"`
	// AllowRegistration lets anyone who can reach this server make an account, and with it start meetings and run rooms.
	// Unset means no, or yes with --dev. Either way, an installation with no accounts takes one: the first account
	// administers the installation, and there would be no other way to make it. STREAMKIT_ALLOW_REGISTRATION overrides this.
	AllowRegistration *bool `yaml:"allow_registration"`
	// MaxMeetingsPerProject limits meetings that can still be joined in one project, those not ended with a link that still
	// works. Zero uses the default of 1000, and negative sets no limit.
	MaxMeetingsPerProject int  `yaml:"max_meetings_per_project"`
	Mail                  Mail `yaml:"mail"`
}

// defaultMaxMeetingsPerProject is well past what one project should have open, and each one stops counting once its links
// run out, after a day by default.
const defaultMaxMeetingsPerProject = 1000

// RegistrationOpen reports whether accounts can be made once there is one.
func (c *Config) RegistrationOpen() bool {
	if c.ControlPlane.AllowRegistration != nil {
		return *c.ControlPlane.AllowRegistration
	}
	return c.Development
}

// MeetingsPerProject is how many meetings a project can have open. Zero means no limit.
func (c *Config) MeetingsPerProject() int {
	switch limit := c.ControlPlane.MaxMeetingsPerProject; {
	case limit < 0:
		return 0
	case limit == 0:
		return defaultMaxMeetingsPerProject
	default:
		return limit
	}
}

// Mail configures outgoing email.
type Mail struct {
	From     string `yaml:"from"`
	SMTPHost string `yaml:"smtp_host"`
	SMTPPort int    `yaml:"smtp_port"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// Dir writes messages to files instead of sending them, for development and tests.
	Dir string `yaml:"dir"`
}

// Default returns production defaults. Keys must still be configured.
func Default() *Config {
	return &Config{
		Port:     7880,
		LogLevel: "info",
		RTC: RTC{
			ScreenShareMaxPlayoutDelayMs: 100,
			UDPPort:                      7882,
			TCPPort:                      7881,
			MaxBitrate:                   30_000_000,
			SinglePeerConnection:         true,
		},
		Room: Room{
			AutoCreate:             true,
			EmptyTimeout:           300,
			DepartureTimeout:       20,
			MeetingMaxParticipants: 100,
		},
		Limits: Limits{
			MaxSignalMessageBytes: 1 << 20,
			MaxDataPacketBytes:    64 * 1024,
			MaxMetadataBytes:      64 * 1024,
			// A data stream sends chunks of 15 KB, so these two run out at about the same point.
			MaxDataMessagesPerSecond: 500,
			MaxDataBytesPerSecond:    8 << 20,
		},
	}
}

// DevKey and DevSecret are well-known credentials used only with --dev.
const (
	DevKey    = "devkey"
	DevSecret = "secret"
)

// Load reads an optional YAML file, applies the environment and validates the result.
func Load(path string, dev bool) (*Config, error) {
	c := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(data, c); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	// The same thing as a file, for a container with a read-only root, where there may be no file to give it. It comes after any
	// file, and before the variables below, which name one setting each.
	if text := os.Getenv("STREAMKIT_CONFIG_YAML"); text != "" {
		if err := yaml.Unmarshal([]byte(text), c); err != nil {
			return nil, fmt.Errorf("parse STREAMKIT_CONFIG_YAML: %w", err)
		}
	}
	if env := os.Getenv("STREAMKIT_KEYS"); env != "" {
		keys, err := ParseKeys(env)
		if err != nil {
			return nil, fmt.Errorf("STREAMKIT_KEYS: %w", err)
		}
		c.Keys = keys
	}
	if ip := os.Getenv("STREAMKIT_NODE_IP"); ip != "" {
		c.RTC.NodeIP = ip
	}
	if v := os.Getenv("STREAMKIT_METRICS_TOKEN"); v != "" {
		c.MetricsToken = v
	}
	if v := os.Getenv("STREAMKIT_OTLP_ENDPOINT"); v != "" {
		c.OTLPEndpoint = v
	}
	if v := os.Getenv("STREAMKIT_TURN_SECRET"); v != "" {
		c.RTC.TURN.Secret = v
	}
	if v := os.Getenv("STREAMKIT_REDIS_URL"); v != "" {
		c.Redis.URL = v
	}
	if v := os.Getenv("STREAMKIT_NODE_URL"); v != "" {
		c.Redis.NodeURL = v
	}
	if v := os.Getenv("STREAMKIT_DATABASE_URL"); v != "" {
		c.ControlPlane.DatabaseURL = v
	}
	if v := os.Getenv("STREAMKIT_WORKER_DB_PASSWORD"); v != "" {
		c.ControlPlane.WorkerDBPassword = v
	}
	if v := os.Getenv("STREAMKIT_MASTER_KEY"); v != "" {
		c.ControlPlane.MasterKey = v
	}
	if v := os.Getenv("STREAMKIT_ALLOW_REGISTRATION"); v != "" {
		allow, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("STREAMKIT_ALLOW_REGISTRATION: %w", err)
		}
		c.ControlPlane.AllowRegistration = &allow
	}
	if v := os.Getenv("STREAMKIT_SMTP_PASSWORD"); v != "" {
		c.ControlPlane.Mail.Password = v
	}
	if v := os.Getenv("STREAMKIT_S3_ACCESS_KEY"); v != "" {
		c.Media.Storage.S3.AccessKey = v
	}
	if v := os.Getenv("STREAMKIT_S3_SECRET_KEY"); v != "" {
		c.Media.Storage.S3.SecretKey = v
	}
	if dev {
		c.Development = true
		c.RTC.IncludeLoopback = true
		if len(c.Keys) == 0 {
			c.Keys = map[string]string{DevKey: DevSecret}
		}
		if c.BindAddress == "" {
			c.BindAddress = "127.0.0.1"
		}
	}
	return c, c.Validate()
}

// ParseKeys parses "key1: secret1, key2: secret2".
func ParseKeys(s string) (map[string]string, error) {
	keys := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(part, ":")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" || v == "" {
			return nil, errors.New(`expected "key: secret" pairs separated by commas`)
		}
		keys[k] = v
	}
	return keys, nil
}

// HopSecret is the key nodes prove to each other that a forwarded request came from one of them. It comes from what they
// must share anyway to accept the same tokens, so there is nothing more to set up, or to get out of step: the master
// key, or without a control plane, the static API keys.
func (c *Config) HopSecret() []byte {
	mac := hmac.New(sha256.New, []byte("elnova-streamkit cluster hop"))
	if c.ControlPlane.MasterKey != "" {
		mac.Write([]byte(c.ControlPlane.MasterKey))
		return mac.Sum(nil)
	}
	for _, name := range slices.Sorted(maps.Keys(c.Keys)) {
		_, _ = fmt.Fprintf(mac, "%d:%s:%d:%s", len(name), name, len(c.Keys[name]), c.Keys[name])
	}
	return mac.Sum(nil)
}

// Validate rejects configurations that cannot work or would be unsafe.
func (c *Config) Validate() error {
	// With a control plane, keys live in the database and static ones are optional.
	if len(c.Keys) == 0 && c.ControlPlane.DatabaseURL == "" {
		return errors.New("no API keys configured: set keys in the config file or STREAMKIT_KEYS, or use --dev")
	}
	for k, s := range c.Keys {
		if !c.Development && len(s) < 32 {
			return fmt.Errorf("the secret for API key %q is shorter than 32 characters", k)
		}
	}
	if c.ControlPlane.DatabaseURL != "" && c.ControlPlane.MasterKey == "" {
		return errors.New("the control plane needs a master key: set STREAMKIT_MASTER_KEY (try: openssl rand -hex 32)")
	}
	if c.Redis.URL != "" && c.Redis.NodeURL == "" {
		return errors.New("clustering needs redis.node_url, or STREAMKIT_NODE_URL: how other nodes reach this one")
	}
	if c.Port <= 0 || c.RTC.UDPPort <= 0 || c.RTC.TCPPort <= 0 {
		return errors.New("port, rtc.udp_port and rtc.tcp_port must all be positive")
	}
	return nil
}
