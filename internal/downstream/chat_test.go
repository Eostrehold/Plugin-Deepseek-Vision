package downstream

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestDiscoverChatURLAndDataURI(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":[{"type":"text","text":"Describe both screenshots."},{"type":"image_url","image_url":{"url":"https://example.com/one.png"}},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAECAw=="}}]}]}`)
	plan, err := discoverChat(body)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.HasImages() || len(plan.Images()) != 2 {
		t.Fatalf("images = %#v", plan.Images())
	}
	images := plan.Images()
	if images[0].Number != 1 || images[1].Number != 2 || images[0].Source != "content" {
		t.Fatalf("image metadata = %#v", images)
	}
	groups := plan.Groups()
	if len(groups) != 1 || len(groups[0].Images) != 2 || groups[0].Prompt != "Describe both screenshots." {
		t.Fatalf("groups = %#v", groups)
	}
}

func TestDiscoverChatHistoryAndToolContent(t *testing.T) {
	body := []byte(`{"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"Earlier request"}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"I found two artifacts."},{"type":"image_url","image_url":{"url":"https://example.com/assistant.png"}}]},` +
		`{"role":"tool","content":[{"type":"text","text":"Tool screenshot"},{"type":"image_url","image_url":{"url":"https://example.com/tool.png"}}]},` +
		`{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/current.png"}}]}` +
		`]}`)
	plan, err := discoverChat(body)
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.InputItemCount(); got != 4 {
		t.Fatalf("input item count = %d", got)
	}
	images := plan.Images()
	if len(images) != 3 || images[0].Number != 1 || images[2].Number != 3 {
		t.Fatalf("images = %#v", images)
	}
	if images[1].Source != "function_call_output" {
		t.Fatalf("tool image source = %q", images[1].Source)
	}
	groups := plan.Groups()
	if len(groups) != 3 || groups[0].Prompt != "I found two artifacts." || groups[1].Prompt != "Tool screenshot" || groups[2].Prompt != "Earlier request" {
		t.Fatalf("groups = %#v", groups)
	}
	details := plan.ImageCountDetails()
	if details.InputItems != 4 || details.ImageBlocks != 3 || details.UniqueImageReferences != 3 || details.DuplicateImageBlocks != 0 || details.ContentImages != 2 || details.FunctionOutputImages != 1 {
		t.Fatalf("count details = %#v", details)
	}
}

