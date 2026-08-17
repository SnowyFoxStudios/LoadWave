<div align="center">

# LoadWave

**Distributed load testing with a live dashboard.**

Write your test in Go or YAML, run it from one machine or fifty, and watch the
results as they happen.

[![CI](https://github.com/SnowyFoxStudios/LoadWave/actions/workflows/ci.yml/badge.svg)](https://github.com/SnowyFoxStudios/LoadWave/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/SnowyFoxStudios/LoadWave.svg)](https://pkg.go.dev/github.com/SnowyFoxStudios/LoadWave)
[![License: AGPL v3](https://img.shields.io/badge/license-AGPL--3.0-blue.svg)](LICENSE)

</div>

---

## What it is

LoadWave generates HTTP load from a fleet of worker processes, merges what they
measure into one coherent picture, and shows it live in a browser — or prints a
report and exits non-zero, if that is what your pipeline needs.

It is one binary. No agent to deploy alongside it, no database to stand up, no
separate UI to host.

```sh
loadwave demo                     # nothing to set up — see it working now
loadwave run test.yaml            # run your test, print a report, exit
loadwave run test.yaml --ui       # ...and watch it in a browser
```

**Why another one?**

- **Percentiles that are actually correct across machines.** Nodes ship whole
  HDR histograms rather than their own percentiles, and the coordinator merges
  the distributions. A p99 from ten agents is the real p99, not an average of
  ten p99s — which is not a number that means anything.
- **Real processes, not just goroutines.** Past a few thousand virtual users a
  single Go runtime's scheduler and garbage collector become the bottleneck
  rather than the system under test. LoadWave spreads the pool across worker
  processes so you keep measuring the server.
- **Tests you can review.** Behaviour is ordinary Go — conditionals, session
  state, custom auth — testable with `go test` against an `httptest.Server`,
  with no coordinator involved. Simple flows can stay in YAML.
- **Built to be a CI gate.** Thresholds are first-class, and the exit code
  distinguishes "the tool broke" from "your service was too slow".

---

## Install

```sh
go install github.com/SnowyFoxStudios/LoadWave/cmd/loadwave@latest
```

Or grab a binary from [Releases](https://github.com/SnowyFoxStudios/LoadWave/releases).
Every archive is one self-contained executable with the dashboard inside it.

Building from source needs **Go 1.26+** and **Node 26+**:

```sh
git clone https://github.com/SnowyFoxStudios/LoadWave.git
cd LoadWave
make build      # ./bin/loadwave
```

---

## See it working

`loadwave demo` starts a small target server of its own and runs a two-scenario
test against it, with the dashboard on <http://localhost:8088>. No
configuration, no service to stand up:

```sh
loadwave demo                        # runs for 10 minutes; Ctrl-C to stop early
loadwave demo --duration 2m --vus 50
loadwave demo --headless             # no dashboard, just the report
```

The demo target is a toy — its numbers say nothing about your hardware. It is
there to show you the dashboard and to prove a fresh build works end to end.

## Your first test

```yaml
# test.yaml
name: storefront
baseURL: https://staging.example.com

load:
  executor: ramping-vus
  stages:
    - { duration: 30s, target: 100 } # ramp up
    - { duration: 5m, target: 100 } # hold
    - { duration: 30s, target: 0 } # ramp down

# Pause after every request, whatever its outcome. Defaults to 1s, which keeps
# a scenario — or a failing endpoint — from being hammered in a tight loop.
# Set it to "0" for a throughput test.
betweenRequests: 500ms-1s

thresholds:
  - { metric: http_req_duration, stat: p95, op: "<", value: 500 }
  - { metric: http_req_failed, stat: rate, op: "<", value: 0.01 }

scenarios:
  - name: browse
    steps:
      - name: list products
        get: /api/products
        expect: [200]
        capture:
          productId: items.0.id # pull a value out of the JSON response

      - think: 1s-3s # jittered, like a real person reading the page

      - name: view product
        get: /api/products/${productId} # ...and use it here
        expect: [200]
```

```sh
loadwave run test.yaml
```

```
──────────────────────────────────────────────────────────────────────────────
  storefront — completed
  30s to 100 VUs, then 5m0s to 100 VUs, then 30s to 0 VUs
  ran for 6m0s across 1 agent
──────────────────────────────────────────────────────────────────────────────

  METRIC             VALUE
  requests           184,209
  throughput         512/s
  duration p95       46.3ms
  failed requests    2,931 (1.59%)
  checks passed      181,278 of 184,209

  REQUEST          COUNT    AVG     P95     P99     MAX     ERRORS
  view product     92,104   30.3ms  48.6ms  50.4ms  91.2ms  3.18%
  list products    92,105   15.1ms  24.7ms  25.5ms  62.1ms  0%

  THRESHOLD                    ACTUAL  RESULT
  http_req_duration p95 < 500  46.34   pass
  http_req_failed rate < 0.01  0.0159  FAIL
```

Exit code `2` — a threshold was breached. Your pipeline just caught a regression.

---

## The dashboard

```sh
loadwave demo                      # a self-contained tour
loadwave run test.yaml --ui        # dashboard alongside a scripted run
loadwave serve                     # long-lived; start runs from the browser
```

Live charts for virtual users, throughput, response time and responses by
status class. The response-time chart draws one line per endpoint — click any
of them, or any row of the Requests table, to isolate it.

Below the charts: a per-endpoint breakdown, threshold verdicts, agent health,
an event log, and a **Failed requests** panel giving each failure's status
code and an excerpt of what the server actually said. Runs can be started,
stopped and rescaled while they are in flight — rescaling takes a ramp, so
raising the target introduces the new users over a period rather than
spawning them all in one tick.

**Stopping a run does not stop LoadWave.** The run ends, the results stay on
screen, and you can download the report or start another test. Ending the
process is a separate, deliberate action: Ctrl-C in the terminal, or the
**Power off** control in the browser.

The whole surface is also a REST API, so anything the dashboard does is
scriptable without it.

### Building a test in the browser

**New run** opens a scenario builder: a form for the load profile, pacing,
thresholds and scenario steps, with the YAML generated beside it as you type.

It is not an alternative format. The panel on the right is the file you would
have written by hand, so **Copy** gives you something to commit and run from
CI with `loadwave run`. Switch to **Edit YAML** at any point to hand-tune the
result, or to paste a file you already have.

Every edit is checked by the same parser the runner uses — the builder does
not reimplement the schema — and the answer is echoed back in words:

```
Profile      30s to 25 VUs, then 2m0s to 25 VUs, then 30s to 0 VUs
Peak VUs     25
Pacing       1s (default)
Scenarios    browse ×1
Thresholds   http_req_duration p95 < 500 · http_req_failed rate < 0.01
```

That is a stronger confirmation than a green tick: it proves the runner read
the form the same way you did. A test with no thresholds is called out, since
it will pass whatever the results turn out to be. **Start run** stays disabled
until the configuration actually parses.

### Taking the results away

```sh
loadwave run test.yaml --report results.html
```

Or use the **Download report** button. Either way you get one self-contained
HTML file — charts included, as inline SVG — with no scripts, no external
assets and no network dependency. It renders the same in an email client, as a
ticket attachment, or opened in two years to settle an argument about when a
regression started.

---

## Going distributed

One machine can usually push more load than people expect. When it cannot,
nothing about the test changes — only where it runs.

On the coordinating host:

```sh
loadwave serve --listen 0.0.0.0:8090
```

On each load-generating host:

```sh
loadwave agent --coordinator loadgen-controller:8090 --workers 8
```

Agents dial **out**, so they need no inbound ports and work from behind NAT.
The coordinator divides the load in proportion to the capacity each agent
advertises, and an agent that joins mid-run is put to work immediately — so
scaling a running test out is just starting another agent. If one dies, the
survivors absorb its share and the dashboard says so.

---

## Writing tests in Go

YAML runs out of road as soon as a test needs a session, a branch, or an
authentication flow that isn't a bearer token. At that point write Go: your
program *becomes* the LoadWave binary, so there is nothing extra to deploy.

```go
package main

import (
    "context"
    "net/http"
    "time"

    "github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
    "github.com/SnowyFoxStudios/LoadWave/pkg/loadwave/run"
)

func main() {
    loadwave.Register(loadwave.Scenario{
        Name:      "checkout",
        OnVUStart: signIn, // once per virtual user, not once per iteration
        Run:       checkout,
    })
    run.Main()
}

func checkout(ctx context.Context, vu *loadwave.VU) error {
    resp, err := vu.HTTP().PostJSON(ctx, "/api/orders", order{Payment: "card"})
    if err != nil {
        return err
    }
    vu.Check("order accepted", resp.StatusCode == http.StatusCreated)
    vu.ThinkBetween(ctx, time.Second, 3*time.Second)
    return nil
}
```

```sh
go build -o checkout ./cmd/checkout
./checkout run --url https://staging.example.com --vus 200 --duration 5m --ui
./checkout agent --coordinator loadgen-controller:8090   # the same binary
```

Because the scenarios are compiled in, every host in the fleet is provably
running the same test. See [`examples/checkout`](examples/checkout) for a
worked example with login, per-user state and a custom metric.

Scenarios are ordinary Go, so they are testable without any LoadWave
infrastructure at all:

```go
func TestCheckout(t *testing.T) {
    server := httptest.NewServer(myHandler())
    defer server.Close()

    factory, _ := loadwave.NewHTTPClientFactory(loadwave.HTTPOptions{BaseURL: server.URL})
    vu := loadwave.NewVU(loadwave.VUConfig{ID: 1, HTTP: factory.New()})

    if err := checkout(t.Context(), vu); err != nil {
        t.Fatal(err)
    }
}
```

---

## In CI

```yaml
- name: Load test
  run: loadwave run test.yaml --out results.json --report results.html

- name: Keep the results
  if: always()
  uses: actions/upload-artifact@v4
  with:
    name: load-test
    path: results.*
```

| Exit code | Meaning |
| --------- | ------------------------------------------------------- |
| `0`       | The run completed and every threshold passed. |
| `1`       | LoadWave could not do what was asked: bad configuration, no agents, unreachable coordinator. |
| `2`       | The run completed, but a threshold was breached. |
| `130`     | Interrupted. |

The distinction between `1` and `2` is deliberate: "the tool broke" and "the
service was too slow" call for very different responses from a pipeline.

---

## Commands

| Command | What it does |
| ------------------- | ---------------------------------------------------- |
| `loadwave demo`     | Run a self-contained demo against a built-in target. Nothing to configure. |
| `loadwave run`      | Run a test to completion and print a report. Add `--ui` for the dashboard. |
| `loadwave serve`    | Long-lived coordinator and dashboard; runs are started from the browser or the API. Pass `--allow-shutdown` to expose the browser's Power off control. |
| `loadwave agent`    | Join a coordinator and generate load on this machine. |
| `loadwave validate` | Check a configuration without running it. Belongs in a pre-commit hook. |
| `loadwave version`  | Print version and build information. |

`loadwave <command> --help` has the flags.

---

## Documentation

| | |
| ------------------------------------------- | -------------------------------------------- |
| [Configuration reference](docs/configuration.md) | Every field of the YAML format. |
| [Metrics](docs/metrics.md)                  | What is measured, and how it is aggregated. |
| [Architecture](docs/architecture.md)        | How the coordinator, agents and workers fit together. |
| [Distributed runs](docs/distributed.md)     | Running across machines, and what happens when one dies. |
| [Go SDK](https://pkg.go.dev/github.com/SnowyFoxStudios/LoadWave/pkg/loadwave) | API reference. |
| [Contributing](CONTRIBUTING.md)             | How to build, test and submit changes. |

---

## Status

Early. The architecture is settled and the whole path — coordinator, agents,
worker processes, merged metrics, dashboard — works and is covered by tests
that spawn real subprocesses. The API may still change before `v1.0.0`.

Protocols other than HTTP, an open-model arrival-rate executor, and persisting
results beyond a run are on the roadmap. Issues and pull requests welcome.

## License

[AGPL-3.0-or-later](LICENSE). Using LoadWave to test your own systems carries
no obligations. Offering a **modified** LoadWave to others as a network service
means publishing your changes.
