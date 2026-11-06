package streamkit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"elnova-streamkit.local/pkg/pb"
)

const (
	testKey    = "testkey"
	testSecret = "testsecret-that-is-long-enough-for-tests"
)

var testKeys = Keys{testKey: testSecret}

// handMade signs arbitrary claims, for the tokens this SDK refuses to make.
func handMade(t *testing.T, method jwt.SigningMethod, secret any, claims jwt.MapClaims) string {
	t.Helper()
	token, err := jwt.NewWithClaims(method, claims).SignedString(secret)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func payload(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", token)
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTokenCarriesEveryGrant(t *testing.T) {
	video := &VideoGrant{
		RoomCreate: true, RoomList: true, RoomRecord: true, RoomAdmin: true, RoomJoin: true, Room: "r",
		CanPublish: Bool(false), CanSubscribe: Bool(true), CanPublishData: Bool(false),
		CanUpdateOwnMetadata: Bool(true), IngressAdmin: true, Hidden: true, Recorder: true, Agent: true,
		CanSubscribeMetrics: Bool(true), CanManageAgentSession: Bool(true), DestinationRoom: "d",
	}
	video.SetCanPublishSources(pb.TrackSource_CAMERA, pb.TrackSource_SCREEN_SHARE_AUDIO)
	token, err := NewAccessToken(testKey, testSecret).
		SetIdentity("alice").SetName("Alice").SetMetadata("m").SetAttributes(map[string]string{"a": "1"}).
		SetKind(pb.ParticipantInfo_AGENT).SetKindDetails(pb.ParticipantInfo_CLOUD_AGENT).
		SetVideoGrant(video).
		SetSIPGrant(&SIPGrant{Admin: true, Call: true}).
		SetAgentGrant(&AgentGrant{Admin: true, SimulationAdmin: true, DatabaseAdmin: true, DispatchAdmin: true}).
		SetInferenceGrant(&InferenceGrant{Perform: true}).
		SetObservabilityGrant(&ObservabilityGrant{Write: true}).
		SetAgentEndpointGrant(&AgentEndpointGrant{Call: true, AgentName: "n", Deployment: "prod"}).
		SetAgentThreadGrant(&AgentThreadGrant{All: true, Scope: "s", ThreadIDs: []string{"t"}, List: true, Create: true, Write: true, Read: true, Delete: true}).
		SetRoomConfig(&pb.RoomConfiguration{Name: "r", EmptyTimeout: 11, DepartureTimeout: 12, MaxParticipants: 13, Metadata: "cfg"}).
		SetRoomPreset("preset").SetSha256("abc").
		ToJWT()
	if err != nil {
		t.Fatal(err)
	}

	// The names on the wire are part of the protocol.
	claims := payload(t, token)
	for _, name := range []string{"sub", "iss", "exp", "nbf", "iat", "name", "kind", "kindDetails", "metadata", "attributes", "video", "sip", "agent",
		"inference", "observability", "agentEndpoint", "agentThread", "roomConfig", "roomPreset", "sha256"} {
		if _, ok := claims[name]; !ok {
			t.Errorf("claim %q is missing", name)
		}
	}
	if claims["sub"] != "alice" || claims["iss"] != testKey || claims["kind"] != "agent" {
		t.Errorf("claims: %v", claims)
	}
	if got := claims["kindDetails"].([]any)[0]; got != "cloud_agent" {
		t.Errorf("kindDetails: %v", got)
	}
	v := claims["video"].(map[string]any)
	for _, name := range []string{"roomCreate", "roomList", "roomRecord", "roomAdmin", "roomJoin", "room", "canPublish", "canSubscribe", "canPublishData", "canPublishSources",
		"canUpdateOwnMetadata", "ingressAdmin", "hidden", "recorder", "agent", "canSubscribeMetrics", "canManageAgentSession", "destinationRoom"} {
		if _, ok := v[name]; !ok {
			t.Errorf("video grant %q is missing", name)
		}
	}
	if got := v["canPublishSources"].([]any); got[0] != "camera" || got[1] != "screen_share_audio" {
		t.Errorf("canPublishSources: %v", got)
	}
	// A room configuration uses the same JSON names the reference tools write.
	if rc := claims["roomConfig"].(map[string]any); rc["emptyTimeout"] != float64(11) || rc["maxParticipants"] != float64(13) {
		t.Errorf("roomConfig: %v", rc)
	}

	got, err := VerifyToken(token, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	if got.Identity != "alice" || got.Name != "Alice" || got.APIKey != testKey || got.Metadata != "m" || got.Attributes["a"] != "1" {
		t.Errorf("verified: %+v", got)
	}
	if !got.Video.RoomAdmin || *got.Video.CanPublish || !*got.Video.CanSubscribe || got.Video.DestinationRoom != "d" {
		t.Errorf("video: %+v", got.Video)
	}
	if !got.SIP.Admin || !got.SIP.Call || !got.Agent.DispatchAdmin || !got.Inference.Perform || !got.Observability.Write {
		t.Errorf("other grants: %+v", got.ClaimGrants)
	}
	if got.AgentEndpoint.AgentName != "n" || got.AgentThread.ThreadIDs[0] != "t" || got.RoomPreset != "preset" || got.Sha256 != "abc" {
		t.Errorf("other grants: %+v", got.ClaimGrants)
	}
	if got.RoomConfig.GetMaxParticipants() != 13 || got.RoomConfig.GetMetadata() != "cfg" {
		t.Errorf("room config: %v", got.RoomConfig)
	}
}

func TestTokenLifetime(t *testing.T) {
	token, err := NewAccessToken(testKey, testSecret).SetIdentity("a").SetValidFor(time.Minute).ToJWT()
	if err != nil {
		t.Fatal(err)
	}
	c := payload(t, token)
	if got := c["exp"].(float64) - c["nbf"].(float64); got != 60 {
		t.Errorf("lifetime %v", got)
	}

	token, err = NewAccessToken(testKey, testSecret).SetIdentity("a").ToJWT()
	if err != nil {
		t.Fatal(err)
	}
	c = payload(t, token)
	if got := c["exp"].(float64) - c["nbf"].(float64); got != DefaultTokenTTL.Seconds() {
		t.Errorf("default lifetime %v", got)
	}
}

func TestTokenRefusesWhatCannotWork(t *testing.T) {
	if _, err := NewAccessToken("", testSecret).ToJWT(); !errors.Is(err, ErrMissingCredentials) {
		t.Errorf("no key: %v", err)
	}
	if _, err := NewAccessToken(testKey, "").ToJWT(); !errors.Is(err, ErrMissingCredentials) {
		t.Errorf("no secret: %v", err)
	}
	if _, err := NewAccessToken(testKey, testSecret).SetVideoGrant(&VideoGrant{RoomJoin: true, Room: "r"}).ToJWT(); !errors.Is(err, ErrJoinNeedsIdentity) {
		t.Errorf("no identity: %v", err)
	}
	if _, err := NewAccessToken(testKey, testSecret).SetIdentity("a").SetVideoGrant(&VideoGrant{RoomJoin: true}).ToJWT(); !errors.Is(err, ErrJoinNeedsIdentity) {
		t.Errorf("no room: %v", err)
	}
}

func TestVerifyTokenRules(t *testing.T) {
	now := time.Now()
	base := func() jwt.MapClaims {
		return jwt.MapClaims{"iss": testKey, "sub": "a", "exp": now.Add(time.Hour).Unix(), "video": map[string]any{"roomJoin": true}}
	}
	with := func(key string, v any) jwt.MapClaims {
		c := base()
		c[key] = v
		return c
	}
	secret := []byte(testSecret)
	noExp := base()
	delete(noExp, "exp")
	cases := []struct {
		name  string
		token string
		want  error
	}{
		{"valid", handMade(t, jwt.SigningMethodHS256, secret, base()), nil},
		{"expired within the leeway", handMade(t, jwt.SigningMethodHS256, secret, with("exp", now.Add(-50*time.Second).Unix())), nil},
		{"expired past the leeway", handMade(t, jwt.SigningMethodHS256, secret, with("exp", now.Add(-70*time.Second).Unix())), ErrInvalidToken},
		{"not yet valid within the leeway", handMade(t, jwt.SigningMethodHS256, secret, with("nbf", now.Add(50*time.Second).Unix())), nil},
		{"not yet valid past the leeway", handMade(t, jwt.SigningMethodHS256, secret, with("nbf", now.Add(70*time.Second).Unix())), ErrInvalidToken},
		{"no expiry", handMade(t, jwt.SigningMethodHS256, secret, noExp), ErrInvalidToken},
		{"wrong secret", handMade(t, jwt.SigningMethodHS256, []byte("another secret"), base()), ErrInvalidToken},
		{"unknown key", handMade(t, jwt.SigningMethodHS256, secret, with("iss", "other")), ErrUnknownAPIKey},
		{"no issuer", handMade(t, jwt.SigningMethodHS256, secret, jwt.MapClaims{"exp": now.Add(time.Hour).Unix()}), ErrUnknownAPIKey},
		{"another algorithm", handMade(t, jwt.SigningMethodHS384, secret, base()), ErrInvalidToken},
		{"no signature", handMade(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, base()), ErrInvalidToken},
		{"not a token", "not.a.token", ErrInvalidToken},
		{"empty", "", ErrInvalidToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := VerifyToken(tc.token, testKeys)
			if tc.want == nil && err != nil {
				t.Fatalf("want success, got %v", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

func TestVerifyTokenIdentityAndUnknownClaims(t *testing.T) {
	c, err := VerifyToken(handMade(t, jwt.SigningMethodHS256, []byte(testSecret), jwt.MapClaims{
		"iss": testKey, "jti": "from-jti", "exp": time.Now().Add(time.Hour).Unix(),
		"somethingNew": map[string]any{"x": 1}, "aud": []string{"a", "b"},
		"roomConfig": map[string]any{"empty_timeout": 5, "maxParticipants": 6, "fieldFromTheFuture": true},
	}), testKeys)
	if err != nil {
		t.Fatal(err)
	}
	if c.Identity != "from-jti" {
		t.Errorf("identity %q", c.Identity)
	}
	if c.Video != nil {
		t.Errorf("video %+v", c.Video)
	}
	// Other tools may write either spelling of a field.
	if c.RoomConfig.GetEmptyTimeout() != 5 || c.RoomConfig.GetMaxParticipants() != 6 {
		t.Errorf("room config %v", c.RoomConfig)
	}
}

func TestWebhookReceiver(t *testing.T) {
	body := []byte(`{"event":"room_started","id":"EV_1","createdAt":"1700000000","room":{"name":"r","sid":"RM_1"},"somethingNew":{"x":1}}`)
	signature, err := SignWebhook(body, testKey, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	receiver := NewWebhookReceiver(testKeys)

	event, err := receiver.Receive(body, signature)
	if err != nil {
		t.Fatal(err)
	}
	if event.GetEvent() != "room_started" || event.GetRoom().GetName() != "r" || event.GetCreatedAt() != 1700000000 {
		t.Errorf("event %v", event)
	}

	r := httptest.NewRequest("POST", "/", strings.NewReader(string(body)))
	r.Header.Set("Authorization", signature)
	if _, err := receiver.ReceiveRequest(r); err != nil {
		t.Errorf("from a request: %v", err)
	}

	tampered := append([]byte{}, body...)
	tampered[10] ^= 1
	if _, err := receiver.Receive(tampered, signature); !errors.Is(err, ErrWebhookBadChecksum) {
		t.Errorf("tampered: %v", err)
	}
	if _, err := receiver.Receive(body, ""); !errors.Is(err, ErrWebhookNoAuthorization) {
		t.Errorf("no header: %v", err)
	}
	// Senders send a bare token, so anything else is not from one.
	if _, err := receiver.Receive(body, "Bearer "+signature); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("bearer: %v", err)
	}
	if _, err := NewWebhookReceiver(Keys{testKey: "not the secret"}).Receive(body, signature); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("wrong secret: %v", err)
	}
	if _, err := NewWebhookReceiver(Keys{"other": testSecret}).Receive(body, signature); !errors.Is(err, ErrUnknownAPIKey) {
		t.Errorf("unknown key: %v", err)
	}
	// A valid token with no body hash covers nothing.
	noSum, err := NewAccessToken(testKey, testSecret).ToJWT()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Receive(body, noSum); !errors.Is(err, ErrWebhookBadChecksum) {
		t.Errorf("no hash: %v", err)
	}
	notJSON := []byte("not json")
	signature, err = SignWebhook(notJSON, testKey, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Receive(notJSON, signature); err == nil {
		t.Error("a body that is not JSON should fail")
	}
}

func TestClientFromEnvironment(t *testing.T) {
	t.Setenv("STREAMKIT_URL", "")
	if _, err := NewClientFromEnv(); err == nil {
		t.Error("no address should fail")
	}
	t.Setenv("STREAMKIT_URL", "wss://example.invalid")
	t.Setenv("STREAMKIT_API_KEY", testKey)
	t.Setenv("STREAMKIT_API_SECRET", testSecret)
	c, err := NewClientFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.Room.url != "https://example.invalid" || c.Egress.apiKey != testKey || c.Ingress.apiSecret != testSecret {
		t.Errorf("client: %+v", c.Room.base)
	}
}

func TestErrorsDescribeThemselves(t *testing.T) {
	e := &Error{Code: "not_found", Message: "gone", Status: 404}
	if got := e.Error(); !strings.Contains(got, "not_found") || !strings.Contains(got, "gone") || !strings.Contains(got, "404") {
		t.Errorf("message: %s", got)
	}
	if ErrorCode(e) != "not_found" || ErrorCode(errors.New("other")) != "" {
		t.Error("ErrorCode")
	}
	if errors.Is(e, ErrNotSupported) {
		t.Error("a missing thing is not an unsupported call")
	}
	if !errors.Is(&Error{Code: "unimplemented"}, ErrNotSupported) {
		t.Error("unimplemented should be ErrNotSupported")
	}
}

func TestCreateTokenUsesTheClientsKey(t *testing.T) {
	token, err := NewRoomClient("ws://127.0.0.1:1", testKey, testSecret).CreateToken().SetIdentity("a").ToJWT()
	if err != nil {
		t.Fatal(err)
	}
	if c, err := VerifyToken(token, testKeys); err != nil || c.Identity != "a" {
		t.Errorf("claims: %+v, %v", c, err)
	}
}

// A recording takes longer to fetch than an API call should, so an API timeout must not cut it off.
func TestDownloadOutlastsTheAPITimeout(t *testing.T) {
	const parts = 5
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for range parts {
			_, _ = w.Write([]byte("part"))
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
	}))
	defer server.Close()
	c := NewEgressClient(server.URL, testKey, testSecret, WithHTTPClient(&http.Client{Timeout: 200 * time.Millisecond}))
	var out strings.Builder
	n, err := c.DownloadRecording(context.Background(), "EG_1", &out)
	if err != nil {
		t.Fatal(err)
	}
	if n != parts*4 || out.String() != strings.Repeat("part", parts) {
		t.Errorf("got %d bytes: %q", n, out.String())
	}
	// Everything else keeps its timeout.
	if _, err := c.GetEgress(context.Background(), "EG_1"); err == nil {
		t.Error("an API call slower than the timeout should fail")
	}
}

func TestHTTPURL(t *testing.T) {
	for in, want := range map[string]string{
		"ws://h:1": "http://h:1", "wss://h": "https://h", "http://h/": "http://h", "https://h": "https://h",
	} {
		if got := httpURL(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}
