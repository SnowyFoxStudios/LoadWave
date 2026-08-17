// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

// Command loadwave is the standalone LoadWave binary.
//
// It carries no compiled-in scenarios, so it runs tests written declaratively
// in YAML. A test that needs real logic — conditional flows, custom auth,
// protocols other than HTTP — is written in Go against the SDK instead, and
// that program becomes its own equivalent of this one. See package
// github.com/SnowyFoxStudios/LoadWave/pkg/loadwave/run.
package main

import "github.com/SnowyFoxStudios/LoadWave/pkg/loadwave/run"

func main() { run.Main() }
