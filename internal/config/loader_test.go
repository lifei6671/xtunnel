package config

import (
	"strings"
	"testing"
)

var testSchema = []byte(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["group"],
  "properties": {
    "group": {
      "type": "object",
      "additionalProperties": false,
      "required": ["value", "items", "delay"],
      "properties": {
        "value": {"type": "integer", "minimum": 1, "default": 1},
        "items": {"type": "array", "items": {"type": "string"}, "default": []},
        "delay": {"type": "string", "format": "go-duration", "default": "1s"}
      }
    }
  }
}`)

type testConfig struct {
	Group struct {
		Value int      `json:"value"`
		Items []string `json:"items"`
		Delay Duration `json:"delay"`
	} `json:"group"`
}

func TestLoadPrecedence(t *testing.T) {
	tests := []struct {
		name     string
		options  Options
		expected int
	}{
		{name: "schema default", expected: 1},
		{name: "yaml", options: Options{YAML: []byte("group:\n  value: 2\n")}, expected: 2},
		{
			name: "environment",
			options: Options{
				YAML:        []byte("group:\n  value: 2\n"),
				Environment: []string{"XTUNNEL_GROUP__VALUE=3"},
			},
			expected: 3,
		},
		{
			name: "cli",
			options: Options{
				YAML:        []byte("group:\n  value: 2\n"),
				Environment: []string{"XTUNNEL_GROUP__VALUE=3"},
				CLI:         map[string]string{"group.value": "4"},
			},
			expected: 4,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := Load[testConfig](testSchema, test.options, nil)
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if result.Group.Value != test.expected {
				t.Fatalf("Value = %d, want %d", result.Group.Value, test.expected)
			}
			if result.Group.Delay.String() != "1s" {
				t.Fatalf("Delay = %s, want 1s", result.Group.Delay)
			}
		})
	}
}

func TestLoadRejectsInvalidSources(t *testing.T) {
	tests := []struct {
		name    string
		options Options
		match   string
	}{
		{
			name:    "unknown yaml field",
			options: Options{YAML: []byte("group:\n  unknown: 1\n")},
			match:   "additional properties",
		},
		{
			name:    "duplicate yaml key",
			options: Options{YAML: []byte("group:\n  value: 1\n  value: 2\n")},
			match:   "already defined",
		},
		{
			name:    "multiple yaml documents",
			options: Options{YAML: []byte("group: {}\n---\ngroup: {}\n")},
			match:   "multiple documents",
		},
		{
			name:    "unknown environment variable",
			options: Options{Environment: []string{"XTUNNEL_UNKNOWN=1"}},
			match:   "unknown XTUNNEL environment variable",
		},
		{
			name:    "unknown cli override",
			options: Options{CLI: map[string]string{"group.unknown": "1"}},
			match:   "unknown CLI config override",
		},
		{
			name:    "invalid duration",
			options: Options{CLI: map[string]string{"group.delay": "soon"}},
			match:   "invalid Go duration",
		},
		{
			name:    "integer below schema minimum",
			options: Options{CLI: map[string]string{"group.value": "0"}},
			match:   "minimum",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load[testConfig](testSchema, test.options, nil)
			if err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("Load() error = %v, want substring %q", err, test.match)
			}
		})
	}
}

func TestLoadParsesJSONArrayOverride(t *testing.T) {
	result, err := Load[testConfig](testSchema, Options{
		Environment: []string{`XTUNNEL_GROUP__ITEMS=["a","b"]`},
	}, nil)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := strings.Join(result.Group.Items, ","); got != "a,b" {
		t.Fatalf("Items = %q, want %q", got, "a,b")
	}
}
