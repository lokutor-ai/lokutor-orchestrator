package orchestrator

import (
	"strings"
	"testing"
)

func testTools() []Tool {
	return []Tool{{Type: "function", Function: map[string]interface{}{
		"name": "book",
		"parameters": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name":  map[string]interface{}{"type": "string"},
				"party": map[string]interface{}{"type": "integer"},
				"kind":  map[string]interface{}{"type": "string", "enum": []interface{}{"a", "b"}},
			},
			"required": []interface{}{"name", "party"},
		},
	}}, {Type: "function", Function: map[string]interface{}{"name": "end_call"}}}
}

func TestCheckToolArguments(t *testing.T) {
	cases := []struct {
		name, tool, args string
		want             string // substring of the message, "" = accepted
	}{
		{"all given", "book", `{"name":"Ana","party":2}`, ""},
		{"blank string", "book", `{"name":"  ","party":2}`, "name"},
		{"missing", "book", `{"name":"Ana"}`, "party"},
		{"null", "book", `{"name":"Ana","party":null}`, "party"},
		{"all empty", "book", `{"name":"","party":0,"kind":""}`, "name"},
		{"wrong type", "book", `{"name":"Ana","party":"two"}`, "party must be of type integer"},
		{"enum", "book", `{"name":"Ana","party":2,"kind":"z"}`, "kind must be one of a, b"},
		{"not json", "book", `{oops`, "not a valid JSON object"},
		{"no schema", "end_call", ``, ""},
		{"unknown tool", "nope", `{}`, ""},
		{"zero is a value", "book", `{"name":"Ana","party":0}`, ""},
	}
	for _, c := range cases {
		got := checkToolArguments(testTools(), c.tool, c.args)
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want containing %q", c.name, got, c.want)
		}
	}
}
