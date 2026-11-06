package main

import (
	"flag"
	"fmt"
	"text/tabwriter"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"elnova-streamkit.local/pkg/pb"
)

func roomCommand() *command {
	participants := participantCommand()
	participants.name = "participants"
	return &command{
		name:  "room",
		usage: "create, list, change and delete rooms",
		subs: []*command{
			roomCreateCommand(), roomListCommand(), roomUpdateCommand(), roomDeleteCommand(), joinCommand(), participants,
			withName(muteCommand(), "mute-track"), roomUpdateSubscriptionsCommand(), withName(dataSendCommand(), "send-data"),
		},
	}
}

func withName(c *command, name string) *command {
	c.name = name
	return c
}

func oneRoom(c *call) (string, error) {
	if len(c.args) != 1 {
		return "", usagef("give one room name")
	}
	return c.args[0], nil
}

func roomCreateCommand() *command {
	var emptyTimeout, departureTimeout, maxParticipants uint
	var metadata string
	return &command{
		name:  "create",
		usage: "create a room",
		args:  "ROOM_NAME",
		flags: func(fs *flag.FlagSet) {
			fs.UintVar(&emptyTimeout, "empty-timeout", 0, "`SECONDS` to keep the room open before anyone joins")
			fs.UintVar(&departureTimeout, "departure-timeout", 0, "`SECONDS` to keep the room open after the last person leaves")
			fs.UintVar(&maxParticipants, "max-participants", 0, "most `PEOPLE` allowed at once")
			fs.StringVar(&metadata, "metadata", "", "room `METADATA`")
		},
		run: func(c *call) error {
			room, err := oneRoom(c)
			if err != nil {
				return err
			}
			client, _, err := c.global.client()
			if err != nil {
				return err
			}
			out, err := client.Room.CreateRoom(c.ctx, &pb.CreateRoomRequest{
				Name: room, EmptyTimeout: uint32(emptyTimeout), DepartureTimeout: uint32(departureTimeout),
				MaxParticipants: uint32(maxParticipants), Metadata: metadata,
			})
			if err != nil {
				return err
			}
			return printProto(c.stdout, out)
		},
	}
}

func roomListCommand() *command {
	return &command{
		name:    "list",
		aliases: []string{"ls"},
		usage:   "list open rooms, optionally only those named",
		args:    "[ROOM_NAME ...]",
		run: func(c *call) error {
			client, _, err := c.global.client()
			if err != nil {
				return err
			}
			out, err := client.Room.ListRooms(c.ctx, &pb.ListRoomsRequest{Names: c.args})
			if err != nil {
				return err
			}
			if c.global.json {
				return printProto(c.stdout, out)
			}
			tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "SID\tNAME\tPARTICIPANTS\tPUBLISHERS\tCREATED")
			for _, r := range out.GetRooms() {
				created := time.Unix(r.GetCreationTime(), 0).UTC().Format(time.RFC3339)
				fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\n", r.GetSid(), r.GetName(), r.GetNumParticipants(), r.GetNumPublishers(), created)
			}
			return tw.Flush()
		},
	}
}

func roomUpdateCommand() *command {
	var metadata string
	var hasMetadata bool
	return &command{
		name:  "update",
		usage: "change a room's metadata",
		args:  "ROOM_NAME",
		flags: func(fs *flag.FlagSet) {
			fs.Func("metadata", "new room `METADATA`; empty clears it", func(v string) error {
				metadata, hasMetadata = v, true
				return nil
			})
		},
		run: func(c *call) error {
			room, err := oneRoom(c)
			if err != nil {
				return err
			}
			if !hasMetadata {
				return usagef("--metadata is required")
			}
			client, _, err := c.global.client()
			if err != nil {
				return err
			}
			out, err := client.Room.UpdateRoomMetadata(c.ctx, &pb.UpdateRoomMetadataRequest{Room: room, Metadata: metadata})
			if err != nil {
				return err
			}
			return printProto(c.stdout, out)
		},
	}
}

func roomDeleteCommand() *command {
	return &command{
		name:  "delete",
		usage: "close a room and disconnect everyone",
		args:  "ROOM_NAME",
		run: func(c *call) error {
			room, err := oneRoom(c)
			if err != nil {
				return err
			}
			client, _, err := c.global.client()
			if err != nil {
				return err
			}
			if _, err := client.Room.DeleteRoom(c.ctx, &pb.DeleteRoomRequest{Room: room}); err != nil {
				return err
			}
			fmt.Fprintf(c.stderr, "Deleted room %s\n", room)
			return nil
		},
	}
}

