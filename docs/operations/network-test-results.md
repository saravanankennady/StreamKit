# Network test results

**Every result here is emulation on one machine.** Packets crossed Linux virtual bridges between containers and browsers on
the same host, with loss, delay, jitter and rate limits added by `tc netem` and blocks added by `iptables`. No real WiFi, phone
network, router, NAT, firewall or second device was involved. Nothing here shows that a networking feature works on a real
network, and no feature should be called complete because of it. Real multi-machine testing needs hardware the project does
not have yet.

The numbers come from `tests/network/`, on 2026-11-05. `tests/network/report.mjs` printed the detailed tables from what the tests
recorded. Run IDs are `run-1` and `run-2`. Each row in the detailed tables gives the machine's load and how busy its CPU was when
the case started.

## Summary

Our SDK on both servers unless it says otherwise. Latency is from drawing at the publisher to presenting at the viewer. In the last
column, "met" means ours met every check for that case. Ours met every check in 33 of 43 cases on the last run of each, and the
reference in 26. The other 10 are listed under [What does not pass](#what-does-not-pass).

| Scenario | Reference | Ours | Checks on ours |
| --- | --- | --- | --- |
| Hand-measured WiFi, single layer: `delay 10ms 8ms 25% loss 0.5%` | p50 64, p99 82 ms, 29.4 fps | p50 63, p99 89 ms, 29.6 fps | met |
| Same, simulcast, adaptive stream on. Not like with like: the reference chose 960 px for this element size, ours 1920 px | p50 80, p99 99 ms, 29 fps at 960 px | p50 56, p99 321 ms, 27.9 fps at 1920 px | met |
| 1% loss, viewer's link | 27.9 fps, longest gap 133 ms | 29 fps, 100 ms | met |
| 5% loss, viewer's link | 23.1 fps, 251 ms | 27.3 fps, 215 ms | met |
| 10% loss, viewer's link | 15 fps, 4067 ms | 25.2 fps, 331 ms | met |
| 20% loss, viewer's link | 1.5 fps, 16066 ms | 18.2 fps at 960 px, 3600 ms | **missed: 2 s limit on a gap** |
| 5% loss, official client on `/rtc/v1` | 25 fps, 350 ms, 1920 px | 24.5 fps, 917 ms, 960 px | met |
| 5% loss, sharer's link, through `ifb` | 29 fps, 84 ms | 25.5 fps, 334 ms | met |
| 200 ms round trip, viewer's link | p50 313 ms, 29.3 fps, browser saw 201 ms | p50 164 ms, 29.6 fps, browser saw 201 ms | met |
| 200 ms round trip, sharer's link | 27.5 fps | See [below](#what-does-not-pass) | **missed in run 2** |
| Jitter, `delay 10ms 8ms 25%` | 29.2 fps, 1920 px throughout | See [below](#what-does-not-pass) | **missed in run 2** |
| Jitter, `delay 30ms 20ms 25%` | 28.6 fps, 1920 px throughout | See [below](#what-does-not-pass) | **missed in run 2** |
| Camera, 1 Mbps from the start: first frame, what plays, back at the top after the lift | 355 ms, 5 fps at 640 px, 44.0 s | 236 ms, 17.2 fps at 640 px, 11.7 s | met |
| Camera, 500 kbps from the start | 413 ms, 8.6 fps at 320 px, 36.1 s | 283 ms, 17.4 fps at 320 px, 12.0 s | met |
| Camera, joining at 1 Mbps | 136 ms, 10 fps at 640 px | 158 ms, 19.9 fps at 320 px | met |
| Camera, joining at 500 kbps | 245 ms, 9.9 fps at 640 px | 201 ms, 19.8 fps at 320 px | met |
| Camera, step down to 1 Mbps: longest stall, back at the top | 2.8 s, 21 ms, having stayed at 1280 px at 7.3 fps | 5.2 s, 17.8 s, at 640 px and 14.3 fps meanwhile | met |
| Camera, step down to 500 kbps | 3.1 s, 30.9 s | 10.2 s, 6.8 s | met, but a 10 s stall is long |
| Screen share, 1 Mbps from the start: first frame, what plays, back at the top | 5.9 s, 7.4 fps, 11.5 s | 0.68 s, **0.9 fps**, 12.8 s | **missed** |
| Screen share, 500 kbps from the start | 1.1 s, 0.1 fps, 18.6 s | 1.2 s, **0 fps**, 16.2 s | **missed** |
| Screen share, joining at 1 Mbps | 5.3 s, 8.4 fps | 0.55 s, 20.3 fps | met |
| Screen share, joining at 500 kbps | no frame in 40 s | 0.84 s, then **0 fps** | **missed** |
| Screen share, step down to 1 Mbps: longest stall, back at the top | 6.5 s, 9.8 s | 5.7 s, 9.1 s | met |
| Screen share, step down to 500 kbps | frozen until the lift, 15.0 s | frozen until the lift, 13.1 s | **missed** |
| 5 s outage: first frame after it, in run 2 and run 1 | 0.3 s, 2.2 s | 7.9 s, 4.4 s | met in run 2. Run 1 was at a load of 35 |
| 30 s outage: first frame after it, both clients reconnect | 3.5 s, 7.4 s | 1.6 s, 1.8 s | met in run 2 |
| UDP blocked from the start: time to connect, fps | 3.4 s, 25.1 fps, over TCP | 1.1 s, 25.8 fps, over TCP | met |
| UDP blocked during a call: steady video after | 9.2 s, over TCP | 9.2 s, over TCP | met |
| TURN through coturn, browser to coturn over UDP | 25.7 fps, relayed | 25.5 fps, relayed | met |
| TURN through coturn, UDP blocked, browser to coturn over TCP | 26.3 fps, relayed over TCP | 26.4 fps, relayed over TCP | met |
| Signalling cut with `ss -K`, media untouched: longest gap | 3667 ms | 250 ms | met |
| Sharer's signalling closed by the client | 83 ms | 117 ms | met |
| Viewer full reconnect: steady video after | 1.8 s | 1.8 s | met |
| Sharer full reconnect, publishing again | 56 ms | 60 ms | met |
| Client's address changes: moved client sees video, other client sees it, fps after | 18.1 s, 2.4 s, 19.8 | 14.0 s, 5.3 s, 7.8 | **missed: 10 fps afterwards** |
| Firefox publishes, Chrome receives. Chrome publishes, Firefox receives. Two Firefox. Recovery. 5% loss | all met | all met | met, 5 of 5 |
| H264, three layers, viewer moves between them | met | met | met |
| H264 on 1 Mbps: what plays, back at the top | 3.8 fps at 320 px, 38.0 s | 7 fps at 640 px, 2.3 s | met |
| Screen share with audio: packets in 4 s, samples decoded, audio energy | 200, 192000, 0 | 201, 192480, 0 | met. Packets arrive and decode. **The fake source is silent, so nothing shows it would be heard** |

The "stayed lost" percentages in the detailed tables do not compare across servers. Ours resends on the media stream, so a packet
that comes back no longer counts as lost. The reference resends on RTX, which Chrome counts separately.

These cases ran at different commits. `run-1` was at 1528a2e, `run-2` up to TURN, loss, round trips and jitter at 2371c9d, and
bandwidth at 64299df. Between them, 2371c9d only concerns SVC streams, and 64299df only changes what counts as a failed move up.

### What does not pass

- **Screen share below about 1 Mbps.** The first frame is fast, then the picture can freeze. At 500 kbps it stayed frozen until the
  link recovered, on both servers. At 1 Mbps from the start, ours played 0.9 fps where the reference played 7.4 fps, though joining
  and stepping down at 1 Mbps were fine. The likely cause, not proven: our SDK lets a share's low layer use up to a quarter of the top
  layer's bitrate, 1.25 Mbps here, and an encoder refining a sharp page keeps near that for several seconds after each keyframe. The
  official client's default is the same quarter (`computeDefaultScreenShareSimulcastPresets` in its `publishUtils.ts`).
- **20% loss.** Ours played 18.2 fps with one 3.6 s gap, where the limit is 2 s. The reference played 1.5 fps with a 16 s gap.
- **Network switch.** The address did change, from `172.21.0.3` to `172.22.0.3`. Video came back in 14.0 s, against 18.1 s on the
  reference, and that is under the 15 s limit. But the moved client then played 7.8 fps, where the limit is 10. The likely cause, not
  proven: the outage reads as congestion, so the link is judged short and temporal layers are dropped for a while afterwards.
- **Jitter and the sharer's round trip.** See [the repeat](#repeat-of-what-looked-wrong).

### Costs of what was changed

- On a good link, the top layer now arrives later: 3.5 to 4.6 s after the first frame on ours, 1.4 to 1.8 s on the reference. This is
  the wait for browser reports to show no queue on the path, and it is what keeps a thin link from flooding at the start.
- On a link too thin for the top layer, ours still tries it now and then, and each failed try costs the picture a second or two. The
  reference has probing for this, and its timelines show no such tries. Ours tried at about 15 s and 28 s in the camera test.
- Retransmission no longer uses RTX on `/rtc/v1`, where it is negotiated. Packets are resent on the media stream, as on `/rtc`.

### Compared with the first hand measurements

Those had our server at p50 121-125 ms, p99 247-279 ms and the reference at p50 106-111 ms, p99 206-236 ms, under the same netem
line. Here, on a quieter machine, both are near p50 64 ms in single layer. So load on this machine moved these numbers by about a
factor of two. Compare rows from one run, and do not read much into small differences between the servers.

## How it was set up

| | |
| --- | --- |
| Machine | 28 threads, 30 GB, Linux 7.0.0, shared with other agents and a desktop session |
| Containers | Docker 29.1.3, `alpine:3.20` with iproute2 and iptables, the server binary mounted in |
| Our server | Static build of this branch, `--dev`, `bind_address: 0.0.0.0`, `rtc.node_ip` set to the container's address |
| Reference server | `reference/oracle/bin/x/livekit-server` 1.13.11, `--dev --bind 0.0.0.0 --node-ip <container>` |
| Browsers | Chrome 156.0.8078.47 on the host, except the network switch. Playwright's Firefox 157 for the Firefox tests |
| Client | Our SDK on both servers, except one loss case that uses the official client on `/rtc/v1` |
| TURN | `coturn/coturn:4.7-alpine`, the image production names, in a container |

The host's own network is not in the path. Browsers on the host reach containers over a Docker bridge, and nothing leaves the
machine. The host's WiFi address changed during this work, and its bridges did not.

How impairment works:

- **Server to browser:** `tc netem` on the container's `eth0`.
- **Browser to server:** an `ifb` device works on this kernel, so traffic arriving on `eth0` is redirected to `ifb0` and shaped
  there. The kernel loads the module by itself when a container with `NET_ADMIN` asks for the device.
- **One client at a time:** both browsers have the same address, so a `prio` qdisc and `u32` filters on client ports send only
  one client's packets through netem. Without this, a viewer's thin link would also delay the sharer's feedback, which no real
  viewer's link does. Whole-link impairment is used only to repeat the first hand measurements, and for outages.
- **Network switch:** Chrome runs in its own container and moves from one Docker network to another, with a router container
  joining both to the server's network. Its address really changes, the server stays at one address, and nothing is translated.

What the viewer measures: each frame it presents, read through `requestVideoFrameCallback`, with a clock barcode read from the
pixels for latency, as `sdk-harness/screen-latency.mjs` does. Screen shares are real tab capture at 1920x1080, 30 fps, simulcast,
`contentHint: detail`, and are stopped unless the capture is the browser's own tab. Cameras are a moving canvas with a tone,
except in the network switch, the Firefox publishing tests and screen audio, which use the browsers' fake devices.

Thresholds come from what a person would accept and are written at the top of each test file. Only ours can fail a test. The
reference's numbers are recorded beside ours, and if it misses a threshold too, the table says so.

To run it:

```
make test-network                     # builds, installs, then runs every group, each under the machine's heavy-work lock
cd tests/network && node run-all.mjs bandwidth     # only some groups
node report.mjs <run ID>              # prints tables from tests/network/results/<run ID>.jsonl
```

## What the suite found and what was fixed

All of these were found on emulated links, with our server and the reference side by side.

| Found | Cause | Fix |
| --- | --- | --- |
| Nothing Firefox published reached anyone on our server. The reference accepted the same client | Firefox offers bundled sections with port 0 and `a=bundle-only`, and the server took port 0 for a rejected section | `server/participant/participant.go`, three lines |
| With `--dev` and `rtc.node_ip` together, signalling worked and media never connected | A loopback socket was advertised under the node's address, and ICE kept it as the first of two identical candidates | A node address now turns loopback off |
| First frame on 1 Mbps took 13.8 s, against 6.2 s on the reference with the same settings | The top layer went first. Its keyframe is more than a thin link's queue holds, and it never finished arriving | A track starts on its lowest layer until browser reports show no queue on the path, 1.5 s to 6 s |
| A viewer on a thin link stayed frozen even on a low layer | Pion's NACK responder answered everything. A browser that lost a thousand packets asks for all of them each round trip, which crowded out the keyframe that would have ended it | Each DownTrack answers NACKs itself, and stops after a keyframe request, for anything before the latest keyframe, and on leaving a layer, and is held to a share of what is forwarded |
| On a thin link, repeated keyframes kept the stream expensive | A browser that has decoded nothing asks every 3 s. Each keyframe restarts the encoder's refining, so the stream costs more | On a link found to be short, repeated requests are passed on less and less often, up to every 8 s |
| A layer too big for the link took many seconds to give up | Congestion had to last 2 s, and a cut could leave the same layer still fitting | After a move up, one bad report takes it back, and a cut always goes below what is allocated |
| Even the lowest layer could be more than the link carries | Nothing could be sent but all of a layer | VP8 temporal layers are dropped, with sequence numbers and picture IDs closed up. It shares `maxTemporal` with the frame rate limit on SVC streams |
| Quality did not come back within a minute of a link recovering | The wait doubled on every cut, even in one bout of congestion, and the limit rose 8% a second | One bout counts once, the limit rises 15% a second, and what flows with no complaint raises it |
| A signalling drop froze the viewer for up to 5 s on ours, and for under 0.4 s on the reference | Our client restarted ICE on a publisher that was still connected. The gap dropped its bandwidth estimate, and the encoder held back the top layer | The client only restarts ICE on a publisher that has lost its connection. This is in `sdk-js`, so **the official client still restarts ICE here, and was not measured for this** |
| Each description listed header extensions in a different order | Pion writes them from a map. Chrome can recreate its send streams when the list differs | Sorted by ID in what goes to the browser. On its own this did not cure the freeze above, so it is a precaution |
| A subscriber's ICE restart cost a second | Pion waits a second before choosing a peer reflexive candidate, which is what a browser with no media permission looks like | `SetPrflxAcceptanceMinWait(0)` |
| After a network switch, the moved client waited 16 s on ours and 18 s on the reference | The client only reacted to a connection reaching failed, or to a signalling ping timing out after 15 s | A connection the client relies on that stays disconnected for 3 s starts a resume |

