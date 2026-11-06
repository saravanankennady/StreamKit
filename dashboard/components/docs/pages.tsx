"use client";

import Link from "next/link";
import * as React from "react";
import { Callout } from "@/components/ui/badge";
import { CodeBlock } from "@/components/ui/copy";
import { ServiceStatus, useDocsAddress } from "./shell";

// Written from the code in this repository. Where something is still being built, it says so, and the media
// service pages ask the running server rather than promise anything.

function AddressNote() {
  const a = useDocsAddress();
  return a.real ? (
    <p>
      Snippets on this page use the address of your project, <strong>{a.projectName}</strong>: <code>{a.ws}</code>.
    </p>
  ) : (
    <p>
      Snippets use <code>{a.ws}</code> as a placeholder. Sign in and they show your project&rsquo;s real address.
    </p>
  );
}

const JoinSnippet = () => {
  const { ws } = useDocsAddress();
  return (
    <CodeBlock
      label="join.ts"
      code={`import { Room, RoomEvent, Track } from "@elnova-streamkit/client";

const room = new Room();

room.on(RoomEvent.TrackSubscribed, (track, publication, participant) => {
  // Video becomes a <video> element, audio an <audio> element.
  document.body.appendChild(track.attach());
});

await room.connect("${ws}", token);
await room.localParticipant.enableCameraAndMicrophone();`}
    />
  );
};

function GettingStarted() {
  const { ws } = useDocsAddress();
  return (
    <>
      <h1>Getting started</h1>
      <p>By the end of this page you will have a server running, a project with an API key, and a browser sending video through a room.</p>
      <AddressNote />
      <h2>1. Run the server</h2>
      <p>
        The server is one Go binary. Its control plane, which is accounts, projects and this dashboard&rsquo;s API, needs PostgreSQL. For local development, the repository has a Compose file for it:
      </p>
      <CodeBlock
        label="Terminal"
        code={`docker compose up -d postgres
go build -o bin/streamkit-server ./server/cmd/streamkit-server`}
      />
      <CodeBlock
        label="streamkit.yaml"
        code={`control_plane:
  database_url: postgres://streamkit:streamkit-dev-only@127.0.0.1:5433/streamkit?sslmode=disable
  master_key: "paste the output of: openssl rand -hex 32"
  public_dashboard_url: http://localhost:3000
  public_ws_url: ws://localhost:7880`}
      />
      <CodeBlock label="Terminal" code={`bin/streamkit-server --dev --config streamkit.yaml`} />
      <p>
        <code>--dev</code> binds to 127.0.0.1, and advertises the loopback address to clients, so it works only from this machine. For a real deployment, read <Link href="/docs/self-hosting">Self-hosted deployment</Link>.
      </p>
      <h2>2. Run the dashboard</h2>
      <CodeBlock
        label="Terminal"
        code={`cd dashboard
npm ci
STREAMKIT_API_URL=http://localhost:7880 \\
NEXT_PUBLIC_STREAMKIT_WS_ORIGIN=ws://localhost:7880 npx next dev -p 3000`}
      />
      <p>Open it at the same address you gave as <code>public_dashboard_url</code>, because the live feed only accepts that origin. Use <code>localhost</code>, not <code>127.0.0.1</code>.</p>
      <h2>3. Create an account, a project and a key</h2>
      <ol>
        <li>Register. The first account on a server becomes its platform administrator.</li>
        <li>Create a project. It holds rooms, keys and webhooks, and shares nothing with other projects.</li>
        <li>Create an API key and save its secret. It is shown once.</li>
      </ol>
      <h2>4. Get a token</h2>
      <p>
        Clients join with an access token signed with your API key. To try things out, make one on the{" "}
        <Link href="/developer?tab=tokens">token generator</Link>. In production, your backend mints them: see <Link href="/docs/authentication">Authentication</Link>.
      </p>
      <h2>5. Join a room</h2>
      <p>
        The SDK is in this repository as <code>sdk-js</code>, and is not on a package registry yet. Build it, then install it by path:
      </p>
      <CodeBlock
        label="Terminal"
        code={`(cd sdk-js && npm ci && npm run build)
npm install ../path/to/elnova-streamkit/sdk-js`}
      />
      <JoinSnippet />
      <p>
        Browsers only allow camera access on HTTPS or localhost, and only after someone has interacted with the page. The room opens by itself when the first person joins, unless you turned that off for your project. Clients connect to <code>{ws}</code>.
      </p>
      <p>
        Open the <Link href="/monitor">live monitor</Link> and you will see them within a second, with their bitrate and frame rate.
      </p>
      <h2>What next</h2>
      <ul>
        <li><Link href="/docs/publishing">Publish and receive media</Link></li>
        <li><Link href="/docs/rooms-and-participants">Control rooms from your backend</Link></li>
        <li><Link href="/docs/webhooks">Get events when things happen</Link></li>
      </ul>
    </>
  );
}

