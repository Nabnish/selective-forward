# SFU — Architecture & Build Plan

A self-hostable Selective Forwarding Unit for small-group live video, in Go, with
per-subscriber adaptive quality that can be measured and defended.

---

## 0. What this is and isn't

**Is:** a media router. Publishers send three simulcast encodings; the server forwards
RTP packets, choosing per subscriber which encoding to relay. No decoding, no encoding,
no transcoding — the CPU cost per subscriber stays tiny and predictable.

**Isn't (explicit non-goals for v1):**

| Excluded | Why |
|---|---|
| SFU cascading / multi-node rooms | A month of work; room affinity solves 99% of your load |
| Recording, HLS output | Separate pipeline, separate project, low interview value |
| E2EE / insertable streams | Fundamentally at odds with layer switching |
| Your own TURN server | Run coturn; writing TURN teaches you nothing new |
| Transcoding fallback | The entire point is that you don't transcode |
| Auth beyond signed room JWTs | Not what the project is proving |

Guard these. Every one of them is how this project dies at 60% done.

---

## 1. Component map

```
┌─────────────┐   HTTPS/WSS    ┌──────────────────┐
│   Browser   │◄──────────────►│  Signalling      │  (stateless HTTP+WS)
│  (Next.js)  │                │  - JWT verify    │
└──────┬──────┘                │  - room alloc    │
       │                       │  - SDP relay     │
       │  DTLS/SRTP over UDP   └────────┬─────────┘
       │  (single muxed port)           │ in-process (v1)
       ▼                                ▼
┌──────────────────────────────────────────────────┐
│                   SFU Node (Go)                  │
│                                                  │
│  ICE/DTLS/SRTP  ──►  pion/webrtc PeerConnections │
│         │                                        │
│         ▼                                        │
│   ┌──────────┐   ┌────────────┐   ┌───────────┐  │
│   │ Publisher│──►│  Router    │──►│Subscriber │  │
│   │ Ingest   │   │  (per      │   │ Downtrack │  │
│   │ 3 layers │   │   track)   │   │ + queue   │  │
│   └──────────┘   └─────┬──────┘   └─────┬─────┘  │
│                        │                │        │
│                  ┌─────▼──────┐   ┌─────▼─────┐  │
│                  │  Layer     │◄──│ BWE       │  │
│                  │  Selector  │   │ (TWCC/GCC)│  │
│                  └────────────┘   └───────────┘  │
│                                                  │
│  Room registry · Prometheus · pprof · health     │
└──────────────────────────────────────────────────┘
```

**Do not put a load balancer in the media path.** UDP + ICE + DTLS means the media
connection is between the browser and one specific node's IP:port. Signalling tells
the client which node to talk to; media goes direct. This constraint drives the whole
scaling section below — internalise it early.

---

## 2. Core data model

```
Room
 ├── id, created_at, max_participants
 └── Participants[]
      ├── id, display_name, role (publisher | subscriber | both)
      ├── PublisherPC   (*webrtc.PeerConnection)
      ├── SubscriberPC  (*webrtc.PeerConnection)
      └── PublishedTracks[]
           ├── kind (audio | video)
           ├── Layers[] (rid: "q" | "h" | "f")   ← video only
           │    ├── ssrc, current bitrate, last keyframe ts, active
           └── Subscriptions[] (one per other participant)
                ├── currentLayer, targetLayer
                ├── seqRewriter, tsRewriter
                └── sendQueue (bounded chan)
```

**Two peer connections per participant, not one.** Publisher PC and subscriber PC are
separate. This is what every production SFU does, and the reason is renegotiation: when
someone joins or leaves, the subscriber PC needs new transceivers and a fresh
offer/answer, and you don't want that churn to interrupt the publisher's uplink.

---

## 3. Signalling protocol

One WebSocket per participant. JSON messages, versioned envelope
(`{"v":1,"type":"...","payload":{...}}`).

**Client → Server:** `join`, `offer` (publisher), `answer` (subscriber),
`trickle` (ICE candidate), `mute`, `leave`

