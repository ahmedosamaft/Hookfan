package router

import (
	"encoding/json"
	"reflect"
	"testing"
)

func mustDoc(t *testing.T, s string) any {
	t.Helper()
	doc, err := ParseJSON([]byte(s))
	if err != nil {
		t.Fatalf("ParseJSON: %v", err)
	}
	return doc
}

func TestMatchFilterTypes(t *testing.T) {
	doc := mustDoc(t, `{"object":"whatsapp_business_account","entry":[{"id":"WABA_ONE","changes":[{"field":"messages"}]}]}`)

	tests := []struct {
		name string
		sub  Subscription
		keys []string
		want bool
	}{
		{"all matches anything", Subscription{FilterType: FilterAll}, []string{"X"}, true},
		{"all matches with no keys", Subscription{FilterType: FilterAll}, nil, true},

		{
			name: "routing_key_in hit",
			sub:  Subscription{FilterType: FilterRoutingKeyIn, RoutingKeys: []string{"WABA_ONE"}},
			keys: []string{"WABA_ONE"}, want: true,
		},
		{
			name: "routing_key_in miss",
			sub:  Subscription{FilterType: FilterRoutingKeyIn, RoutingKeys: []string{"OTHER"}},
			keys: []string{"WABA_ONE"}, want: false,
		},
		{
			// Any-overlap: a batch carrying several assets reaches a subscriber
			// interested in any one of them.
			name: "routing_key_in overlaps a multi-key batch",
			sub:  Subscription{FilterType: FilterRoutingKeyIn, RoutingKeys: []string{"WABA_TWO"}},
			keys: []string{"WABA_ONE", "WABA_TWO"}, want: true,
		},
		{
			name: "routing_key_in with no event keys",
			sub:  Subscription{FilterType: FilterRoutingKeyIn, RoutingKeys: []string{"WABA_ONE"}},
			keys: nil, want: false,
		},

		{
			name: "jsonpath eq hit",
			sub: Subscription{FilterType: FilterJSONPathMatch, FilterExpr: []Condition{
				{Path: "object", Op: OpEq, Value: "whatsapp_business_account"}}},
			want: true,
		},
		{
			name: "jsonpath eq miss",
			sub: Subscription{FilterType: FilterJSONPathMatch, FilterExpr: []Condition{
				{Path: "object", Op: OpEq, Value: "instagram"}}},
			want: false,
		},
		{
			// Conditions are ANDed.
			name: "jsonpath all conditions must hold",
			sub: Subscription{FilterType: FilterJSONPathMatch, FilterExpr: []Condition{
				{Path: "object", Op: OpEq, Value: "whatsapp_business_account"},
				{Path: "entry[*].id", Op: OpEq, Value: "NOPE"}}},
			want: false,
		},
		{
			name: "jsonpath neq on absent field is true",
			sub: Subscription{FilterType: FilterJSONPathMatch, FilterExpr: []Condition{
				{Path: "missing", Op: OpNeq, Value: "x"}}},
			want: true,
		},
		{
			name: "jsonpath in",
			sub: Subscription{FilterType: FilterJSONPathMatch, FilterExpr: []Condition{
				{Path: "entry[*].id", Op: OpIn, Value: []any{"A", "WABA_ONE"}}}},
			want: true,
		},
		{
			name: "jsonpath exists",
			sub: Subscription{FilterType: FilterJSONPathMatch, FilterExpr: []Condition{
				{Path: "entry[*].changes", Op: OpExists}}},
			want: true,
		},
		{
			name: "jsonpath exists false on absent field",
			sub: Subscription{FilterType: FilterJSONPathMatch, FilterExpr: []Condition{
				{Path: "nope", Op: OpExists, Value: false}}},
			want: true,
		},
		{
			name: "jsonpath contains substring",
			sub: Subscription{FilterType: FilterJSONPathMatch, FilterExpr: []Condition{
				{Path: "object", Op: OpContains, Value: "whatsapp"}}},
			want: true,
		},
		{
			name: "empty filter_expr matches nothing",
			sub:  Subscription{FilterType: FilterJSONPathMatch},
			want: false,
		},
		{
			// An unknown type must never behave like "all".
			name: "unknown filter type matches nothing",
			sub:  Subscription{FilterType: "sometime-later"},
			keys: []string{"WABA_ONE"}, want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Match(tt.sub, tt.keys, doc); got != tt.want {
				t.Errorf("Match() = %v, want %v", got, tt.want)
			}
		})
	}
}