function Authentication() {
  const { http } = useDocsAddress();
  return (
    <>
      <h1>Authentication</h1>
      <p>There are three kinds of credential. Each does one job.</p>
      <table>
        <thead>
          <tr>
            <th>Credential</th>
            <th>Who holds it</th>
            <th>What it is for</th>
          </tr>
        </thead>
        <tbody>
          <tr>
            <td>Session cookie</td>
            <td>A person in this dashboard</td>
            <td>Managing projects, keys, webhooks and everything else here</td>
          </tr>
          <tr>
            <td>API key and secret</td>
            <td>Your backend</td>
            <td>Signing access tokens and verifying webhooks. Never put these in a browser or an app</td>
          </tr>
          <tr>
            <td>Access token</td>
            <td>A client, or your backend</td>
            <td>Joining a room, or calling the REST API with the rights it carries</td>
          </tr>
        </tbody>
      </table>
      <h2>Access tokens</h2>
      <p>
        An access token is a JWT signed with HS256, using your API secret. <code>iss</code> is the API key, which also decides the project. <code>sub</code> is the participant&rsquo;s identity. Rights go in a <code>video</code> claim.
      </p>
      <CodeBlock
        label="Minting a token in Node, with no dependencies"
        code={`import { createHmac } from "node:crypto";

const b64 = (v) => Buffer.from(v).toString("base64url");

export function accessToken(apiKey, apiSecret, identity, room) {
  const now = Math.floor(Date.now() / 1000);
  const head = b64(JSON.stringify({ alg: "HS256", typ: "JWT" }));
  const body = b64(JSON.stringify({
    iss: apiKey,
    sub: identity,
    nbf: now,
    exp: now + 6 * 3600,
    video: { roomJoin: true, room, canPublish: true, canSubscribe: true, canPublishData: true },
  }));
  const sig = createHmac("sha256", apiSecret).update(\`\${head}.\${body}\`).digest("base64url");
  return \`\${head}.\${body}.\${sig}\`;
}`}
      />
      <p>Any JWT library works. Here is what the server looks at in <code>video</code>:</p>
      <table>
        <thead>
          <tr>
            <th>Claim</th>
            <th>Allows</th>
          </tr>
        </thead>
        <tbody>
          <tr><td><code>roomJoin</code> and <code>room</code></td><td>Joining that room</td></tr>
          <tr><td><code>canPublish</code></td><td>Sending media. Defaults to true</td></tr>
          <tr><td><code>canPublishSources</code></td><td>Limits which sources, such as <code>camera</code> or <code>screen_share</code></td></tr>
          <tr><td><code>canSubscribe</code></td><td>Receiving media. Defaults to true</td></tr>
          <tr><td><code>canPublishData</code></td><td>Sending data messages. If unset, follows <code>canPublish</code></td></tr>
          <tr><td><code>canUpdateOwnMetadata</code></td><td>Changing their own name and metadata. Defaults to false</td></tr>
          <tr><td><code>hidden</code></td><td>Being in a room without others seeing them</td></tr>
          <tr><td><code>roomCreate</code></td><td>Creating and closing rooms through the API</td></tr>
          <tr><td><code>roomList</code></td><td>Listing rooms, and reading analytics, the monitor and logs</td></tr>
          <tr><td><code>roomAdmin</code> and <code>room</code></td><td>Managing that room&rsquo;s participants and tracks, and its metadata</td></tr>
          <tr><td><code>roomRecord</code></td><td>Starting, listing and stopping recordings and egress</td></tr>
          <tr><td><code>ingressAdmin</code></td><td>Managing ingresses</td></tr>
        </tbody>
      </table>
      <h2>Calling the REST API</h2>
      <p>Send the token as a bearer token. The project comes from the key, so there is no project header.</p>
      <CodeBlock
        code={`curl ${http}/api/v1/rooms \\
  -H "Authorization: Bearer $TOKEN"`}
      />
      <p>
        Routes for keys, webhook changes, project settings and the audit log only accept a session. A token cannot mint more tokens, because a leaked one would then be good forever.
      </p>
      <h2>Rotating a key</h2>
      <p>
        Rotation gives you a new key and secret, and keeps the old pair working for an overlap you choose, a day by default, so running services have time to switch. Revoking ends a key at once on the node that handled the request, and within 30 seconds on other nodes.
      </p>
      <h2>Roles</h2>
      <p>
        People belong to organizations, as owner, admin or member. Members can see and operate rooms, but only owners and admins can manage keys, webhooks and project settings, or read the audit log. Inviting people has no route or page yet.
      </p>
    </>
  );
}

function RoomsAndParticipants() {
  const { http } = useDocsAddress();
  return (
    <>
      <h1>Rooms and participants</h1>
      <AddressNote />
      <h2>Creating a room</h2>
      <p>A room opens in one of two ways: when you create it, or by itself when the first person joins with a token for it. To require explicit creation, turn off <em>Open rooms when someone joins</em> in Developer settings.</p>
      <CodeBlock
        code={`curl -X POST ${http}/api/v1/rooms \\
  -H "Authorization: Bearer $TOKEN" \\
  -H "Content-Type: application/json" \\
  -d '{"name": "standup", "max_participants": 10, "empty_timeout": 300}'`}
      />
      <p>
        Needs a token with <code>roomCreate</code>. It answers 201 for a new room, and 200 with the existing room if that name is already open. A room is not renamed or reconfigured by asking again.
      </p>
      <table>
        <thead>
          <tr>
            <th>Field</th>
            <th>Meaning</th>
          </tr>
        </thead>
        <tbody>
          <tr><td><code>name</code></td><td>Required, and unique among a project&rsquo;s open rooms</td></tr>
          <tr><td><code>metadata</code></td><td>Any string, readable by everyone in the room</td></tr>
          <tr><td><code>empty_timeout</code></td><td>Seconds a room nobody has joined stays open. Default 300</td></tr>
          <tr><td><code>departure_timeout</code></td><td>Seconds a room stays open after the last person leaves. Default 20</td></tr>
          <tr><td><code>max_participants</code></td><td>0 means no limit</td></tr>
        </tbody>
      </table>
      <p>Defaults can be set per project in <Link href="/developer?tab=settings">Developer settings</Link>.</p>
      <h2>Room states</h2>
      <p>
        A room is <code>creating</code> until someone joins, <code>active</code> while someone is in it, and <code>empty</code> after everyone leaves, until its departure timeout runs out. Closed rooms leave <code>GET /rooms</code>, and stay in <code>GET /rooms/history</code> with their peak and total participants.
      </p>
      <h2>Changing and closing a room</h2>
      <CodeBlock
        label="Set metadata. Needs roomAdmin for this room."
        code={`curl -X PATCH ${http}/api/v1/rooms/standup \\
  -H "Authorization: Bearer $TOKEN" \\
  -H "Content-Type: application/json" \\
  -d '{"metadata": "{\\"topic\\": \\"retro\\"}"}'`}
      />
      <CodeBlock
        label="Close it and disconnect everyone. Needs roomCreate."
        code={`curl -X DELETE ${http}/api/v1/rooms/standup \\
  -H "Authorization: Bearer $TOKEN"`}
      />
      <h2>Participants</h2>
      <p>These routes need <code>roomAdmin</code> for the room in question.</p>
      <CodeBlock
        code={`# Who is in the room, with live media figures
curl "${http}/api/v1/participants?room=standup" -H "Authorization: Bearer $TOKEN"

# Remove someone. Their token still works, so they can rejoin unless your app stops them.
curl -X DELETE ${http}/api/v1/participants/standup/alice -H "Authorization: Bearer $TOKEN"`}
      />
      <h3>Permissions</h3>
      <p>
        <code>PATCH /participants/&#123;room&#125;/&#123;identity&#125;</code> changes a name, metadata, attributes or permissions. A permission object replaces the whole set, so send all of it. Sources are lowercase names.
      </p>
      <CodeBlock
        code={`curl -X PATCH ${http}/api/v1/participants/standup/alice \\
  -H "Authorization: Bearer $TOKEN" \\
  -H "Content-Type: application/json" \\
  -d '{"permission": {"can_publish": true, "can_subscribe": true, "can_publish_data": true,
       "can_publish_sources": ["microphone"]}}'`}
      />
      <h3>Muting</h3>
      <CodeBlock
        code={`curl -X POST ${http}/api/v1/tracks/standup/alice/$TRACK_SID/mute \\
  -H "Authorization: Bearer $TOKEN" \\
  -H "Content-Type: application/json" \\
  -d '{"muted": true}'`}
      />
      <Callout tone="warn" title="Unmuting returns 412 unless the server allows it">
        Turning on someone&rsquo;s microphone from afar is a privacy decision, so it is off until you set <code>room.enable_remote_unmute: true</code> in the server config. The error code is <code>remote_unmute_disabled</code>.
      </Callout>
      <h2>Live figures</h2>
      <p>
        <code>GET /monitor</code> returns a snapshot of the project on the node that serves the request, and <code>GET /monitor/live</code> sends the same every second over a WebSocket. Round trip time and jitter are measured on media a participant receives, so they read zero until they subscribe to something.
      </p>
    </>
  );
}

