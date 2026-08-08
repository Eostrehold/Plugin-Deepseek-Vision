package downstream

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const tinyImage = "data:image/png;base64,AAECAw=="

func TestSanitizedCodexViewImageFixture(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "responses", "08-codex-view-image-reanalysis.json"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Discover(body, Options{AgentReanalysisEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	groups := plan.Groups()
	if len(groups) != 1 || groups[0].Tool == nil || groups[0].Tool.CallID != "call_fixture" || !strings.Contains(groups[0].Tool.Focus, "re-check only alignment") {
		t.Fatalf("fixture groups=%#v", groups)
	}
	if strings.Contains(groups[0].Tool.Focus, "/home/example") {
		t.Fatalf("attachment metadata leaked into VLM focus: %q", groups[0].Tool.Focus)
	}
	out, err := plan.RewriteGroupsText([]string{"fixture-specific layout analysis"})
	if err != nil || !strings.Contains(string(out), "Task-specific visual reanalysis") || strings.Contains(string(out), tinyImage) {
		t.Fatalf("rewritten=%s err=%v", out, err)
	}
}

func TestResponsesViewImageTailToolOutputBecomesActiveReanalysis(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","name":"view_image"}],"input":[` +
		`{"role":"user","content":[{"type":"input_text","text":"Use the current screenshot."},{"type":"input_text","text":"Inspect only the abnormal spacing."}]},` +
		`{"role":"assistant","content":[{"type":"input_text","text":"assistant commentary must not become VLM focus"}]},` +
		`{"type":"function_call","name":"view_image","call_id":"call_1","arguments":"{\"path\":\"/home/user/.codex/attachments/att_1/shot.png\",\"detail\":\"original\"}"},` +
		`{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_image","image_url":"` + tinyImage + `"}]}` +
		`]}`)
	plan, err := Discover(body, Options{AgentReanalysisEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	groups := plan.Groups()
	if len(groups) != 1 || groups[0].Tool == nil {
		t.Fatalf("groups=%#v", groups)
	}
	tool := groups[0].Tool
	if tool.Name != "view_image" || tool.CallID != "call_1" || tool.Focus != "Use the current screenshot.\n\nInspect only the abnormal spacing." || strings.Contains(tool.Focus, "assistant commentary") || tool.Detail != "original" || tool.CacheMode != "refresh" || !tool.Active || !tool.Tail || tool.Location != "input[3]" {
		t.Fatalf("tool=%#v", tool)
	}
	rewritten, err := plan.RewriteGroupsText([]string{"new spacing analysis"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rewritten), "Task-specific visual reanalysis") || !strings.Contains(string(rewritten), "Earlier analyses may still contain complementary details") {
		t.Fatalf("missing reanalysis marker: %s", rewritten)
	}
}

func TestResponsesReanalysisRequiresDeclaredAssociatedTailCall(t *testing.T) {
	tests := []struct {
		name  string
		tools string
		call  string
		extra string
	}{
		{name: "undeclared", tools: `[]`, call: "call_1"},
		{name: "unknown tool", tools: `[{"type":"function","name":"screenshot"}]`, call: "call_1"},
		{name: "unmatched id", tools: `[{"type":"function","name":"view_image"}]`, call: "different"},
		{name: "historical", tools: `[{"type":"function","name":"view_image"}]`, call: "call_1", extra: `,{"role":"user","content":[{"type":"input_text","text":"future request"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			callName := "view_image"
			if tt.name == "unknown tool" {
				callName = "screenshot"
			}
			body := []byte(`{"tools":` + tt.tools + `,"input":[` +
				`{"role":"user","content":[{"type":"input_text","text":"focus"}]},` +
				`{"type":"function_call","name":"` + callName + `","call_id":"call_1","arguments":"{}"},` +
				`{"type":"function_call_output","call_id":"` + tt.call + `","output":[{"type":"input_image","image_url":"` + tinyImage + `"}]}` + tt.extra + `]}`)
			plan, err := Discover(body, Options{AgentReanalysisEnabled: true})
			if err != nil {
				t.Fatal(err)
			}
			if groups := plan.Groups(); len(groups) != 1 || groups[0].Tool != nil {
				t.Fatalf("groups=%#v", groups)
			}
		})
	}
}

func TestDedicatedReanalysisArgumentsAndLimit(t *testing.T) {
	validArgs := `{"attachment_ids":["att_1"],"focus":"read the small labels","detail":"original","cache":"no_store"}`
	body := dedicatedResponsesBody(1, validArgs)
	plan, err := Discover([]byte(body), Options{AgentReanalysisEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	tool := plan.Groups()[0].Tool
	if tool == nil || tool.Focus != "read the small labels" || tool.Detail != "original" || tool.CacheMode != "no_store" {
		t.Fatalf("tool=%#v", tool)
	}
	maxIDs := make([]string, 16)
	for i := range maxIDs {
		maxIDs[i] = `"att_` + string(rune('a'+i)) + `"`
	}
	boundaryArgs := `{"attachment_ids":[` + strings.Join(maxIDs, ",") + `],"focus":"` + strings.Repeat("界", 2000) + `"}`
	if _, err := Discover([]byte(dedicatedResponsesBody(1, boundaryArgs)), Options{AgentReanalysisEnabled: true}); err != nil {
		t.Fatalf("valid boundary rejected: %v", err)
	}

	invalid := []string{
		`{}`,
		`{"attachment_ids":[],"focus":"x"}`,
		`{"attachment_ids":[1],"focus":"x"}`,
		`{"attachment_ids":["a"],"focus":""}`,
		`{"attachment_ids":["a"],"focus":"x","detail":"low"}`,
		`{"attachment_ids":["a"],"focus":"x","cache":"normal"}`,
		`{"attachment_ids":["a"],"focus":"x","path":"/tmp/a"}`,
		`{"attachment_ids":[` + strings.Join(append(maxIDs, `"att_extra"`), ",") + `],"focus":"x"}`,
		`{"attachment_ids":["a"],"focus":"` + strings.Repeat("x", 2001) + `"}`,
	}
	for _, args := range invalid {
		_, err := Discover([]byte(dedicatedResponsesBody(1, args)), Options{AgentReanalysisEnabled: true})
		var planner *Error
		if !errors.As(err, &planner) || planner.StatusCode != 400 {
			t.Fatalf("args=%s err=%v", args, err)
		}
	}

	_, err = Discover([]byte(dedicatedResponsesBody(4, validArgs)), Options{AgentReanalysisEnabled: true})
	var planner *Error
	if !errors.As(err, &planner) || planner.StatusCode != 413 || planner.Limit != LimitReanalysisCount {
		t.Fatalf("limit err=%#v", err)
	}
}

func TestChatAndClaudeTailReanalysisAssociation(t *testing.T) {
	chat := []byte(`{"tools":[{"type":"function","function":{"name":"view_image"}}],"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"chat focus"}]},` +
		`{"role":"assistant","tool_calls":[{"id":"call_chat","type":"function","function":{"name":"view_image","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"call_chat","content":[{"type":"image_url","image_url":{"url":"` + tinyImage + `"}}]}` +
		`]}`)
	chatPlan, err := discoverChat(chat, Options{AgentReanalysisEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	chatTool := chatPlan.Groups()[0].Tool
	if chatTool == nil || chatTool.Focus != "chat focus" || chatTool.Location != "messages[2]" || !chatTool.Tail {
		t.Fatalf("chat tool=%#v", chatTool)
	}

	claude := []byte(`{"tools":[{"name":"deepseek_vision_reanalyze"}],"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"ignored inferred focus"}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"tool_1","name":"deepseek_vision_reanalyze","input":{"attachment_ids":["att_1"],"focus":"claude explicit focus"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAECAw=="}}]}]}` +
		`]}`)
	claudePlan, err := discoverClaude(claude, Options{AgentReanalysisEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	claudeTool := claudePlan.Groups()[0].Tool
	if claudeTool == nil || claudeTool.Focus != "claude explicit focus" || claudeTool.Location != "messages[2].content[0]" || !claudeTool.Tail {
		t.Fatalf("claude tool=%#v", claudeTool)
	}
}

func TestToolCallMustPrecedeOutputAcrossProtocols(t *testing.T) {
	responses := []byte(`{"tools":[{"type":"function","name":"view_image"}],"input":[` +
		`{"role":"user","content":[{"type":"input_text","text":"focus"}]},` +
		`{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_image","image_url":"` + tinyImage + `"}]},` +
		`{"type":"function_call","name":"view_image","call_id":"call_1","arguments":"{}"}` +
		`]}`)
	responsesPlan, err := Discover(responses, Options{AgentReanalysisEnabled: true})
	if err != nil || responsesPlan.Groups()[0].Tool != nil {
		t.Fatalf("Responses out-of-order association: groups=%#v err=%v", responsesPlan.Groups(), err)
	}

	chat := []byte(`{"tools":[{"type":"function","function":{"name":"view_image"}}],"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"focus"}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":[{"type":"image_url","image_url":{"url":"` + tinyImage + `"}}]},` +
		`{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"view_image","arguments":"{}"}}]}` +
		`]}`)
	chatPlan, err := discoverChat(chat, Options{AgentReanalysisEnabled: true})
	if err != nil || chatPlan.Groups()[0].Tool != nil {
		t.Fatalf("Chat out-of-order association: groups=%#v err=%v", chatPlan.Groups(), err)
	}

	claude := []byte(`{"tools":[{"name":"view_image"}],"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"focus"}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAECAw=="}}]}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"tool_1","name":"view_image","input":{}}]}` +
		`]}`)
	claudePlan, err := discoverClaude(claude, Options{AgentReanalysisEnabled: true})
	if err != nil || claudePlan.Groups()[0].Tool != nil {
		t.Fatalf("Claude out-of-order association: groups=%#v err=%v", claudePlan.Groups(), err)
	}
}

func TestDuplicateReanalysisCallIDsRejectedAcrossProtocols(t *testing.T) {
	tests := []struct {
		name string
		run  func() error
	}{
		{name: "responses", run: func() error {
			_, err := Discover([]byte(`{"tools":[{"type":"function","name":"view_image"}],"input":[`+
				`{"type":"function_call","name":"view_image","call_id":"dup","arguments":"{}"},`+
				`{"type":"function_call","name":"view_image","call_id":"dup","arguments":"{}"}]}`), Options{AgentReanalysisEnabled: true})
			return err
		}},
		{name: "chat", run: func() error {
			_, err := discoverChat([]byte(`{"tools":[{"type":"function","function":{"name":"view_image"}}],"messages":[`+
				`{"role":"assistant","tool_calls":[{"id":"dup","function":{"name":"view_image","arguments":"{}"}}]},`+
				`{"role":"assistant","tool_calls":[{"id":"dup","function":{"name":"view_image","arguments":"{}"}}]}]}`), Options{AgentReanalysisEnabled: true})
			return err
		}},
		{name: "claude", run: func() error {
			_, err := discoverClaude([]byte(`{"tools":[{"name":"view_image"}],"messages":[`+
				`{"role":"assistant","content":[{"type":"tool_use","id":"dup","name":"view_image","input":{}}]},`+
				`{"role":"assistant","content":[{"type":"tool_use","id":"dup","name":"view_image","input":{}}]}]}`), Options{AgentReanalysisEnabled: true})
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var planner *Error
			if err := test.run(); !errors.As(err, &planner) || planner.StatusCode != 400 {
				t.Fatalf("duplicate error=%v", err)
			}
		})
	}
}

func TestChatAndClaudeCodexPathOptIn(t *testing.T) {
	path := "/home/demo/.codex/attachments/att_1/shot.png"
	metadata := "# Files mentioned by the user:\n\n## shot.png: " + path + "\n\nInspect spacing"
	tests := []struct {
		name     string
		discover func(bool) (Plan, error)
	}{
		{name: "chat", discover: func(enabled bool) (Plan, error) {
			body := `{"tools":[{"type":"function","function":{"name":"view_image"}}],"messages":[{"role":"user","content":[` +
				`{"type":"text","text":` + quoted(metadata) + `},{"type":"image_url","image_url":{"url":"` + tinyImage + `"}}]}]}`
			return discoverChat([]byte(body), Options{AgentReanalysisEnabled: enabled})
		}},
		{name: "claude", discover: func(enabled bool) (Plan, error) {
			body := `{"tools":[{"name":"view_image"}],"messages":[{"role":"user","content":[` +
				`{"type":"text","text":` + quoted(metadata) + `},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAECAw=="}}]}]}`
			return discoverClaude([]byte(body), Options{AgentReanalysisEnabled: enabled})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, enabled := range []bool{false, true} {
				plan, err := test.discover(enabled)
				if err != nil {
					t.Fatal(err)
				}
				out, err := plan.RewriteGroupsText([]string{"analysis"})
				if err != nil {
					t.Fatal(err)
				}
				if retained := strings.Contains(string(out), path); retained != enabled {
					t.Fatalf("enabled=%v retained=%v output=%s", enabled, retained, out)
				}
			}
		})
	}
}

func TestCodexPathSanitizationDefaultAndOptIn(t *testing.T) {
	valid := "/home/demo/.codex/attachments/att_1/shot.png"
	markdown := "# Files mentioned by the user:\n\n## shot.png: " + valid + "\n## My request:\nInspect spacing"
	makeBody := func(path, tools string) []byte {
		return []byte(`{"tools":` + tools + `,"input":[{"role":"user","content":[` +
			`{"type":"input_text","text":` + quoted(markdown) + `},` +
			`{"type":"input_text","text":"<image path=\"` + path + `\">"},` +
			`{"type":"input_image","image_url":"` + tinyImage + `"}` +
			`]}]}`)
	}
	for _, test := range []struct {
		name    string
		enabled bool
		tools   string
		path    string
		keep    bool
	}{
		{name: "default", tools: `[{"type":"function","name":"view_image"}]`, path: valid},
		{name: "enabled undeclared", enabled: true, tools: `[]`, path: valid},
		{name: "enabled valid", enabled: true, tools: `[{"type":"function","name":"view_image"}]`, path: valid, keep: true},
		{name: "traversal", enabled: true, tools: `[{"type":"function","name":"view_image"}]`, path: "/home/demo/.codex/attachments/att_1/../secret.png"},
		{name: "non codex", enabled: true, tools: `[{"type":"function","name":"view_image"}]`, path: "/tmp/shot.png"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := makeBody(test.path, test.tools)
			plan, err := Discover(body, Options{AgentReanalysisEnabled: test.enabled})
			if err != nil {
				t.Fatal(err)
			}
			out, err := plan.RewriteGroupsText([]string{"analysis"})
			if err != nil {
				t.Fatal(err)
			}
			hasPath := strings.Contains(string(out), test.path)
			if hasPath != test.keep {
				t.Fatalf("keep=%v output=%s", test.keep, out)
			}
		})
	}
}

func TestCodexAttachmentPathValidationUnixAndWindows(t *testing.T) {
	ordinaryMarkdown := "## Endpoint: /v1/responses\nKeep this API documentation"
	if got := sanitizeAttachmentText(ordinaryMarkdown, false); got != ordinaryMarkdown {
		t.Fatalf("ordinary Markdown path-like text changed: %q", got)
	}
	if paths := attachmentPathsToRedact(map[string]any{"text": ordinaryMarkdown}, false); len(paths) != 0 {
		t.Fatalf("ordinary Markdown path was collected: %#v", paths)
	}
	valid := []string{
		"/home/demo/.codex/attachments/att_1/shot.png",
		`C:\Users\demo\.codex\attachments\att_2\shot.png`,
	}
	for _, path := range valid {
		if !validCodexAttachmentPath(path) {
			t.Errorf("valid path rejected: %q", path)
		}
		if got := sanitizeAttachmentText("<image path=\""+path+"\">", true); !strings.Contains(got, path) {
			t.Errorf("allowed path redacted: %q", got)
		}
	}
	invalid := []string{
		"relative/.codex/attachments/att/file.png",
		"/home/demo/.codex/attachments/att/../secret.png",
		"/home/demo/.codex/attachments//shot.png",
		"/tmp/shot.png",
		`\\server\share\.codex\attachments\att\shot.png`,
	}
	for _, path := range invalid {
		if validCodexAttachmentPath(path) {
			t.Errorf("invalid path accepted: %q", path)
		}
		if got := sanitizeAttachmentText("<image path=\""+path+"\">", true); strings.Contains(got, path) {
			t.Errorf("invalid path retained: %q", got)
		}
	}
}

func dedicatedResponsesBody(count int, args string) string {
	items := []string{`{"role":"user","content":[{"type":"input_text","text":"fallback focus"}]}`}
	for i := 1; i <= count; i++ {
		id := string(rune('0' + i))
		items = append(items,
			`{"type":"function_call","name":"deepseek_vision_reanalyze","call_id":"call_`+id+`","arguments":`+quoted(args)+`}`,
			`{"type":"function_call_output","call_id":"call_`+id+`","output":[{"type":"input_image","image_url":"https://example.com/`+id+`.png"}]}`,
		)
	}
	return `{"tools":[{"type":"function","name":"deepseek_vision_reanalyze"}],"input":[` + strings.Join(items, ",") + `]}`
}

func quoted(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	return `"` + value + `"`
}
