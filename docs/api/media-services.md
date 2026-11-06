# Media services API

The contract between the dashboard and the egress and ingress services, under `/api/v1`, with the same
authentication, project scoping and error format as the rest of the API. Times are RFC 3339.

## Egress and recordings

A recording is an egress with a file output. Both lists read the same data.

```
GET  /egress                  list, newest first: { items, next_cursor }
POST /egress                  start a job
GET  /egress/{id}             one job
POST /egress/{id}/stop        stop a running job
GET  /recordings              jobs with a file output: { items, next_cursor }
GET  /recordings/{id}         one job
GET  /recordings/{id}/file    the file; supports Range, so it plays in a <video> element and downloads
POST /recordings/{id}/retry   start a new job with the same request; returns the new job
DELETE /recordings/{id}       delete the file and the record
```

`POST /egress` body:

```json
{
  "type": "room_composite | participant | audio_only | video_only",
  "room": "room name",
  "participant": "identity, required for participant",
  "layout": "grid | speaker",
  "output": { "kind": "file", "format": "mp4 | webm" },
  "stream_urls": ["rtmp://..."]
}
```

`output` and `stream_urls` can be combined, and at least one is required. `audio_only` and `video_only` apply to
the whole room, or to one person when `participant` is set. WebM output is available where the codec allows it.

Job:

```json
{
  "id": "EG_...", "project_id": "...", "room_name": "...", "room_sid": "...",
  "type": "room_composite", "status": "starting | active | ending | complete | failed | aborted",
  "format": "mp4", "output_url": "/api/v1/recordings/EG_.../file", "stream_urls": [],
  "file_size": 0, "duration_ms": 0, "error": "",
  "created_at": "...", "started_at": null, "ended_at": null, "retry_of": null
}
```

Starting with no worker available returns 503 with code `egress_unavailable`. That includes every worker being
full, or draining. A project already running `media.egress.max_jobs_per_project` jobs gets 429 with code `project_limit`.
A room that is not open returns 404 with code `room_not_found`.

A `<video>` element cannot set headers, so the file route also takes the project as a query parameter:
`<video src="/api/v1/recordings/EG_.../file?project_id=...">`. Add `download=1` to save it instead of playing it.
`output_url` is set from the start on any job with a file output, and the route returns 404 with code
`recording_not_ready` until there is a file.

Status means:

- `starting`: waiting for a worker, or for something to record. A recording of an empty room stays here until someone
  publishes, for up to `media.egress.wait_for_media_seconds`, 15 minutes by default. `started_at` is when media began.
- `active`: media is arriving at the worker. `ending`: stopping, and finishing the file. A recording that reaches
  `media.egress.max_duration_seconds`, 24 hours by default, stops by itself and finishes as usual.
- `complete`: the output is stored, and the recording ended because someone stopped it, the room closed, or it reached its
  longest. Nothing says `complete` before the file is durable.
- `aborted`: it ended with nothing recorded, because it was stopped, the room closed or nothing arrived in time, before
  anyone published. There is no file.
- `failed`: it broke, and `error` says how. `file_size` above zero means part of it was saved and can be played. That
  includes the recorder losing the room while the room may be going on without it: a server restarting, the recorder being
  removed, or a connection that could not be brought back.

`POST /egress/{id}/stop` on a job that has ended returns 409 with code `egress_not_running`. Retrying or deleting a job
that is running returns 409 with code `egress_running`. Retrying when the room has closed returns 404 with code
`room_not_found`. Deleting a recording whose file is on a storage backend the server is no longer using returns 409 with code
`recording_unavailable`, and nothing is deleted.

`stream_urls` in a job are redacted: `rtmp://host/app/{abc...xyz}`. Stream URLs accept `rtmp://` and `rtmps://`, with the key
at the end of the path. A username and password in the address are refused, because they would have to be on ffmpeg's
command line. The certificate of an `rtmps://` destination is checked against its name, using the system's trusted roots, or
those in `SSL_CERT_FILE`. A stream destination that fails, including one that cannot be used when recording starts, shows up as
an `egress_stream_failed` event, and the rest of the job carries on: a job with a file output keeps recording, and a job with
only streams fails when the last one has gone.

A token needs the `roomRecord` grant for these routes, and `ingressAdmin` for the ingress routes.

## Ingress

