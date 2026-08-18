// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
)

// Template is a string with ${name} placeholders, parsed once and rendered
// once per use.
//
// Declarative steps render their URL, headers and body on every iteration —
// tens of thousands of times a second across a fleet — so the parse is done
// when the scenario is built rather than in the hot path, and a template with
// no placeholders costs nothing at all to render.
type Template struct {
	// literals and names interleave: literals[0], names[0], literals[1], ...
	// with one more literal than name.
	literals []string
	names    []string
	static   bool
}

// ParseTemplate compiles a template string.
func ParseTemplate(s string) (*Template, error) {
	if !strings.Contains(s, "${") {
		return &Template{literals: []string{s}, static: true}, nil
	}

	t := &Template{}
	rest := s
	for {
		open := strings.Index(rest, "${")
		if open < 0 {
			t.literals = append(t.literals, rest)
			break
		}
		end := strings.Index(rest[open:], "}")
		if end < 0 {
			return nil, fmt.Errorf("unterminated ${ in %q", s)
		}
		end += open

		name := strings.TrimSpace(rest[open+2 : end])
		if name == "" {
			return nil, fmt.Errorf("empty placeholder in %q", s)
		}

		t.literals = append(t.literals, rest[:open])
		t.names = append(t.names, name)
		rest = rest[end+1:]
	}
	return t, nil
}

// MustParseTemplate is ParseTemplate for templates known good at build time.
func MustParseTemplate(s string) *Template {
	t, err := ParseTemplate(s)
	if err != nil {
		panic(err)
	}
	return t
}

// Render substitutes the placeholders. Unknown names render as empty strings
// rather than failing: a capture that did not fire should produce a request
// that visibly misses, not an iteration that dies before making one.
func (t *Template) Render(vars *Vars) string {
	if t.static {
		return t.literals[0]
	}

	var b strings.Builder
	for i, literal := range t.literals {
		b.WriteString(literal)
		if i < len(t.names) {
			b.WriteString(vars.Get(t.names[i]))
		}
	}
	return b.String()
}

// IsStatic reports whether the template has no placeholders.
func (t *Template) IsStatic() bool { return t.static }

// String returns the original template text.
func (t *Template) String() string {
	if t.static {
		return t.literals[0]
	}
	var b strings.Builder
	for i, literal := range t.literals {
		b.WriteString(literal)
		if i < len(t.names) {
			b.WriteString("${")
			b.WriteString(t.names[i])
			b.WriteString("}")
		}
	}
	return b.String()
}

// Vars holds one iteration's variables: those declared on the scenario, those
// captured from earlier responses, and a handful of built-ins.
type Vars struct {
	values map[string]string
	vu     *loadwave.VU
}

// NewVars creates a variable scope seeded with the scenario's declarations.
func NewVars(vu *loadwave.VU, declared map[string]string) *Vars {
	values := make(map[string]string, len(declared)+4)
	for k, v := range declared {
		values[k] = v
	}
	return &Vars{values: values, vu: vu}
}

// Set records a captured value.
func (v *Vars) Set(name, value string) { v.values[name] = value }

// Built-in variable names, all prefixed so they cannot collide with a user's
// own declarations.
const (
	VarVUID      = "__vu"
	VarIteration = "__iteration"
	VarShard     = "__shard"
	VarShards    = "__shards"
	VarRandom    = "__random"
	VarUUID      = "__uuid"
	VarTimestamp = "__timestamp"
	VarUnixMilli = "__unixMilli"
)

// Get resolves a variable, checking user values before built-ins so a
// scenario can shadow one deliberately.
func (v *Vars) Get(name string) string {
	if value, ok := v.values[name]; ok {
		return value
	}
	if v.vu == nil {
		return ""
	}

	switch name {
	case VarVUID:
		return strconv.FormatInt(v.vu.ID(), 10)
	case VarIteration:
		return strconv.Itoa(v.vu.Iteration())
	case VarShard:
		return strconv.FormatUint(uint64(v.vu.Shard().Index), 10)
	case VarShards:
		return strconv.FormatUint(uint64(v.vu.Shard().Count), 10)
	case VarRandom:
		return strconv.FormatInt(v.vu.Rand().Int64(), 10)
	case VarUUID:
		return newUUIDv4()
	case VarTimestamp:
		return time.Now().UTC().Format(time.RFC3339)
	case VarUnixMilli:
		return strconv.FormatInt(time.Now().UnixMilli(), 10)
	default:
		return ""
	}
}

// newUUIDv4 generates a random UUID.
//
// Written out rather than pulled in as a dependency: this is the only place
// LoadWave needs one, and crypto/rand never fails on any platform Go supports.
func newUUIDv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10

	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}
