# SIP API

Two ways in. SDKs and CLIs for this protocol use Twirp, at `/twirp/livekit.SIP/<Method>`, with the messages in `proto/vendor/livekit/livekit_sip.proto`. The
dashboard uses REST, under `/api/v1/sip`. They run the same code. How to run the service is in [docs/deployment/sip.md](../deployment/sip.md).

## Twirp

| Method | Grant |
| --- | --- |
| `CreateSIPInboundTrunk`, `CreateSIPOutboundTrunk`, `UpdateSIPInboundTrunk`, `UpdateSIPOutboundTrunk`, `GetSIPInboundTrunk`, `GetSIPOutboundTrunk`, `ListSIPInboundTrunk`, `ListSIPOutboundTrunk`, `ListSIPTrunk`, `DeleteSIPTrunk` | `sip.admin` |
| `CreateSIPDispatchRule`, `UpdateSIPDispatchRule`, `ListSIPDispatchRule`, `DeleteSIPDispatchRule` | `sip.admin` |
| `CreateSIPParticipant` | `sip.call` |
| `TransferSIPParticipant` | `sip.call`, and `roomAdmin` for that room |

`CreateSIPParticipant` returns when the participant has joined the room, or with `wait_until_answered`, when the phone has answered. A phone that does not answer comes
back as an error whose metadata has `sip_status_code` and `sip_status`, and its Twirp code follows from the SIP status.

`TransferSIPParticipant` returns when the transfer has finished. A failed one has `sip_transfer_reason` in its error's metadata, and `sip_status_code` if a SIP response said why.

## Differences an SDK user will notice

- `auth_username` and `auth_password` are always empty in responses. A PIN is `****`. Sending a trunk or rule back as you read it keeps what it has.
- Creating an inbound trunk with neither `allowed_addresses` nor credentials is refused.
- A `replace` without credentials keeps the trunk's. To remove them, send an `update` with `auth_username` and `auth_password` set to empty strings.
- `Via`, `To` and `Route` are refused as custom headers. They are deprecated or unchecked in the reference.
- `sip_request_uri`, `sip_from_header` and `sip_to_header` are ignored. `destination_country`, `krisp_enabled` and `room_preset` are stored and do nothing.

The rest of what differs is in [docs/deployment/sip.md](../deployment/sip.md#differences-from-the-reference).

## REST

Bodies and responses are the messages above in JSON, with their proto field names. Errors have the same shape as the rest of the API. A person signed in can use all of these
if they are an owner or admin of the project, and a member can read. A token with `sip.admin` can read. Writing needs a person: programs use Twirp.

```
GET    /sip/trunks/inbound         { items }, each with has_credentials
POST   /sip/trunks/inbound         create, from a SIPInboundTrunkInfo
GET    /sip/trunks/inbound/{id}    one
PATCH  /sip/trunks/inbound/{id}    change, from a SIPInboundTrunkUpdate
DELETE /sip/trunks/inbound/{id}    delete
GET    /sip/trunks/outbound        as above, for outbound trunks
POST   /sip/trunks/outbound
GET    /sip/trunks/outbound/{id}
PATCH  /sip/trunks/outbound/{id}
DELETE /sip/trunks/outbound/{id}
GET    /sip/dispatch-rules         { items }
POST   /sip/dispatch-rules         create, from a SIPDispatchRuleInfo
PATCH  /sip/dispatch-rules/{id}    change, from a SIPDispatchRuleUpdate
DELETE /sip/dispatch-rules/{id}    delete
GET    /sip/calls                  { items, next_cursor }, newest first
POST   /sip/calls                  place a call, from a CreateSIPParticipantRequest
GET    /sip/calls/{id}             one call
POST   /sip/calls/{id}/hangup      end a call. 409 with code sip_call_ended if it already has
```

A call:

```json
{
  "id": "SCL_...", "project_id": "...", "direction": "inbound | outbound",
  "status": "queued | dialing | ringing | active | ended | failed",
  "trunk_id": "ST_...", "dispatch_rule_id": "SDR_...",
  "room_name": "...", "room_sid": "...", "participant_identity": "...", "participant_sid": "...",
  "from_user": "", "from_host": "", "to_user": "", "to_host": "",
  "sip_call_id": "the Call-ID on the wire", "audio_codec": "PCMU", "media_encryption": "",
  "sip_status_code": 200, "sip_status": "OK", "disconnect_reason": "CLIENT_INITIATED", "error": "",
  "created_at": "...", "answered_at": null, "ended_at": null
}
```

`from_user` and `to_user` are empty when the rule or request hides the phone number.

## In the room

A call's participant has kind `SIP`, and an identity of `sip_<number>` unless a request names one. A rule with `hide_phone_number` hashes it.

| Attribute | |
| --- | --- |
| `sip.callID` | This call's ID, `SCL_...`. |
| `sip.callIDFull`, `sip.callTag` | The Call-ID and From tag on the wire, for an inbound call. |
| `sip.trunkID`, `sip.ruleID` | Which trunk and rule. |
| `sip.phoneNumber`, `sip.trunkPhoneNumber`, `sip.hostname` | The other end, this end, and the other end's host. Not there when the number is hidden. |
| `sip.callStatus` | `dialing`, `ringing`, `automation`, `active` or `hangup`. |
| `sip.h.<name>` | Headers, if a trunk's `include_headers` asks for them. |
| `lk.sip.inviteTime` | When the INVITE arrived, in Unix milliseconds. |

Anyone in the room can send DTMF by publishing `SipDTMF` data, and receives it the same way. They can call the RPC methods `lk.sip.GetRemoteHeaders` and `lk.sip.EndCall` on the
participant. Removing the participant from the room hangs up.
