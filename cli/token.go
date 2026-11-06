package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"elnova-streamkit.local/pkg/pb"
	streamkit "elnova-streamkit.local/sdks/server-go"
)

func tokenCommand() *command {
	return &command{
		name:  "token",
		usage: "create and check access tokens",
		subs:  []*command{tokenCreateCommand(), tokenVerifyCommand()},
	}
}

func tokenCreateCommand() *command {
	var (
		room, identity, name, metadata, validFor, grantJSON, attributeFile, kind, roomPreset, roomConfigFile string
		create, list, join, admin, egress, ingress, updateMetadata, tokenOnly, hidden                        bool
		noPublish, noSubscribe, noPublishData, sipAdmin, sipCall, agentAdmin, agentDispatch, inference       bool
		observability                                                                                        bool
		sources                                                                                              stringList
		attributes                                                                                           repeatedString
	)
	return &command{
		name:  "create",
		usage: "create an access token",
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&room, "room", "", "`NAME` of the room")
			fs.StringVar(&room, "r", "", "`NAME` of the room")
			fs.StringVar(&identity, "identity", "", "`ID` of the participant")
			fs.StringVar(&identity, "i", "", "`ID` of the participant")
			fs.StringVar(&name, "name", "", "display `NAME` of the participant")
			fs.StringVar(&name, "n", "", "display `NAME` of the participant")
			fs.StringVar(&metadata, "metadata", "", "participant `METADATA`")
			fs.Var(&attributes, "attribute", "participant attribute as `KEY=VALUE`; can be repeated")
			fs.StringVar(&attributeFile, "attribute-file", "", "read attributes from a JSON `FILE`")
			fs.StringVar(&validFor, "valid-for", "5m", "how long the token lasts, such as 5m or 1h10m")
			fs.BoolVar(&create, "create", false, "may create and delete rooms")
			fs.BoolVar(&list, "list", false, "may list rooms")
			fs.BoolVar(&join, "join", false, "may join a room; needs --room and --identity")
			fs.BoolVar(&admin, "admin", false, "may moderate a room; needs --room")
			fs.BoolVar(&egress, "egress", false, "may use recording and stream outputs")
			fs.BoolVar(&ingress, "ingress", false, "may manage ingress")
			fs.BoolVar(&updateMetadata, "allow-update-metadata", false, "may change their own name and metadata")
			fs.Var(&sources, "allow-source", "only publish these `SOURCES`: camera, microphone, screen_share, screen_share_audio")
			fs.BoolVar(&noPublish, "no-publish", false, "may not publish")
			fs.BoolVar(&noSubscribe, "no-subscribe", false, "may not subscribe")
			fs.BoolVar(&noPublishData, "no-publish-data", false, "may not send data")
			fs.BoolVar(&hidden, "hidden", false, "invisible to other participants")
			fs.StringVar(&grantJSON, "grant", "", "more room permissions, as `JSON`; merged with the flags")
			fs.StringVar(&kind, "kind", "", "`KIND` of participant: standard, ingress, egress, sip, agent, connector, bridge")
			fs.StringVar(&roomConfigFile, "room-config", "", "JSON `FILE` of the configuration a room gets if this token's join creates it")
			fs.StringVar(&roomPreset, "room-preset", "", "`NAME` of a stored room configuration")
			fs.BoolVar(&sipAdmin, "sip-admin", false, "may manage SIP resources")
			fs.BoolVar(&sipCall, "sip-call", false, "may place SIP calls")
			fs.BoolVar(&agentAdmin, "agent-admin", false, "may manage hosted agents")
			fs.BoolVar(&agentDispatch, "agent-dispatch-admin", false, "may manage the agent dispatch queue")
			fs.BoolVar(&inference, "inference", false, "may call hosted inference")
			fs.BoolVar(&observability, "observability-write", false, "may publish observability data")
			fs.BoolVar(&tokenOnly, "token-only", false, "print only the token")
		},
		run: func(c *call) error {
			creds, err := c.global.credentials()
			if err != nil {
				return err
			}
			if err := creds.needSigning(); err != nil {
				return err
			}
			ttl, err := time.ParseDuration(validFor)
			if err != nil || ttl <= 0 {
				return usagef("--valid-for must be a time such as 5m or 1h10m")
			}
			grant := &streamkit.VideoGrant{
				Room: room, RoomCreate: create, RoomList: list, RoomJoin: join, RoomAdmin: admin,
				RoomRecord: egress, IngressAdmin: ingress, Hidden: hidden, CanPublishSources: sources,
			}
			if updateMetadata {
				grant.CanUpdateOwnMetadata = streamkit.Bool(true)
			}
			if noPublish {
				grant.CanPublish = streamkit.Bool(false)
			}
			if noSubscribe {
				grant.CanSubscribe = streamkit.Bool(false)
			}
			if noPublishData {
				grant.CanPublishData = streamkit.Bool(false)
			}
			if grantJSON != "" {
				if err := json.Unmarshal([]byte(grantJSON), grant); err != nil {
					return usagef("--grant is not valid JSON: %v", err)
				}
			}
			if grant.RoomAdmin && grant.Room == "" {
				return usagef("--admin needs --room")
			}
			if grant.RoomJoin && (grant.Room == "" || identity == "") {
				return usagef("--join needs --room and --identity")
			}
			if !create && !list && !join && !admin && !egress && !ingress && grantJSON == "" && !sipAdmin && !sipCall &&
				!agentAdmin && !agentDispatch && !inference && !observability && roomConfigFile == "" {
				return usagef("say what the token may do, such as --join")
			}

			at := streamkit.NewAccessToken(creds.APIKey, creds.secret).
				SetIdentity(identity).SetName(name).SetMetadata(metadata).SetValidFor(ttl).SetVideoGrant(grant).SetRoomPreset(roomPreset)
			attrs, err := keyValues(attributes)
			if err != nil {
				return err
			}
			if attributeFile != "" {
				data, err := os.ReadFile(attributeFile)
				if err != nil {
					return err
				}
				var fromFile map[string]string
				if err := json.Unmarshal(data, &fromFile); err != nil {
					return fmt.Errorf("%s: %w", attributeFile, err)
				}
				if attrs == nil {
					attrs = map[string]string{}
				}
				for k, v := range fromFile {
					attrs[k] = v
				}
			}
			at.SetAttributes(attrs)
			if kind != "" {
				v, ok := pb.ParticipantInfo_Kind_value[strings.ToUpper(kind)]
				if !ok {
					return usagef("unknown --kind %q", kind)
				}
				at.SetKind(pb.ParticipantInfo_Kind(v))
			}
			if sipAdmin || sipCall {
				at.SetSIPGrant(&streamkit.SIPGrant{Admin: sipAdmin, Call: sipCall})
			}
			if agentAdmin || agentDispatch {
				at.SetAgentGrant(&streamkit.AgentGrant{Admin: agentAdmin, DispatchAdmin: agentDispatch})
			}
			if inference {
				at.SetInferenceGrant(&streamkit.InferenceGrant{Perform: true})
			}
			if observability {
				at.SetObservabilityGrant(&streamkit.ObservabilityGrant{Write: true})
			}
			if roomConfigFile != "" {
				data, err := os.ReadFile(roomConfigFile)
				if err != nil {
					return err
				}
				config := &pb.RoomConfiguration{}
				if err := protojson.Unmarshal(data, config); err != nil {
					return fmt.Errorf("%s: %w", roomConfigFile, err)
				}
				at.SetRoomConfig(config)
			}
			token, err := at.ToJWT()
			if err != nil {
				return err
			}
			switch {
			case tokenOnly:
				fmt.Fprintln(c.stdout, token)
			case c.global.json:
				return writeJSON(c.stdout, map[string]any{"token": token, "identity": identity, "room": grant.Room, "url": creds.URL})
			default:
				fmt.Fprintln(c.stdout, "Token:", token)
				if creds.URL != "" {
					fmt.Fprintln(c.stdout, "Server:", creds.URL)
				}
			}
			return nil
		},
	}
}

func tokenVerifyCommand() *command {
	return &command{
		name:  "verify",
		usage: "check a token with this project's secret and print its claims",
		args:  "TOKEN",
		run: func(c *call) error {
			if len(c.args) != 1 {
				return usagef("give one token")
			}
			creds, err := c.global.credentials()
			if err != nil {
				return err
			}
			if err := creds.needSigning(); err != nil {
				return err
			}
			claims, err := streamkit.VerifyToken(c.args[0], streamkit.Keys{creds.APIKey: creds.secret})
			if err != nil {
				return err
			}
			view := map[string]any{
				"identity": claims.Identity, "name": claims.Name, "kind": claims.Kind, "metadata": claims.Metadata,
				"attributes": claims.Attributes, "video": claims.Video, "sip": claims.SIP, "agent": claims.Agent,
				"api_key": claims.APIKey, "expires_at": claims.ExpiresAt.UTC().Format(time.RFC3339),
			}
			if claims.RoomConfig != nil {
				data, err := protojson.Marshal(claims.RoomConfig)
				if err != nil {
					return errors.New("could not show the room configuration")
				}
				view["room_config"] = json.RawMessage(data)
			}
			return writeJSON(c.stdout, view)
		},
	}
}