func roomUpdateSubscriptionsCommand() *command {
	var room, identity string
	var subscribe, unsubscribe bool
	return &command{
		name:  "update-subscriptions",
		usage: "subscribe a participant to tracks, or unsubscribe them",
		args:  "TRACK_SID ...",
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&room, "room", "", "`NAME` of the room")
			fs.StringVar(&room, "r", "", "`NAME` of the room")
			fs.StringVar(&identity, "identity", "", "`ID` of the participant")
			fs.StringVar(&identity, "i", "", "`ID` of the participant")
			fs.BoolVar(&subscribe, "subscribe", false, "subscribe to the tracks")
			fs.BoolVar(&unsubscribe, "unsubscribe", false, "unsubscribe from the tracks")
		},
		run: func(c *call) error {
			if room == "" || identity == "" {
				return usagef("--room and --identity are required")
			}
			if subscribe == unsubscribe {
				return usagef("use one of --subscribe and --unsubscribe")
			}
			if len(c.args) == 0 {
				return usagef("name at least one track")
			}
			client, _, err := c.global.client()
			if err != nil {
				return err
			}
			_, err = client.Room.UpdateSubscriptions(c.ctx, &pb.UpdateSubscriptionsRequest{Room: room, Identity: identity, TrackSids: c.args, Subscribe: subscribe})
			return err
		},
	}
}

func participantCommand() *command {
	return &command{
		name:  "participant",
		usage: "list, inspect, change and remove participants",
		subs: []*command{
			participantListCommand(), participantGetCommand(), participantRemoveCommand(), muteCommand(), participantUpdateCommand(),
		},
	}
}

// roomAndIdentity reads the participant a command names: with flags, or with the room as a flag and the identity as an
// argument, as both forms are in common use.
func roomAndIdentity(c *call, room, identity string) (string, string, error) {
	if identity == "" && len(c.args) == 1 {
		identity = c.args[0]
	}
	if room == "" || identity == "" {
		return "", "", usagef("--room and --identity are required")
	}
	return room, identity, nil
}

func participantListCommand() *command {
	var room string
	return &command{
		name:    "list",
		aliases: []string{"ls"},
		usage:   "list a room's participants",
		args:    "[ROOM_NAME]",
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&room, "room", "", "`NAME` of the room")
			fs.StringVar(&room, "r", "", "`NAME` of the room")
		},
		run: func(c *call) error {
			if room == "" && len(c.args) == 1 {
				room = c.args[0]
			}
			if room == "" {
				return usagef("give a room name")
			}
			client, _, err := c.global.client()
			if err != nil {
				return err
			}
			out, err := client.Room.ListParticipants(c.ctx, &pb.ListParticipantsRequest{Room: room})
			if err != nil {
				return err
			}
			if c.global.json {
				return printProto(c.stdout, out)
			}
			tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "SID\tIDENTITY\tNAME\tSTATE\tTRACKS")
			for _, p := range out.GetParticipants() {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\n", p.GetSid(), p.GetIdentity(), p.GetName(), p.GetState(), len(p.GetTracks()))
			}
			return tw.Flush()
		},
	}
}

func participantGetCommand() *command {
	var room, identity string
	return &command{
		name:  "get",
		usage: "show one participant",
		args:  "[IDENTITY]",
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&room, "room", "", "`NAME` of the room")
			fs.StringVar(&room, "r", "", "`NAME` of the room")
			fs.StringVar(&identity, "identity", "", "`ID` of the participant")
			fs.StringVar(&identity, "i", "", "`ID` of the participant")
		},
		run: func(c *call) error {
			r, id, err := roomAndIdentity(c, room, identity)
			if err != nil {
				return err
			}
			client, _, err := c.global.client()
			if err != nil {
				return err
			}
			out, err := client.Room.GetParticipant(c.ctx, &pb.RoomParticipantIdentity{Room: r, Identity: id})
			if err != nil {
				return err
			}
			return printProto(c.stdout, out)
		},
	}
}

func participantRemoveCommand() *command {
	var room, identity string
	return &command{
		name:  "remove",
		usage: "disconnect a participant",
		args:  "[IDENTITY]",
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&room, "room", "", "`NAME` of the room")
			fs.StringVar(&room, "r", "", "`NAME` of the room")
			fs.StringVar(&identity, "identity", "", "`ID` of the participant")
			fs.StringVar(&identity, "i", "", "`ID` of the participant")
		},
		run: func(c *call) error {
			r, id, err := roomAndIdentity(c, room, identity)
			if err != nil {
				return err
			}
			client, _, err := c.global.client()
			if err != nil {
				return err
			}
			if _, err := client.Room.RemoveParticipant(c.ctx, &pb.RoomParticipantIdentity{Room: r, Identity: id}); err != nil {
				return err
			}
			fmt.Fprintf(c.stderr, "Removed %s from %s\n", id, r)
			return nil
		},
	}
}