function Publishing() {
  return (
    <>
      <h1>Publishing media</h1>
      <p>These examples use the <Link href="/docs/js-sdk">JavaScript SDK</Link>. In React, <Link href="/docs/react-sdk">the hooks</Link> do this for you.</p>
      <h2>Microphone and camera</h2>
      <CodeBlock
        code={`const local = room.localParticipant;

await local.setMicrophoneEnabled(true);
await local.setCameraEnabled(true);

// Turn the camera off. Others see the track as muted.
await local.setCameraEnabled(false);`}
      />
      <p><code>enableCameraAndMicrophone()</code> turns both on.</p>
      <h2>Choosing quality and devices</h2>
      <CodeBlock
        code={`import { Room, VideoPresets } from "@elnova-streamkit/client";

const room = new Room({
  videoCaptureDefaults: { resolution: VideoPresets.h720.resolution },
  // Simulcast and dynacast are on by default. Set simulcast to false to send one size.
  publishDefaults: { simulcast: true },
});

await room.switchActiveDevice("videoinput", deviceId);`}
      />
      <p>With simulcast on, the camera is sent in several sizes, and the server forwards each subscriber the one that fits their bandwidth.</p>
      <h2>Receiving</h2>
      <p>By default, everyone is subscribed to everything. Attach tracks as they arrive, and remove the elements when they go.</p>
      <CodeBlock
        code={`room.on(RoomEvent.TrackSubscribed, (track, publication, participant) => {
  const el = track.attach();
  el.dataset.sid = publication.trackSid;
  document.body.appendChild(el);
});

room.on(RoomEvent.TrackUnsubscribed, (track) => {
  track.detach().forEach((el) => el.remove());
});`}
      />
      <p>
        Browsers block sound until someone interacts with the page. If <code>room.canPlaybackAudio</code> is false, show a button that calls <code>room.startAudio()</code>.
      </p>
      <h2>Data messages</h2>
      <CodeBlock
        code={`await room.localParticipant.publishData(new TextEncoder().encode("hello"), {
  reliable: true,
  topic: "chat",
});

room.on(RoomEvent.DataReceived, (payload, participant, kind, topic) => {
  console.log(participant?.identity, new TextDecoder().decode(payload));
});`}
      />
      <h2>Permissions</h2>
      <p>
        If a token has <code>canPublish</code> off, or leaves a source out of <code>canPublishSources</code>, publishing fails with a <code>PublishTrackError</code>. Permissions can also be changed mid-call from the Participants page or the REST API.
      </p>
    </>
  );
}

function ScreenSharing() {
  return (
    <>
      <h1>Screen sharing</h1>
      <p>A screen share is a separate track from the camera, with its own source, so someone can show their face and their screen together.</p>
      <CodeBlock
        code={`await room.localParticipant.setScreenShareEnabled(true, {
  audio: true,               // Tab or system audio, where the browser offers it
  contentHint: "detail",     // Keep text sharp: "detail", "text" or "motion"
  video: { displaySurface: "window" }, // Suggests what the picker opens on
});

// Stop sharing
await room.localParticipant.setScreenShareEnabled(false);`}
      />
      <p>
        It must follow a click or a key press, because browsers refuse <code>getDisplayMedia</code> otherwise. People can also stop sharing from the browser&rsquo;s own bar, and the SDK unpublishes the track when they do.
      </p>
      <h2>Showing a share</h2>
      <p>Check the track&rsquo;s source when it arrives, to lay it out differently from a camera:</p>
      <CodeBlock
        code={`import { RoomEvent, TrackSource } from "@elnova-streamkit/client";

room.on(RoomEvent.TrackSubscribed, (track, publication, participant) => {
  if (publication.source === TrackSource.ScreenShare) {
    stage.replaceChildren(track.attach());
  }
});`}
      />
      <h2>Allowing it</h2>
      <p>
        A token with no <code>canPublishSources</code> can publish anything. To stop someone sharing, leave <code>screen_share</code> and <code>screen_share_audio</code> out of that list, in their token or in their permissions on the Participants page.
      </p>
      <h2>Limits</h2>
      <ul>
        <li>Phones mostly cannot share a screen from a browser.</li>
        <li>Which audio can be captured depends on the browser and the operating system.</li>
      </ul>
    </>
  );
}