**Server → Client:** `joined` (room state snapshot), `offer` (subscriber, on renegotiation),
`answer` (publisher), `trickle`, `participant_joined`, `participant_left`,
`track_published`, `layer_switched` (for your debug overlay), `error`

**Sequencing rule that will bite you:** the subscriber PC uses server-initiated
renegotiation. The server sends an offer whenever the set of forwarded tracks changes.
Two joins landing within milliseconds of each other will collide — you need a per-participant
negotiation lock with a "renegotiation needed" flag that coalesces, not a queue of offers.
Decide now whether you serialise this with a mutex or with the room actor from §5.

---

## 4. The media path

### 4.1 Ingest

The client publishes video with three encodings via `sendEncodings` on the transceiver:

| rid | Resolution | Target bitrate | Scale factor |
|---|---|---|---|
| `q` | 180p | ~150 kbps | 4 |
| `h` | 360p | ~500 kbps | 2 |
| `f` | 720p | ~1.7 Mbps | 1 |

Each arrives as a distinct `*webrtc.TrackRemote` with its own SSRC. Audio is Opus,
single encoding, always forwarded — never drop audio for bandwidth.

Enable these header extensions on ingest: `transport-cc` (mandatory — no TWCC, no BWE),
`abs-send-time`, and `ssrc-audio-level` (RFC 6464) for active-speaker detection.

### 4.2 Forwarding and rewriting

One goroutine per incoming layer reads packets and hands them to the router, which
copies to each subscription. **Every subscription needs its own rewritten packet**, because
each subscriber may be on a different layer with a different history.

What has to be rewritten per subscriber:

- **SSRC** → the SSRC the subscriber negotiated for that track
- **Sequence number** → strictly monotonic across layer switches. Keep a per-subscription
  offset; on switch, `offset += (lastSentSeq + 1) - firstSeqOfNewLayer`. Handle 16-bit wraparound.
- **Timestamp** → same idea, but clock-rate aware (90kHz video). The new layer's timestamp
  base is unrelated to the old one's.
- **VP8 picture ID / TL0PICIDX** (if you support VP8) → also monotonic, also rewritten.
  H.264 has no equivalent, which is a real argument for making H.264 your v1 codec and
  adding VP8 later.
- **Marker bit** → preserve; it delimits frames and the receiver depends on it.

Write the rewriter as a pure, testable struct: packets in, packets out, no I/O.
It is the single highest-value unit-test target in the project.

### 4.3 Layer switching

The switch itself is the project. Sequence:

1. Selector decides `targetLayer != currentLayer`
2. Send PLI (RTCP Picture Loss Indication) to the publisher **for that layer's SSRC**
3. Keep forwarding the old layer — do not cut over yet
4. When a keyframe arrives on the target layer, cut over at that packet boundary
5. Reset rewriter offsets, mark switch complete, emit metric + `layer_switched` event

Two things people get wrong: cutting over before the keyframe (subscriber sees green
smear until the next natural keyframe), and PLI storms (ten subscribers switching at once
send ten PLIs; the publisher re-encodes a keyframe for each). Rate-limit PLI per layer —
one per ~500ms, and coalesce concurrent requests.

### 4.4 NACK and RTX

The subscriber NACKs the **rewritten** sequence numbers it saw. So the retransmit buffer
must live per subscription, holding already-rewritten packets — you cannot answer from
the publisher's original stream. A ring buffer of ~500 packets per video subscription is
the usual size. Send retransmits on the RTX SSRC with the `apt` payload type.

Upstream (server → publisher) NACK is also worth having; pion's interceptor gives you the
generator side largely for free.

---

## 5. Concurrency model

This is where Go earns its place on your resume, so make a deliberate choice and be able
to defend it.

**Recommended: actor per room, bounded queue per subscriber.**

- One goroutine owns each room's mutable state (participants, subscriptions). All
  mutations arrive as commands on a channel. No locks on room state, no lock-ordering bugs.
- One goroutine per incoming RTP layer (`track.Read` loop). It must never block. It copies
  packets into each subscription's queue with a non-blocking send.
