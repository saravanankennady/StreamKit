package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var testKeys = StaticKeys{ProjectID: "p1", Keys: map[string]string{"k1": "a-long-enough-secret-for-testing-0001"}}

func testKey() Key { k, _ := testKeys.Lookup("k1"); return k }

func TestRoundTrip(t *testing.T) {
	yes := true
	tok, err := Mint(testKey(), &Grants{
		Identity: "alice",
		Name:     "Alice",
		Metadata: "m",
		Video:    &VideoGrant{RoomJoin: true, Room: "r1", CanUpdateOwnMetadata: &yes},
	}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	v, err := Verify(tok, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	g := v.Grants
	if g.Identity != "alice" || g.Name != "Alice" || g.Metadata != "m" || !g.Video.RoomJoin || g.Video.Room != "r1" {
		t.Fatalf("grants not preserved: %+v %+v", g, g.Video)
	}
	if v.Key.ProjectID != "p1" {
		t.Fatalf("project = %q", v.Key.ProjectID)
	}
	if !g.Video.GetCanUpdateOwnMetadata() {
		t.Fatal("canUpdateOwnMetadata lost")
	}
}

func TestPermissionDefaults(t *testing.T) {
	v := &VideoGrant{}
	if !v.GetCanPublish() || !v.GetCanSubscribe() || !v.GetCanPublishData() {
		t.Fatal("publish, subscribe and data default to allowed")
	}
	if v.GetCanUpdateOwnMetadata() {
		t.Fatal("canUpdateOwnMetadata defaults to denied")
	}
	no := false
	v = &VideoGrant{CanPublish: &no}
	if v.CanPublishSource("camera") {
		t.Fatal("source allowed although canPublish is false")
	}
	v = &VideoGrant{CanPublishSources: []string{"camera"}}
	if !v.CanPublishSource("CAMERA") || v.CanPublishSource("microphone") {
		t.Fatal("source filter not applied")
	}
	if (&VideoGrant{CanPublish: &no}).GetCanPublishData() {
		t.Fatal("unset canPublishData follows canPublish")
	}
	yes := true
	if !(&VideoGrant{CanPublish: &no, CanPublishData: &yes}).GetCanPublishData() {
		t.Fatal("canPublishData, when set, is what counts")
	}
}

func TestRoomConfigClaim(t *testing.T) {
	key := testKey()
	sign := func(config any) string {
		tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"iss": key.APIKey, "sub": "a", "exp": time.Now().Add(time.Minute).Unix(), "roomConfig": config,
		}).SignedString([]byte(key.Secret))
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	// Tools write either spelling of a field, and newer ones write fields this server does not know.
	v, err := Verify(sign(map[string]any{"emptyTimeout": 5, "max_participants": 6, "fromTheFuture": true}), testKeys)
	if err != nil {
		t.Fatal(err)
	}
	if v.Grants.RoomConfig.GetEmptyTimeout() != 5 || v.Grants.RoomConfig.GetMaxParticipants() != 6 {
		t.Fatalf("room config: %v", v.Grants.RoomConfig)
	}
	if clone := v.Grants.Clone(); clone.RoomConfig.GetEmptyTimeout() != 5 {
		t.Fatal("a copy lost the room configuration")
	}
	if _, err := Verify(sign("not an object"), testKeys); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("malformed room config: %v", err)
	}
}

func TestRejections(t *testing.T) {
	good, _ := Mint(testKey(), &Grants{Identity: "a", Video: &VideoGrant{RoomJoin: true}}, time.Minute)

	cases := []struct {
		name string
		tok  func() string
		want error
	}{
		{"empty", func() string { return "" }, ErrInvalidToken},
		{"garbage", func() string { return "not-a-jwt" }, ErrInvalidToken},
		{"unknown key", func() string {
			s, _ := Mint(Key{APIKey: "other", Secret: testKey().Secret}, &Grants{Identity: "a"}, time.Minute)
			return s
		}, ErrInvalidAPIKey},
		{"wrong secret", func() string {
			s, _ := Mint(Key{APIKey: "k1", Secret: "wrong-secret-wrong-secret-wrong-secret"}, &Grants{Identity: "a"}, time.Minute)
			return s
		}, ErrInvalidToken},
		{"tampered", func() string { return good[:len(good)-2] + "xx" }, ErrInvalidToken},
		{"expired beyond leeway", func() string {
			now := time.Now()
			s, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
				Issuer: "k1", ExpiresAt: jwt.NewNumericDate(now.Add(-2 * Leeway)),
			}).SignedString([]byte(testKey().Secret))
			return s
		}, ErrInvalidToken},
		{"no expiry", func() string {
			s, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{Issuer: "k1"}).SignedString([]byte(testKey().Secret))
			return s
		}, ErrInvalidToken},
		{"other hmac algorithm", func() string {
			s, _ := jwt.NewWithClaims(jwt.SigningMethodHS384, jwt.RegisteredClaims{
				Issuer: "k1", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute)),
			}).SignedString([]byte(testKey().Secret))
			return s
		}, ErrInvalidToken},
		{"alg none", func() string {
			s, _ := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.RegisteredClaims{
				Issuer: "k1", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute)),
			}).SignedString(jwt.UnsafeAllowNoneSignatureType)
			return s
		}, ErrInvalidToken},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Verify(c.tok(), testKeys); !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}

func TestExpiredWithinLeewayAccepted(t *testing.T) {
	s, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Issuer: "k1", ExpiresAt: jwt.NewNumericDate(time.Now().Add(-Leeway / 2)),
	}).SignedString([]byte(testKey().Secret))
	if _, err := Verify(s, testKeys); err != nil {
		t.Fatal(err)
	}
}

func TestCloneIsDeep(t *testing.T) {
	g := &Grants{Identity: "a", Video: &VideoGrant{Room: "r"}, Attributes: map[string]string{"x": "1"}}
	c := g.Clone()
	c.Video.Room = "other"
	c.Attributes["x"] = "2"
	if g.Video.Room != "r" || g.Attributes["x"] != "1" || c.Identity != "a" {
		t.Fatal("clone shares state with original")
	}
}

func TestIdentityFallsBackToJTI(t *testing.T) {
	s, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Issuer: "k1", ID: "from-jti", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute)),
	}).SignedString([]byte(testKey().Secret))
	v, err := Verify(s, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	if v.Grants.Identity != "from-jti" {
		t.Fatalf("identity = %q", v.Grants.Identity)
	}
}