// The core guarantee: one delivery per service, no matter how many
// subscriptions or routing keys matched.
func TestMatchAllDeduplicatesByService(t *testing.T) {
	subs := []Subscription{
		{ID: 1, ServiceID: 100, FilterType: FilterRoutingKeyIn, RoutingKeys: []string{"WABA_ONE"}},
		{ID: 2, ServiceID: 100, FilterType: FilterRoutingKeyIn, RoutingKeys: []string{"WABA_TWO"}},
		{ID: 3, ServiceID: 200, FilterType: FilterAll},
	}
	// A batch carrying both assets: service 100 matches through two separate
	// subscriptions and must still receive exactly one delivery.
	result := MatchAll(subs, []string{"WABA_ONE", "WABA_TWO"}, nil)

	if len(result.ServiceIDs) != 2 {
		t.Fatalf("ServiceIDs = %v, want exactly 2 services", result.ServiceIDs)
	}
	want := []int64{100, 200}
	if !reflect.DeepEqual(result.ServiceIDs, want) {
		t.Errorf("ServiceIDs = %v, want %v", result.ServiceIDs, want)
	}
	// Provenance is preserved: both subscriptions are recorded.
	if got := result.SubscriptionsByService[100]; !reflect.DeepEqual(got, []int64{1, 2}) {
		t.Errorf("service 100 matched by %v, want [1 2]", got)
	}
	if result.UsedDefault {
		t.Error("UsedDefault = true, want false when real subscriptions matched")
	}
}

func TestMatchAllDefaultFallback(t *testing.T) {
	subs := []Subscription{
		{ID: 1, ServiceID: 100, FilterType: FilterRoutingKeyIn, RoutingKeys: []string{"OTHER"}},
		{ID: 2, ServiceID: 999, FilterType: FilterRoutingKeyIn, RoutingKeys: []string{"NOBODY"}, IsDefault: true},
	}

	t.Run("nothing matches so default receives it", func(t *testing.T) {
		result := MatchAll(subs, []string{"UNKNOWN_ASSET"}, nil)
		if !result.UsedDefault {
			t.Error("UsedDefault = false, want true")
		}
		if !reflect.DeepEqual(result.ServiceIDs, []int64{999}) {
			t.Errorf("ServiceIDs = %v, want [999]", result.ServiceIDs)
		}
	})

	t.Run("default is not used when something matched", func(t *testing.T) {
		result := MatchAll(subs, []string{"OTHER"}, nil)
		if result.UsedDefault {
			t.Error("UsedDefault = true, want false")
		}
		if !reflect.DeepEqual(result.ServiceIDs, []int64{100}) {
			t.Errorf("ServiceIDs = %v, want [100]", result.ServiceIDs)
		}
	})

	t.Run("no default and no match yields nothing", func(t *testing.T) {
		result := MatchAll(subs[:1], []string{"UNKNOWN"}, nil)
		if len(result.ServiceIDs) != 0 {
			t.Errorf("ServiceIDs = %v, want empty", result.ServiceIDs)
		}
	})
}

// A default subscription that also matches on its own merits is counted once.
func TestMatchAllDefaultThatAlsoMatches(t *testing.T) {
	subs := []Subscription{
		{ID: 1, ServiceID: 100, FilterType: FilterAll, IsDefault: true},
	}
	result := MatchAll(subs, []string{"ANY"}, nil)

	if len(result.ServiceIDs) != 1 {
		t.Fatalf("ServiceIDs = %v, want 1", result.ServiceIDs)
	}
	if got := result.SubscriptionsByService[100]; len(got) != 1 {
		t.Errorf("subscription recorded %d times, want 1", len(got))
	}
	if result.UsedDefault {
		t.Error("UsedDefault = true, but the subscription matched on its own merits")
	}
}

func TestParseConditions(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{"valid eq", `[{"path":"object","op":"eq","value":"x"}]`, ""},
		{"valid in", `[{"path":"a","op":"in","value":["x","y"]}]`, ""},
		{"valid exists", `[{"path":"a","op":"exists"}]`, ""},
		{"empty array", `[]`, ""},
		{"missing path", `[{"op":"eq","value":"x"}]`, "path is required"},
		{"bad op", `[{"path":"a","op":"regex","value":"x"}]`, "op must be one of"},
		{"in requires array", `[{"path":"a","op":"in","value":"x"}]`, `requires value to be an array`},
		{"invalid path", `[{"path":"a[[[","op":"eq","value":"x"}]`, "invalid JSONPath"},
		{"not an array", `{"path":"a"}`, "must be a JSON array"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseConditions(json.RawMessage(tt.raw))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ParseConditions() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ParseConditions() error = nil, want %q", tt.wantErr)
			}
			if !contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// Numeric values must compare correctly regardless of how the JSON decoder
// typed them.
func TestValuesEqualAcrossNumericTypes(t *testing.T) {
	doc := mustDoc(t, `{"n":102290129340398,"f":1.5}`)
	sub := Subscription{FilterType: FilterJSONPathMatch, FilterExpr: []Condition{
		{Path: "n", Op: OpEq, Value: float64(102290129340398)}}}
	if !Match(sub, nil, doc) {
		t.Error("large integer did not compare equal")
	}

	sub.FilterExpr = []Condition{{Path: "n", Op: OpEq, Value: "102290129340398"}}
	if !Match(sub, nil, doc) {
		t.Error("integer did not compare equal to its string form")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
