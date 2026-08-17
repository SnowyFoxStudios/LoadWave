# Architecture

How LoadWave is put together, and why. This is the document to read before
changing anything structural.

---

## The four tiers

```
┌─────────────────────────────────────────────────────────────┐
│  coordinator                                                │
│  ─────────────────────────────────────────────────────────  │
│  • divides the load across agents                           │
│  • merges every node's metrics into one picture             │
│  • evaluates thresholds and decides the verdict             │
│  • serves the REST API, the WebSocket stream and the UI     │
└─────────────────────────────────────────────────────────────┘
        ▲ gRPC, one long-lived bidirectional stream per agent
        │ agents dial OUT — no inbound ports on load generators
┌───────┴─────────────────────────────────────────────────────┐
│  agent                                    (one per machine) │
│  ─────────────────────────────────────────────────────────  │
│  • spawns and supervises worker processes                   │
│  • subdivides its quota across them                         │
│  • relays their telemetry upward                            │
└─────────────────────────────────────────────────────────────┘
        ▲ the same gRPC protocol, over a Unix domain socket
┌───────┴─────────────────────────────────────────────────────┐
│  worker                                   (one OS process)  │
│  ─────────────────────────────────────────────────────────  │
│  • runs the engine: a pool of virtual users                 │
│  • aggregates locally, flushes a batch every second         │
└─────────────────────────────────────────────────────────────┘
        │
        └── VU · VU · VU · …        one goroutine each, looping a scenario
```

---

## Why these boundaries

### Why worker *processes* rather than more goroutines

Virtual users are goroutines, which is what makes tens of thousands of them
affordable. But one Go runtime has one scheduler and one garbage collector, and
past a few thousand busy VUs those become the bottleneck rather than the system
under test. At that point you are measuring your own generator.

Splitting the pool across OS processes gives each slice its own runtime. It
also means a scenario that panics in one worker costs that worker's share, not
the whole run.

The cost is that metrics have to cross a process boundary, which is what the
control protocol and the mergeable histograms are for.

### Why agents dial out

The node always initiates the connection, at both tiers. Three things follow:

- Load generators need **no inbound ports**. They work from behind NAT and
  through egress-only firewalls, which is what most cloud environments give
  you by default.
- A broken stream is an **unambiguous liveness signal**. There is no need to
  infer death from a timeout on a connection nobody was using.
- Adding capacity is just starting another agent. There is nothing to register
  and no discovery to configure.

### Why one protocol for both tiers

`internal/control` is used unchanged for agent↔coordinator over TCP and
worker↔agent over a Unix socket. Two tiers with the same shape — a supervisor
handing down commands and receiving telemetry — do not need two protocols, and
one implementation means reconnection, backpressure and session replacement are
solved once and correct in both places.

### Why the coordinator holds everything

Threshold evaluation and the pass/fail verdict live on the coordinator because
it is the only component that sees the whole run. An agent evaluating its own
share would be answering a different, less useful question.

---

## How load is divided

The coordinator decides **quotas**, not per-tick virtual user counts.

At the start of a run it apportions the profile's peak across agents, weighted
by the capacity each advertised, using the largest-remainder method in
`internal/apportion`. A sixteen-core host carries proportionally more than a
two-core one. The agent then subdivides its own quota across its workers the
same way.

Each node evaluates the profile **locally**, scaled to its quota:

```
localTarget(t) = round(quota × globalTarget(t) / globalPeak)
```

`globalTarget(t)` is a pure function of elapsed time, and every node measures
elapsed time from the same agreed start instant, which the coordinator sets a
couple of seconds in the future so that every node has its orders before the
clock starts.

That is the important design choice. Pushing a VU count on every tick would
make ramping depend on the network being healthy every 100ms; instead the
control plane stays quiet, quotas change only when the fleet does, and a node
that misses messages for a few seconds rejoins the curve exactly where it
should be rather than where it left off.

Each node also gets a `(shardIndex, shardCount)` pair and a block of the
virtual user id space, so fixtures can be partitioned and ids allocated with no
further coordination.

---

## How metrics flow

```
VU ──observe──▶ sharded recorder ──flush every 1s──▶ MetricBatch
                                                          │
                                       agent relays it unchanged
                                                          ▼
                                             coordinator's store
                                                    │        │
                                          cumulative        time buckets
                                        (full fidelity)   (reduced, charts)
```

**Recording** is the hottest path in the program — several observations per
request, tens of thousands of requests per second — so it does no allocation in
steady state. Observations go into a sharded map keyed by metric and label
hash, with each virtual user pinned to one shard.

Scalar metrics are sharded widely to minimise contention. Trend metrics are
sharded narrowly, because each shard holds its own histogram for every series
it sees and a histogram costs kilobytes where a counter costs dozens of bytes.

**Flushing** merges the shards, encodes each series as a delta, and hands it to
the control client. Batches are deltas rather than running totals so a dropped
one costs a single interval instead of skewing everything after it. Buckets are
aligned to the wall clock so nodes agree on what "12:00:03" means.

