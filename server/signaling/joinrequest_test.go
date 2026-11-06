package signaling

import (
	"net/http/httptest"
	"testing"

	"elnova-streamkit.local/pkg/pb"
)

func TestJoinClientInfoComesFromTheHeaderWhenTheClientLeavesItOut(t *testing.T) {
	const chrome = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36"
	const firefox = "Mozilla/5.0 (Android 14; Mobile; rv:128.0) Gecko/128.0 Firefox/128.0"
	const safari = "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15"
	request := func(ua string) *pb.ClientInfo {
		r := httptest.NewRequest("GET", "/rtc/v1", nil)
		r.Header.Set("User-Agent", ua)
		return joinClientInfo(&pb.JoinRequest{ClientInfo: &pb.ClientInfo{Sdk: pb.ClientInfo_JS, Protocol: 17}}, r)
	}
	if got := request(chrome); got.GetBrowser() != "Chrome" || got.GetOs() != "Linux" || got.GetProtocol() != 17 {
		t.Errorf("chrome: %v", got)
	}
	if got := request(firefox); got.GetBrowser() != "Firefox" || got.GetOs() != "Android" {
		t.Errorf("firefox: %v", got)
	}
	if got := request(safari); got.GetBrowser() != "Safari" || got.GetOs() != "Mac OS X" {
		t.Errorf("safari: %v", got)
	}

	r := httptest.NewRequest("GET", "/rtc/v1", nil)
	r.Header.Set("User-Agent", chrome)
	said := joinClientInfo(&pb.JoinRequest{ClientInfo: &pb.ClientInfo{Sdk: pb.ClientInfo_JS, Browser: "said", Os: "said"}}, r)
	if said.GetBrowser() != "said" || said.GetOs() != "said" {
		t.Errorf("what the client said should stand: %v", said)
	}
	native := joinClientInfo(&pb.JoinRequest{ClientInfo: &pb.ClientInfo{Sdk: pb.ClientInfo_GO}}, r)
	if native.GetBrowser() != "" {
		t.Errorf("a native SDK has no browser: %v", native)
	}
}