- One goroutine per subscription draining its queue to the wire.
- Bounded queue, and on overflow: **drop video, never audio.** Prefer dropping whole frames
  over partial ones — a half-delivered frame is worse than a missing one. Count every drop
  as a metric; a subscriber that keeps overflowing should be force-downgraded a layer,
  which is a nice feedback loop into the selector.

**Allocation discipline:** you are touching every packet on every subscription. Use
`sync.Pool` for packet buffers, avoid `[]byte` growth in the hot path, and profile with
`pprof` before you optimise anything. Expect allocation, not CPU, to be your first wall.

**Single muxed UDP port:** use `webrtc.NewICEUDPMux` on one port (e.g. 7881) rather than
a port per connection. Without this, firewalls and containers make you miserable, and
you'll exhaust ephemeral ports under load.

---

## 6. Bandwidth estimation and layer selection

### 6.1 Estimation

Send-side BWE via TWCC. Subscribers report arrival times of your packets in TWCC feedback;
you run a GCC-style estimator (delay-gradient + loss-based, take the minimum) to get a
target bitrate per subscriber. `pion/interceptor/pkg/cc` gives you a send-side BWE
implementation — use it, don't write GCC from scratch.

### 6.2 Selection policy

Input: estimated bandwidth, current layer, layer bitrates, time since last switch,
recent queue-drop count, whether the participant is the active speaker.

```
if estimate < currentLayerBitrate * 0.85          → down-switch immediately
if estimate > nextLayerBitrate * 1.25
   sustained for 5s
   and last switch > 10s ago                       → up-switch
otherwise                                          → hold
```

Down fast, up slow. Asymmetric hysteresis is the whole trick — symmetric thresholds
oscillate, and oscillation looks far worse to a viewer than sitting one layer low.

**Probing:** you cannot discover you have headroom for 1.7 Mbps while sending 500 kbps.
GCC needs padding packets to probe upward. Decide whether you implement probing (correct,
more work) or accept that up-switches only happen when a burst naturally reveals headroom
(simpler, but your "adaptive" claim is weaker). Write down which you chose and why.

**Active speaker:** use the audio-level extension with a short rolling window and hysteresis,
and bias the speaker's video toward a higher layer for everyone. Cheap to build, very
demoable.

---

## 7. Build plan

Each milestone has an acceptance test. Don't start the next one until it passes.

**M0 — Spike (target: 1 week)**
One publisher, one subscriber, hardcoded room, video visible in Chrome. Start from pion's
examples. *Accept:* you see yourself, relayed through the server.
*Kill criterion:* if this takes three weeks, you've learned something cheap — reconsider.

**M1 — Rooms (1 week)**
Real signalling protocol, N participants, join/leave, renegotiation, clean teardown.
*Accept:* 4 browser tabs, everyone sees everyone; close a tab and no goroutine leaks
(check `pprof` goroutine count returns to baseline within 5s).

**M2 — Simulcast + selection (2–3 weeks) ← the project**
Three-layer ingest, per-subscriber selection, seq/ts rewriting, PLI on switch, hysteresis.
*Accept:* throttle one subscriber with `tc netem` to 300 kbps; it drops to `q`, others stay
on `f`; no visible corruption at the switch; remove the throttle and it climbs back.

**M3 — Loss resilience (1 week)**
Per-subscription retransmit buffer, NACK responder, RTX.
*Accept:* at 5% induced packet loss, video stays watchable and your metrics show
retransmits being served.

**M4 — Observability (3–4 days)**
Prometheus metrics, client debug overlay reading `getStats()` plus your `layer_switched`
events, `/debug/pprof`, structured logs with room/participant IDs.
*Accept:* you can point at a graph and explain a switch that happened 5 minutes ago.

**M5 — Load harness + writeup (1–2 weeks)**2
Synthetic peers, ramp test, the benchmark table. *Accept:* the README numbers in §10 exist
and you can reproduce them with one command.

---

## 8. Testing strategy

**Unit (fast, most of your tests)**
- Sequence/timestamp rewriter: table-driven, including wraparound, mid-frame switch,
  out-of-order arrival, duplicate packets. This is where subtle bugs live.
- Layer selector: feed synthetic bandwidth traces (step down, step up, sawtooth, noisy
  plateau) and assert on switch counts. A trace that produces 40 switches is a failed test —
  assert oscillation bounds, not just correctness.
