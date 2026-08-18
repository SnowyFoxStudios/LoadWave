# Metrics

What LoadWave measures, how it combines measurements from many machines, and
where the numbers can mislead you.

---

## The built-in metrics

The HTTP client records these automatically. Durations are in **milliseconds**;
rates are fractions of one.

| Metric | Kind | What it is |
| ------ | ---- | ---------- |
| `http_reqs` | counter | Requests issued. |
| `http_req_duration` | trend | The whole request, first byte written to last byte read. |
| `http_req_waiting` | trend | Time to first byte — the server's own think time, with connection setup and body transfer excluded. |
| `http_req_connecting` | trend | TCP setup. Recorded only on a fresh connection. |
| `http_req_tls_handshaking` | trend | TLS handshake. Fresh connections only. |
| `http_req_failed` | rate | Share of requests judged unsuccessful. |
| `http_req_bytes_in` / `_out` | counter | Bytes read and written. |
| `iterations` | counter | Completed scenario iterations. |
| `iteration_duration` | trend | One iteration, **excluding** think time. |
| `iteration_failed` | rate | Share of iterations that returned an error. |
| `vus` | gauge | Virtual users currently executing. |
| `checks` | rate | Share of checks that passed. |
| `errors` | counter | Errors reported by scenarios. |

### `http_req_duration` versus `http_req_waiting`

`duration` is what a user experiences. `waiting` is what the server is
responsible for. When `duration` rises but `waiting` does not, the server is
fine and you are looking at connection churn, TLS, or a saturated generator.
That distinction is usually the first thing worth checking.

### Connection metrics are only recorded on fresh connections

A reused connection has no setup cost, and recording a zero for it would drag
every percentile toward zero. So `http_req_connecting` and
`http_req_tls_handshaking` count only the requests that actually opened a
connection — their `count` is deliberately lower than `http_reqs`.

### What counts as a failure

By default, a transport error or a status of 400 or above. A step's `expect`
list overrides that, so a scenario probing for a 404 is not charged a failure
for finding one.

An HTTP 500 is **not** a transport error. At load-test altitude it is a
successful exchange with a bad status — the scenario gets the response and can
decide what it means.

---

## Labels

Every observation carries labels. The dashboard groups and filters on them.

| Label | Value |
| ----- | ----- |
| `scenario` | The scenario that produced it. |
| `name` | The call site's stable identity. |
| `method` | HTTP method. |
| `status` | Status code as a string, or `0` if no response arrived. |
| `error` | A bounded classification of a transport failure. |
| `check` | The name given to a check. |

### Cardinality is the thing to be careful about

Every distinct label combination is a separate time series held in memory for
the length of the run. A label with unbounded values — a user id, a timestamp,
a raw error string — creates a series per value and will exhaust memory.

Two defences are built in:

**Request names collapse automatically.** `DeriveRequestName` replaces path
segments that look like identifiers with `*`, so `/users/1` and `/users/2`
share one series instead of two million. Numeric segments, UUIDs and long hex
strings are collapsed. Set `name` explicitly whenever the heuristic would still
leave you with high cardinality.

**Transport errors are classified, not quoted.** The `error` label is drawn
from a fixed vocabulary — `timeout`, `connection_refused`, `connection_reset`,
`dns`, `tls`, `eof`, `too_many_redirects`, `canceled`, `unknown` — because raw
error text embeds addresses and ports and would be unbounded.

Beyond that, each node caps itself at 5,000 series and the coordinator caps
itself again. Past the cap, observations for **new** series are dropped and
counted. A bounded, visibly lossy run beats an out-of-memory kill, and the
report says so explicitly:

```
warning: 1,284 samples were dropped by nodes that hit their series cap;
         a high-cardinality tag is the usual cause
```

If you see that, find the runaway tag.

---

## How aggregation works

This is the part that is easy to get subtly, silently wrong, so it is worth
understanding.

### Percentiles cannot be averaged

The p99 of ten agents is **not** the average of ten p99s. That number does not
mean anything. Getting it right means combining the underlying distributions,
not their summaries.

