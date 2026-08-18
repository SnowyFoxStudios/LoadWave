// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package buildinfo reports what this binary is.
//
// Every node advertises its version when it joins, so an operator can see at a
// glance that one agent in a fleet is running last week's build — a genuinely
// common cause of results that make no sense.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
)

// Values stamped in at link time by the release build:
//
//	-ldflags "-X github.com/SnowyFoxStudios/LoadWave/internal/buildinfo.version=v1.2.3 ..."
//
// A `go install`ed or `go run` binary leaves them empty, and the values are
// recovered from the module's embedded VCS metadata instead.
var (
	version = ""
	commit  = ""
	date    = ""
)

var resolved = sync.OnceValue(func() Info {
	info := Info{
		Version:   version,
		Commit:    commit,
		BuildDate: date,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}

	build, ok := debug.ReadBuildInfo()
	if !ok {
		return info.withDefaults()
	}

	if info.Version == "" && build.Main.Version != "" && build.Main.Version != "(devel)" {
		info.Version = build.Main.Version
	}
	for _, setting := range build.Settings {
		switch setting.Key {
		case "vcs.revision":
			if info.Commit == "" {
				info.Commit = setting.Value
			}
		case "vcs.time":
			if info.BuildDate == "" {
				info.BuildDate = setting.Value
			}
		case "vcs.modified":
			if setting.Value == "true" {
				info.Dirty = true
			}
		}
	}
	return info.withDefaults()
})

// Info describes the running binary.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"buildDate"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
	Dirty     bool   `json:"dirty"`
}

func (i Info) withDefaults() Info {
	if i.Version == "" {
		i.Version = "dev"
	}
	return i
}

// Get returns the build information.
func Get() Info { return resolved() }

// Version returns just the version string.
func Version() string { return resolved().Version }

// String renders a one-line summary for `--version` output.
func (i Info) String() string {
	var b strings.Builder
	b.WriteString("loadwave ")
	b.WriteString(i.Version)

	if i.Commit != "" {
		short := i.Commit
		if len(short) > 12 {
			short = short[:12]
		}
		fmt.Fprintf(&b, " (%s", short)
		if i.Dirty {
			b.WriteString("-dirty")
		}
		b.WriteString(")")
	}
	fmt.Fprintf(&b, " %s %s", i.GoVersion, i.Platform)
	if i.BuildDate != "" {
		fmt.Fprintf(&b, " built %s", i.BuildDate)
	}
	return b.String()
}
