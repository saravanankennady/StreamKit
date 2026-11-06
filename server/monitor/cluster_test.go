package monitor

import "testing"

func TestMergeAddsNodes(t *testing.T) {
	local := &ProjectSnapshot{
		ActiveRooms: 1, Participants: 2, BitrateIn: 100, BitrateOut: 200, AvgRTTMs: 10, PacketLossRatio: 0.01,
		Rooms: []RoomStat{{Name: "a"}}, ParticipantStats: []ParticipantStat{{Identity: "x"}},
	}
	peer := &ProjectSnapshot{
		ActiveRooms: 2, Participants: 6, BitrateIn: 300, BitrateOut: 400, AvgRTTMs: 30, PacketLossRatio: 0.05,
		Rooms: []RoomStat{{Name: "b"}, {Name: "c"}}, ParticipantStats: []ParticipantStat{{Identity: "y"}},
	}
	got := Merge(local, []*ProjectSnapshot{peer})
	if got.ActiveRooms != 3 || got.Participants != 8 || got.BitrateIn != 400 || got.BitrateOut != 600 {
		t.Fatalf("counts do not add up: %+v", got)
	}
	if got.AvgRTTMs != 25 {
		t.Fatalf("rtt = %v, want the average weighted by participants", got.AvgRTTMs)
	}
	if got.PacketLossRatio != 0.05 {
		t.Fatalf("loss = %v, want the worst", got.PacketLossRatio)
	}
	if len(got.Rooms) != 3 || len(got.ParticipantStats) != 2 {
		t.Fatalf("lists were not merged: %+v", got)
	}
	if len(local.Rooms) != 1 {
		t.Fatal("merging changed the input")
	}
	if Merge(local, nil) != local {
		t.Fatal("with no peers there is nothing to do")
	}
}
