// Package router evaluates JSONPath expressions against webhook payloads and
// matches events to subscriptions.
package router

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/ohler55/ojg/jp"
	"github.com/ohler55/ojg/oj"
)

// pathCache memoises parsed JSONPath expressions. Listener paths are fixed
// configuration evaluated on every inbound webhook, so parsing once matters on
// the ingest hot path.
var pathCache sync.Map // string -> jp.Expr

// ParsePath compiles a JSONPath expression, caching the result.
//
// Dotted paths are accepted as a convenience: the spec's example `entry.0.id`
// is rewritten to `entry[0].id` so operators can write either form.
func ParsePath(path string) (jp.Expr, error) {
	if cached, ok := pathCache.Load(path); ok {
		return cached.(jp.Expr), nil
	}
	expr, err := jp.ParseString(normalizePath(path))
	if err != nil {
		return nil, fmt.Errorf("invalid JSONPath %q: %w", path, err)
	}
	pathCache.Store(path, expr)
	return expr, nil
}

// normalizePath rewrites bare numeric segments into bracket indices, so that
// `entry.0.id` and `entry[0].id` are equivalent.
func normalizePath(path string) string {
	if !strings.Contains(path, ".") {
		return path
	}
	parts := strings.Split(path, ".")
	var b strings.Builder
	for i, p := range parts {
		if _, err := strconv.Atoi(p); err == nil && i > 0 {
			b.WriteString("[" + p + "]")
			continue
		}
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(p)
	}
	return b.String()
}

// ParseJSON decodes a payload for path evaluation. It is only ever used on a
// copy of the body for extraction and filtering; the stored and forwarded bytes
// are always the originals.
func ParseJSON(body []byte) (any, error) {
	v, err := oj.Parse(body)
	if err != nil {
		return nil, fmt.Errorf("parse JSON payload: %w", err)
	}
	return v, nil
}

// ExtractStrings evaluates path against doc and returns every scalar result as
// a string, deduplicated and order-preserving.
//
// A Meta batch carries one entry per asset, so `entry[*].id` yields every
// WhatsApp Business Account ID in the request. The event keeps all of them.
func ExtractStrings(doc any, expr jp.Expr) []string {
	results := expr.Get(doc)
	if len(results) == 0 {
		return nil
	}
	out := make([]string, 0, len(results))
	seen := make(map[string]struct{}, len(results))
	for _, r := range results {
		s, ok := scalarString(r)
		if !ok {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// scalarString converts a JSON scalar to its string form. Objects and arrays
// are rejected: a routing key must be a single value.
func scalarString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case int64:
		return strconv.FormatInt(t, 10), true
	case int:
		return strconv.Itoa(t), true
	case float64:
		// Meta sends numeric ids that must not pick up an exponent or a
		// trailing ".0" when stringified.
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(t), true
	default:
		return "", false
	}
}

// ExtractRoutingKeys parses body and evaluates path, returning every routing
// key found. A payload that is not valid JSON, or a path that matches nothing,
// yields no keys and no error: such an event is still persisted and still
// reaches the listener's default subscription.
func ExtractRoutingKeys(body []byte, path string) []string {
	if path == "" {
		return nil
	}
	expr, err := ParsePath(path)
	if err != nil {
		return nil
	}
	doc, err := ParseJSON(body)
	if err != nil {
		return nil
	}
	return ExtractStrings(doc, expr)
}
