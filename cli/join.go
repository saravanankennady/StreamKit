package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"elnova-streamkit.local/pkg/pb"
	streamkit "elnova-streamkit.local/sdks/server-go"
	"elnova-streamkit.local/tests/load/loadgen"
)

// The test pattern: a moving picture and a tone, encoded ahead of time by media/make.sh. Encoding needs libvpx and
// libopus, and this way the CLI needs neither.
var (
	//go:embed media/pattern.ivf
	patternVideo []byte
	//go:embed media/tone.ogg
	patternAudio []byte
)

func testPattern() (video, audio *loadgen.Frames, err error) {
	if video, err = loadgen.ParseIVF(bytes.NewReader(patternVideo)); err != nil {
		return nil, nil, err
	}
	if audio, err = loadgen.ParseOgg(bytes.NewReader(patternAudio)); err != nil {
		return nil, nil, err
	}
	return video, audio, nil
}

// joinEvent is one thing a participant saw. With --json each is a line of output, which is how the SDK tests watch what
// their calls did to a real participant.
type joinEvent struct {
	Event      string            `json:"event"`
	Room       string            `json:"room,omitempty"`
	RoomSID    string            `json:"room_sid,omitempty"`
	Metadata   *string           `json:"metadata,omitempty"`
	Identity   string            `json:"identity,omitempty"`
	SID        string            `json:"sid,omitempty"`
	Name       string            `json:"name,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
	CanPublish *bool             `json:"can_publish,omitempty"`
	Server     string            `json:"server,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	Source     string            `json:"source,omitempty"`
	Muted      *bool             `json:"muted,omitempty"`
	Packets    int64             `json:"packets,omitempty"`
	Bytes      int64             `json:"bytes,omitempty"`
	Topic      string            `json:"topic,omitempty"`
	Channel    string            `json:"channel,omitempty"`
	From       string            `json:"from,omitempty"`
	Data       string            `json:"data_base64,omitempty"`
	Reason     string            `json:"reason,omitempty"`
	Error      string            `json:"error,omitempty"`
	Tracks     []string          `json:"tracks,omitempty"`
}

func joinCommand() *command {
	var (
		identity, name, metadata, token, publishFiles string
		attributes                                    repeatedString
		publishPattern, videoOnly, audioOnly          bool
		noAutoSubscribe                               bool
		duration                                      time.Duration
	)
	return &command{
		name:  "join",
		usage: "join a room as a real participant, and optionally publish a test pattern",
		args:  "ROOM_NAME",
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&identity, "identity", "", "`ID` of the participant")
			fs.StringVar(&identity, "i", "", "`ID` of the participant")
			fs.StringVar(&name, "name", "", "display `NAME`")
			fs.StringVar(&metadata, "metadata", "", "participant `METADATA`")
			fs.Var(&attributes, "attribute", "attribute as `KEY=VALUE`; can be repeated")
			fs.StringVar(&token, "token", "", "join with this `TOKEN` instead of signing one")
			fs.BoolVar(&publishPattern, "publish-test-pattern", false, "publish a moving test picture and a tone")
			fs.BoolVar(&publishPattern, "publish-demo", false, "the same as --publish-test-pattern")
			fs.BoolVar(&videoOnly, "video-only", false, "with --publish-test-pattern, publish only the picture")
			fs.BoolVar(&audioOnly, "audio-only", false, "with --publish-test-pattern, publish only the tone")
			fs.StringVar(&publishFiles, "publish", "", "publish these `FILES`: comma-separated .ivf (VP8) and .ogg (Opus)")
			fs.BoolVar(&noAutoSubscribe, "no-auto-subscribe", false, "do not subscribe to tracks unless told to")
			fs.DurationVar(&duration, "duration", 0, "leave after this long; by default stay until interrupted")
		},
		run: func(c *call) error {
			room, err := oneRoom(c)
			if err != nil {
				return err
			}
			creds, err := c.global.credentials()
			if err != nil {
				return err
			}
			if creds.URL == "" {
				return errors.New("no server address. Set STREAMKIT_URL, use --url or --project, or run 'streamkit project add'")
			}
			if identity == "" {
				identity = fmt.Sprintf("cli-%d", time.Now().UnixNano())
			}
			if token == "" {
				if err := creds.needSigning(); err != nil {
					return err
				}
				attrs, err := keyValues(attributes)
				if err != nil {
					return err
				}
				token, err = streamkit.NewAccessToken(creds.APIKey, creds.secret).
					SetIdentity(identity).SetName(name).SetMetadata(metadata).SetAttributes(attrs).SetValidFor(time.Hour).
					SetVideoGrant(&streamkit.VideoGrant{RoomJoin: true, Room: room}).ToJWT()
				if err != nil {
					return err
				}
			}

			client := &loadgen.Client{URL: creds.URL, Token: token, Identity: identity, NoAutoSubscribe: noAutoSubscribe}
			if publishPattern {
				video, audio, err := testPattern()
				if err != nil {
					return err
				}
				if !audioOnly {
					client.Video = video
				}
				if !videoOnly {
					client.Audio = audio
				}
			}
			if publishFiles != "" {
				for _, file := range strings.Split(publishFiles, ",") {
					switch {
					case strings.HasSuffix(file, ".ivf"):
						client.Video, err = loadgen.LoadIVF(file)
					case strings.HasSuffix(file, ".ogg"):
						client.Audio, err = loadgen.LoadOgg(file)
					default:
						err = fmt.Errorf("cannot publish %s: only .ivf (VP8) and .ogg (Opus) are supported", file)
					}
					if err != nil {
						return err
					}
				}
			}
			client.Publish = client.Video != nil || client.Audio != nil

			r := &joinReporter{c: c, tracks: map[string]*trackCount{}}
			client.Hooks = loadgen.Hooks{
				Signal: r.signal,
				Connected: func() {
					r.emit(joinEvent{Event: "connected"})
				},
				Track: func(t *webrtc.TrackRemote) {
					r.emit(joinEvent{Event: "track_subscribed", SID: t.ID(), Kind: t.Kind().String(), Identity: t.StreamID()})
				},
				Packet: r.packet,
				DataChannel: func(label string) {
					r.emit(joinEvent{Event: "data_channel_open", Channel: label})
				},
				Data: r.data,
			}
			ctx := c.ctx
			if duration > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, duration)
				defer cancel()
			}
			go r.reportMedia(ctx)
			err = client.Run(ctx)
			var left *loadgen.LeftError
			switch {
			case errors.As(err, &left):
				return nil
			case err != nil:
				r.emit(joinEvent{Event: "error", Error: err.Error()})
				return errSilent
			}
			r.emit(joinEvent{Event: "left", Reason: "CLIENT_INITIATED"})
			return nil
		},
	}
}

