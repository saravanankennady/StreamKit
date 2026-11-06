package signaling

import (
	"net/http"
	"regexp"
	"strings"

	"google.golang.org/protobuf/proto"

	"elnova-streamkit.local/pkg/pb"
)

// joinClientInfo returns what a join request says about its client. Browsers leave out what the rest of this server needs
// to know about them, which the User-Agent header says instead.
func joinClientInfo(join *pb.JoinRequest, r *http.Request) *pb.ClientInfo {
	info := &pb.ClientInfo{}
	if c := join.GetClientInfo(); c != nil {
		info = proto.Clone(c).(*pb.ClientInfo)
	}
	info.Address = r.RemoteAddr
	switch info.GetSdk() {
	case pb.ClientInfo_JS, pb.ClientInfo_REACT_NATIVE, pb.ClientInfo_FLUTTER, pb.ClientInfo_UNITY, pb.ClientInfo_UNKNOWN:
		applyUserAgent(info, r.UserAgent())
	}
	return info
}

var (
	firefoxVersion = regexp.MustCompile(`Firefox/([\d.]+)`)
	edgeVersion    = regexp.MustCompile(`Edg(?:e|A|iOS)?/([\d.]+)`)
	chromeVersion  = regexp.MustCompile(`(?:Chrome|CriOS)/([\d.]+)`)
	safariVersion  = regexp.MustCompile(`Version/([\d.]+).*Safari/`)
)

// applyUserAgent fills in the browser and operating system, leaving alone anything the client did say. It only has to be
// good enough for the per-browser rules: Firefox, and whether it is on Linux or Android.
func applyUserAgent(info *pb.ClientInfo, ua string) {
	if info.GetBrowser() == "" {
		for _, b := range []struct {
			name string
			re   *regexp.Regexp
		}{
			{"Firefox", firefoxVersion},
			{"Edge", edgeVersion},
			{"Chrome", chromeVersion},
			{"Safari", safariVersion},
		} {
			if m := b.re.FindStringSubmatch(ua); m != nil {
				info.Browser, info.BrowserVersion = b.name, m[1]
				break
			}
		}
	}
	if info.GetOs() == "" {
		switch {
		case strings.Contains(ua, "Android"):
			info.Os = "Android"
		case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"):
			info.Os = "iOS"
		case strings.Contains(ua, "Windows"):
			info.Os = "Windows"
		case strings.Contains(ua, "Mac OS X"):
			info.Os = "Mac OS X"
		case strings.Contains(ua, "CrOS"):
			info.Os = "Chrome OS"
		case strings.Contains(ua, "Linux"):
			info.Os = "Linux"
		}
	}
}