**Merging** happens on the coordinator. Histograms merge losslessly, which is
what makes a fleet-wide p99 the real p99 rather than an average of percentiles.

The details are in [metrics.md](metrics.md).

---

## What happens when something breaks

The failure modes were designed for, not discovered afterwards.

| Failure | What happens |
| ------- | ------------ |
| **Agent disconnects** | Its stream breaks, the coordinator notices immediately, and the survivors' quotas are recomputed to absorb its share. The dashboard records the event. |
| **Agent goes quiet without disconnecting** | Missed heartbeats past `AgentTimeout` evict it, then the same rebalance. |
| **Agent reconnects** | It reuses its node id, so the coordinator replaces the stale session rather than counting it twice. If a run is in progress, it is given a share and starts contributing. |
| **New agent joins mid-run** | Put to work immediately. Scaling a running test out is just starting another agent. |
| **Worker process dies** | The agent notices, reports it upward, and redistributes to the surviving workers. The run continues at the requested VU count. |
| **A scenario panics** | Recovered per iteration, logged with its stack, charged as a failed iteration. Ten thousand VUs are not taken down by one nil map access. |
| **Metric queue fills** | Telemetry is dropped rather than blocking the generator, and the drop is counted and surfaced. A load test must not slow down because its reporting channel is congested. |
| **Series cap reached** | New series are dropped and counted; existing ones keep recording. The report warns that the numbers understate reality. |
| **Batch arrives too late** | Rejected and counted, with a warning about clock skew, rather than appearing out of order in the chart. |
| **Coordinator restarts** | Agents reconnect on a jittered backoff — jittered so a fleet does not come back in lockstep and knock it over again. In-flight run state is lost; runs are in-memory only. |
| **Agents never confirm a stop** | The coordinator waits out the grace budget plus a margin, then finishes the run with what it has. A dead agent cannot leave a CI job hanging forever. |

---

## Stopping

Getting this right matters more than it sounds, because a careless stop
manufactures a cliff of cancelled requests that then appear as failures in the
results you are about to read.

A graceful stop cascades, and **each tier waits**:

1. The coordinator broadcasts the stop and marks the run stopping.
2. Each agent forwards it to its workers, then waits for them to report
   finishing — it does **not** kill them straight away.
3. Each worker asks its engine to stop; the engine lets in-flight iterations
   finish within the grace budget before cancelling.
4. Workers report a terminal phase; agents fold those into one phase each and
   report upward.
5. The coordinator finishes the run when every agent has reported terminal, or
   when the grace budget plus a margin has elapsed.

The CLI then waits a moment longer before reading the results, so the final
metric buckets have closed. Without that, every run would quietly lose its last
second.

---

## Design notes

**Generated protobuf code is committed.** So `go get` works with no protobuf
toolchain. CI regenerates and fails if it has drifted.

**The dashboard is embedded in the binary.** Deployment is one file, and the UI
can never be a different version from the coordinator serving it. A clone that
has never run a frontend build embeds a placeholder and the server explains how
to build the real thing, so contributors working only on Go never need Node.

**Runs are in-memory.** There is no database. Retention is a rolling window,
and history is lost when the coordinator exits. Persistence is a roadmap item,
deliberately deferred rather than half-built.

**One run at a time.** Concurrent runs would share worker processes and network
capacity, and the resulting numbers would measure the interference rather than
the system under test. Runs are sequential, though: a finished run leaves the
coordinator up, holding its results, ready for the next one.

**Stopping a run and stopping the process are different things.** A run ending
— whether it finished, was stopped from the dashboard, or breached an
abort-on-fail threshold — leaves the coordinator serving. Taking away the
results at the moment somebody wants to read them would be a strange thing to
do. Ending the process needs its own deliberate action: Ctrl-C, or the
dashboard's Power off control where the deployment allows it.

---

## Where things live

| Package | Responsibility |
| ------- | -------------- |
| `pkg/loadwave` | The public SDK. Depends on nothing internal. |
| `pkg/loadwave/run` | `run.Main()`. Separate so the SDK stays free of the CLI's dependencies. |
| `internal/engine` | The VU pool and the load-profile executors. |
| `internal/metrics` | Recording, HDR histograms, the coordinator's store. |
| `internal/control` | The bidirectional stream, shared by both tiers. |
| `internal/coordinator` | Run lifecycle, apportionment, thresholds, the live stream. |
| `internal/agent` | Worker supervision. |
| `internal/worker` | The load-generating process. |
| `internal/scenario` | YAML config and the declarative interpreter. |
| `internal/httpapi` | REST, WebSocket, embedded UI. |
| `internal/apportion` | Integer division that always adds back up. |
| `internal/idspace` | Static partitioning of the virtual user id range. |
| `web/` | The React dashboard. |

The dependency rule: `internal/*` may depend on `pkg/loadwave`, never the
reverse. That is what keeps the SDK importable by a scenario's own unit tests
without dragging in a coordinator.
