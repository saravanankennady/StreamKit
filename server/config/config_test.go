package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDevDefaults(t *testing.T) {
	t.Setenv("STREAMKIT_KEYS", "")
	c, err := Load("", true)
	if err != nil {
		t.Fatal(err)
	}
	if c.Keys[DevKey] != DevSecret || !c.RTC.IncludeLoopback {
		t.Fatalf("dev defaults not applied: %+v", c)
	}
}

func TestProductionRequiresStrongKeys(t *testing.T) {
	t.Setenv("STREAMKIT_KEYS", "")
	if _, err := Load("", false); err == nil {
		t.Fatal("expected an error with no keys")
	}
	t.Setenv("STREAMKIT_KEYS", "k: short")
	if _, err := Load("", false); err == nil {
		t.Fatal("expected an error for a short secret")
	}
	t.Setenv("STREAMKIT_KEYS", "k: 0123456789012345678901234567890123")
	if _, err := Load("", false); err != nil {
		t.Fatal(err)
	}
}

func TestSinglePeerConnectionIsOnUnlessTurnedOff(t *testing.T) {
	t.Setenv("STREAMKIT_KEYS", "")
	c, err := Load("", true)
	if err != nil {
		t.Fatal(err)
	}
	if !c.RTC.SinglePeerConnection {
		t.Fatal("it should be on by default")
	}
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte("rtc:\n  single_peer_connection: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = Load(p, true)
	if err != nil {
		t.Fatal(err)
	}
	if c.RTC.SinglePeerConnection {
		t.Fatal("a file should be able to turn it off")
	}
}

func TestRegistrationIsClosedUnlessSaidOtherwise(t *testing.T) {
	t.Setenv("STREAMKIT_KEYS", "k: 0123456789012345678901234567890123")
	t.Setenv("STREAMKIT_ALLOW_REGISTRATION", "")
	production, err := Load("", false)
	if err != nil {
		t.Fatal(err)
	}
	if production.RegistrationOpen() {
		t.Fatal("registration should be closed by default")
	}
	dev, err := Load("", true)
	if err != nil {
		t.Fatal(err)
	}
	if !dev.RegistrationOpen() {
		t.Fatal("development should stay open, with nobody else able to reach it")
	}

	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte("control_plane:\n  allow_registration: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(p, false); err != nil || !c.RegistrationOpen() {
		t.Fatalf("open in the file: %v", err)
	}
	if err := os.WriteFile(p, []byte("control_plane:\n  allow_registration: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(p, true); err != nil || c.RegistrationOpen() {
		t.Fatalf("closed in the file, with --dev: %v", err)
	}
	t.Setenv("STREAMKIT_ALLOW_REGISTRATION", "true")
	if c, err := Load(p, false); err != nil || !c.RegistrationOpen() {
		t.Fatalf("the environment should win: %v", err)
	}
	t.Setenv("STREAMKIT_ALLOW_REGISTRATION", "perhaps")
	if _, err := Load("", false); err == nil {
		t.Fatal("a setting that is not true or false should say so")
	}
}

func TestMeetingAndConnectLimitDefaults(t *testing.T) {
	c := Default()
	if c.Room.MeetingMaxParticipants == 0 {
		t.Fatal("meetings should have a participant cap by default")
	}
	if c.MeetingsPerProject() == 0 {
		t.Fatal("projects should have a meeting cap by default")
	}
	c.ControlPlane.MaxMeetingsPerProject = -1
	if c.MeetingsPerProject() != 0 {
		t.Fatal("negative should mean no limit")
	}
	c.ControlPlane.MaxMeetingsPerProject = 5
	if c.MeetingsPerProject() != 5 {
		t.Fatal("a set limit should be used")
	}

	if perMinute, burst := c.RTCConnectLimit(); perMinute == 0 || burst == 0 {
		t.Fatal("production should have a connection limit by default")
	}
	c.Limits.RTCConnectsPerMinute = -1
	if perMinute, _ := c.RTCConnectLimit(); perMinute != 0 {
		t.Fatal("negative should mean no limit")
	}
	c.Limits.RTCConnectsPerMinute = 0
	c.Development = true
	if perMinute, _ := c.RTCConnectLimit(); perMinute != 0 {
		t.Fatal("--dev has every client on one address, so it should have no limit unless asked")
	}
	c.Limits.RTCConnectsPerMinute, c.Limits.RTCConnectBurst = 7, 2
	if perMinute, burst := c.RTCConnectLimit(); perMinute != 7 || burst != 2 {
		t.Fatalf("a set limit should be used: %d %d", perMinute, burst)
	}
}

func TestHopSecretComesFromWhatNodesShare(t *testing.T) {
	master := func(key string) []byte {
		return (&Config{ControlPlane: ControlPlane{MasterKey: key}}).HopSecret()
	}
	static := func(keys map[string]string) []byte { return (&Config{Keys: keys}).HopSecret() }
	if string(master("a")) != string(master("a")) || string(static(map[string]string{"k": "s", "l": "t"})) != string(static(map[string]string{"l": "t", "k": "s"})) {
		t.Fatal("nodes with the same settings got different secrets")
	}
	if string(master("a")) == string(master("b")) || string(static(map[string]string{"k": "s"})) == string(static(map[string]string{"k": "t"})) {
		t.Fatal("different settings gave the same secret")
	}
	if string(master("a")) == "a" {
		t.Fatal("the secret is the master key itself")
	}
}

func TestParseKeys(t *testing.T) {
	k, err := ParseKeys("a: 1, b:2")
	if err != nil || k["a"] != "1" || k["b"] != "2" {
		t.Fatalf("got %v, %v", k, err)
	}
	for _, bad := range []string{"a", "a:", ":b", "a:1,"} {
		if _, err := ParseKeys(bad); err == nil {
			t.Fatalf("%q should not parse", bad)
		}
	}
}

func TestFileOverridesDefaults(t *testing.T) {
	t.Setenv("STREAMKIT_KEYS", "")
	p := filepath.Join(t.TempDir(), "c.yaml")
	body := "port: 9000\nkeys:\n  k: 0123456789012345678901234567890123\nroom:\n  empty_timeout: 10\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p, false)
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 9000 || c.Room.EmptyTimeout != 10 || c.Room.DepartureTimeout != 20 {
		t.Fatalf("unexpected config: %+v", c)
	}
}

func TestConfigCanComeFromTheEnvironment(t *testing.T) {
	t.Setenv("STREAMKIT_KEYS", "")
	t.Setenv("STREAMKIT_CONFIG_YAML", "region: from-env\nmedia:\n  egress:\n    max_jobs: 7\n")
	t.Setenv("STREAMKIT_MASTER_KEY", "")
	c, err := Load("", true)
	if err != nil {
		t.Fatal(err)
	}
	if c.Region != "from-env" || c.Media.Egress.MaxJobs != 7 {
		t.Fatalf("%+v", c.Media.Egress)
	}

	// On top of a file, setting by setting, and under the variables that name one thing.
	file := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(file, []byte("region: from-file\nlog_level: warn\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STREAMKIT_S3_SECRET_KEY", "from-its-own-variable")
	t.Setenv("STREAMKIT_CONFIG_YAML", "region: from-env\nmedia:\n  storage:\n    s3:\n      secret_key: from-yaml\n")
	if c, err = Load(file, true); err != nil {
		t.Fatal(err)
	}
	if c.Region != "from-env" || c.LogLevel != "warn" || c.Media.Storage.S3.SecretKey != "from-its-own-variable" {
		t.Fatalf("%q %q %q", c.Region, c.LogLevel, c.Media.Storage.S3.SecretKey)
	}

	t.Setenv("STREAMKIT_CONFIG_YAML", "region: [")
	if _, err := Load("", true); err == nil || !strings.Contains(err.Error(), "STREAMKIT_CONFIG_YAML") {
		t.Fatalf("a bad value should say where it came from: %v", err)
	}
}