function Webhooks() {
  return (
    <>
      <h1>Webhooks</h1>
      <p>
        Add an endpoint on the <Link href="/webhooks">Webhooks page</Link>, and the server POSTs JSON to it when something happens in the project. The delivery history there shows every attempt and why it failed.
      </p>
      <h2>Events</h2>
      <table>
        <thead>
          <tr>
            <th>Event</th>
            <th>When</th>
          </tr>
        </thead>
        <tbody>
          <tr><td><code>room_started</code>, <code>room_finished</code></td><td>A room session opens or closes</td></tr>
          <tr><td><code>participant_joined</code>, <code>participant_left</code></td><td>Someone arrives or goes</td></tr>
          <tr><td><code>participant_connection_aborted</code></td><td>Someone signalled, but could not establish media</td></tr>
          <tr><td><code>track_published</code>, <code>track_unpublished</code></td><td>A track starts or stops</td></tr>
          <tr><td><code>egress_started</code>, <code>egress_updated</code>, <code>egress_ended</code></td><td>A recording or egress job changes. See <Link href="/docs/egress">Egress</Link></td></tr>
          <tr><td><code>ingress_started</code>, <code>ingress_ended</code></td><td>A stream starts or stops arriving. See <Link href="/docs/ingress">Ingress</Link></td></tr>
        </tbody>
      </table>
      <p>An endpoint with no events selected receives all of them.</p>
      <h2>What arrives</h2>
      <CodeBlock
        label="POST to your endpoint"
        code={`Content-Type: application/webhook+json
User-Agent: Elnova-StreamKit
Authorization: eyJhbGciOiJIUzI1NiIs...

{
  "event": "room_started",
  "id": "EV_...",
  "createdAt": "1793693623",
  "room": { "sid": "RM_...", "name": "standup", "creationTime": "1793693623" }
}`}
      />
      <p>
        Field names are in lowerCamelCase, and default values are left out, unlike the REST API. Numbers that can be large, such as <code>createdAt</code>, are strings. Track events carry only the identifying fields of the room and participant. A test event from the dashboard says <code>room_started</code>, in a room named <code>test-delivery</code>.
      </p>
      <h2>Verifying a delivery</h2>
      <p>
        <code>Authorization</code> holds a JWT, with no Bearer prefix, signed with the secret of the endpoint&rsquo;s signing key. Its <code>iss</code> is the API key, and its <code>sha256</code> claim is the base64 SHA-256 of the exact body. Signatures last 5 minutes, which limits replay. Check them against the raw bytes, before parsing.
      </p>
      <CodeBlock
        label="Express, with jsonwebtoken"
        code={`import crypto from "node:crypto";
import express from "express";
import jwt from "jsonwebtoken";

const app = express();

app.post("/streamkit-events", express.raw({ type: "application/webhook+json" }), (req, res) => {
  let claims;
  try {
    claims = jwt.verify(req.headers.authorization, process.env.STREAMKIT_API_SECRET, {
      algorithms: ["HS256"],
      issuer: process.env.STREAMKIT_API_KEY,
    });
  } catch {
    return res.sendStatus(401);
  }
  const sum = crypto.createHash("sha256").update(req.body).digest("base64");
  if (claims.sha256 !== sum) return res.sendStatus(401);

  const event = JSON.parse(req.body);
  console.log(event.event, event.room?.name);
  res.sendStatus(200);
});`}
      />
      <h2>Delivery and retries</h2>
      <ul>
        <li>A 2xx answer means delivered. Each attempt has 10 seconds.</li>
        <li>429 and 5xx answers, except 501, and connection failures, retry up to 6 attempts in all, after 1, 5, 15, 30 and 60 seconds.</li>
        <li>Any other answer is a considered reply, so it fails at once.</li>
        <li>Redirects are not followed. A delivery only goes where it was configured to.</li>
        <li>Failed deliveries can be sent again from the delivery history. History is kept for 30 days.</li>
        <li>A delivery can arrive more than once if a node dies mid-send. Use the event&rsquo;s <code>id</code> to spot repeats.</li>
      </ul>
      <p>
        URLs on private networks and localhost are refused, unless the server sets <code>control_plane.allow_private_webhooks</code>. This is checked again at connect time, so a public name that later resolves to an internal address cannot be used to reach your network.
      </p>
    </>
  );
}

