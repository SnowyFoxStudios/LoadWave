# Distributed runs

Running LoadWave across several machines, and what to expect when one of them
goes away.

---

## Do you actually need this?

Probably not yet. One reasonably sized machine running four worker processes
will generate more load than most services can absorb, and a single-machine run
is far easier to reason about.

Go distributed when you hit one of these:

- **The generator is the bottleneck.** CPU is pinned on the load machine, or
  `http_req_connecting` climbs while `http_req_waiting` stays flat. You are
  measuring yourself.
- **You have run out of ports.** A single host has roughly 28,000 ephemeral
  ports per destination.
- **Network egress is capped**, on the host or in its availability zone.
- **You need traffic from more than one place**, because geography or a load
  balancer's distribution is part of what you are testing.

Nothing about the test changes when you distribute it — only where it runs.

---

## Setting it up

On the coordinating host:

```sh
loadwave serve --listen 0.0.0.0:8090
```

That starts the coordinator on `:8090` for agents, the dashboard on `:8088`,
and — by default — a local agent too. Pass `--local-agent=false` if this host
should only coordinate.

On each load-generating host:

```sh
loadwave agent --coordinator loadgen-controller:8090 --workers 8
```

Then start a run from the dashboard, or through the API:

```sh
curl -X POST --data-binary @test.yaml http://loadgen-controller:8088/api/v1/runs
```

Agents dial **out**, so they need no inbound ports and work from behind NAT.
Only the coordinator needs a reachable address.

### Everyone must run the same binary

For a YAML test, any `loadwave` of the same version will do.

For a Go SDK test, the scenarios are compiled into the binary — so the binary
*is* the test, and every host must have the same one:

```sh
GOOS=linux GOARCH=amd64 go build -o checkout ./cmd/checkout
# ship `checkout` to every load host, then on each:
./checkout agent --coordinator loadgen-controller:8090
```

This is a feature rather than a chore: it is impossible for one host to be
quietly running last week's scenario. The agent advertises its version and the
dashboard shows it, so a mismatch is visible immediately.

---

## How the load is divided

Each agent advertises a virtual user capacity — by default 1,000 per core, or
whatever `--max-vus` says. The coordinator apportions the profile's peak in
proportion to those figures, so a 16-core host carries roughly eight times what
a 2-core host does rather than both getting the same share and the small one
falling behind.

Each agent then subdivides its own quota across its worker processes.

Nodes evaluate the load profile locally against a start instant the coordinator
agrees with everyone, so the ramp stays smooth without per-tick coordination.
Each node rounds independently, so the fleet's total can differ from the exact
curve by a virtual user or two mid-ramp. At peak it is exact.

### Labels

Labels are for your own grouping in the dashboard:

```sh
loadwave agent --coordinator ctl:8090 --label region=eu-west-1 --label tier=large
```

---

## Partitioning test data

Every virtual user has an id that is unique across the **whole fleet**, not
just its process. That is what lets each simulated user have its own account
without two machines colliding:

```go
username := fmt.Sprintf("loadtest-user-%d", vu.ID())
```

Or in YAML:

```yaml
json: { username: "loadtest-user-${__vu}" }
```

For fixtures loaded from a file, each node is given a static
`(shardIndex, shardCount)` pair so it can take its slice arithmetically, with
no coordination at runtime:

```go
mine := loadwave.Slice(vu.Shard(), allCustomers)
```

Ids and shards come from ranges the coordinator carves up in advance, so
allocation is a local increment and collisions are impossible by construction.

---

## Clocks

Metric buckets are aligned to the wall clock so that every node's "12:00:03"
means the same second and the coordinator can merge them without interpolating.

**Run NTP on every host.** A few hundred milliseconds of skew is absorbed by
the coordinator's grace window. Seconds of skew will get batches rejected, and
the report will say so:

```
warning: 12 metric batches arrived too late to be counted; check clock skew between hosts
```

---

## When a machine goes away

| What happens | The effect |
| ------------ | ---------- |
| **An agent disconnects** | Its stream breaks and the coordinator notices at once. The survivors' quotas are recomputed to absorb its share, so the run continues at the requested VU count. |
| **An agent goes quiet** | Missed heartbeats past 15 seconds evict it, then the same rebalance. |
| **An agent comes back** | It reuses its node id, so the coordinator replaces the stale session rather than counting the host twice, and gives it a share of the run in progress. |
| **A new agent joins mid-run** | Put to work immediately. Scaling a running test out is just starting another agent. |
| **A worker process dies** | Its agent redistributes to the surviving workers on that host. Logged and surfaced in the dashboard. |
| **The coordinator restarts** | Agents reconnect on a jittered backoff. The run's state is lost — runs are in-memory only. |

Every one of these is recorded in the event log, so a result that looks strange
can be traced to the fleet changing shape underneath it.

### What this costs you

Absorbing a dead agent's share means the survivors do more work than they were
sized for. If they were already at capacity, the run continues but the
generator is now the bottleneck — watch `http_req_connecting` and the host's
CPU before trusting numbers from after the event.

---

## Sizing

Rough starting points, worth measuring rather than trusting:

| Machine | Workers | Virtual users |
| ------- | ------- | ------------- |
| 2 cores | 1 | up to ~2,000 |
| 4 cores | 3 | up to ~5,000 |
| 8 cores | 7 | up to ~15,000 |
| 16 cores | 15 | up to ~30,000 |

The real ceiling depends far more on what your scenario does than on the core
count. A scenario that parses large JSON bodies costs an order of magnitude
more per iteration than one that fetches a health check.

### Raise the file descriptor limit

Every connection is a descriptor, and the default limit will bite long before
the CPU does:

```sh
ulimit -n 65535
```

### Watch for the generator becoming the bottleneck

- `http_req_connecting` rising while `http_req_waiting` stays flat
- CPU pinned on the load hosts
- Throughput flat while the VU count keeps climbing

All three mean the numbers are now about your generator. Add a machine.

---

## Securing it

**The control plane is unauthenticated and unencrypted.** Anyone who can reach
the coordinator's agent port can join the fleet and receive test plans — which
may contain credentials. Anyone who can reach the dashboard port can start and
stop runs.

Run it on a trusted network: a VPN, a private subnet, or behind an
authenticating proxy. Do not expose either port to the internet. Authentication
and mutual TLS are on the roadmap; until then this is the deployment
assumption. See [SECURITY.md](../SECURITY.md).

`--read-only` serves the dashboard without allowing runs to be started or
stopped, which is useful when showing a run to an audience.

---

## Kubernetes

A `Deployment` of agents against a coordinator `Service` works well, since
agents dial out and need no addressable identity:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: loadwave-agent
spec:
  replicas: 10
  selector:
    matchLabels: { app: loadwave-agent }
  template:
    metadata:
      labels: { app: loadwave-agent }
    spec:
      containers:
        - name: agent
          image: your-registry/loadwave-checkout:v1.2.3
          args:
            - agent
            - --coordinator=loadwave-coordinator:8090
            - --workers=3
          resources:
            requests: { cpu: "4", memory: 2Gi }
            limits: { cpu: "4", memory: 4Gi }
```

Two things to get right:

**Set CPU requests equal to limits.** A throttled agent produces latency
measurements that describe the throttling rather than the target.

**Keep `--workers` at or below the CPU limit.** More worker processes than
cores adds scheduling overhead without adding throughput.

Scaling the deployment mid-run works: new agents join and are given a share.

---

## See also

- [Architecture](architecture.md) — why it is shaped this way
- [Metrics](metrics.md) — how measurements from many hosts are merged
- [Configuration reference](configuration.md) — `workersPerAgent` and the rest
