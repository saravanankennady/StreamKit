package telemetry

import "testing"

func TestRouteGroupHidesCustomerData(t *testing.T) {
	cases := map[string]string{
		"/rtc":                                     "/rtc",
		"/rtc/validate":                            "/rtc",
		"/twirp/livekit.RoomService/CreateRoom":    "/twirp/livekit.RoomService/CreateRoom",
		"/api/v1/rooms/secret-room-name":           "/api/v1/rooms",
		"/api/v1/participants/secret-room/someone": "/api/v1/participants",
		"/metrics":                                 "/metrics",
		"/":                                        "/",
	}
	for path, want := range cases {
		if got := routeGroup(path); got != want {
			t.Errorf("%s: got %s, want %s", path, got, want)
		}
	}
}
