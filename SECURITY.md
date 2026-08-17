# Security policy

## Reporting a vulnerability

Please report privately, through
[GitHub's security advisories](https://github.com/SnowyFoxStudios/LoadWave/security/advisories/new),
rather than opening a public issue.

Include what you can: what an attacker gains, how to reproduce it, and the
version you found it in. You will get an acknowledgement within a few days and
an assessment within two weeks. We will tell you when a fix ships and credit
you in the advisory unless you would rather we did not.

## Supported versions

LoadWave is pre-1.0. Fixes go onto `main` and into the next release; older
releases are not patched. Once 1.0 lands, this section will say something
firmer.

## Threat model

Knowing what LoadWave assumes is as useful as knowing what it defends against.

**The control plane is unauthenticated and unencrypted.** Agents dial the
coordinator over plaintext gRPC and are trusted on connection. Anyone who can
reach the coordinator's agent port can join the fleet, receive test plans —
which may contain credentials — and report fabricated metrics. Anyone who can
reach the dashboard port can start, stop and rescale runs.

Anyone who can reach the dashboard port of a `loadwave run --ui` or
`loadwave demo` process can also end it, through the Power off control. That
is deliberate for those two — the process belongs to whoever is looking at the
dashboard. `loadwave serve` does **not** expose it unless started with
`--allow-shutdown`, because a long-lived coordinator is usually run under a
supervisor and a browser tab should not be able to take it down.

**Run LoadWave on a trusted network.** Put the coordinator behind a VPN, a
private subnet or an authenticating proxy. Do not expose either port to the
internet. Authentication and mutual TLS are on the roadmap; until then this is
the deployment assumption, not an oversight to be discovered later.

**LoadWave generates traffic on purpose.** That is the product. Only point it
at systems you are authorised to test. A distributed run is, mechanically, a
distributed denial of service — the difference is permission.

**Test configurations are executable input.** A configuration can direct
requests anywhere, and a Go scenario is arbitrary code compiled into the
binary. Treat a configuration from an untrusted source exactly as you would
treat a script from one.

**Worker sockets are owner-only.** The agent's Unix control socket is created
with mode `0600`, so local access is limited to the user running the agent.

## What we consider a vulnerability

- Remote code execution, or escaping the intended request-generation behaviour.
- A crash or resource exhaustion reachable from a *target server's* response —
  a malicious response body must not be able to take down the generator.
- Credentials from a test configuration leaking somewhere they should not: a
  log line, an error message, the dashboard.
- Anything that lets a joined agent compromise the coordinator beyond
  submitting false metrics.

## What we do not

- The unauthenticated control plane, documented above.
- Being able to generate load against a host you point it at.
- Resource exhaustion caused by your own configuration — a million virtual
  users on a laptop will not go well, and that is expected.
