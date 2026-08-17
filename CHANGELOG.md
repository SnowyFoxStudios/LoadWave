# Changelog

All notable changes to LoadWave are recorded here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Distributed execution: a coordinator, agents that dial out to it, and worker
  processes supervised per host. Agents joining mid-run are put to work
  immediately; when one dies, the survivors absorb its share.
- Load profiles: `constant-vus` and `ramping-vus`, with linear interpolation
  between stages and an optional cap on iterations started per second.
- Two ways to write a test: a typed Go SDK (`pkg/loadwave`) compiled into your
  own binary, and a declarative YAML format with templating, JSON capture and
  think times.
- Metric aggregation built on mergeable HDR histograms, so percentiles stay
  correct when a run is spread over many machines.
- Thresholds evaluated against whole-run aggregates, with `abortOnFail` to cut
  a run short, and exit codes that distinguish a tool failure from a breached
  threshold.
- A live dashboard, embedded in the binary: charts for virtual users,
  throughput, response-time percentiles and responses by status class, plus a
  per-endpoint table, agent health and an event log.
- REST and WebSocket APIs covering everything the dashboard can do.
- `loadwave validate`, for checking a configuration in a pre-commit hook.
- Cardinality limits on every tier, with dropped samples surfaced in the report
  rather than silently understating the results.

[Unreleased]: https://github.com/SnowyFoxStudios/LoadWave/compare/main...HEAD