func TestDiscoverChatStringContentAndNoImagePassthrough(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"plain text"},{"role":"assistant","content":"answer"}]}`)
	plan, err := discoverChat(body)
	if err != nil {
		t.Fatal(err)
	}
	if plan.HasImages() || plan.InputItemCount() != 2 {
		t.Fatalf("unexpected plan = %#v", plan)
	}
	out, err := plan.RewriteGroupsText(nil)
	if err != nil || string(out) != string(body) {
		t.Fatalf("passthrough = %q, err=%v", out, err)
	}
	missingMessages := []byte(`{"model":"x"}`)
	noMessages, err := discoverChat(missingMessages)
	if err != nil {
		t.Fatal(err)
	}
	out, err = noMessages.RewriteGroupsText(nil)
	if err != nil || string(out) != string(missingMessages) {
		t.Fatalf("missing messages passthrough = %q, err=%v", out, err)
	}
}

func TestDiscoverChatMalformedAndUnsupportedReferences(t *testing.T) {
	tests := []struct {
		name string
		body string
		kind ErrorKind
		code int
	}{
		{"messages object", `{"messages":{}}`, ErrorMalformedRequest, 400},
		{"message scalar", `{"messages":["bad"]}`, ErrorMalformedRequest, 400},
		{"content object", `{"messages":[{"role":"user","content":{}}]}`, ErrorMalformedRequest, 400},
		{"block scalar", `{"messages":[{"content":["bad"]}]}`, ErrorMalformedRequest, 400},
		{"image url scalar", `{"messages":[{"content":[{"type":"image_url","image_url":"https://example.com/x.png"}]}]}`, ErrorMalformedRequest, 400},
		{"image url missing", `{"messages":[{"content":[{"type":"image_url","image_url":{}}]}]}`, ErrorMalformedRequest, 400},
		{"image url wrong type", `{"messages":[{"content":[{"type":"image_url","image_url":{"url":3}}]}]}`, ErrorMalformedRequest, 400},
		{"ftp", `{"messages":[{"content":[{"type":"image_url","image_url":{"url":"ftp://example.com/x.png"}}]}]}`, ErrorUnsupportedImage, 422},
		{"private", `{"messages":[{"content":[{"type":"image_url","image_url":{"url":"http://127.0.0.1/x.png"}}]}]}`, ErrorUnsupportedImage, 422},
		{"non-image data", `{"messages":[{"content":[{"type":"image_url","image_url":{"url":"data:text/plain;base64,SGVsbG8="}}]}]}`, ErrorUnsupportedImage, 422},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := discoverChat([]byte(test.body))
			if err == nil {
				t.Fatal("expected planner error")
			}
			var plannerErr *Error
			if !errors.As(err, &plannerErr) {
				t.Fatalf("error type = %T", err)
			}
			if plannerErr.Kind != test.kind || plannerErr.StatusCode != test.code {
				t.Fatalf("error = %#v", plannerErr)
			}
		})
	}
}

func TestDiscoverChatReferenceAndUniqueImageLimits(t *testing.T) {
	body := []byte(`{"messages":[{"content":[` +
		`{"type":"image_url","image_url":{"url":"https://example.com/one.png"}},` +
		`{"type":"image_url","image_url":{"url":"https://example.com/one.png"}},` +
		`{"type":"image_url","image_url":{"url":"https://example.com/two.png"}}` +
		`]}]}`)
	plan, err := discoverChat(body, Options{MaxImages: 2})
	if err != nil {
		t.Fatal(err)
	}
	details := plan.ImageCountDetails()
	if details.ImageBlocks != 3 || details.UniqueImageReferences != 2 || details.DuplicateImageBlocks != 1 {
		t.Fatalf("count details = %#v", details)
	}
	_, err = discoverChat(body, Options{MaxImages: 1})
	var plannerErr *Error
	if !errors.As(err, &plannerErr) || plannerErr.Kind != ErrorLimitsExceeded || plannerErr.StatusCode != 413 || plannerErr.Limit != LimitImageCount {
		t.Fatalf("limit error = %#v", err)
	}
	_, err = discoverChat(body, Options{MaxReferenceBytes: len("https://example.com/one.png") - 1})
	if !errors.As(err, &plannerErr) || plannerErr.Kind != ErrorLimitsExceeded || plannerErr.Limit != LimitImageReference || plannerErr.StatusCode != 413 {
		t.Fatalf("reference limit error = %#v", err)
	}
}

func TestRewriteChatAtomicallyPreservesFieldsAndUsesLocalNumbers(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4-flash","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"history"},{"type":"image_url","image_url":{"url":"https://example.com/history.png"}}],"name":"first"},` +
		`{"role":"user","content":[{"type":"text","text":"compare these"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"},"image_extra":{"keep":false}},{"type":"image_url","image_url":{"url":"https://example.com/b.png"}}],"tool_calls":[{"id":"call_1"}],"metadata":{"keep":true}},` +
		`{"role":"tool","content":[{"type":"image_url","image_url":{"url":"https://example.com/tool.png"}},{"type":"text","text":"tool metadata"}],"tool_call_id":"call_1"}` +
		`]}`)
	plan, err := discoverChat(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.RewriteGroupsText([]string{""}); err == nil {
		t.Fatal("empty group result must fail")
	}
	rewritten, err := plan.RewriteGroupsText([]string{
		"history analysis",
		"Joint result mentions https://example.com/a.png and https://example.com/b.png",
		"tool result",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rewritten), "image_url") || strings.Contains(string(rewritten), "https://example.com/") {
		t.Fatalf("rewritten body retained image reference: %s", rewritten)
	}
	var root map[string]any
	if err := json.Unmarshal(rewritten, &root); err != nil {
		t.Fatal(err)
	}
	messages := root["messages"].([]any)
	first := messages[0].(map[string]any)
	if first["name"] != "first" {
		t.Fatal("message field was dropped")
	}
	second := messages[1].(map[string]any)
	if second["metadata"].(map[string]any)["keep"] != true {
		t.Fatal("unknown message field was dropped")
	}
	if len(second["tool_calls"].([]any)) != 1 {
		t.Fatal("tool calls were dropped")
	}
	secondContent := second["content"].([]any)
	if secondContent[0].(map[string]any)["text"] != "compare these" {
		t.Fatal("non-image content changed")
	}
	if !strings.Contains(secondContent[1].(map[string]any)["text"].(string), "[Image 1 — already analyzed") || !strings.Contains(secondContent[2].(map[string]any)["text"].(string), "[Image 2 — already analyzed") {
		t.Fatalf("local marker numbering = %#v", secondContent)
	}
	if strings.Count(string(rewritten), "Joint visual analysis") != 3 {
		t.Fatalf("group result count = %d", strings.Count(string(rewritten), "Joint visual analysis"))
	}
	check, err := discoverChat(rewritten)
	if err != nil || check.HasImages() {
		t.Fatalf("rediscovery = %#v, err=%v", check, err)
	}
	secondRewrite, err := check.RewriteGroupsText(nil)
	if err != nil || string(secondRewrite) != string(rewritten) {
		t.Fatalf("idempotent rewrite changed body: %s, err=%v", secondRewrite, err)
	}
}