function JsSdk() {
  const { ws } = useDocsAddress();
  return (
    <>
      <h1>JavaScript SDK</h1>
      <Callout title="Version 0.1, and new">
        <code>@elnova-streamkit/client</code> is in this repository as <code>sdk-js</code>, and is not on a package registry yet. It is a browser client, written in TypeScript. There is no server SDK yet: your backend signs tokens with any JWT library, and calls the REST API directly.
      </Callout>
      <h2>Install</h2>
      <CodeBlock
        code={`(cd sdk-js && npm ci && npm run build)
npm install ../path/to/elnova-streamkit/sdk-js`}
      />
      <h2>Connect</h2>
      <CodeBlock
        code={`import { ConnectionState, Room, RoomEvent } from "@elnova-streamkit/client";

const room = new Room({ adaptiveStream: true });

room.on(RoomEvent.ConnectionStateChanged, (state) => console.log(state));
room.on(RoomEvent.Disconnected, (reason) => console.log("left", reason));

await room.connect("${ws}", token);`}
      />
      <p>
        The SDK reconnects by itself when the network drops. Tell people about it with <code>Reconnecting</code> and <code>Reconnected</code>. Pass your own <code>reconnectPolicy</code> to change the timing.
      </p>
      <h2>Options</h2>
      <table>
        <thead>
          <tr>
            <th><code>new Room(options)</code></th>
            <th>Meaning</th>
          </tr>
        </thead>
        <tbody>
          <tr><td><code>adaptiveStream</code></td><td>Receive each video only as large as it is shown, and not at all while it cannot be seen</td></tr>
          <tr><td><code>dynacast</code></td><td>Stop encoding video layers nobody is watching. On by default</td></tr>
          <tr><td><code>audioCaptureDefaults</code>, <code>videoCaptureDefaults</code></td><td>Defaults for capture</td></tr>
          <tr><td><code>publishDefaults</code></td><td>Defaults for publishing, such as simulcast</td></tr>
          <tr><td><code>stopLocalTrackOnUnpublish</code></td><td>Stop capture when a track is unpublished. On by default</td></tr>
          <tr><td><code>disconnectOnPageLeave</code></td><td>Leave when the page goes away, so others see it straight away. On by default</td></tr>
          <tr><td><code>reconnectPolicy</code></td><td>Your own retry timing</td></tr>
        </tbody>
      </table>
      <table>
        <thead>
          <tr>
            <th><code>room.connect(url, token, options)</code></th>
            <th>Meaning</th>
          </tr>
        </thead>
        <tbody>
          <tr><td><code>autoSubscribe</code></td><td>Receive every track without asking. On by default</td></tr>
          <tr><td><code>websocketTimeout</code></td><td>Milliseconds to wait for signalling. Default 15000</td></tr>
          <tr><td><code>peerConnectionTimeout</code></td><td>Milliseconds to wait for a peer connection. Default 15000</td></tr>
          <tr><td><code>maxRetries</code></td><td>Attempts when the server cannot be reached. Default 1</td></tr>
          <tr><td><code>rtcConfig</code></td><td>Merged with what the server provides. ICE servers here replace the server&rsquo;s</td></tr>
        </tbody>
      </table>
      <h2>Main objects</h2>
      <ul>
        <li><strong>Room:</strong> <code>connect</code>, <code>disconnect</code>, <code>localParticipant</code>, <code>remoteParticipants</code>, <code>startAudio</code>, <code>switchActiveDevice</code>.</li>
        <li><strong>LocalParticipant:</strong> <code>setMicrophoneEnabled</code>, <code>setCameraEnabled</code>, <code>setScreenShareEnabled</code>, <code>enableCameraAndMicrophone</code>, <code>publishTrack</code>, <code>unpublishTrack</code>, <code>publishData</code>, <code>setMetadata</code>, <code>setName</code>, <code>setAttributes</code>.</li>
        <li><strong>RemoteParticipant:</strong> <code>identity</code>, <code>name</code>, <code>metadata</code>, <code>attributes</code>, and its track publications.</li>
        <li><strong>Track:</strong> <code>attach</code>, <code>detach</code>, <code>source</code>, <code>kind</code>.</li>
        <li><strong>Events:</strong> <code>RoomEvent</code>, <code>ParticipantEvent</code> and <code>TrackEvent</code> list everything you can listen for.</li>
        <li><strong>Errors:</strong> <code>ConnectionError</code>, with a <code>ConnectionErrorReason</code>, and <code>PublishTrackError</code>, <code>TrackInvalidError</code> and others.</li>
      </ul>
      <p>The full surface is what <code>sdk-js/src/index.ts</code> exports.</p>
      <h2>Not implemented yet</h2>
      <ul>
        <li>Single peer connection mode. The server answers 404 to it on purpose, and clients use two peer connections.</li>
        <li>Remote procedure calls between participants. The server does not implement them yet.</li>
      </ul>
    </>
  );
}

function ReactSdk() {
  const { ws } = useDocsAddress();
  return (
    <>
      <h1>React SDK</h1>
      <Callout title="Version 0.1, and new">
        <code>@elnova-streamkit/react</code> is in this repository as <code>sdk-react</code>, and is not on a package registry yet. It needs <code>@elnova-streamkit/client</code> and React 18 or 19. It brings no styles: components expose their state as data attributes for you to style.
      </Callout>
      <h2>A small meeting page</h2>
      <CodeBlock
        code={`import {
  ParticipantTile,
  RoomAudioRenderer,
  RoomProvider,
  getTrackReferenceId,
  useLocalParticipant,
  useTracks,
} from "@elnova-streamkit/react";
import { TrackSource } from "@elnova-streamkit/client";

function Stage() {
  const tracks = useTracks([{ source: TrackSource.Camera, withPlaceholder: true }]);
  return tracks.map((t) => <ParticipantTile key={getTrackReferenceId(t)} trackRef={t} />);
}

function Controls() {
  const { localParticipant, isMicrophoneEnabled } = useLocalParticipant();
  return (
    <button onClick={() => localParticipant.setMicrophoneEnabled(!isMicrophoneEnabled)}>
      {isMicrophoneEnabled ? "Mute" : "Unmute"}
    </button>
  );
}

export function Meeting({ token }) {
  return (
    <RoomProvider serverUrl="${ws}" token={token} audio video>
      <Stage />
      <Controls />
      <RoomAudioRenderer />
    </RoomProvider>
  );
}`}
      />
      <p>
        <code>RoomProvider</code> joins when it mounts and leaves when it unmounts. Changing its <code>audio</code>, <code>video</code> or <code>screen</code> props turns those on and off while connected. Its <code>useRoom</code> hook does the same without a context, if you would rather pass the room around.
      </p>
      <p><code>RoomAudioRenderer</code> plays everyone&rsquo;s audio. Without it, or per-participant <code>AudioTrack</code>s, you will hear nothing.</p>
      <h2>Hooks</h2>
      <table>
        <thead>
          <tr>
            <th>Hook</th>
            <th>Gives you</th>
          </tr>
        </thead>
        <tbody>
          <tr><td><code>useRoom</code></td><td>A connected room for the lifetime of a component</td></tr>
          <tr><td><code>useRoomContext</code></td><td>The room from the nearest provider</td></tr>
          <tr><td><code>useConnectionState</code></td><td>Connecting, connected, reconnecting or disconnected</td></tr>
          <tr><td><code>useParticipants</code>, <code>useRemoteParticipants</code></td><td>Who is in the room</td></tr>
          <tr><td><code>useLocalParticipant</code></td><td>You, and whether your camera, microphone and screen are on</td></tr>
          <tr><td><code>useTracks</code></td><td>Tracks by source, with placeholders to reserve layout space</td></tr>
          <tr><td><code>useParticipantTrack</code></td><td>One participant&rsquo;s track for a source</td></tr>
          <tr><td><code>useIsSpeaking</code>, <code>useActiveSpeakers</code></td><td>Who is talking</td></tr>
          <tr><td><code>useConnectionQuality</code></td><td>A participant&rsquo;s network quality</td></tr>
          <tr><td><code>useDataChannel</code></td><td>Send and receive messages, by topic</td></tr>
          <tr><td><code>useMediaDevices</code>, <code>useMediaDeviceSelect</code></td><td>Camera, microphone and speaker pickers</td></tr>
          <tr><td><code>useAudioPlayback</code></td><td>Whether the browser is blocking sound, and a way to start it</td></tr>
        </tbody>
      </table>
      <h2>Components</h2>
      <ul>
        <li><code>RoomProvider</code>: joins a room and provides it to everything inside.</li>
        <li><code>ParticipantTile</code>: one person&rsquo;s video and name. Style it with <code>data-speaking</code>, <code>data-video-muted</code>, <code>data-audio-muted</code> and <code>data-quality</code>.</li>
        <li><code>VideoTrack</code> and <code>AudioTrack</code>: a single track, attached and detached for you.</li>
        <li><code>RoomAudioRenderer</code>: everyone&rsquo;s audio.</li>
      </ul>
    </>
  );
}

