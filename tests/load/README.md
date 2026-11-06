# Load testing

`cmd/loadgen` connects real WebRTC clients: real ICE, DTLS and SRTP, with publishers sending pre-encoded VP8 and Opus at
about 1 Mbit/s, and subscribers decrypting and counting every packet. Nothing is simulated, which is also why it needs real
CPU: the generator costs about as much as the server does. For meaningful numbers at scale, run it on separate machines.

```bash
make load-samples
bin/streamkit-server --dev                      # in another terminal
tests/load/ladder.sh my-machine           # results go to tests/load/results-my-machine.jsonl
```

One step by hand:

```bash
bin/loadgen -url ws://server:7880 -publishers 5 -subscribers 95 -duration 60s
```

`streamkit perf load-test` runs the same thing with the CLI's built-in test pattern, so it needs no sample files. See
[the CLI](../../cli/README.md).

Each line of results is one step. The important fields:

| Field | Meaning |
| --- | --- |
| `connected`, `failed` | Clients that connected, and that failed |
| `tracks_received`, `tracks_expected` | Whether everyone got everything |
| `server_cores` | CPU the server used, in cores, from its own process metrics |
| `server_out_mbps`, `received_mbps` | What it sent, and what clients got |
| `loss_percent_at_subscribers` | Sequence gaps seen by subscribers |

Loss over loopback is not network loss. It means someone, the server or the generator, could not keep up.
Video from this generator is one 640x360 stream with no simulcast, so a real call costs more per participant.

Results are only true of the machine they were measured on. None are recorded yet: the ladder has not run on a quiet machine.
