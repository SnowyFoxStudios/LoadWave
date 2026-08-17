# Contributing to LoadWave

Thanks for wanting to help. This document is what you need to get productive:
how to build it, how it is organised, and what a change is expected to look
like before it is merged.

## Getting set up

You need **Go 1.26+** and **Node 26+**. Nothing else — every other tool is
pinned and installed into `.tools/` on demand.

```sh
git clone https://github.com/SnowyFoxStudios/LoadWave.git
cd LoadWave

make build          # ./bin/loadwave, with the dashboard embedded
make check          # everything CI runs
```

`make help` lists every target.

Working on the dashboard is nicer with hot reload:

```sh
make dev            # coordinator on :8088, Vite on :5173 proxying to it
```

Then open <http://localhost:5173> and start a run from the browser.

If you are only touching Go, you never need Node: `make build-nodashboard`
skips the frontend, and the server explains how to build it when someone hits
the UI.

## How the pieces fit together

Four tiers, each with one job:

```
    coordinator ── control plane; merges everything, serves the dashboard
         │  gRPC (agents dial out)
      agent ───── supervises one host's worker processes
         │  gRPC over a Unix socket
      worker ──── one OS process running a slice of the virtual users
         │
        VU ────── one goroutine, looping one scenario
```

| Package | What lives there |
| --------------------- | ------------------------------------------------- |
| `pkg/loadwave`        | The public SDK. Scenarios, VUs, the HTTP client, metric types. |
| `pkg/loadwave/run`    | `run.Main()` — turns a user's program into a LoadWave binary. |
| `internal/engine`     | The virtual user pool and the load-profile executors. |
| `internal/metrics`    | Recording, HDR histograms, and the coordinator's store. |
| `internal/control`    | The bidirectional gRPC stream, used at both tiers. |
| `internal/coordinator`| Run lifecycle, apportionment, thresholds. |
| `internal/agent`      | Worker process supervision. |
| `internal/worker`     | The process that generates load. |
| `internal/scenario`   | YAML configuration and the declarative step interpreter. |
| `internal/httpapi`    | REST, WebSocket, and serving the embedded dashboard. |
| `internal/cli`        | The command line. |
| `web/`                | The React dashboard. |

[docs/architecture.md](docs/architecture.md) explains the reasoning behind the
shape.

## What a good change looks like

**Explain the why, not the what.** The code says what it does. Comments should
say why it does it that way — what breaks otherwise, what was tried instead.
`// increment the counter` is noise; `// Deltas, not totals: a dropped batch
then costs one interval instead of skewing every interval after it` is the
comment worth writing.

**Test the failure path.** A test that only proves the happy path works is
half a test. The interesting cases here are the ones where something goes
wrong: an agent disappears mid-run, a batch arrives late, a scenario panics, a
tag has unbounded cardinality.

**Be careful with anything that touches a number.** Metric aggregation is the
part of this project that is easiest to get subtly, silently wrong. Percentiles
cannot be averaged; means have to be re-weighted by count; a rate is a fraction
plus a denominator. If your change touches `internal/metrics`, say in the pull
request what you did to convince yourself the numbers are right.

**Run the race detector.** Almost everything here is concurrent. `make test`
runs with `-race`; please do not skip it.

## Before you open a pull request

```sh
make check
```

That runs formatting, `go vet`, golangci-lint, buf lint, the frontend lint and
type check, and both test suites with the race detector. CI runs the same
thing, so a green `make check` should mean a green build.

If you changed a `.proto` file:

```sh
make generate       # and commit the result in gen/
```

Generated code is committed so that `go get` works without a protobuf
toolchain. CI fails if it has drifted.

## Testing

```sh
make test           # everything, with -race
make test-go        # Go only
make test-short     # skips the slow integration suite
make cover          # writes coverage.html
make bench          # the hot-path benchmarks
```

The integration suite in `internal/integration` is worth knowing about: it runs
a real coordinator, a real agent, and real worker **subprocesses** — the test
binary re-executes itself as a worker. It is the only place the distributed
path is exercised end to end, so if you change the control protocol, the
supervision logic, or metric relaying, that is where a regression will show up.

## Style

Go code follows standard `gofmt` and the usual conventions; golangci-lint
enforces the rest. A few things it cannot check:

- **Name things for what they are, not what they do to them.** `awaitWorkersFinished`, not `waitLoop`.
- **Errors read as sentences, lowercase, with context.** `fmt.Errorf("parse base URL %q: %w", url, err)`.
- **Errors an operator will see should say what to do.** "no agents are connected; start one with `loadwave agent`" beats "no agents".
- **Prefer a comment over a clever line.** This is a tool people debug at three in the morning.

For the dashboard: TypeScript in strict mode, no `any`, and Prettier decides
formatting. Before writing chart code, read the note at the top of
`web/src/index.css` — the colour palette is validated for contrast and
colour-vision deficiency as a *set*, and changing one value in isolation
invalidates it.

## Reporting bugs

Use the issue templates. The single most useful thing you can include is the
smallest configuration that still reproduces the problem, plus the output with
`--log-level debug`.

For security vulnerabilities, please use
[private reporting](https://github.com/SnowyFoxStudios/LoadWave/security/advisories/new)
rather than a public issue. See [SECURITY.md](SECURITY.md).

## Proposing larger changes

Open a discussion or an issue first. It is much easier to talk about the shape
of a new executor or a new protocol before it has been written than after.

## Licence

LoadWave is AGPL-3.0-or-later. By contributing you agree that your work is
licensed under the same terms. Please add the SPDX header to new files:

```go
// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later
```

## Code of conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md). Be
decent to each other.