function MediaStatusNote({ name }: { name: string }) {
  return (
    <Callout tone="warn" title="New, and depends on your build">
      The badge above asks this server whether it has the {name} routes. Where it does not, the page in the console says the service is not running.
    </Callout>
  );
}

function Recording() {
  return (
    <>
      <h1>Recording</h1>
      <ServiceStatus path="/recordings" name="Recording" />
      <MediaStatusNote name="recording" />
      <p>A recording is an egress job with a file output. The two lists read the same data: anything recorded also appears under <Link href="/docs/egress">Egress</Link>.</p>
      <h2>What has to be running</h2>
      <p>
        <code>streamkit-server</code> takes the requests. A separate worker, <code>streamkit-egress</code>, does the recording, with headless Chrome and ffmpeg. Run one with the same config file as the server:
      </p>
      <CodeBlock
        code={`go build -o bin/streamkit-egress ./egress/cmd/streamkit-egress
bin/streamkit-egress --config streamkit.yaml`}
      />
      <p>Workers appear on the <Link href="/servers">Servers page</Link>. By default, one takes as many recordings at once as a third of its CPU cores. Files go where <code>media.storage</code> says: a local directory, or S3-compatible storage.</p>
      <h2>Start a recording</h2>
      <p>The room must be open. From your backend, this needs a token with <code>roomRecord</code>.</p>
      <CodeBlock
        code={`POST /api/v1/egress

{
  "type": "room_composite",
  "room": "standup",
  "layout": "grid",
  "output": { "kind": "file", "format": "mp4" }
}`}
      />
      <p>With no worker available, this answers 503 with the code <code>egress_unavailable</code>. If the room is not open, it answers 404 with <code>room_not_found</code>.</p>
      <h2>Routes</h2>
      <table>
        <thead>
          <tr>
            <th>Route</th>
            <th>What it does</th>
          </tr>
        </thead>
        <tbody>
          <tr><td><code>GET /recordings</code></td><td>Jobs with a file output, newest first</td></tr>
          <tr><td><code>GET /recordings/&#123;id&#125;</code></td><td>One job</td></tr>
          <tr><td><code>GET /recordings/&#123;id&#125;/file</code></td><td>The file. Supports Range, so it plays in a video element and seeks. Add <code>download=1</code> to save it</td></tr>
          <tr><td><code>POST /recordings/&#123;id&#125;/retry</code></td><td>Starts a new job with the same request, once this one has ended</td></tr>
          <tr><td><code>DELETE /recordings/&#123;id&#125;</code></td><td>Deletes the file and the record, once the job has ended</td></tr>
        </tbody>
      </table>
      <p>A job&rsquo;s <code>status</code> is <code>starting</code>, <code>active</code>, <code>ending</code>, <code>complete</code>, <code>failed</code> or <code>aborted</code>. Stop a running one with <code>POST /egress/&#123;id&#125;/stop</code>. WebM output is available where the codec allows it.</p>
      <p>A video element cannot send headers, so with a session cookie, name the project in the query string: <code>?project_id=</code>.</p>
    </>
  );
}

function Egress() {
  return (
    <>
      <h1>Egress</h1>
      <ServiceStatus path="/egress" name="Egress" />
      <MediaStatusNote name="egress" />
      <p>Egress sends a room out of the platform: to a file, to RTMP destinations, or to both at once. It needs a <code>streamkit-egress</code> worker: see <Link href="/docs/recording">Recording</Link>.</p>
      <CodeBlock
        code={`POST /api/v1/egress

{
  "type": "room_composite | participant | audio_only | video_only",
  "room": "standup",
  "participant": "identity, required for participant",
  "layout": "grid | speaker",
  "output": { "kind": "file", "format": "mp4 | webm" },
  "stream_urls": ["rtmp://a.rtmp.youtube.com/live2/your-key"]
}`}
      />
      <p>
        <code>output</code> and <code>stream_urls</code> can be combined, and at least one is required, with up to 5 stream URLs. <code>audio_only</code> and <code>video_only</code> apply to the whole room, or to one person when <code>participant</code> is set.
      </p>
      <p>Stream URLs usually contain a stream key, so the API only ever returns them with the key removed. URLs on private networks are refused unless the server sets <code>media.allow_private_addresses</code>.</p>
      <h2>Routes</h2>
      <table>
        <thead>
          <tr>
            <th>Route</th>
            <th>What it does</th>
          </tr>
        </thead>
        <tbody>
          <tr><td><code>GET /egress</code></td><td>List, newest first</td></tr>
          <tr><td><code>POST /egress</code></td><td>Start a job. 503 with <code>egress_unavailable</code> when no worker is available</td></tr>
          <tr><td><code>GET /egress/&#123;id&#125;</code></td><td>One job</td></tr>
          <tr><td><code>POST /egress/&#123;id&#125;/stop</code></td><td>Stop a running job. 409 with <code>egress_not_running</code> if it has ended</td></tr>
        </tbody>
      </table>
    </>
  );
}

