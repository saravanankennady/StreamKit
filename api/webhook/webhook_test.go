package webhook

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"testing"

	"elnova-streamkit.local/server/auth"
)

func TestBuildRequestSignsTheBody(t *testing.T) {
	key := auth.Key{APIKey: "k", Secret: "a-long-enough-secret-for-testing-0001"}
	body := []byte(`{"event":"room_started"}`)
	req, err := BuildRequest(context.Background(), "https://example.com/hook", body, key)
	if err != nil {
		t.Fatal(err)
	}
	if ct := req.Header.Get("Content-Type"); ct != "application/webhook+json" {
		t.Fatalf("content type = %q", ct)
	}
	token := req.Header.Get("Authorization")
	if len(token) < 7 || token[:7] == "Bearer " {
		t.Fatalf("receivers need the raw token, got %q", token)
	}
	verified, err := auth.Verify(token, auth.StaticKeys{Keys: map[string]string{"k": key.Secret}})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if verified.Grants.Sha256 != base64.StdEncoding.EncodeToString(sum[:]) {
		t.Fatal("hash does not match the body")
	}
	sent, _ := io.ReadAll(req.Body)
	if string(sent) != string(body) {
		t.Fatal("body changed")
	}
}

func TestIsPrivateIP(t *testing.T) {
	private := []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "::1", "fe80::1", "fc00::1", "0.0.0.0"}
	for _, ip := range private {
		if !IsPrivateIP(net.ParseIP(ip)) {
			t.Errorf("%s should be private", ip)
		}
	}
	public := []string{"8.8.8.8", "93.184.216.34", "100.128.0.1", "2606:4700::1111"}
	for _, ip := range public {
		if IsPrivateIP(net.ParseIP(ip)) {
			t.Errorf("%s should be public", ip)
		}
	}
	if !IsPrivateIP(nil) {
		t.Error("an unparsable address is not safe to connect to")
	}
}

func TestCheckURL(t *testing.T) {
	for _, bad := range []string{"http://localhost/x", "http://127.0.0.1/x", "http://169.254.169.254/latest/meta-data", "http://[::1]/x"} {
		if err := CheckURL(bad, false); !errors.Is(err, ErrPrivateAddress) {
			t.Errorf("%s: %v", bad, err)
		}
		if err := CheckURL(bad, true); err != nil {
			t.Errorf("%s with private allowed: %v", bad, err)
		}
	}
	if err := CheckURL("https://hooks.example.com/x", false); err != nil {
		t.Fatal(err)
	}
}

func TestIsPrivateIPCoversWhatIsNotOnTheInternet(t *testing.T) {
	for _, private := range []string{
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.100.100.200", "0.0.0.0", "0.1.2.3",
		"192.0.0.8", "198.18.0.1", "198.19.255.255", "240.0.0.1", "255.255.255.255", "168.63.129.16",
		"::1", "::", "fe80::1", "fc00::1", "fd00:ec2::254", "fec0::1", "ff02::1", "100::1",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254", "::ffff:198.18.0.1",
		"64:ff9b::7f00:1", "64:ff9b::a9fe:a9fe", "64:ff9b::a00:5", "64:ff9b:1::1", "2002:7f00:1::", "2002:a00:1::", "2002:a9fe:a9fe::", "::7f00:1", "::a00:1",
	} {
		if !IsPrivateIP(net.ParseIP(private)) {
			t.Errorf("%s should count as private", private)
		}
	}
	for _, public := range []string{"1.1.1.1", "8.8.8.8", "142.250.1.1", "2606:4700:4700::1111", "2001:4860:4860::8888", "::ffff:8.8.8.8", "64:ff9b::808:808", "2002:808:808::"} {
		if IsPrivateIP(net.ParseIP(public)) {
			t.Errorf("%s should count as public", public)
		}
	}
	if !IsPrivateIP(nil) {
		t.Error("something that cannot be read should not be trusted")
	}
}