- Retransmit ring buffer: eviction, hit/miss, wraparound.

**Integration (in-process, no browser)**
Pion clients on both ends. Publisher reads a pre-recorded IVF/Ogg file and sends it;
subscriber consumes and asserts: monotonic sequence numbers, no gaps, expected SSRC,
keyframe present after a forced switch. Fast, deterministic, runs in CI.

**Browser E2E (Playwright)**
Chrome with `--use-fake-device-for-media-stream --use-fake-ui-for-media-stream`. Two
contexts join a room; assert via `getStats()` that `framesDecoded` climbs and
`frameWidth` matches the expected layer. Slower — keep this to a handful of scenarios.

**Network impairment**
Docker Compose with `tc netem` on the client containers. Scenarios worth scripting:
constant 300 kbps cap, 5% loss, 200ms RTT, bandwidth step-down then recovery, brief total
outage (ICE restart path). These double as your demo.

**Load**
Synthetic pion peers publishing a looped file, N subscribers each. Ramp until a defined
failure condition: p99 forwarding latency > 50ms, or drop rate > 0.1%, or CPU > 80%. Record
where it broke and why — that answer is worth more than the number.

**Soak**
One hour at moderate load with churn (join/leave every few seconds). Watch RSS and
goroutine count. Any upward slope is a leak, and this is the test that finds the bug you'd
otherwise ship.

---

## 9. Handling multiple traffic / scale

### 9.1 Understand the shape of the load

Outbound packet rate is `publishers × subscribers` per room. A 10-person room with everyone
on camera is 90 forwarding paths. This is quadratic in room size and linear in room count —
so **cap room size** (v1: 8–10 video publishers, with the rest audio-only or paginated) and
scale room *count* horizontally.

Your load metric is **aggregate outbound bitrate**, not participant count. Someone on 180p
costs a tenth of someone on 720p. Track bitrate, and admit/reject on it.

### 9.2 Vertical first

Do this before you touch clustering, and you'll get further than you expect:

- Single muxed UDP port; raise `SO_RCVBUF`/`SO_SNDBUF` (`SetReadBuffer`, a few MB)
- `sync.Pool` for packet buffers; zero allocations in the forwarding loop
- Batched syscalls where pion allows it — syscall overhead dominates at high packet rates
- `GOMAXPROCS` matched to the container's CPU limit (Go doesn't read cgroup limits by
  default — use `automaxprocs` or set it explicitly, otherwise you get scheduler thrash)
- Profile before optimising, and put the flame graph in your writeup

A tuned single node handling several hundred concurrent subscribers is a perfectly good
result to publish, and honestly stated it beats a vague claim of "scales horizontally."

### 9.3 Horizontal: room affinity

The rule: **all participants of a room live on one node.** No cross-node media, no cascading.

```
Client → signalling (stateless, behind normal HTTP LB)
           │
           ├─ look up room in Redis:  room:{id} → node_id
           │  (miss → allocator picks least-loaded healthy node, SETNX)
           │
           └─ respond with { node_addr, ice_servers, token }
                    │
Client ─────────────┴────► media direct to that node's IP:port (UDP)
```

- Nodes heartbeat load (outbound bitrate, CPU, room count) into Redis with a TTL
- Allocator picks least-loaded, not round-robin — rooms have wildly different weights
- Signalling stays stateless and horizontally trivial; only the WS connection is sticky,
  and you can co-locate it with the node it allocated

**Admission control:** each node has a hard bitrate ceiling. Above ~80%, it stops accepting
new rooms; existing rooms keep working. Never let a node accept the room that degrades
every other room on it — graceful refusal beats universal degradation.

**Draining for deploys:** mark node draining → allocator stops assigning → wait for rooms
to empty (or force-migrate with a client-side reconnect, which is a visible blip). Long-lived
UDP sessions mean you cannot deploy the way you deploy Next.js. Say this out loud in
interviews; it's the kind of operational awareness that separates candidates.

### 9.4 NAT traversal at scale