LoadWave uses [HDR histograms](https://hdrhistogram.github.io/HdrHistogram/),
which merge losslessly. Each node ships its whole distribution and the
coordinator merges them, so a p99 across a fleet is the true p99.

Every node must use an identical bucket layout or the merge is meaningless, so
the resolution is fixed run-wide and a node reporting a different one is
rejected rather than accommodated.

The default resolution is 0.1ms to 60s at two significant figures — about 1%
relative error, which is far finer than the noise in any real load test, and
roughly 13KB per histogram.

### A percentile can read slightly above the maximum

`max` is tracked exactly; a percentile is the upper bound of the histogram
bucket it falls in. At 1% precision a p99 can therefore come out a shade above
the largest value actually observed. It is a property of bucketed histograms,
not a bug — and the alternative, keeping every sample so percentiles are exact,
does not survive contact with millions of requests.

### Values above the ceiling are clamped, not dropped

An observation past 60s is recorded at 60s. It understates the outlier, but a
request that hit its timeout is nearly always the most interesting sample in
the run, and discarding it would make the tail look better than it is.

### Batches are deltas, not totals

A node reports what happened in each interval, never a running total. A dropped
batch then costs one interval of data instead of skewing every interval after
it — which matters because a node reconnecting after a network blip is routine.

### Time buckets are aligned to the wall clock

Every node cuts its buckets at the same instants, so the coordinator can merge
them without interpolating and a p99 for 12:00:03 really is everyone's traffic
in that second. Clocks need to be roughly in step, which NTP handles; a grace
period absorbs the rest. Batches arriving after it are dropped and counted, and
the report warns about clock skew.

### Two levels of retention

Keeping a full histogram per series per second for an hour would cost tens of
gigabytes. So the coordinator keeps:

- **Cumulative** aggregates at full label cardinality and full histogram
  fidelity, for the whole run. This drives the endpoint table and every
  threshold.
- **Time buckets** at reduced dimensionality — per metric and per scenario —
  holding scalars plus four precomputed percentiles. This drives the live
  charts. The percentiles are computed from the merged histogram *before* it is
  released, so they are correct; the histogram itself is then discarded.
- **Per-endpoint buckets** holding a sum and a count only, and therefore an
  average. This is what the response-time chart plots one line from.

The last of those is deliberately average-only. A histogram per endpoint per
second would cost gigabytes over an hour, where a sum and a count cost
thirty-two bytes. Percentiles *per endpoint* are still exact for the whole run
— that is what the endpoint table shows — but they are not available second by
second.

### Use the totals, not the per-series numbers

The API exposes both `series` — one entry per label combination — and `totals`,
one correctly merged aggregate per metric. Always use `totals` for a whole-run
figure. Folding `series` yourself produces plausible-looking numbers that
disagree with the thresholds, because means need re-weighting by count and
percentiles need the distributions.

The same applies to `endpoints`: those percentiles are recomputed from each
endpoint's merged distribution across all of its status codes. Taking the
maximum of the per-status percentiles — the obvious shortcut — reports the tail
of whichever status happened to be slowest, and the two diverge most exactly
when an endpoint starts failing.

---

## Why a request failed

Metrics can say that 3% of requests failed. They cannot say that they were all
`502 payment declined` on one endpoint, and that is usually the question.

So alongside the counters, LoadWave keeps a small table of failure *kinds*,
aggregated by request name, method, status and transport error class — every
one of those already bounded. Each row carries a short excerpt of what the
server actually said: the response body, or the transport error's text,
collapsed to a single line and clipped to 240 characters.

The excerpt is captured the first time each kind is seen and not again. A run
where everything fails would otherwise spend its time copying error strings,
and the second occurrence's body almost never says anything the first did not.
The count keeps rising regardless.

Distinct kinds are capped, per node and again on the coordinator. Past the cap
new kinds are dropped and counted, and the dashboard and report both say so
rather than presenting a partial list as complete.

This appears as the **Failed requests** panel in the dashboard and a section of
the HTML report.

## Custom metrics

Scenarios can emit their own. They are ordinary metrics: they appear in the
dashboard with percentiles and can carry thresholds.

```go
const cartValue = "cart_value"

func checkout(ctx context.Context, vu *loadwave.VU) error {
    // ...
    vu.Metrics().Trend(cartValue, vu.Labels(), receipt.Total)
    return nil
}
```

```go
vu.Metrics().Count("orders_placed", vu.Labels(), 1)
vu.Metrics().Rate("payment_succeeded", vu.Labels(), ok)
vu.Metrics().Gauge("queue_depth", vu.Labels(), float64(depth))
```

Trend values must be in the same 0.1ms-to-60s window as the built-ins, scaled
into whatever unit makes sense for your metric.

Add labels with `vu.Tag`, which applies to everything the VU emits for the rest
of the iteration:

```go
vu.Tag("flow", "purchase")
```

Keep tag values to a small fixed set. A tag per customer is a series per
customer.

---

## Checks

A check records a named assertion and returns its result, so it composes into
control flow:

```go
if !vu.Check("logged in", resp.StatusCode == http.StatusOK) {
    return fmt.Errorf("login failed: %d", resp.StatusCode)
}
```

A failing check does **not** by itself fail the iteration — return an error to
do that. Checks measure how often something held; errors say the iteration did
not accomplish what it set out to.

`Checkf` adds a message logged on failure. The message is not used as a label,
so it can safely contain specific values.

---

## Reading the numbers

**`http_reqs` far below what you expected.** Usually the generator, not the
target. Check whether virtual users are spending their time in `think`, and
whether `maxIterationRate` is throttling.

**`duration` climbing while `waiting` stays flat.** Not the server. Connection
churn, TLS renegotiation, or a saturated generator. Check
`http_req_connecting`, and whether `maxIdleConnsPerHost` is too low.

**A p99 far above the p95, with a flat max.** The max is probably the histogram
ceiling — you have requests hitting their timeout, and their real durations are
unknown.

**`iteration_duration` much larger than the sum of the requests.** Time is
going somewhere other than HTTP: scenario logic, JSON decoding, or contention
in your own code. Think time and request pacing are both already excluded.

**Throughput lower than the virtual user count suggests.** Check
`betweenRequests`. It defaults to one second, so fifty virtual users with a
single-request scenario produce roughly fifty requests a second, not thousands.
That is deliberate — see [the configuration reference](configuration.md) — and
`--between-requests 0` removes it.

**Any "samples were dropped" warning.** The reported numbers understate
reality. Fix the cardinality before trusting the run.

---

## See also

- [Configuration reference](configuration.md) — thresholds and tags
- [Architecture](architecture.md) — where aggregation happens
- [Distributed runs](distributed.md) — what merging looks like across machines
