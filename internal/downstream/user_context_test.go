package downstream

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUserContextIndexSelection(t *testing.T) {
	var index userContextIndex
	index.recordTurn(0,
		positionedUserText{pos: 1, text: " first "},
		positionedUserText{pos: 4, text: "second"},
	)
	index.recordTurn(2, positionedUserText{pos: 8, text: "later"})

	if got := index.itemText(0, 100); got != "first\n\nsecond" {
		t.Fatalf("item text = %q", got)
	}
	if got := index.nearest(0, 3, 100, true); got != "second" {
		t.Fatalf("same-item nearest = %q", got)
	}
	if got := index.nearest(1, 6, 100, false); got != "second" {
		t.Fatalf("global nearest = %q", got)
	}
	if got := index.latestBefore(2, 100); got != "first\n\nsecond" {
		t.Fatalf("latest before = %q", got)
	}
	if got := index.lastTurnIndex(); got != 2 {
		t.Fatalf("last turn = %d", got)
	}
}

func TestUserContextIndexTiePrefersEarlierPosition(t *testing.T) {
	var index userContextIndex
	index.recordTurn(0, positionedUserText{pos: 2, text: "earlier"})
	index.recordTurn(1, positionedUserText{pos: 4, text: "later"})
	if got := index.nearest(2, 3, 100, false); got != "earlier" {
		t.Fatalf("tie selection = %q", got)
	}
}

func TestUserContextIndexEmptyLatestTurnStopsFallback(t *testing.T) {
	var index userContextIndex
	index.recordTurn(0, positionedUserText{pos: 1, text: "stale"})
	index.recordTurn(2)
	if got := index.latestBefore(3, 100); got != "" {
		t.Fatalf("empty latest turn leaked %q", got)
	}
	if got := index.lastTurnIndex(); got != 2 {
		t.Fatalf("last turn = %d", got)
	}
}

func TestUserContextIndexTruncatesJoinedRunes(t *testing.T) {
	var index userContextIndex
	index.recordTurn(0,
		positionedUserText{pos: 1, text: "你好"},
		positionedUserText{pos: 2, text: "世界"},
	)
	if got := index.itemText(0, 3); got != "你好\n" {
		t.Fatalf("truncated text = %q", got)
	}
}

func TestStringAndArrayUserContentProduceEquivalentFocus(t *testing.T) {
	tests := []struct {
		name        string
		stringBody  string
		arrayBody   string
		discover    func([]byte) (Plan, error)
		expectFocus string
	}{
		{
			name: "responses",
			stringBody: `{"input":[{"role":"user","content":"inspect spacing"},` +
				`{"type":"function_call_output","output":[{"type":"input_image","image_url":"https://example.com/responses.png"}]}]}`,
			arrayBody: `{"input":[{"role":"user","content":[{"type":"input_text","text":"inspect spacing"}]},` +
				`{"type":"function_call_output","output":[{"type":"input_image","image_url":"https://example.com/responses.png"}]}]}`,
			discover: func(body []byte) (Plan, error) { return Discover(body) }, expectFocus: "inspect spacing",
		},
		{
			name: "chat",
			stringBody: `{"messages":[{"role":"user","content":"inspect spacing"},` +
				`{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/chat.png"}}]}]}`,
			arrayBody: `{"messages":[{"role":"user","content":[{"type":"text","text":"inspect spacing"}]},` +
				`{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/chat.png"}}]}]}`,
			discover: func(body []byte) (Plan, error) { return discoverChat(body) }, expectFocus: "inspect spacing",
		},
		{
			name: "claude",
			stringBody: `{"messages":[{"role":"user","content":"inspect spacing"},` +
				`{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.com/claude.png"}}]}]}`,
			arrayBody: `{"messages":[{"role":"user","content":[{"type":"text","text":"inspect spacing"}]},` +
				`{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.com/claude.png"}}]}]}`,
			discover: func(body []byte) (Plan, error) { return discoverClaude(body) }, expectFocus: "inspect spacing",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, body := range []string{test.stringBody, test.arrayBody} {
				plan, err := test.discover([]byte(body))
				if err != nil {
					t.Fatal(err)
				}
				groups := plan.Groups()
				images := plan.Images()
				if len(groups) != 1 || len(images) != 1 || groups[0].Prompt != test.expectFocus || images[0].FocusHint != test.expectFocus {
					t.Fatalf("groups=%#v images=%#v", groups, images)
				}
			}
		})
	}
}

func TestChatStringAndArrayTraversalCoordinatesMatch(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "string",
			body: `{"messages":[` +
				`{"role":"user","content":"earlier focus"},` +
				`{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/chat.png"}}]},` +
				`{"role":"user","content":"later focus"}` +
				`]}`,
		},
		{
			name: "single text blocks",
			body: `{"messages":[` +
				`{"role":"user","content":[{"type":"text","text":"earlier focus"}]},` +
				`{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/chat.png"}}]},` +
				`{"role":"user","content":[{"type":"text","text":"later focus"}]}` +
				`]}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := discoverChat([]byte(test.body))
			if err != nil {
				t.Fatal(err)
			}
			groups := plan.Groups()
			if len(groups) != 1 || groups[0].Prompt != "earlier focus" {
				t.Fatalf("groups=%#v", groups)
			}
		})
	}
}

func TestStringHistoryContractFixturesProvideFocus(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		discover    func([]byte) (Plan, error)
		expectFocus string
		expectTool  bool
	}{
		{
			name: "responses", path: filepath.Join("..", "..", "testdata", "responses", "09-string-history-view-image.json"),
			discover: func(body []byte) (Plan, error) {
				return Discover(body, Options{AgentReanalysisEnabled: true})
			},
			expectFocus: "Check button alignment, overflow, and abnormal whitespace.", expectTool: true,
		},
		{
			name: "chat", path: filepath.Join("..", "..", "testdata", "chat", "04-string-history-image.json"),
			discover:    func(body []byte) (Plan, error) { return discoverChat(body) },
			expectFocus: "Inspect the dashboard for alignment, overflow, and abnormal whitespace.",
		},
		{
			name: "claude", path: filepath.Join("..", "..", "testdata", "claude", "04-string-history-image.json"),
			discover:    func(body []byte) (Plan, error) { return discoverClaude(body) },
			expectFocus: "Inspect the dashboard for alignment, overflow, and abnormal whitespace.",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, err := os.ReadFile(test.path)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := test.discover(body)
			if err != nil {
				t.Fatal(err)
			}
			groups := plan.Groups()
			images := plan.Images()
			if len(groups) != 1 || len(images) != 1 || groups[0].Prompt != test.expectFocus || images[0].FocusHint != test.expectFocus {
				t.Fatalf("groups=%#v images=%#v", groups, images)
			}
			if test.expectTool && (groups[0].Tool == nil || groups[0].Tool.Focus != test.expectFocus) {
				t.Fatalf("tool context = %#v", groups[0].Tool)
			}
		})
	}
}
