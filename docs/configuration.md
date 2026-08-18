# Configuration reference

A LoadWave test is a YAML file. Every field is listed here, with its default
and the reason it exists.

Unknown fields are **rejected**, not ignored. A misspelled key in a load test is
an expensive kind of bug: the run appears to work and quietly measures
something other than what you asked for.

Check a file without running it:

```sh
loadwave validate test.yaml
```

If you would rather not start from a blank file, the dashboard's **New run**
dialog has a builder that generates one — see
[the dashboard](../README.md#building-a-test-in-the-browser). Its output is an
ordinary configuration file, meant to be copied out and committed; nothing in
this reference is builder-only, and nothing here is out of its reach except
Go-defined scenarios, which it can select but not write.

---

## Top level

```yaml
name: storefront-checkout
baseURL: https://staging.example.com
workersPerAgent: 4
betweenRequests: 1s-3s

load: { ... }
http: { ... }
tags: { ... }
thresholds: [ ... ]
scenarios: [ ... ]
```

| Field | Type | Default | Meaning |
| ----- | ---- | ------- | ------- |
| `name` | string | `loadwave` | Identifies the run in the dashboard and in result files. |
| `baseURL` | string | — | Prefixed to every relative request path. Must be absolute. |
| `workersPerAgent` | int | cores − 1 | Worker **processes** each agent spawns. See below. |
| `betweenRequests` | duration or range | `1s` | Pause after every request, whatever its outcome. See below. |
| `load` | object | required | The shape of the load over time. |
| `http` | object | `{}` | HTTP client settings. |
| `tags` | map | `{}` | Labels attached to every metric the run produces. |
| `thresholds` | list | `[]` | Pass/fail assertions. |
| `scenarios` | list | all registered | What to run. |

### `workersPerAgent`

Virtual users are goroutines, and past a few thousand of them one Go runtime's
scheduler and garbage collector — not the system under test — become the
bottleneck. Spreading the pool across processes buys that headroom back, and
means a scenario that panics cannot take the whole generator down.

One per core, less one, is a reasonable default; the spare core leaves room for
supervision and metric reporting. Raising it past the core count does not help.

### `betweenRequests`

A pause inserted after **every** request, whatever its outcome. This is the
run's pacing floor, and the default is one second.

```yaml
betweenRequests: 1s        # the default
betweenRequests: 500ms-2s  # jittered, which is usually better
betweenRequests: "0"       # none — flat out
```

It is deliberately not zero by default. Without it:

- a scenario with no think time of its own loops as fast as the network allows;
- a scenario whose request **fails instantly** — a refused connection, a 500
  from a cache — loops as fast as the CPU allows.

The second is the dangerous one. It is how a load test turns into an accidental
denial of service against a service that has already fallen over, and it is why
the pause applies on the failure path too rather than only after a successful
response.

Because it applies after the last request of an iteration as well, it also
paces the gap between iterations. A scenario's own `think` steps are additional
to it.

**Prefer a range.** Identical pauses make every virtual user march in lockstep,
producing traffic in synchronised bursts rather than the smooth arrival pattern
a real population generates — and the bursts are what your service ends up
being measured against.

**Set it to `"0"` for a throughput test**, where flat out is the point. Note the
quotes: unquoted `0` is a number, and this field is a duration string.

Individual steps override it, including down to none:

```yaml
steps:
  - get: /api/products
  - get: /api/products/${id}
    betweenRequests: 200ms   # quicker after this one
  - post: /api/orders
    betweenRequests: "0"     # straight on to the next
```

Pacing is excluded from `iteration_duration`, exactly as `think` is: it is not
work, and counting it would make every iteration look a second slower than it
is. It is also interruptible, so a stopping run does not sit through everyone's
pause.

`loadwave validate` prints the effective value, so it is never left implicit.

---

## `load`

### `constant-vus`

Holds a fixed number of virtual users. Needs either a `duration` or an
`iterations` budget — without one the run would never end, which is rejected.

```yaml
load:
  executor: constant-vus
  vus: 50
  duration: 5m
```

### `ramping-vus`

Moves linearly between stage targets. Every profile starts from zero, so the
first stage ramps up rather than starting at its own target.

```yaml
load:
  executor: ramping-vus
  stages:
    - { duration: 30s, target: 100 } # 0 → 100
    - { duration: 5m, target: 100 } # hold
    - { duration: 2m, target: 500 } # push past it, looking for the knee
    - { duration: 30s, target: 0 } # ramp down
```

| Field | Type | Default | Meaning |
| ----- | ---- | ------- | ------- |
| `executor` | string | `constant-vus` | `constant-vus` or `ramping-vus`. |
| `vus` | int | `1` | Virtual users to hold. `constant-vus` only. |
| `duration` | duration | — | How long to hold. `constant-vus` only. |
| `stages` | list | — | Legs of the ramp. `ramping-vus` only. |
| `iterations` | int | `0` | Stop after this many iterations in total, across every node. Zero means the duration governs. |
| `maxIterationRate` | int | `0` | Cap on iterations **started** per second, fleet-wide. Zero leaves the VU count as the only control. |
| `gracefulStop` | duration | `30s` | How long in-flight iterations get to finish when the run stops. |

Durations are Go duration strings: `500ms`, `30s`, `5m`, `1h30m`.

### `maxIterationRate` throttles iterations, not requests

A scenario issuing five requests per iteration at `maxIterationRate: 100`
produces roughly 500 requests per second. This is the arrival rate a
closed-model run would otherwise leave implicit — useful when you want to hold
throughput steady rather than concurrency.

### `gracefulStop`

When a run ends, virtual users are asked to finish their current iteration
before exiting. Without this, stopping manufactures a cliff of cancelled
requests that then appear as failures in the very results you are about to
read. Past the budget, they are cancelled anyway — a scenario that ignores its
context must not be able to hold the run open forever.

---

## `http`

```yaml
http:
  timeout: 20s
  headers:
    Accept: application/json
    Authorization: Bearer ${TOKEN}
  followRedirects: false
  insecureSkipTLSVerify: true
```

| Field | Type | Default | Meaning |
| ----- | ---- | ------- | ------- |
| `timeout` | duration | `30s` | Bounds a whole request, body included. |
| `headers` | map | `{}` | Sent with every request. Per-step headers win. |
| `userAgent` | string | Go's default | Overrides `User-Agent`. |
| `insecureSkipTLSVerify` | bool | `false` | Skip certificate validation. |
| `maxIdleConnsPerHost` | int | `512` | Pooled idle connections per host. |
| `disableKeepAlives` | bool | `false` | Fresh connection per request, so setup cost is measured too. |
| `disableCompression` | bool | `false` | Stop requesting gzip. |
| `followRedirects` | bool | `false` | Follow 3xx responses. |
| `maxRedirects` | int | `10` | Redirect depth cap. |
| `isolatePerVU` | bool | `false` | Give every VU its own connection pool. |
| `discardBody` | bool | `false` | Stream response bodies to nowhere. Bytes are still counted. |
| `maxBodyBytes` | int | `4194304` | How much of a body to buffer. |
| `proxy` | string | environment | Proxy URL. |

### Two defaults worth knowing about

**`maxIdleConnsPerHost` defaults to 512, not Go's 2.** Go's default throttles a
load test to a trickle of connection churn, so it ends up measuring the client
rather than the server. This is the single most common way a hand-rolled Go
load test produces numbers that are quietly wrong.

**`followRedirects` is off.** A load test usually wants to measure the redirect
itself rather than silently follow it and attribute the cost to the first
request.

**`isolatePerVU` is off.** Sharing one pool is far cheaper. Turn it on when you
need each virtual user to behave like a genuinely separate client — at the cost
of a file descriptor per VU per host, which will hit ulimits at high VU counts.

---

## `scenarios`

An entry with `steps` is defined here. An entry with only a `name` refers to a
scenario compiled into the binary with the Go SDK. Leaving the list out runs
everything the binary has registered.

If there are no scenarios anywhere — no `steps`, nothing compiled in — but a
`baseURL` is set, LoadWave synthesises a single GET of that URL's path. That is
what makes the one-liner smoke test work:

```sh
loadwave run --url https://example.com/health --vus 50 --duration 30s
```

```yaml
scenarios:
  - name: browse
    weight: 3 # runs three times as often as a weight-1 scenario
    description: A visitor looking around.
    vars:
      category: electronics
    steps: [ ... ]

  - name: checkout # defined in Go
    weight: 1
```

A virtual user is assigned to one scenario for its whole life, not re-drawn
each iteration. That is what makes per-user state coherent: a user who logged
in during `OnVUStart` stays logged in.

### Steps

Each step is either a request or a pause.

```yaml
steps:
  - name: list products # metric label; derived if omitted
    get: /api/products # or post/put/patch/delete/head, or method + url
    headers: { X-Request-Id: "${__uuid}" }
    query: { page: "1" }
    json: { filter: "${category}" } # or `form`, or raw `body`
    expect: [200] # acceptable statuses
    timeout: 10s # overrides the run-wide timeout
    capture:
      productId: items.0.id # pull a value out of the JSON response

  - think: 1s-3s # a pause, drawn uniformly from the range
```

| Field | Meaning |
| ----- | ------- |
| `name` | Metric label. Derived from the method and path if omitted, with variable-looking segments collapsed to `*`. |
| `get` / `post` / `put` / `patch` / `delete` / `head` | Shorthand setting both method and URL. |
| `method`, `url` | The explicit form. Use one or the other, not both. |
| `headers`, `query` | Merged over the run-wide settings. Values are templated. |
| `json`, `form`, `body` | The request body. At most one. Templated at any depth. |
| `expect` | Acceptable status codes. Anything else fails the step's check and ends the iteration. |
| `capture` | Variables extracted from the JSON response. |
| `timeout` | Per-step override. |
| `think` | Pause instead of requesting. `2s`, or a range like `1s-3s`. |

### Always jitter your think times

`think: 1s-3s`, not `think: 2s`. Constant think times make virtual users march
in lockstep and produce artificial traffic spikes that no real population
would.

### `capture` paths

Field access and array indexing, in either dotted or bracketed form:

```yaml
capture:
  id: id
  token: $.data.token
  firstSku: items.0.sku
  alsoFirstSku: items[0].sku
```

This is deliberately not JSONPath. Filters and recursive descent would be a
query language to document, test and explain, in exchange for capabilities a
load test almost never wants.

### Templating

`${name}` interpolates in URLs, headers, query values and bodies. Unknown names
render as empty strings, so a capture that did not fire produces a request that
visibly misses rather than an iteration that dies before making one.

Built-ins, all prefixed so they cannot collide with your own variables:

| Variable | Value |
| -------- | ----- |
| `${__vu}` | The virtual user's run-wide unique id. |
| `${__iteration}` | Zero-based iteration number. |
| `${__shard}` / `${__shards}` | This node's data partition, and the total count. |
| `${__random}` | A random integer from this VU's own generator. |
| `${__uuid}` | A random v4 UUID. |
| `${__timestamp}` | Now, RFC 3339. |
| `${__unixMilli}` | Now, epoch milliseconds. |

`${__vu}` is how you give each virtual user its own account without two nodes
colliding — ids are unique across the whole fleet, not just the process.

---

## `thresholds`

A run that breaches any threshold exits `2`. That is what makes LoadWave usable
as a CI gate.

```yaml
thresholds:
  - { metric: http_req_duration, stat: p95, op: "<", value: 500 }
  - { metric: http_req_failed, stat: rate, op: "<", value: 0.01 }
  - { metric: checks, stat: rate, op: ">", value: 0.99 }
  - { metric: http_req_failed, stat: rate, op: "<", value: 0.25, abortOnFail: true }
```

| Field | Meaning |
| ----- | ------- |
| `metric` | Any metric name, built-in or custom. |
| `stat` | `count`, `rate`, `avg`, `min`, `max`, `p50`, `p90`, `p95`, `p99`, `p999`. |
| `op` | `<`, `<=`, `>`, `>=`. |
| `value` | Milliseconds for durations; a fraction of one for rates. |
| `abortOnFail` | Stop the run immediately on breach rather than only failing at the end. |

Thresholds are evaluated against **whole-run** aggregates, because that is the
question a CI gate is asking. A breach **latches**: a p95 that recovers by the
end of the run still breached, and a gate looking only at the final instant
would let a real regression through.

A threshold on a metric that was never produced is reported as **not
measured**, not as a pass. "We never measured it" and "it was fine" are very
different answers to hand back to a pipeline.

`abortOnFail` is for conditions where finishing teaches you nothing — an error
rate of 25% means the environment is broken, not slow.

---

## Overriding from the command line

Every field with an obvious flag has one, and flags win over the file. Useful
for varying one number per environment without editing anything:

```sh
loadwave run test.yaml --vus 500 --duration 10m
loadwave run test.yaml --tag env=prod --threshold 'http_req_duration:p99<1000'
loadwave run --url https://example.com --vus 50 --duration 30s   # no file at all
```

```
--url, --name, --vus, --duration, --stages, --iterations, --rate,
--workers, --graceful-stop, --tag, --scenario, --threshold,
--insecure, --header, --timeout
```

Two more control the output rather than the run:

```
--out results.json      the whole snapshot, for scripting
--report results.html   a self-contained report with charts, for people
```

Pacing has a flag too, which is the quickest way to turn a smoke test into a
throughput test:

```sh
loadwave run test.yaml --between-requests 0
loadwave run test.yaml --between-requests 200ms-1s
```

`--stages` takes `duration:target` pairs: `--stages 30s:100,5m:100,30s:0`.

`--threshold` takes `metric:stat<value`: `--threshold 'http_req_duration:p95<500'`.

---

## See also

- [Metrics](metrics.md) — what is measured and how it is aggregated
- [Distributed runs](distributed.md) — running across machines
- [Architecture](architecture.md) — why it is built this way
