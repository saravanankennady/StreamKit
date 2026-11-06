package participant

import (
	"log/slog"
	"testing"

	"google.golang.org/protobuf/proto"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/config"
)

// hearing is a room that only remembers which data packets it was given.
type hearing struct {
	Listener
	seqs []uint32
}

func (h *hearing) OnDataPacket(_ *Participant, _ pb.DataPacket_Kind, dp *pb.DataPacket) {
	h.seqs = append(h.seqs, dp.GetSequence())
}

func chattyParticipant(messages, bytes int) (*Participant, *hearing) {
	h := &hearing{}
	p := &Participant{
		params:     Params{Listener: h, MaxDataPacketBytes: 64 * 1024, MaxDataMessagesPerSecond: messages, MaxDataBytesPerSecond: bytes},
		log:        slog.New(slog.DiscardHandler),
		permission: &pb.ParticipantPermission{CanPublishData: true},
	}
	p.setupDataLimits()
	return p, h
}

func dataPacket(t *testing.T, seq uint32, size int) []byte {
	t.Helper()
	data, err := proto.Marshal(&pb.DataPacket{Sequence: seq, Value: &pb.DataPacket_User{User: &pb.UserPacket{Payload: make([]byte, size)}}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestAFloodOfDataIsCutOff(t *testing.T) {
	const perSecond = 100
	p, h := chattyParticipant(perSecond, 0)
	sent := 20 * perSecond
	for i := range sent {
		p.onDataMessage(pb.DataPacket_RELIABLE, dataPacket(t, uint32(i+1), 10))
	}
	// A loop this short takes a good deal less than a second, so what got through is about the burst.
	if len(h.seqs) >= sent/2 || len(h.seqs) < perSecond {
		t.Fatalf("%d of %d got through at a limit of %d a second", len(h.seqs), sent, perSecond)
	}
}

func TestAFloodOfBytesIsCutOff(t *testing.T) {
	const perSecond = 256 * 1024
	p, h := chattyParticipant(0, perSecond)
	sent := 200
	for range sent {
		p.onDataMessage(pb.DataPacket_LOSSY, dataPacket(t, 0, 32*1024))
	}
	if total := len(h.seqs) * 32 * 1024; total >= sent*32*1024/2 {
		t.Fatalf("%d bytes got through at a limit of %d a second", total, perSecond)
	}
}

// One message as large as is allowed still gets through, whatever the limit is set to.
func TestALimitDoesNotRefuseTheLargestMessage(t *testing.T) {
	p, h := chattyParticipant(0, 1024)
	p.onDataMessage(pb.DataPacket_RELIABLE, dataPacket(t, 1, 60*1024))
	if len(h.seqs) != 1 {
		t.Fatal("a message within the size limit could never be sent")
	}
}

// What real use looks like, from tests/interoperability/sdk.test.mjs: 500 messages at once, all of which must arrive.
func TestTheDefaultLimitsLetABurstThrough(t *testing.T) {
	limits := config.Default().Limits
	p, h := chattyParticipant(limits.MaxDataMessagesPerSecond, limits.MaxDataBytesPerSecond)
	for i := range 500 {
		p.onDataMessage(pb.DataPacket_RELIABLE, dataPacket(t, uint32(i+1), 2000))
	}
	if len(h.seqs) != 500 {
		t.Fatalf("%d of 500 got through", len(h.seqs))
	}
}

// A client sends reliable data again after a resume. Doing so must not use up what it is allowed, and a message that was
// refused must not count as delivered, or going round again could never deliver it.
func TestResendingOverTheLimitKeepsDeliveryExactlyOnce(t *testing.T) {
	p, h := chattyParticipant(10, 0)
	// Nothing comes back by itself during this test, however slowly the machine is running.
	p.dataMessages.SetLimit(0)
	burst := p.dataMessages.Burst()
	seq := uint32(0)
	for range burst - 1 {
		seq++
		p.onDataMessage(pb.DataPacket_RELIABLE, dataPacket(t, seq, 10))
	}
	if len(h.seqs) != burst-1 {
		t.Fatalf("%d of the first %d got through", len(h.seqs), burst-1)
	}

	// All of these are duplicates, however many there are, and they leave the last of the allowance alone.
	for s := uint32(1); s <= seq; s++ {
		p.onDataMessage(pb.DataPacket_RELIABLE, dataPacket(t, s, 10))
	}
	if len(h.seqs) != burst-1 {
		t.Fatalf("a duplicate got through: %d", len(h.seqs))
	}
	seq++
	p.onDataMessage(pb.DataPacket_RELIABLE, dataPacket(t, seq, 10))
	if len(h.seqs) != burst {
		t.Fatal("resending used up what this client is allowed")
	}

	// Out of allowance, this is refused and not recorded, so it is not a duplicate when it comes again.
	seq++
	before := len(h.seqs)
	p.onDataMessage(pb.DataPacket_RELIABLE, dataPacket(t, seq, 10))
	if len(h.seqs) != before {
		t.Fatal("over the limit, and it got through")
	}
	if got := p.lastReliableSeq.Load(); got != seq-1 {
		t.Fatalf("a refused message was recorded as received: %d, want %d", got, seq-1)
	}
	p.dataMessages.SetLimit(1000)
	p.dataMessages.SetBurst(10)
	for len(h.seqs) == before {
		p.onDataMessage(pb.DataPacket_RELIABLE, dataPacket(t, seq, 10))
	}
	if got := h.seqs[len(h.seqs)-1]; got != seq {
		t.Fatalf("got %d, want %d", got, seq)
	}
	if got := p.lastReliableSeq.Load(); got != seq {
		t.Fatalf("recorded %d, want %d", got, seq)
	}
}

func TestNoLimitMeansNoLimit(t *testing.T) {
	p, h := chattyParticipant(0, 0)
	for i := range 5000 {
		p.onDataMessage(pb.DataPacket_LOSSY, dataPacket(t, uint32(i), 10))
	}
	if len(h.seqs) != 5000 {
		t.Fatalf("%d of 5000 got through", len(h.seqs))
	}
}
