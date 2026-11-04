package orchestrator

import (
	"encoding/json"
	"fmt"
	"strings"
)

// checkToolArguments tests a tool call's arguments against the tool's own JSON schema (the
// "parameters" the agent was given) and returns a message for the model when they cannot be right:
// arguments that are not a JSON object, a required field that is missing, null or blank, a value of
// the wrong type, or one outside the field's enum. The empty string means nothing is wrong, and a
// tool with no schema is never refused.
//
// Nothing here knows any tool by name. A model at low reasoning effort sometimes calls a tool with
// every required field blank while it is still missing the caller's answers; running that books
// nothing, but it spends the turn and can run a side effect on nonsense. The caller is not told: the
// message is the tool's result, the model reads it in the same turn and either calls again with what
// the caller already said or asks for what is missing.
func checkToolArguments(tools []Tool, name, arguments string) string {
	schema := toolParameters(tools, name)
	if schema == nil {
		return ""
	}
	args := map[string]interface{}{}
	if s := strings.TrimSpace(arguments); s != "" {
		if err := json.Unmarshal([]byte(s), &args); err != nil {
			return "The tool was not run: its arguments are not a valid JSON object. Call it again with a JSON object."
		}
	}
	props, _ := schema["properties"].(map[string]interface{})

	var missing, wrong []string
	if req, ok := schema["required"].([]interface{}); ok {
		for _, r := range req {
			key, _ := r.(string)
			if key == "" {
				continue
			}
			v, present := args[key]
			if !present || v == nil {
				missing = append(missing, key)
				continue
			}
			if s, isStr := v.(string); isStr && strings.TrimSpace(s) == "" {
				missing = append(missing, key)
			}
		}
	}
	for key, v := range args {
		spec, _ := props[key].(map[string]interface{})
		if spec == nil || v == nil {
			continue
		}
		if t, _ := spec["type"].(string); t != "" && !jsonTypeMatches(t, v) {
			wrong = append(wrong, fmt.Sprintf("%s must be of type %s", key, t))
			continue
		}
		if enum, ok := spec["enum"].([]interface{}); ok && len(enum) > 0 && !inEnum(enum, v) {
			wrong = append(wrong, fmt.Sprintf("%s must be one of %s", key, enumList(enum)))
		}
	}
	if len(missing) == 0 && len(wrong) == 0 {
		return ""
	}
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, "missing or empty required arguments: "+strings.Join(missing, ", "))
	}
	parts = append(parts, wrong...)
	return "The tool was not run: " + strings.Join(parts, "; ") +
		". If the caller has already given you these, call the tool again with them; if not, ask the caller for what is missing. Do not invent values."
}

// toolParameters is the JSON schema of the named tool's parameters, or nil.
func toolParameters(tools []Tool, name string) map[string]interface{} {
	for _, t := range tools {
		raw, err := json.Marshal(t.Function)
		if err != nil {
			continue
		}
		var fn struct {
			Name       string                 `json:"name"`
			Parameters map[string]interface{} `json:"parameters"`
		}
		if json.Unmarshal(raw, &fn) != nil || fn.Name != name {
			continue
		}
		return fn.Parameters
	}
	return nil
}

func jsonTypeMatches(t string, v interface{}) bool {
	switch t {
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == float64(int64(f))
	case "array":
		_, ok := v.([]interface{})
		return ok
	case "object":
		_, ok := v.(map[string]interface{})
		return ok
	}
	return true
}

func inEnum(enum []interface{}, v interface{}) bool {
	for _, e := range enum {
		if fmt.Sprint(e) == fmt.Sprint(v) {
			return true
		}
	}
	return false
}

func enumList(enum []interface{}) string {
	out := make([]string, 0, len(enum))
	for _, e := range enum {
		out = append(out, fmt.Sprint(e))
	}
	return strings.Join(out, ", ")
}