```
GET    /ingress                list: { items }
POST   /ingress                create
GET    /ingress/{id}           one
PATCH  /ingress/{id}           change name, room, identity or enabled
DELETE /ingress/{id}           delete, and stop any running stream
POST   /ingress/{id}/stop      stop the running stream; the ingress can be used again
POST   /ingress/{id}/rotate-key  new stream key; returned once
GET    /ingress/{id}/logs      { items }, newest first, from the event log
```

`POST /ingress` body:

```json
{
  "name": "Main stage",
  "input_type": "rtmp | whip | url",
  "room": "room name",
  "participant_identity": "stage",
  "participant_name": "Main stage",
  "source_url": "for url input: https://... or srt://..."
}
```

A source's address often has a password or a token in it, so it is kept sealed, and `source_url` in responses is redacted the
way `stream_urls` are.

Ingress:

```json
{
  "id": "IN_...", "name": "...", "input_type": "rtmp", "room_name": "...", "participant_identity": "...",
  "participant_name": "...", "source_url": "", "enabled": true,
  "publish_url": "rtmp://host:1935/live",
  "stream_key": "only in the responses to create and rotate-key",
  "status": "inactive | buffering | publishing | error",
  "error": "",
  "stats": { "bitrate_in": 0, "width": 0, "height": 0, "fps": 0, "audio_codec": "", "video_codec": "", "started_at": null },
  "created_at": "...", "updated_at": "..."
}
```

For WHIP, `publish_url` is the endpoint and the stream key is sent as `Authorization: Bearer <key>`.

Creating an ingress works without a worker, because it is only configuration. A worker that is not running
shows in `status` when a stream arrives, which it cannot. Check `GET /servers` for worker health: a worker's row has
`stats.kind` of `egress_worker` or `ingress_worker`, and `stats.healthy`. Platform administrators also see `stats.max_jobs`,
`stats.active_jobs` and, when `healthy` is false, `stats.unhealthy_reason`. A worker that is not healthy takes no new work
until it is fixed: for a recording worker, because its working directory or storage cannot be written, its disk is full, or
Chrome or ffmpeg cannot run; for an ingest worker, because ffmpeg cannot run or the RTMP port has stopped answering.

An RTMP publisher sends to `publish_url` with the stream key as the stream name: `rtmp://host:1935/live/<key>`. A wrong
key, a disabled ingress or a second publisher on one key is refused. So is a WHIP publisher, with 401, 403 and 409, and with
429 for too many attempts from one address, or a project already running `media.ingress.max_sessions_per_project` streams.
The same `Authorization` header is required on `DELETE` of the WHIP resource. Stream keys of a project that has been deleted
or suspended work no more than a wrong key would.

A URL input starts when it is created, and accepts `http`, `https`, `rtmp` and `srt://` in caller mode, in the containers
ffmpeg can read directly, and HLS. An `rtmp://` address cannot hold a username and password. It ends when the source
does, or on stop, and does not start again by itself. `PATCH` with `"enabled": true` starts it again. The exception is a
worker going away, whether it is restarting or has died: nobody asked for that stream to end and no publisher can bring it
back, so the next worker takes it up, unless someone had asked it to stop.

`PATCH` of `room`, `participant_identity` or `enabled: false` ends a running stream, since it cannot take those on while
it runs. They apply when the publisher reconnects, or for a URL input, when it starts again by itself. `rotate-key` ends a
running stream too, and on a URL input returns 409 with code `no_stream_key`.

Pushing over SRT is not implemented, and is not on the way without new work. ffmpeg's SRT listener takes one caller per process
per port, and accepts it before anything can check who it is, so it has no place for a stream key. Doing it with ffmpeg would
take a UDP port and a waiting process for each ingress, and doing it properly takes an SRT listener in the worker that
routes callers by stream ID. The worker refuses `mode=listener` in a URL input for a related reason: a URL from a project
must not open a port on the worker.

In `stats`, `bitrate_in` is bits per second. For a pull over SRT, where the worker cannot see the source's bytes, it is the
transcoded bitrate instead. For WHIP, which is not decoded on the server, `width`, `height` and `fps` stay zero.

`status` is `publishing` only once media is actually going into the room. A stream that produces nothing for a minute
(`media.ingress.stall_seconds`) ends with `status` of `error`, and `error` says whether the transcoder or the source had
stopped.

## Running the workers

`streamkit-egress` and `streamkit-ingress` read the same config file as the server, or the same text in `STREAMKIT_CONFIG_YAML`, and
find their work in the database. They need `control_plane.database_url`, `control_plane.master_key` and `media.ws_url`. See `media`
in `server/config/config.go`.
They never change the schema: the server does that, and workers wait for it when they start. Give them a role of their own by
setting `STREAMKIT_WORKER_DB_PASSWORD` for the server, which lets `streamkit_worker` log in, and using that role in the workers'
`STREAMKIT_DATABASE_URL`. It can reach the job tables and what those need, and nothing about users or organizations.

