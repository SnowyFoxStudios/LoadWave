// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package scenario

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Path is a compiled accessor into a decoded JSON document.
//
// This is deliberately not JSONPath. Declarative scenarios need to pull an id
// out of a response and put it in the next URL; supporting filters, wildcards
// and recursive descent would mean a query language to document, test and
// explain, in exchange for capabilities a load test almost never wants. What
// is supported is field access and array indexing, in either dotted or
// bracketed form:
//
//	id
//	$.data.token
//	items.0.sku
//	items[0].sku
type Path struct {
	steps  []pathStep
	source string
}

// pathStep is one field name or array index.
type pathStep struct {
	field string
	index int
	isIdx bool
}

// ParsePath compiles a capture expression.
func ParsePath(expr string) (*Path, error) {
	trimmed := strings.TrimSpace(expr)
	if trimmed == "" {
		return nil, errors.New("capture path is empty")
	}
	// A leading $ or $. is accepted for familiarity, and ignored.
	trimmed = strings.TrimPrefix(trimmed, "$.")
	trimmed = strings.TrimPrefix(trimmed, "$")
	trimmed = strings.TrimPrefix(trimmed, ".")

	p := &Path{source: expr}
	for _, segment := range splitPath(trimmed) {
		if segment == "" {
			return nil, fmt.Errorf("capture path %q has an empty segment", expr)
		}
		if idx, err := strconv.Atoi(segment); err == nil {
			if idx < 0 {
				return nil, fmt.Errorf("capture path %q has a negative index", expr)
			}
			p.steps = append(p.steps, pathStep{index: idx, isIdx: true})
			continue
		}
		p.steps = append(p.steps, pathStep{field: segment})
	}

	if len(p.steps) == 0 {
		return nil, fmt.Errorf("capture path %q resolves to nothing", expr)
	}
	return p, nil
}

// splitPath breaks a path into segments, treating dots and brackets alike.
func splitPath(expr string) []string {
	replaced := strings.NewReplacer("[", ".", "]", "").Replace(expr)
	parts := strings.Split(replaced, ".")

	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// String returns the original expression.
func (p *Path) String() string { return p.source }

// Extract decodes a JSON document and pulls out the value at this path,
// rendered as a string.
func (p *Path) Extract(data []byte) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("cannot capture %q: response body is empty", p.source)
	}

	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", fmt.Errorf("cannot capture %q: response is not JSON: %w", p.source, err)
	}

	current := doc
	for i, step := range p.steps {
		var err error
		current, err = step.apply(current)
		if err != nil {
			return "", fmt.Errorf("cannot capture %q at segment %d: %w", p.source, i+1, err)
		}
	}
	return stringify(current)
}

// apply walks one step of the path.
func (s pathStep) apply(node any) (any, error) {
	if s.isIdx {
		arr, ok := node.([]any)
		if !ok {
			return nil, fmt.Errorf("expected an array, found %s", jsonKind(node))
		}
		if s.index >= len(arr) {
			return nil, fmt.Errorf("index %d is out of range for an array of %d", s.index, len(arr))
		}
		return arr[s.index], nil
	}

	obj, ok := node.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected an object, found %s", jsonKind(node))
	}
	value, ok := obj[s.field]
	if !ok {
		return nil, fmt.Errorf("no field %q", s.field)
	}
	return value, nil
}

// stringify renders a captured value for substitution into a later request.
func stringify(v any) (string, error) {
	switch typed := v.(type) {
	case nil:
		return "", nil
	case string:
		return typed, nil
	case bool:
		return strconv.FormatBool(typed), nil
	case float64:
		// JSON numbers decode as float64. Rendering an integral value as
		// "42" rather than "42.000000" matters, because it usually goes
		// straight into a URL path.
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10), nil
		}
		return strconv.FormatFloat(typed, 'f', -1, 64), nil
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return "", fmt.Errorf("cannot render captured value: %w", err)
		}
		return string(encoded), nil
	}
}

// jsonKind names a decoded JSON value's type, for error messages.
func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case float64:
		return "a number"
	case string:
		return "a string"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	default:
		return "an unknown value"
	}
}
