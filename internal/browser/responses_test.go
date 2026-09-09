package browser

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"macbridge/internal/core"
)

func responseBody(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if e := json.Unmarshal([]byte(raw), &m); e != nil {
		t.Fatal(e)
	}
	return m
}
func planFor(t *testing.T, tools string) *responsePlan {
	t.Helper()
	p, e := prepareResponse(responseBody(t, `{"model":"chatgpt-browser","input":"Hello","tools":`+tools+`}`))
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func responseCode(t *testing.T, e error, want string, status int) {
	t.Helper()
	var f *ResponsesError
	if !errors.As(e, &f) || f.Code != want || f.HTTPStatus() != status || f.ErrorCode() != want {
		t.Fatalf("error=%#v, wanted %s/%d", e, want, status)
	}
}

func TestResponsesNormalizeCatalogAndHistory(t *testing.T) {
	body := responseBody(t, `{"model":"chatgpt-runtime/chatgpt-sol","reasoning":{"effort":"xhigh"},"instructions":"Solve this task.","input":[
 {"type":"message","role":"developer","content":[{"type":"input_text","text":"first"},{"type":"text","text":"second"}]},
 {"type":"function_call","namespace":"files","name":"read","call_id":"call1","arguments":"{}"},
 {"type":"function_call_output","call_id":"call1","output":{"result":1}},
 {"type":"custom_tool_call","name":"apply","call_id":"call2","input":"program"},
 {"type":"custom_tool_call_output","call_id":"call2","output":"done"},
 {"type":"reasoning","summary":[{"text":"summary"}]}],
 "tools":[{"type":"web_search"},{"type":"namespace","name":"files","tools":[{"type":"function","name":"read","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}]},{"type":"custom","name":"apply","format":{"type":"grammar","syntax":"lark","definition":"start: /./"}}]}`)
	p, e := prepareResponse(body)
	if e != nil {
		t.Fatal(e)
	}
	if p.model != "chatgpt-sol" || p.browserModel != "gpt-5-6-thinking" || p.effort != "max" || len(p.tools) != 2 {
		t.Fatalf("plan=%#v", p)
	}
	for _, fragment := range []string{"firstsecond", "files__read", "function_call_output", "custom_tool_call_output", "summary", "grammar", "<END_OF_TURN_DATA>"} {
		if !strings.Contains(p.prompt, fragment) {
			t.Fatalf("prompt omitted %q", fragment)
		}
	}
	d, e := parseResponseDecision(`{"kind":"tool_calls","calls":[{"name":"files__read","arguments":{"path":"/tmp/a"}},{"name":"apply","input":"program"}]}`, p)
	if e != nil {
		t.Fatal(e)
	}
	response := buildResponse(p, d)
	out := response["output"].([]map[string]any)
	if len(out) != 2 || out[0]["name"] != "read" || out[0]["namespace"] != "files" || out[1]["type"] != "custom_tool_call" {
		t.Fatalf("outputs=%#v", out)
	}
	if response["model"] != "chatgpt-sol" || response["status"] != "completed" {
		t.Fatalf("response=%#v", response)
	}
	for effort, want := range map[string]string{"low": "low", "medium": "standard", "high": "high", "xhigh": "max", "max": "max", "ultra": "max"} {
		body["reasoning"] = map[string]any{"effort": effort}
		p, e = prepareResponse(body)
		if e != nil || p.effort != want {
			t.Fatalf("reasoning %s: %#v %v", effort, p, e)
		}
	}
}
func TestResponsesDecisionValidationAndRecovery(t *testing.T) {
	p := planFor(t, `[{"type":"function","name":"exec","parameters":{"type":"object","properties":{"input":{"type":"string"}},"required":["input"],"additionalProperties":false}},{"type":"custom","name":"patch"}]`)
	for _, text := range []string{
		`{"kind":"tool_calls","calls":[{"name":"exec","input":"text(7);"}]}`,
		`{"kind":"tool_calls","calls":[{"name":"exec","input":"text("ok");\nconst r=/\d/;"}]}`,
		`{"kind":"tool_calls","calls":[{"name":"patch","arguments":{"input":"text("ok");\nconst r=/\d/;"}}]}`,
	} {
		d, e := parseResponseDecision(text, p)
		if e != nil {
			t.Fatalf("program recovery failed: %v", e)
		}
		raw, _ := json.Marshal(d.calls)
		if !strings.Contains(string(raw), "text") {
			t.Fatalf("program lost: %s", raw)
		}
	}
	recovered, e := parseResponseDecision(`{"kind":"tool_calls","calls":[{"name":"patch","input":"text("ok");\nconst r=/\d/;"}]}`, p)
	if e != nil || recovered.calls[0]["input"] != "text(\"ok\");\nconst r=/\\d/;" {
		t.Fatalf("raw program changed: %#v %v", recovered, e)
	}
	for _, text := range []string{
		`{"kind":"tool_calls","calls":[{"name":"exec","input":"text("a");"},{"name":"exec","input":"text("b");"}]}`,
		`{"kind":"tool_calls","calls":[{"name":"exec","input":"text("a");","other":"field"}]}`,
		`{"kind":"tool_calls","calls":`,
	} {
		_, e := parseResponseDecision(text, p)
		responseCode(t, e, "chatgpt_responses_malformed_envelope", 502)
	}
	_, e = parseResponseDecision(`{"kind":"tool_calls","calls":[{"name":"unknown","arguments":{}}]}`, p)
	responseCode(t, e, "chatgpt_responses_unknown_tool", 502)
	_, e = parseResponseDecision(`{"kind":"tool_calls","calls":[{"name":"exec","input":"a","arguments":{"input":"b"}}]}`, p)
	responseCode(t, e, "chatgpt_responses_malformed_tool_call", 502)
	p.parallel = false
	_, e = parseResponseDecision(`{"kind":"tool_calls","calls":[{"name":"patch","input":"a"},{"name":"patch","input":"b"}]}`, p)
	responseCode(t, e, "chatgpt_responses_tool_call_limit", 502)
	for _, text := range []string{"plain final response", "```json\n{\"kind\":\"message\",\"text\":\"plain final response\"}\n```", "Some framing {\"kind\":\"message\",\"text\":\"plain final response\"} after"} {
		d, e := parseResponseDecision(text, p)
		if e != nil || !d.message || d.text != "plain final response" {
			t.Fatalf("message=%#v error=%v", d, e)
		}
	}
}
func TestResponsesRequestLimits(t *testing.T) {
	for _, tc := range []struct {
		body, code string
		status     int
	}{
		{`{"model":"missing"}`, "model_not_found", 404},
		{`{"model":"chatgpt-sol","reasoning":{"effort":"extreme"}}`, "chatgpt_responses_unsupported_reasoning_effort", 400},
		{`{"model":"chatgpt-browser","previous_response_id":"old"}`, "chatgpt_responses_stateful_request", 400},
		{`{"model":"chatgpt-browser","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"x"}]}]}`, "chatgpt_responses_unsupported_content", 400},
		{`{"model":"chatgpt-browser","tools":[{"type":"computer_use_preview"}]}`, "chatgpt_responses_unsupported_tool", 400},
		{`{"model":"chatgpt-browser","tools":[{"type":"function","name":"same"},{"type":"custom","name":"same"}]}`, "chatgpt_responses_invalid_request", 400},
	} {
		_, e := prepareResponse(responseBody(t, tc.body))
		responseCode(t, e, tc.code, tc.status)
	}
	_, e := prepareResponse(map[string]any{"model": "chatgpt-browser", "input": strings.Repeat("x", responsePromptLimit)})
	responseCode(t, e, "chatgpt_responses_prompt_limit", 413)
	var tools []any
	for i := 0; i < 129; i++ {
		tools = append(tools, map[string]any{"type": "function", "name": core.ID()})
	}
	_, e = prepareResponse(map[string]any{"model": "chatgpt-browser", "tools": tools})
	responseCode(t, e, "chatgpt_responses_tool_limit", 413)
}
func TestResponsesProjectAndRuntimeDispatch(t *testing.T) {
	dir := t.TempDir()
	cfg := core.Config{DataDir: dir}
	file := filepath.Join(dir, "chatgpt-runtime.json")
	if e := os.WriteFile(file, []byte(`{"projectId":"g-p-project123"}`), 0600); e != nil {
		t.Fatal(e)
	}
	request := map[string]any{"model": "chatgpt-runtime/chatgpt-sol", "input": "hi", "reasoning": map[string]any{"effort": "high"}}
	calls := 0
	call := func(ctx context.Context, name string, args map[string]any) (any, error) {
		calls++
		if name != "chatgpt_conversation_start" || args["project_id"] != "g-p-project123" || args["thinking_effort"] != "high" || args["model"] != "gpt-5-6-thinking" || args["continue_in_work"] != false || args["transport"] != "runtime" {
			t.Fatalf("dispatch arguments=%#v", args)
		}
		return map[string]any{"ok": true, "complete": true, "assistant_text": `{"kind":"message","text":"done"}`}, nil
	}
	response, e := runResponses(context.Background(), request, cfg, call)
	if e != nil || calls != 1 {
		t.Fatalf("response=%#v error=%v calls=%d", response, e, calls)
	}
	if response["output"].([]map[string]any)[0]["content"].([]map[string]any)[0]["text"] != "done" {
		t.Fatalf("final text lost: %#v", response)
	}
	if e = os.Chmod(file, 0644); e != nil {
		t.Fatal(e)
	}
	e = New(cfg).ValidateResponses(request)
	responseCode(t, e, "chatgpt_responses_project_config_permissions", 500)
	if calls != 1 {
		t.Fatal("validation invoked browser")
	}
	t.Setenv("MAC_DEV_BRIDGE_CHATGPT_RESPONSES_PROJECT_ID", "g-p-envproject")
	if project, e := responseProject(cfg); e != nil || project != "g-p-envproject" {
		t.Fatalf("project=%q err=%v", project, e)
	}
	_, e = runResponses(context.Background(), request, cfg, func(context.Context, string, map[string]any) (any, error) {
		return map[string]any{"ok": false, "error": map[string]any{"code": "CHATGPT_RUNTIME_CONTRACT_CHANGED", "message": "contract changed"}}, nil
	})
	responseCode(t, e, "CHATGPT_RUNTIME_CONTRACT_CHANGED", 424)
}