function Ingress() {
  return (
    <>
      <h1>Ingress</h1>
      <ServiceStatus path="/ingress" name="Ingress" />
      <MediaStatusNote name="ingress" />
      <p>An ingress brings an outside stream into a room, as a participant: push from OBS over RTMP, push over WHIP, or have the server pull from a URL.</p>
      <h2>What has to be running</h2>
      <p>
        RTMP and URL inputs need a worker, <code>streamkit-ingress</code>, which listens for RTMP on port 1935 by default and transcodes with ffmpeg. WHIP publishes straight to <code>streamkit-server</code>, so it needs no worker.
      </p>
      <CodeBlock
        code={`go build -o bin/streamkit-ingress ./ingress/cmd/streamkit-ingress
bin/streamkit-ingress --config streamkit.yaml`}
      />
      <h2>Create one</h2>
      <p>From your backend, this needs a token with <code>ingressAdmin</code>.</p>
      <CodeBlock
        code={`POST /api/v1/ingress

{
  "name": "Main stage",
  "input_type": "rtmp | whip | url",
  "room": "standup",
  "participant_identity": "stage",
  "participant_name": "Main stage",
  "source_url": "for url input: https://... or srt://..."
}`}
      />
      <p>
        Creating one works without a worker, because it is only configuration. The response has <code>publish_url</code>, and <code>stream_key</code>, which is shown only here and when you rotate it.
      </p>
      <h2>Using it</h2>
      <ul>
        <li><strong>RTMP:</strong> in OBS, set the server to <code>publish_url</code> and paste the stream key.</li>
        <li><strong>WHIP:</strong> <code>publish_url</code> is the endpoint, and the key goes in <code>Authorization: Bearer &lt;key&gt;</code>.</li>
        <li><strong>URL:</strong> a worker pulls from <code>source_url</code>. There is no stream key.</li>
      </ul>
      <h2>Routes</h2>
      <table>
        <thead>
          <tr>
            <th>Route</th>
            <th>What it does</th>
          </tr>
        </thead>
        <tbody>
          <tr><td><code>GET /ingress</code></td><td>List</td></tr>
          <tr><td><code>POST /ingress</code></td><td>Create</td></tr>
          <tr><td><code>GET /ingress/&#123;id&#125;</code></td><td>One, with <code>status</code> and health <code>stats</code></td></tr>
          <tr><td><code>PATCH /ingress/&#123;id&#125;</code></td><td>Change name, room, identity or enabled</td></tr>
          <tr><td><code>DELETE /ingress/&#123;id&#125;</code></td><td>Delete, and stop any running stream</td></tr>
          <tr><td><code>POST /ingress/&#123;id&#125;/stop</code></td><td>Stop the running stream. The ingress can be used again</td></tr>
          <tr><td><code>POST /ingress/&#123;id&#125;/rotate-key</code></td><td>New stream key, returned once. 409 for URL inputs, which have none</td></tr>
          <tr><td><code>GET /ingress/&#123;id&#125;/logs</code></td><td>Newest first, from the event log</td></tr>
        </tbody>
      </table>
      <p><code>status</code> is <code>inactive</code>, <code>buffering</code>, <code>publishing</code> or <code>error</code>. An RTMP push with no worker running never connects, so it stays <code>inactive</code>. Check the Servers page for worker health.</p>
    </>
  );
}

