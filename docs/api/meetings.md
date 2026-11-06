# Meetings

What the meeting app at `/meet` runs on, under `/api/v1`. Share links are in the API reference. This covers who can
start a meeting, who can run one, and why it is built the way it is.

## Decisions

**Starting a meeting takes an account.** `POST /meetings` needs a dashboard session, on a project the person belongs
to, and that project needs an API key to sign tokens with. An access token cannot call it, even with every grant, for
the reason it cannot make a share link: a leaked token could otherwise make itself a way back in. Nobody signed in gets
401. Each account can make 2 meetings a minute, with a burst of 20. No room exists until the first person connects, so
a meeting nobody comes to costs one database row and two links.

This adds nothing to what someone who registers could already do: they can already make a project, a key, and rooms.
It does mean an open registration server lets anyone start meetings, which is one reason registration is closed by default:
see [Accounts](../deployment/self-hosting.md#accounts). A project can have 1000 meetings open at once, by default.

**Authority is on the server, in a row.** A host link gives whoever opens it the host role. The token they get carries
no admin grant, and is no more powerful than a guest's: it says who they are. What they may do is read from
`meeting_members` when they ask, so taking away a moderator or removing someone takes effect at once, whatever is left
on their token, which the server keeps refreshing for 6 hours at a time. Nobody can give themselves a role, because
tokens from share links do not carry `canUpdateOwnMetadata`, and the `meeting.role` attribute is only what the page
shows. Nothing enforces anything from it, but it does mark a token as one a link handed out: such a token with no row is
refused, since the row is what would have said whether it is still good. A project should not use that attribute in its own tokens.

| | guest | moderator | host |
|---|---|---|---|
| Talk, share, chat, react, raise a hand | yes | yes | yes |
| Mute someone, mute everyone, lower a hand | | yes | yes |
| Remove someone | | guests only | anyone but themselves |
| Let people in from the waiting room | | yes | yes |
| Make someone a moderator | | | yes |
| Lock, waiting room, change the title | | | yes |
| Start and stop the recording | | | yes |
| End the meeting for everyone | | | yes |

A host stays a host, and more hosts come from the host link. Anyone holding it is a host, so it should be treated as a
key: the meeting's own "Copy link" button only ever gives out the guest link, and the page takes a host code out of the
address bar and the browser's history as soon as it knows what it is. That means reloading as a host needs the host link
again, so the browser asks before leaving the page. A guest link stays in the address, for passing on and reloading.

**Muting stops the sound at the server.** A mute asks the person's client to mute, and the server stops forwarding that
audio too, so a client that ignores the request is not heard either. People can unmute themselves, as in other meeting
products: a host quiets a room, and does not decide who may ever speak. Only audio is held back: a camera is stopped by
its client, and the keyframe it restarts with must not be lost to a mute message arriving a moment later.

**Removing someone refuses their token, not the person.** They are disconnected if they are connected, and every token
issued to that identity is refused from then on, whichever API removed them, including the dashboard and the room
service. Being offline does not save a token: anyone a link handed one to can be removed. The room service still answers
"not found" for someone not connected, as the reference does, and they are refused all the same. The share link itself
still works, because it does not name anyone: opening it again gives a new identity. A host who wants someone to stay
out locks the meeting or turns on the waiting room. The app offers no way back to someone removed, but that is manners,
not security.

**Turning off a link** stops new people using it, and reaches those who already have. A host who came through a host link
that has been turned off stops being a host at once, and cannot reconnect, because that link may be why it was turned off.
Their session stays connected with no more power than a guest, until someone removes them. Someone holding a token from
any turned-off link who has not connected yet cannot start. A guest already in the meeting keeps their place, and can
reconnect: turning off an invitation is not throwing out the people who took it. To carry on hosting, make a new host link.

**Locking.** Nobody new can join, whether with the link or with a token they got before the lock and have not used.
Whoever has already been in can reconnect after a dropped connection, and hosts can always come in. Locking sends away
anyone still in the waiting room. The project's own tokens and recorders are not affected: those are not the meeting's
guests.

**Waiting room.** A guest gets a ticket instead of a token, and swaps it at `/join/wait` once a host or moderator lets
them in. Until then they have no token, so there is nothing to work around. A token from before the waiting room was
turned on, not used yet, is refused. Turning the waiting room off lets in whoever is waiting. Tickets are stored as
hashes, pay out once, and lapse after 15 seconds of not being asked about.

**Ending a meeting.** Everyone is disconnected, every job recording or streaming the room stops first so a recording
finishes as a whole file, every token the meeting handed out stops working, and the guest link says it has ended. A host
opening the host link starts it again, with the door back to how a new meeting starts. Tokens from before stay dead.

**Hand raising is an attribute, set through a route.** It is state that someone arriving later should see, which is what
attributes are for. A client could only set one itself with `canUpdateOwnMetadata`, which would also let it rename
itself, so the server sets it on request.

**Chat and reactions are data messages** between the browsers in the meeting. The server stamps the real sender. Nothing
is stored, so someone who joins later does not see earlier messages, and messages are gone when the meeting ends.

**Recording** uses `POST /egress` and `POST /egress/{id}/stop`. A host's token has no `roomRecord` grant, because that
reaches every recording in the project, including deleting them. Those two routes also accept a host for the one job
the record button asks for: a file recording of the whole of their own room. It cannot name another room, one person,
or a stream address, and it reads no recordings. It starts one at a time, and not while anything else is recording a file
of that room, so a host link cannot fill the installation's capacity. A project's own credentials can run several in a room,
as the reference allows, and those are limited per project. Everyone sees that recording is on from what the server reports,
which is a real recorder in the room. The file goes to the project's Recordings page. A server-side recording cannot
access end-to-end encrypted media, and the meeting app does not use end-to-end encryption.

**The summary is a record, not a write-up.** It says when the meeting ran, who was there under what names, and whether
it was recorded, from the server's room history. People who gave the same name appear once. Only someone who was connected
can read it, and only of the latest session they were in: holding a token from a link says nothing about having been there. It says nothing about what
was said, because nothing here knows. A written summary would need a transcript and a language model provider, which
would have to be chosen and paid for. None is configured, and none of this is that.

**Network quality** is what the server scores from the loss, delay and jitter it measures. Before its first score the
page says it has not measured yet.

**Metadata.** Once a room has a meeting, its room metadata belongs to the meeting: `{ title, locked, waiting_room }`, as
everyone in it sees them. It is only for showing. Setting it through `PATCH /rooms/{name}` changes what the page shows
until the next change to the meeting, and changes nothing about who can come in. Roles and raised hands are the
attributes `meeting.role` and `meeting.hand` on each participant.

## Routes

Everything under one meeting takes `Authorization: Bearer <the token they joined with>`.

```
POST   /meetings                                session only: { title, waiting_room, expires_in_seconds }
                                                returns { room, title, waiting_room, expires_at, host_url, guest_url }.
                                                429 if the project has as many open as it may
GET    /meetings/{room}                         { room, role, identity, title, locked, waiting_room }, and for those
                                                running it { waiting, waiting_count, invite_url, recording }. waiting is
                                                the 100 who have waited longest
PATCH  /meetings/{room}                         host: { title, locked, waiting_room }
POST   /meetings/{room}/end                     host
POST   /meetings/{room}/mute-all                moderator: { muted }
POST   /meetings/{room}/hand                    anyone: { raised }
GET    /meetings/{room}/summary                 anyone who was in it, not anyone removed
POST   /meetings/{room}/waiting/{identity}      moderator: { admit }
DELETE /meetings/{room}/waiting                 moderator: turns away everyone waiting. returns { turned_away }
POST   /meetings/{room}/participants/{identity}/mute        moderator
POST   /meetings/{room}/participants/{identity}/remove      moderator, for guests. host, for anyone else
POST   /meetings/{room}/participants/{identity}/lower-hand  moderator
PATCH  /meetings/{room}/participants/{identity}             host: { role: moderator | guest }
POST   /join                                    as before. 202 with { status, ticket } if there is a waiting room.
                                                423 if locked, 410 if ended, 429 if it has handed out as many as it may
POST   /join/wait                               { ticket, cancel }: 200 still waiting, 201 with a token, 204 gave up
POST   /join-links                              as before, with role: guest | host
```

## Against a flood

Someone holding a guest link needs no account, so these are what limit them.

- **Joining** is limited to 120 a minute from one address, with bursts of 60. A link hands out 5000 identities and a meeting
  20000, counting rows still being kept, and past that `/join` answers 429. A host link only counts its own, so a flood of guests
  does not keep hosts out, and a host can make another link. Rows from people who never got in go after a day. These numbers
  are about rows in a table, not people in a room.
- **Being in the room** is limited by `room.meeting_max_participants`, 100 by default, for a room a share link opens. `0` sets no
  limit. `room.max_participants` and a project's own limit say more than this does, and a room a project opened itself keeps what
  it was opened with. This number says how far a leaked link can go. It is not a measurement of what your machines can carry.
- **Connecting** to `/rtc` is limited to 600 a minute from one address, with bursts of 300, set by `limits.rtc_connects_per_minute`
  and `limits.rtc_connect_burst`, and counted once on the node that first takes it. An IPv6 address counts by its /64. Coming back
  to a session you have is not counted against your address, because a network change gives you a new one, and when an office's
  connection drops, everyone there is back at once. A session can try 120 times a minute. Behind a proxy, set
  `control_plane.trust_proxy_headers`: only the last entry in `X-Forwarded-For` counts, which is the one that proxy added, so
  a client cannot get round this by sending its own.
- **The waiting room** shows hosts the 100 who have waited longest, and how many there are in all. "Turn everyone away" clears it.
  They can knock again, so a host who wants this to last locks the meeting too.
- **Chat and other data** are limited per participant at the server, to 500 messages a second and 8 MB a second, with short bursts to
  twice and four times that, set by `limits.max_data_messages_per_second` and `limits.max_data_bytes_per_second`. Past that, messages
  are dropped and counted in `streamkit_data_packets_dropped_total`, and that applies to any room, not just a meeting. This is a
  limit on the server's work, and that much chat would still overwhelm a reader, so the page keeps the latest 500 messages.

## Limits

- Tested on one node, over loopback. The routes under one meeting are routed to the node hosting the room as room routes
  are, and what they enforce is in the database. Ending a meeting and removing someone have also been run through two server
  processes on one machine, over loopback, with `make test-multinode`. None of this has been run across machines or over a
  real network.
- `setSinkId` chooses a speaker in Chrome and Edge. Safari and browsers on iOS cannot choose one from a page, and Firefox
  only for a device picked in its own prompt, so the app says it plays through the default speaker there. Only Chrome has
  been tested.
- A screen's audio depends on the browser and what is shared: Chrome offers a tab's audio everywhere, and a whole
  screen's on Windows and ChromeOS. Firefox and Safari do not offer it. Where the browser leaves it out, the app says the
  screen is being shared without sound. Only Chrome has been tested, with its fake capture devices.
