package router

import (
	"reflect"
	"testing"
)

// metaBatch is the payload shape Meta posts: a wrapper with one entry per
// asset, each entry's id being the WhatsApp Business Account ID.
const metaBatch = `{
  "object": "whatsapp_business_account",
  "entry": [
    {"id": "WABA_ONE", "changes": [{"field": "messages", "value": {"messages": [{"id": "wamid.AAA"}]}}]},
    {"id": "WABA_TWO", "changes": [{"field": "messages", "value": {"messages": [{"id": "wamid.BBB"}]}}]}
  ]
}`

func TestExtractRoutingKeys(t *testing.T) {
	tests := []struct {
		name string
		body string
		path string
		want []string
	}{
		{
			// The headline case: one request carrying two assets yields both
			// keys on a single event, which is what lets one batch match two
			// different subscriptions.
			name: "multi-entry batch yields every id",
			body: metaBatch,
			path: "entry[*].id",
			want: []string{"WABA_ONE", "WABA_TWO"},
		},
		{
			name: "single entry",
			body: `{"entry":[{"id":"ONLY"}]}`,
			path: "entry[*].id",
			want: []string{"ONLY"},
		},
		{
			// The spec writes this form; it must behave like entry[0].id.
			name: "dotted index form",
			body: metaBatch,
			path: "entry.0.id",
			want: []string{"WABA_ONE"},
		},
		{
			name: "bracket index form",
			body: metaBatch,
			path: "entry[0].id",
			want: []string{"WABA_ONE"},
		},
		{
			name: "numeric ids stringify without exponent",
			body: `{"entry":[{"id":102290129340398}]}`,
			path: "entry[*].id",
			want: []string{"102290129340398"},
		},
		{
			name: "duplicate ids are collapsed",
			body: `{"entry":[{"id":"SAME"},{"id":"SAME"}]}`,
			path: "entry[*].id",
			want: []string{"SAME"},
		},
		{
			name: "nested path",
			body: metaBatch,
			path: "entry[*].changes[*].field",
			want: []string{"messages"},
		},
		// The remaining cases must not error: an event that yields no routing
		// key is still persisted and still reaches the default subscription.
		{name: "path matches nothing", body: `{"entry":[]}`, path: "entry[*].id", want: nil},
		{name: "missing field", body: `{"other":1}`, path: "entry[*].id", want: nil},
		{name: "invalid JSON", body: `{not json`, path: "entry[*].id", want: nil},
		{name: "empty path", body: metaBatch, path: "", want: nil},
		{name: "object result is not a scalar", body: `{"entry":[{"id":{"a":1}}]}`, path: "entry[*].id", want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractRoutingKeys([]byte(tt.body), tt.path)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ExtractRoutingKeys() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParsePathRejectsInvalid(t *testing.T) {
	if _, err := ParsePath("entry[[["); err == nil {
		t.Fatal("ParsePath accepted a malformed expression")
	}
}

func TestParsePathCaches(t *testing.T) {
	a, err := ParsePath("entry[*].id")
	if err != nil {
		t.Fatalf("ParsePath: %v", err)
	}
	b, err := ParsePath("entry[*].id")
	if err != nil {
		t.Fatalf("ParsePath: %v", err)
	}
	// Same compiled expression returned twice: parsing happens on the ingest
	// hot path, so the cache is load-bearing.
	if !reflect.DeepEqual(a, b) {
		t.Error("ParsePath returned different expressions for the same input")
	}
}

func TestNormalizePath(t *testing.T) {
	cases := map[string]string{
		"entry.0.id":        "entry[0].id",
		"entry[0].id":       "entry[0].id",
		"entry.0.changes.1": "entry[0].changes[1]",
		"entry[*].id":       "entry[*].id",
		"id":                "id",
	}
	for in, want := range cases {
		if got := normalizePath(in); got != want {
			t.Errorf("normalizePath(%q) = %q, want %q", in, got, want)
		}
	}
}