function SelfHosting() {
  return (
    <>
      <h1>Self-hosted deployment</h1>
      <Callout title="A candidate, not battle-tested">
        The full guide, including what has and has not been tried, is <code>docs/deployment/self-hosting.md</code> in the repository. The Compose stack is <code>infrastructure/compose/production.yml</code>. Run it on a staging host before relying on it.
      </Callout>
      <h2>How it fits together</h2>
      <p>
        Signalling and APIs go through nginx. Media does not: clients send it straight to the server that hosts their room. So every media server needs its own public IP, and a load balancer can only ever carry signalling.
      </p>
      <h2>Ports</h2>
      <table>
        <thead>
          <tr>
            <th>Port</th>
            <th>Purpose</th>
          </tr>
        </thead>
        <tbody>
          <tr><td>443/TCP</td><td>Dashboard, API and signalling. 80 redirects to it</td></tr>
          <tr><td>7882/UDP</td><td>Media. The important one</td></tr>
          <tr><td>7881/TCP</td><td>Media fallback, for networks that block UDP</td></tr>
          <tr><td>3478/TCP and UDP, 5349/TCP</td><td>STUN and TURN</td></tr>
          <tr><td>49160-49800/UDP</td><td>TURN relay ports</td></tr>
        </tbody>
      </table>
      <p>Never expose 7880, PostgreSQL or Redis publicly.</p>
      <h2>Install</h2>
      <CodeBlock
        code={`cp infrastructure/compose/.env.example .env
# Fill in .env. Generate each secret with: openssl rand -hex 32
docker compose -f infrastructure/compose/production.yml --env-file .env up -d --build`}
      />
      <p>Open your domain and register straight away, because <strong>the first account becomes the platform administrator</strong>.</p>
      <h2>The settings that matter most</h2>
      <table>
        <thead>
          <tr>
            <th>Setting</th>
            <th>Why</th>
          </tr>
        </thead>
        <tbody>
          <tr><td><code>STREAMKIT_PUBLIC_IP</code></td><td>The one address advertised to clients. Wrong here means signalling connects and no media ever flows. The most common failure</td></tr>
          <tr><td><code>STREAMKIT_MASTER_KEY</code></td><td>Encrypts API secrets at rest. Back it up apart from your database. Without it they are unreadable</td></tr>
          <tr><td><code>control_plane.public_dashboard_url</code></td><td>Used in email links, and the only origin the live feed accepts</td></tr>
          <tr><td><code>control_plane.public_ws_url</code></td><td>The address shown as what clients connect to</td></tr>
          <tr><td><code>control_plane.secure_cookies</code></td><td>Turn on over HTTPS</td></tr>
          <tr><td><code>control_plane.trust_proxy_headers</code></td><td>Turn on behind a proxy that overwrites X-Forwarded-For. Otherwise every user shares the proxy&rsquo;s rate limit</td></tr>
          <tr><td><code>room.enable_remote_unmute</code></td><td>Lets the API unmute people. Off by default</td></tr>
        </tbody>
      </table>
      <h2>This dashboard</h2>
      <p>
        The dashboard is a Next.js app. Behind one host, nginx sends <code>/api/</code> to the server, and <code>/api/v1/monitor/live</code> as a WebSocket, so nothing else is needed. If it runs on its own address, set these when you build it:
      </p>
      <table>
        <thead>
          <tr>
            <th>Variable</th>
            <th>Meaning</th>
          </tr>
        </thead>
        <tbody>
          <tr><td><code>STREAMKIT_API_URL</code></td><td>Where the app proxies <code>/api/v1/*</code>. Read at build time</td></tr>
          <tr><td><code>NEXT_PUBLIC_STREAMKIT_WS_ORIGIN</code></td><td>Origin of the live WebSocket, which cannot be proxied by the app. Read at build time. Empty means the page&rsquo;s own origin</td></tr>
        </tbody>
      </table>
      <h2>Media workers</h2>
      <p>Recording and ingest run as separate workers: <code>streamkit-egress</code> and <code>streamkit-ingress</code>, with the same config file as the server. See <Link href="/docs/recording">Recording</Link> and <Link href="/docs/ingress">Ingress</Link>. The Compose file above does not start them yet, so run them yourself, on a machine with Chrome and ffmpeg.</p>
      <h2>Draining a node</h2>
      <p>
        Before maintenance, drain a node on the <Link href="/servers">Servers page</Link>, as a platform administrator. It stops taking new sessions, and existing rooms carry on. Joining a draining node fails with <code>server is draining</code>.
      </p>
      <h2>Retention</h2>
      <p>Events and webhook deliveries are kept for 30 days, usage for 400 days, and servers that have gone quiet for 7 days. Back up PostgreSQL: it is the only store that matters.</p>
    </>
  );
}

function Troubleshooting() {
  return (
    <>
      <h1>Troubleshooting</h1>
      <h2>Connecting</h2>
      <h3>Clients connect, then no video appears</h3>
      <p>
        ICE failed. On a server, check <code>STREAMKIT_PUBLIC_IP</code>, and that UDP 7882 is open from outside. <code>chrome://webrtc-internals</code> shows which candidates were tried. For people on networks that block UDP, check 7881/TCP and set up TURN.
      </p>
      <h3>invalid authorization token, or invalid API key</h3>
      <p>
        The token is malformed, expired, or signed with a key this server does not know. Revoked and rotated keys stay cached for up to 30 seconds on other nodes. A suspended project&rsquo;s keys stop verifying at once.
      </p>
      <h3>this web origin is not allowed to connect to the project</h3>
      <p>The page&rsquo;s origin is not in the project&rsquo;s allowed origins. Add it in Developer settings, with the scheme and no path: <code>https://app.example.com</code>.</p>
      <h3>requested room does not exist</h3>
      <p>Rooms do not open by themselves in this project. Create the room first, or turn that setting back on.</p>
      <h3>max participants exceeded</h3>
      <p>The room is full. Its limit is on its details page.</p>
      <h3>server is draining</h3>
      <p>Someone drained this node. Resume it on the Servers page.</p>
      <h2>In this dashboard</h2>
      <h3>The status line says Polling instead of Live</h3>
      <p>
        The live WebSocket could not connect, so figures are fetched every 3 seconds instead. The server only accepts the origin in <code>public_dashboard_url</code>, so check that the address in your browser matches it exactly: <code>localhost</code> and <code>127.0.0.1</code> are different. Then check that <code>NEXT_PUBLIC_STREAMKIT_WS_ORIGIN</code> points at the server. It is read at build time.
      </p>
      <h3>RTT and jitter read Not measured</h3>
      <p>They are measured on media a participant receives. With one person in a room, nobody is subscribed to anything, so there is nothing to measure.</p>
      <h3>Project required</h3>
      <p>With a session, API calls must name a project in the <code>X-Project-ID</code> header. The dashboard does this for you, and tools using a cookie must too.</p>
      <h3>Too many requests</h3>
      <p>
        Sign-in routes allow 10 attempts at once, then 20 a minute, per address. The rest of the API allows 1200 a minute per user. Behind a proxy, turn on <code>trust_proxy_headers</code>, or everyone shares one address.
      </p>
      <h3>Your email link did not arrive</h3>
      <p>
        With no SMTP settings, the server logs a warning and sends nothing. For development, set <code>control_plane.mail.dir</code>, and messages are written there as files. Links use <code>public_dashboard_url</code>.
      </p>
      <h2>API errors</h2>
      <p>Errors look the same everywhere:</p>
      <CodeBlock code={`{"error": {"code": "validation_failed", "message": "Some fields need attention.", "fields": {"name": "is required"}}}`} />
      <table>
        <thead>
          <tr>
            <th>Code</th>
            <th>Meaning</th>
          </tr>
        </thead>
        <tbody>
          <tr><td><code>unauthenticated</code></td><td>No valid session or token</td></tr>
          <tr><td><code>invalid_token</code></td><td>A token was sent, and it did not verify</td></tr>
          <tr><td><code>forbidden</code></td><td>Your role or the token&rsquo;s grants do not allow it</td></tr>
          <tr><td><code>project_required</code></td><td>Send <code>X-Project-ID</code></td></tr>
          <tr><td><code>unsupported_media_type</code></td><td>Requests with a session must be <code>application/json</code>, even DELETE</td></tr>
          <tr><td><code>room_not_found</code></td><td>That room is not open</td></tr>
          <tr><td><code>participant_not_found</code></td><td>They are not in the room</td></tr>
          <tr><td><code>remote_unmute_disabled</code></td><td>Unmuting needs <code>room.enable_remote_unmute</code>. Status 412</td></tr>
          <tr><td><code>no_api_key</code></td><td>Create an API key for this project first</td></tr>
          <tr><td><code>rate_limited</code></td><td>Wait a moment</td></tr>
          <tr><td><code>egress_unavailable</code></td><td>No recording worker has room. Status 503</td></tr>
        </tbody>
      </table>
    </>
  );
}

export const DOC_PAGES: Record<string, React.ComponentType> = {
  "getting-started": GettingStarted,
  authentication: Authentication,
  "rooms-and-participants": RoomsAndParticipants,
  publishing: Publishing,
  "screen-sharing": ScreenSharing,
  webhooks: Webhooks,
  "js-sdk": JsSdk,
  "react-sdk": ReactSdk,
  recording: Recording,
  egress: Egress,
  ingress: Ingress,
  "self-hosting": SelfHosting,
  troubleshooting: Troubleshooting,
};
