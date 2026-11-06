# streamkit

The command line for Elnova StreamKit: projects, tokens, rooms, participants, data, recording, ingest, webhooks, load tests, and
joining a room as a real participant. If you know the reference server's `lk`, the flags will feel familiar. Where they match
what `lk` does, they have the same names.

The name was chosen over `rea`, which is the command of the reverse-engineering tool this project uses.

## Install

```bash
make cli                                 # builds bin/streamkit
```

It is one binary, and needs nothing beside it. Building it needs Go.

## Example

```bash
bin/streamkit-server --dev &             # a development server, in another terminal

export STREAMKIT_URL=ws://127.0.0.1:7880 STREAMKIT_API_KEY=devkey STREAMKIT_API_SECRET=secret

streamkit room create lobby --empty-timeout 300
streamkit room join lobby --identity camera --publish-test-pattern &      # a real participant, with real media
streamkit room list
streamkit participant list --room lobby
streamkit data send --room lobby --data hello --topic greetings
streamkit token create --join --room lobby --identity alice --valid-for 1h --token-only
streamkit perf load-test --publishers 2 --subscribers 8 --duration 30s
streamkit room delete lobby
```

`--dev` stands in for those three variables when the development server is on its usual port.

## Commands

```
project add NAME, list, remove NAME, set-default NAME
token create, verify TOKEN

room create NAME, list [NAME ...], update NAME --metadata M, delete NAME
room join NAME                    join as a real participant
room update-subscriptions         subscribe a participant to tracks, or unsubscribe them
participant list, get, remove, mute, update
data send

egress start, list, get ID, stop [ID ...], download ID, retry ID, delete ID
ingress create, list, get ID, update ID, stop ID, rotate-key ID, logs ID, delete ID

webhook test --target URL         send a signed test event to a receiver
webhook listen --port N           receive webhooks, check them, and print them

perf load-test                    real WebRTC clients against a server
```

`room participants ...`, `room mute-track` and `room send-data` are the same as `participant ...`, `participant mute` and
`data send`, for anyone used to `lk`. Run any command with `--help`. Add `--json` for output a program can read.

### Which server and which key

In this order: a project named with `--project`; then `--url`, `--api-key` and `--api-secret`, and after them `STREAMKIT_URL`,
`STREAMKIT_API_KEY` and `STREAMKIT_API_SECRET`; then `--dev`; then the default project. Single flags override single fields of
the default project.

The CLI does not read `LIVEKIT_URL` and its friends. Someone with those set for another service should not find themselves
signing tokens with those credentials by accident.

### Projects and secrets

```bash
printf '%s\n' "$SECRET" | streamkit project add production --url wss://rtc.example.com --api-key APIabc --api-secret-stdin --default

streamkit project add staging --url wss://staging.example.com --api-key APIdef --api-secret-env STAGING_SECRET
```

The config is `config.yaml` in `$STREAMKIT_CONFIG_DIR`, `$XDG_CONFIG_HOME/streamkit` or `~/.config/streamkit`. It is written
with mode 600 in a directory with mode 700, and the CLI refuses to read a config or key file other users can read.

A secret is never written in the clear, and no command prints one. There are two ways to keep it:

- **`--api-secret-env NAME`** keeps only the name of an environment variable, and reads the secret from it each time.
- **Otherwise it is sealed** with AES-256-GCM, bound to the project's name, using a key from `STREAMKIT_CONFIG_KEY` (32 bytes,
  hex or base64) or, without that, a private `secret.key` file beside the config. With that file, sealing keeps the secret
  safe if the config alone is pasted, backed up or committed. It does not stop someone who can read all your files, or a
  process running as you. Use `STREAMKIT_CONFIG_KEY` or `--api-secret-env` to keep the key elsewhere.

`--api-secret` works but puts the secret on a command line, where other users of the machine can see it. Prefer the environment
or `--api-secret-stdin`. A stream key is different: `ingress create` and `rotate-key` print it because that is the only time it
is ever shown, so treat that output as secret.

### Joining a room

```bash
streamkit room join lobby --identity bot --publish-test-pattern
streamkit room join lobby --identity bot --publish video.ivf,audio.ogg
streamkit room join lobby --identity watcher --no-auto-subscribe --json --duration 1m
```

This is a real WebRTC session: signalling, ICE, DTLS and SRTP. The test pattern is a moving picture and a tone, pre-encoded in VP8
and Opus by `media/make.sh` and built into the binary, about 0.5 Mbit/s. It prints what it sees: who joined, tracks, data, mutes,
and once a second the packets that really arrived. With `--json`, each is a line of JSON, which is how the SDK tests watch a real
participant.

### Load tests

`perf load-test` is `tests/load/cmd/loadgen` with the built-in test pattern, and takes `--video` and `--audio` files too. Its result
is the same JSON, described in [tests/load](../tests/load/README.md). It fails if a client failed or a track did not arrive. The
clients cost about as much CPU as the server, so a meaningful figure at scale needs separate machines, and the result is only true of
the machines and network it ran on. The server's CPU and bandwidth come from its `/metrics`, which needs `--metrics-token` outside
development, and which the reference server does not serve on this port: those fields are zero there.

## What is not supported

- **Recording and ingest against anything but this server.** They use this server's REST API, which needs a control plane
  (`control_plane.database_url`). Elsewhere they say so and exit 1. `egress list --room` and `--active` and
  `ingress list --room` filter on the client, because the API does not.
- **Types of recording this server does not have:** web, track and track composite, and `update-layout` and `update-stream`.
- **`room participants forward` and `move`, `PerformRpc`:** this server does not implement them.
- **SIP and agent dispatch commands.** This server has no such services. `token create` can still write SIP and agent grants, for a
  reference server.
- **Signing in with a user account.** There is no `cloud auth`. Create API keys in the dashboard, and add them with `project add`.
- **Anything for the hosted cloud service,** and `lk`'s `agent`, `app`, `docs`, `number`, `simulate`, `analytics` and `replay` commands.
- **Load test layouts, simulcast and H.264.** Each client publishes one 640x360 VP8 stream and Opus, so a real call costs more per
  participant. There is no `--video-resolution`, `--layout` or `--simulate-speakers`.
- **Publishing data from `room join`,** or H.264 files.

## Testing

```bash
go test ./cli                            # config, flags and secrets; needs nothing running

# Runs the built binary against real servers: this server and the reference server.
STREAMKIT_TEST_DATABASE_URL='postgres://streamkit:streamkit-dev-only@127.0.0.1:5433/streamkit?sslmode=disable' go test ./cli

# And recording, with a real worker. Needs Chrome and ffmpeg.
STREAMKIT_TEST_WORKERS=1 STREAMKIT_TEST_DATABASE_URL='...' go test ./cli
```

The tests start everything themselves: see `tests/sdk/fixture`. A built `lk` at `.toolchain/bin/lk`, beside or above the repository,
adds tests that its tokens work here, and that it can use this server. Those two skip without it, and **have never been run**: `lk`
needs PortAudio to build, which is not available here. Until they have run, trust them less than the others. Tokens from the reference
server's own tool and from the official Node and Python SDKs are tested, in the SDK tests.