Host candidates work for most users. For symmetric NAT and restrictive corporate networks
you need TURN — run coturn, and note that TURN relays *all* media through it, so its
bandwidth cost is real and separate from your SFU's. Budget for roughly 10–15% of users
needing it.

### 9.5 What you'd do next (document, don't build)

When one room outgrows one node, you need **cascading**: nodes peer with each other and
forward a subset of layers between them. Sketch the design in this doc — relay peer
connections between nodes, per-node subscription aggregation, loop prevention — and mark it
future work. A documented, unbuilt design you can reason about is a strong interview answer.
A half-built one is a liability.

---

## 10. Metrics and the numbers your README must have

**Exported continuously:**
`sfu_rooms_active`, `sfu_participants_active`, `sfu_outbound_bitrate_bytes`,
`sfu_forwarding_latency_seconds` (histogram), `sfu_layer_switches_total{direction}`,
`sfu_packets_dropped_total{reason}`, `sfu_nacks_received_total`, `sfu_rtx_sent_total`,
`sfu_pli_sent_total`, `sfu_bwe_estimate_bytes`, `go_goroutines`.

**In the README, measured not guessed:**

| Metric | Why it lands |
|---|---|
| Subscribers per CPU core at 720p | The headline efficiency number |
| Forwarding latency p50 / p99 (ingest → egress) | Proves you measured the right thing |
| Time-to-clean-frame on a layer switch | The number nobody else has |
| Bandwidth saved vs. naive top-layer-to-all | Quantifies the whole feature |
| Behaviour at 5% loss (with/without NACK) | Shows resilience work was real |
| Where it fell over first, and why | The most valuable line in the file |

State your test conditions: hardware, codec, resolution, publisher count, network profile.
An unqualified number reads as invented; a qualified one reads as engineering.

---

## 11. Failure modes to handle explicitly

| Failure | Handling |
|---|---|
| Slow subscriber | Bounded queue → drop video frames → force layer down → metric |
| Publisher stops sending a layer | Detect staleness (no packet 2s) → mark inactive → reselect |
| PLI storm | Rate-limit per layer, coalesce concurrent requests |
| ICE disconnect | Wait for reconnect window before teardown; support ICE restart |
| Renegotiation collision | Coalesce with a "needs negotiation" flag, not an offer queue |
| Node loss | Room is gone; clients re-join via signalling, get reallocated |
| Seq number wraparound during switch | Explicitly unit-tested (see §8) |
| Redis unavailable | Existing rooms unaffected; new-room allocation fails closed |

---

## 12. Open decisions — make these yourself and record the reasoning

1. **H.264 or VP8 first?** H.264 skips picture-ID rewriting; VP8 has better temporal-layer
   support and broader simulcast behaviour. Pick one, write the trade-off down.
2. **Probing for up-switch:** implement, or accept opportunistic up-switching only?
3. **Temporal layers as well as spatial?** Finer-grained adaptation, meaningfully more
   complexity. Probably v2 — but know why you deferred it.
4. **Actor-per-room vs. fine-grained mutexes?** Both defensible. Interviewers will ask.
5. **Room-size cap and its consequences:** at what N do you switch to paginated video?

Answering these in a `DECISIONS.md` alongside the code is worth more than another feature.

---

## 13. Repo layout

```
/cmd/sfu           main, config, graceful shutdown
/internal/room     actor, participant lifecycle, subscriptions
/internal/rtp      rewriter, retransmit buffer  ← densest unit tests
/internal/bwe      estimator wiring, layer selector
/internal/signal   WS protocol, negotiation
/internal/metrics  Prometheus
/internal/alloc    node registry, room allocation (v2)
/web               Next.js client + debug overlay
/test/load         synthetic pion peers
/test/e2e          Playwright
/deploy            Dockerfile, compose w/ netem, coturn
DECISIONS.md       §12 answered
ARCHITECTURE.md    this file
README.md          demo gif + §10 numbers
```

---

**One warning.** Abandoned at M2 this is worth less than nothing — you'll get asked about it
and have no numbers. M0 through M2 plus a benchmark is a complete, defensible project.
M3–M5 make it excellent. Everything past that is scope creep wearing a good disguise.
