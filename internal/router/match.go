package router

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Filter types.
const (
	FilterAll           = "all"
	FilterRoutingKeyIn  = "routing_key_in"
	FilterJSONPathMatch = "jsonpath_match"
)

// Comparison operators available in a jsonpath_match filter.
const (
	OpEq       = "eq"
	OpNeq      = "neq"
	OpIn       = "in"
	OpExists   = "exists"
	OpContains = "contains"
)

// Condition is one clause of a jsonpath_match filter.
type Condition struct {
	Path  string `json:"path"`
	Op    string `json:"op"`
	Value any    `json:"value,omitempty"`
}

// Subscription is the subset of a stored subscription that matching needs.
// The store type is not used directly so this package stays free of database
// concerns and is trivially testable.
type Subscription struct {
	ID          int64
	ServiceID   int64
	FilterType  string
	RoutingKeys []string
	FilterExpr  []Condition
	IsDefault   bool
}

// Match reports whether a subscription accepts an event.
//
// Conditions are ANDed: every clause must hold. Default subscriptions are not
// evaluated here — they are a fallback applied by MatchAll when nothing else
// matched.
func Match(sub Subscription, routingKeys []string, doc any) bool {
	switch sub.FilterType {
	case FilterAll:
		return true

	case FilterRoutingKeyIn:
		// Any-overlap: a batch carrying several assets reaches a subscriber
		// interested in any one of them.
		return overlaps(routingKeys, sub.RoutingKeys)

	case FilterJSONPathMatch:
		if len(sub.FilterExpr) == 0 {
			return false
		}
		for _, cond := range sub.FilterExpr {
			if !evalCondition(cond, doc) {
				return false
			}
		}
		return true

	default:
		// An unknown filter type must not silently match everything.
		return false
	}
}

// MatchResult is the outcome of matching one event against a listener's
// subscriptions.
type MatchResult struct {
	// ServiceIDs is the deduplicated set of services to deliver to, in a
	// stable order.
	ServiceIDs []int64
	// SubscriptionsByService records which subscriptions caused each delivery,
	// so an operator can answer "why did this service receive this event?".
	SubscriptionsByService map[int64][]int64
	// UsedDefault reports that nothing matched and the fallback was applied.
	UsedDefault bool
}

// MatchAll evaluates every subscription for a listener and collapses the
// result to one delivery per service.
//
// Deduplication is the important part: a service subscribed to two different
// routing keys that both appear in one batch must receive exactly one POST,
// not two identical ones.
func MatchAll(subs []Subscription, routingKeys []string, doc any) MatchResult {
	result := MatchResult{SubscriptionsByService: map[int64][]int64{}}

	var defaults []Subscription
	for _, sub := range subs {
		if sub.IsDefault {
			defaults = append(defaults, sub)
			// A default subscription may also carry a filter; if it matches on
			// its own merits it is treated as a normal match below.
		}
		if !Match(sub, routingKeys, doc) {
			continue
		}
		result.addMatch(sub)
	}

	// Fail safe: an event that matched nothing goes to the default
	// subscription rather than being dropped.
	if len(result.ServiceIDs) == 0 && len(defaults) > 0 {
		result.UsedDefault = true
		for _, sub := range defaults {
			result.addMatch(sub)
		}
	}
	return result
}

func (r *MatchResult) addMatch(sub Subscription) {
	if _, seen := r.SubscriptionsByService[sub.ServiceID]; !seen {
		r.ServiceIDs = append(r.ServiceIDs, sub.ServiceID)
	}
	r.SubscriptionsByService[sub.ServiceID] =
		append(r.SubscriptionsByService[sub.ServiceID], sub.ID)
}

// overlaps reports whether the two sets share any element, mirroring the
// Postgres && operator used when matching in SQL.
func overlaps(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	set := make(map[string]struct{}, len(b))
	for _, v := range b {
		set[v] = struct{}{}
	}
	for _, v := range a {
		if _, ok := set[v]; ok {
			return true
		}
	}
	return false
}

// evalCondition applies one clause to the payload.
func evalCondition(cond Condition, doc any) bool {
	if doc == nil || cond.Path == "" {
		return false
	}
	expr, err := ParsePath(cond.Path)
	if err != nil {
		return false
	}
	found := expr.Get(doc)

	switch cond.Op {
	case OpExists:
		// A bare `exists` means "present"; `{"op":"exists","value":false}`
		// inverts it.
		want := true
		if b, ok := cond.Value.(bool); ok {
			want = b
		}
		return (len(found) > 0) == want

	case OpEq:
		return anyValue(found, func(v any) bool { return valuesEqual(v, cond.Value) })

	case OpNeq:
		// Absent counts as not-equal, which is what an operator expects from
		// "field is not X".
		if len(found) == 0 {
			return true
		}
		return !anyValue(found, func(v any) bool { return valuesEqual(v, cond.Value) })

	case OpIn:
		list, ok := cond.Value.([]any)
		if !ok {
			return false
		}
		return anyValue(found, func(v any) bool {
			for _, want := range list {
				if valuesEqual(v, want) {
					return true
				}
			}
			return false
		})

	case OpContains:
		needle, ok := stringify(cond.Value)
		if !ok {
			return false
		}
		return anyValue(found, func(v any) bool {
			// Substring for strings, membership for arrays.
			if arr, isArr := v.([]any); isArr {
				for _, item := range arr {
					if s, ok := stringify(item); ok && s == needle {
						return true
					}
				}
				return false
			}
			s, ok := stringify(v)
			return ok && strings.Contains(s, needle)
		})

	default:
		return false
	}
}

func anyValue(values []any, pred func(any) bool) bool {
	for _, v := range values {
		if pred(v) {
			return true
		}
	}
	return false
}

// valuesEqual compares a payload value with a filter value. JSON numbers
// arrive as different Go types depending on the decoder, so numeric
// comparison goes through a canonical string form.
func valuesEqual(got, want any) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	gs, gok := stringify(got)
	ws, wok := stringify(want)
	return gok && wok && gs == ws
}

// stringify renders a JSON scalar canonically. Non-scalars are not comparable.
func stringify(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		return strconv.FormatBool(t), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case int64:
		return strconv.FormatInt(t, 10), true
	case int:
		return strconv.Itoa(t), true
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return t.String(), true
		}
		return strconv.FormatFloat(f, 'f', -1, 64), true
	default:
		return "", false
	}
}

// ParseConditions decodes and validates a filter_expr document.
func ParseConditions(raw []byte) ([]Condition, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var conds []Condition
	if err := json.Unmarshal(raw, &conds); err != nil {
		return nil, fmt.Errorf("filter_expr must be a JSON array of {path, op, value}: %w", err)
	}
	for i, c := range conds {
		if c.Path == "" {
			return nil, fmt.Errorf("filter_expr[%d]: path is required", i)
		}
		if _, err := ParsePath(c.Path); err != nil {
			return nil, fmt.Errorf("filter_expr[%d]: %w", i, err)
		}
		switch c.Op {
		case OpEq, OpNeq, OpIn, OpExists, OpContains:
		default:
			return nil, fmt.Errorf(
				"filter_expr[%d]: op must be one of eq, neq, in, exists, contains; got %q", i, c.Op)
		}
		if c.Op == OpIn {
			if _, ok := c.Value.([]any); !ok {
				return nil, fmt.Errorf("filter_expr[%d]: op \"in\" requires value to be an array", i)
			}
		}
	}
	return conds, nil
}