func muteCommand() *command {
	var room, identity, track string
	var unmute, mute bool
	return &command{
		name:  "mute",
		usage: "mute a participant's track. Unmuting needs room.enable_remote_unmute on the server",
		args:  "[TRACK_SID]",
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&room, "room", "", "`NAME` of the room")
			fs.StringVar(&room, "r", "", "`NAME` of the room")
			fs.StringVar(&identity, "identity", "", "`ID` of the participant")
			fs.StringVar(&identity, "i", "", "`ID` of the participant")
			fs.StringVar(&track, "track", "", "`SID` of the track")
			fs.BoolVar(&mute, "mute", false, "mute the track, which is the default")
			fs.BoolVar(&mute, "m", false, "mute the track, which is the default")
			fs.BoolVar(&unmute, "unmute", false, "unmute the track")
			fs.BoolVar(&unmute, "u", false, "unmute the track")
		},
		run: func(c *call) error {
			if track == "" && len(c.args) == 1 {
				track = c.args[0]
			}
			if room == "" || identity == "" || track == "" {
				return usagef("--room, --identity and --track are required")
			}
			if mute && unmute {
				return usagef("use one of --mute and --unmute")
			}
			client, _, err := c.global.client()
			if err != nil {
				return err
			}
			out, err := client.Room.MutePublishedTrack(c.ctx, &pb.MuteRoomTrackRequest{Room: room, Identity: identity, TrackSid: track, Muted: !unmute})
			if err != nil {
				return err
			}
			return printProto(c.stdout, out)
		},
	}
}

func participantUpdateCommand() *command {
	var room, identity, metadata, name, permissions string
	var attributes repeatedString
	return &command{
		name:  "update",
		usage: "change a participant's metadata, name, attributes or permissions",
		args:  "[IDENTITY]",
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&room, "room", "", "`NAME` of the room")
			fs.StringVar(&room, "r", "", "`NAME` of the room")
			fs.StringVar(&identity, "identity", "", "`ID` of the participant")
			fs.StringVar(&identity, "i", "", "`ID` of the participant")
			fs.StringVar(&metadata, "metadata", "", "new `METADATA`")
			fs.StringVar(&name, "name", "", "new display `NAME`")
			fs.Var(&attributes, "attribute", "attribute as `KEY=VALUE`; can be repeated")
			fs.StringVar(&permissions, "permissions", "", "permissions as `JSON`. This replaces the whole set")
		},
		run: func(c *call) error {
			r, id, err := roomAndIdentity(c, room, identity)
			if err != nil {
				return err
			}
			attrs, err := keyValues(attributes)
			if err != nil {
				return err
			}
			req := &pb.UpdateParticipantRequest{Room: r, Identity: id, Metadata: metadata, Name: name, Attributes: attrs}
			if permissions != "" {
				req.Permission = &pb.ParticipantPermission{}
				if err := (protojson.UnmarshalOptions{}).Unmarshal([]byte(permissions), req.Permission); err != nil {
					return usagef("--permissions is not valid: %v", err)
				}
			}
			client, _, err := c.global.client()
			if err != nil {
				return err
			}
			out, err := client.Room.UpdateParticipant(c.ctx, req)
			if err != nil {
				return err
			}
			return printProto(c.stdout, out)
		},
	}
}

func dataCommand() *command {
	return &command{
		name:  "data",
		usage: "send data to a room",
		subs:  []*command{withName(dataSendCommand(), "send")},
	}
}

func dataSendCommand() *command {
	var room, data, topic string
	var identities repeatedString
	var lossy bool
	return &command{
		name:  "send",
		usage: "send data to everyone in a room, or to the identities named",
		args:  "[DATA]",
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&room, "room", "", "`NAME` of the room")
			fs.StringVar(&room, "r", "", "`NAME` of the room")
			fs.StringVar(&data, "data", "", "the `DATA`")
			fs.StringVar(&topic, "topic", "", "`TOPIC` of the message")
			fs.Var(&identities, "identity", "`ID` of a participant to send to; can be repeated")
			fs.BoolVar(&lossy, "lossy", false, "send without retries, which suits data that is soon out of date")
		},
		run: func(c *call) error {
			if data == "" && len(c.args) == 1 {
				data = c.args[0]
			}
			if room == "" || data == "" {
				return usagef("--room and --data are required")
			}
			req := &pb.SendDataRequest{Room: room, Data: []byte(data), Kind: pb.DataPacket_RELIABLE, DestinationIdentities: identities}
			if lossy {
				req.Kind = pb.DataPacket_LOSSY
			}
			if topic != "" {
				req.Topic = &topic
			}
			client, _, err := c.global.client()
			if err != nil {
				return err
			}
			_, err = client.Room.SendData(c.ctx, req)
			return err
		},
	}
}