type trackCount struct {
	kind            string
	packets, bytes  int64
	reportedPackets int64
}

// joinReporter prints what a participant sees.
type joinReporter struct {
	c      *call
	mu     sync.Mutex
	tracks map[string]*trackCount
}

func (r *joinReporter) emit(e joinEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.c.global.json {
		data, err := json.Marshal(e)
		if err == nil {
			fmt.Fprintln(r.c.stdout, string(data))
		}
		return
	}
	data, _ := json.Marshal(e)
	var fields map[string]any
	_ = json.Unmarshal(data, &fields)
	delete(fields, "event")
	parts := make([]string, 0, len(fields))
	for _, k := range slices.Sorted(maps.Keys(fields)) {
		parts = append(parts, fmt.Sprintf("%s=%v", k, fields[k]))
	}
	fmt.Fprintf(r.c.stdout, "%s %s\n", e.Event, strings.Join(parts, " "))
}

func (r *joinReporter) signal(res *pb.SignalResponse) {
	switch m := res.GetMessage().(type) {
	case *pb.SignalResponse_Join:
		meta := m.Join.GetRoom().GetMetadata()
		r.emit(joinEvent{
			Event: "joined", Room: m.Join.GetRoom().GetName(), RoomSID: m.Join.GetRoom().GetSid(), Metadata: &meta,
			Identity: m.Join.GetParticipant().GetIdentity(), SID: m.Join.GetParticipant().GetSid(), Name: m.Join.GetParticipant().GetName(),
			Attributes: m.Join.GetParticipant().GetAttributes(), Server: m.Join.GetServerInfo().GetEdition().String() + " " + m.Join.GetServerInfo().GetVersion(),
		})
	case *pb.SignalResponse_TrackPublished:
		muted := m.TrackPublished.GetTrack().GetMuted()
		r.emit(joinEvent{
			Event: "track_published", SID: m.TrackPublished.GetTrack().GetSid(), Kind: strings.ToLower(m.TrackPublished.GetTrack().GetType().String()),
			Source: strings.ToLower(m.TrackPublished.GetTrack().GetSource().String()), Muted: &muted,
		})
	case *pb.SignalResponse_Mute:
		muted := m.Mute.GetMuted()
		r.emit(joinEvent{Event: "muted", SID: m.Mute.GetSid(), Muted: &muted})
	case *pb.SignalResponse_Update:
		for _, p := range m.Update.GetParticipants() {
			meta := p.GetMetadata()
			e := joinEvent{
				Event: "participant_update", Identity: p.GetIdentity(), SID: p.GetSid(), Name: p.GetName(), Metadata: &meta,
				Attributes: p.GetAttributes(), Reason: p.GetState().String(),
			}
			if perm := p.GetPermission(); perm != nil {
				canPublish := perm.GetCanPublish()
				e.CanPublish = &canPublish
			}
			for _, t := range p.GetTracks() {
				e.Tracks = append(e.Tracks, t.GetSid())
			}
			r.emit(e)
		}
	case *pb.SignalResponse_RoomUpdate:
		meta := m.RoomUpdate.GetRoom().GetMetadata()
		r.emit(joinEvent{Event: "room_update", Room: m.RoomUpdate.GetRoom().GetName(), Metadata: &meta})
	case *pb.SignalResponse_Leave:
		r.emit(joinEvent{Event: "left", Reason: m.Leave.GetReason().String()})
	}
}

func (r *joinReporter) packet(t *webrtc.TrackRemote, size int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := r.tracks[t.ID()]
	if count == nil {
		count = &trackCount{kind: t.Kind().String()}
		r.tracks[t.ID()] = count
	}
	count.packets++
	count.bytes += int64(size)
}

func (r *joinReporter) data(label string, packet *pb.DataPacket) {
	user := packet.GetUser()
	if user == nil {
		return
	}
	r.emit(joinEvent{
		Event: "data", Channel: label, Topic: user.GetTopic(), From: packet.GetParticipantIdentity(),
		Data: base64.StdEncoding.EncodeToString(user.GetPayload()),
	})
}

// reportMedia says, once a second, what has really arrived on each track.
func (r *joinReporter) reportMedia(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var events []joinEvent
		r.mu.Lock()
		for id, count := range r.tracks {
			if count.packets != count.reportedPackets {
				count.reportedPackets = count.packets
				events = append(events, joinEvent{Event: "media", SID: id, Kind: count.kind, Packets: count.packets, Bytes: count.bytes})
			}
		}
		r.mu.Unlock()
		for _, e := range events {
			r.emit(e)
		}
	}
}