`streamkit-egress` needs `google-chrome`, `ffmpeg` and `ffprobe`, and no display or sound server. In a container, Chrome's
sandbox needs `infrastructure/docker/chrome-seccomp.json`, which is Docker's default seccomp profile plus what the sandbox uses
to set itself up. `production.yml` applies it. `media.egress.chrome_no_sandbox` is still there for a host where it cannot work.
The limit on jobs is `media.egress.max_jobs` or `--max-jobs`. Finished recordings go to `media.storage`: the server must be
able to read the same storage as the workers. `SIGTERM` takes no more jobs and finishes the running ones cleanly. `SIGKILL`
leaves the job to fail when its lease runs out, and a worker with that working directory recovers what it can of the
recording, trying again for a day if storage is failing. A lease is 30 seconds by default (`lease_seconds`), and a job is
left alone for ten seconds after that, so that a machine that stalled can pick up where it was.

An installation with several projects should set `media.egress.max_jobs_per_project` and `media.ingress.max_sessions_per_project`,
or one project can take every slot there is. `media.storage.retention_days` removes recordings that long after they ended. The
files of a deleted project are removed shortly after it is, on the next sweep.

Measured with `tests/media/measure.test.mjs` on a 28-thread i7-14700HX, over 30 seconds of steady recording, adding up
everything below the worker:

| Job | Cores | Memory |
| --- | --- | --- |
| Composite of two people, to MP4 or WebM | 0.7 | 760 MB |
| Composite of six people, to MP4 | 1.25 | 800 MB |
| One participant, to MP4 | 0.45 | |
| Audio only | 0.1 | 225 MB |
| Composite of two people, to MP4 and an RTMP stream | 0.65 | |
| One RTMP stream coming in at 640x360, 1280x720, 1920x1080 | 0.15, 0.4, 0.8 | 170 MB at 1280x720 |

Those are Chrome's fake cameras, which are easy to encode. Real video costs more, so measure your own before relying on
a number. The default of `NumCPU / 3` jobs per worker leaves room for that, but it does not look at memory: set
`max_jobs` lower on a machine with less than about 1 GB per job.

`streamkit-ingress` needs `ffmpeg`, and listens for RTMP on `media.ingress.rtmp_addr`. A connection has 20 seconds to start
publishing, and one address can have 32 open at once. URL inputs and stream outputs cannot reach private addresses unless
`media.allow_private_addresses` is set.

### What a media process can reach

Chrome and ffmpeg parse media from strangers, and in the container they run as the same user as the worker that starts them.

- **Secrets.** The worker marks itself undumpable, so nothing of the same user can read its environment or memory through
  `/proc`. Nothing secret goes on a command line, where any process can read it: ffmpeg connects to RTMP through a relay in
  the worker that fills in the key, and to HTTP sources through a proxy that stands in for the address. The one exception
  is an `srt://` source with a passphrase in its address, which ffmpeg has to be given directly. Keep secrets in the
  environment rather than a config file, as `production.yml` does.
- **ffmpeg** starts through a helper that uses Landlock to limit it, on kernels that have it: it can read the system's libraries
  and the one job's directory, nothing else on disk, none of `/proc`, and it can make TCP connections only to the relay or
  proxy it was given. The worker logs at startup how much of this the kernel can do.
- **Chrome** has its own sandbox around renderers. In a container, where the worker already runs with `no_new_privs`, its other
  processes are limited with Landlock too, to its profile and what a browser needs to run. Project settings cannot send its
  connections anywhere: a recorder only uses the operator's ICE servers.
- **The network.** `production.yml` puts workers on a network with only the server and the database.
- **The disk.** `production.yml` gives workers a read-only root. They have no config file: their configuration, which holds no
  secrets, comes in the environment.

What this does not do:

- The worker process itself still holds the master key and a database login that can read API secrets, because it has to
  mint access tokens and open sealed stream addresses. A bug in the worker, or in the kernel, reaches those. Taking them
  out means having the server do that work over an internal API, which has not been built.
- Media processes can still send signals to each other and to the worker, since they are the same user. A compromised one
  can stop other recordings, though not read them.
- UDP is not limited, because Landlock cannot do that on the kernels this has run on, so ffmpeg can send UDP anywhere the
  container can.
