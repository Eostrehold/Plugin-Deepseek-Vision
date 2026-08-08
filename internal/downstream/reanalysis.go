package downstream

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	reanalysisToolName       = "deepseek_vision_reanalyze"
	viewImageToolName        = "view_image"
	maxActiveReanalysisCalls = 3
)

// ToolContext is the protocol-neutral metadata for one active agent-requested
// image reanalysis. Image content remains owned by the protocol-native tool
// result and is never accepted from tool arguments.
type ToolContext struct {
	Name      string `json:"name"`
	CallID    string `json:"call_id"`
	Location  string `json:"location"`
	Focus     string `json:"focus"`
	Detail    string `json:"detail"`
	CacheMode string `json:"cache_mode"`
	Tail      bool   `json:"tail"`
	Active    bool   `json:"active"`
}

type toolCall struct {
	name      string
	callID    string
	arguments any
	index     int
}

func annotateResponsesReanalysis(root map[string]any, groups []PromptGroup, opt Options) (bool, error) {
	declared := declaredResponsesTools(root)
	allowPaths := opt.AgentReanalysisEnabled && declared[viewImageToolName]
	for i := range groups {
		groups[i].AllowAgentReanalysis = allowPaths
	}
	if !opt.AgentReanalysisEnabled {
		return false, nil
	}
	items, _ := root["input"].([]any)
	lastUser := -1
	calls := make(map[string]toolCall)
	for index, raw := range items {
		item, _ := raw.(map[string]any)
		role, _ := item["role"].(string)
		if role == "user" {
			lastUser = index
		}
		if typ, _ := item["type"].(string); typ == "function_call" {
			name, _ := item["name"].(string)
			callID, _ := item["call_id"].(string)
			if callID == "" {
				callID, _ = item["id"].(string)
			}
			if recognizedDeclaredTool(name, declared) && callID != "" {
				if _, duplicate := calls[callID]; duplicate {
					return false, malformed("duplicate image reanalysis call ID", fmt.Sprintf("input[%d]", index))
				}
				calls[callID] = toolCall{name: name, callID: callID, arguments: item["arguments"], index: index}
			}
		}
	}
	active := make(map[string]struct{})
	for i := range groups {
		if groups[i].Source != "function_call_output" || groups[i].InputIndex <= lastUser {
			continue
		}
		item, _ := items[groups[i].InputIndex].(map[string]any)
		callID, _ := item["call_id"].(string)
		call, ok := calls[callID]
		if !ok || call.index <= lastUser || call.index >= groups[i].InputIndex {
			continue
		}
		inferredFocus := groups[i].Prompt
		if call.name == viewImageToolName {
			inferredFocus = latestResponsesUserFocusBefore(items, call.index, opt.MaxFocusChars)
		}
		ctx, err := buildToolContext(call, inferredFocus, opt.MaxFocusChars)
		if err != nil {
			return false, malformed(err.Error(), fmt.Sprintf("input[%d]", groups[i].InputIndex))
		}
		groups[i].Tool = ctx
		ctx.Location = fmt.Sprintf("input[%d]", groups[i].InputIndex)
		ctx.Tail = true
		groups[i].Prompt = ctx.Focus
		active[callID] = struct{}{}
	}
	return allowPaths, validateActiveReanalysisCount(active)
}

func annotateChatReanalysis(root map[string]any, groups []PromptGroup, opt Options) (bool, error) {
	if !opt.AgentReanalysisEnabled {
		return false, nil
	}
	declared := declaredChatTools(root)
	allowReanalysis := declared[viewImageToolName]
	for i := range groups {
		groups[i].AllowAgentReanalysis = allowReanalysis
	}
	messages, _ := root["messages"].([]any)
	lastUser := -1
	calls := make(map[string]toolCall)
	for index, raw := range messages {
		message, _ := raw.(map[string]any)
		role, _ := message["role"].(string)
		if role == "user" {
			lastUser = index
		}
		if role != "assistant" {
			continue
		}
		toolCalls, _ := message["tool_calls"].([]any)
		for _, rawCall := range toolCalls {
			entry, _ := rawCall.(map[string]any)
			callID, _ := entry["id"].(string)
			fn, _ := entry["function"].(map[string]any)
			name, _ := fn["name"].(string)
			if recognizedDeclaredTool(name, declared) && callID != "" {
				if _, duplicate := calls[callID]; duplicate {
					return false, malformed("duplicate image reanalysis call ID", fmt.Sprintf("messages[%d]", index))
				}
				calls[callID] = toolCall{name: name, callID: callID, arguments: fn["arguments"], index: index}
			}
		}
	}
	active := make(map[string]struct{})
	for i := range groups {
		if groups[i].Source != "function_call_output" || groups[i].InputIndex <= lastUser {
			continue
		}
		message, _ := messages[groups[i].InputIndex].(map[string]any)
		callID, _ := message["tool_call_id"].(string)
		call, ok := calls[callID]
		if !ok || call.index <= lastUser || call.index >= groups[i].InputIndex {
			continue
		}
		inferredFocus := groups[i].Prompt
		if call.name == viewImageToolName {
			inferredFocus = latestChatUserFocusBefore(messages, call.index, opt.MaxFocusChars)
		}
		ctx, err := buildToolContext(call, inferredFocus, opt.MaxFocusChars)
		if err != nil {
			return false, malformed(err.Error(), fmt.Sprintf("messages[%d]", groups[i].InputIndex))
		}
		groups[i].Tool = ctx
		ctx.Location = fmt.Sprintf("messages[%d]", groups[i].InputIndex)
		ctx.Tail = true
		groups[i].Prompt = ctx.Focus
		active[callID] = struct{}{}
	}
	return allowReanalysis, validateActiveReanalysisCount(active)
}

func annotateClaudeReanalysis(root map[string]any, groups []claudePromptGroup, opt Options) (bool, error) {
	if !opt.AgentReanalysisEnabled {
		return false, nil
	}
	declared := declaredClaudeTools(root)
	allowReanalysis := declared[viewImageToolName]
	for i := range groups {
		groups[i].AllowAgentReanalysis = allowReanalysis
	}
	messages, _ := root["messages"].([]any)
	lastOrdinaryUser := -1
	calls := make(map[string]toolCall)
	for messageIndex, raw := range messages {
		message, _ := raw.(map[string]any)
		role, _ := message["role"].(string)
		content, _ := message["content"].([]any)
		hasOrdinaryUserContent := false
		for _, rawBlock := range content {
			block, _ := rawBlock.(map[string]any)
			typeName, _ := block["type"].(string)
			if role == "user" && typeName != "tool_result" {
				hasOrdinaryUserContent = true
			}
			if role == "assistant" && typeName == "tool_use" {
				name, _ := block["name"].(string)
				callID, _ := block["id"].(string)
				if recognizedDeclaredTool(name, declared) && callID != "" {
					if _, duplicate := calls[callID]; duplicate {
						return false, malformed("duplicate image reanalysis call ID", fmt.Sprintf("messages[%d]", messageIndex))
					}
					calls[callID] = toolCall{name: name, callID: callID, arguments: block["input"], index: messageIndex}
				}
			}
		}
		if role == "user" && hasOrdinaryUserContent {
			lastOrdinaryUser = messageIndex
		}
	}
	active := make(map[string]struct{})
	for i := range groups {
		group := &groups[i]
		if group.Source != "tool_result" || group.InputIndex <= lastOrdinaryUser || group.container.toolResultIndex < 0 {
			continue
		}
		message, _ := messages[group.InputIndex].(map[string]any)
		content, _ := message["content"].([]any)
		block, _ := content[group.container.toolResultIndex].(map[string]any)
		callID, _ := block["tool_use_id"].(string)
		call, ok := calls[callID]
		if !ok || call.index <= lastOrdinaryUser || call.index >= group.InputIndex {
			continue
		}
		inferredFocus := group.Prompt
		if call.name == viewImageToolName {
			inferredFocus = latestClaudeUserFocusBefore(messages, call.index, opt.MaxFocusChars)
		}
		ctx, err := buildToolContext(call, inferredFocus, opt.MaxFocusChars)
		if err != nil {
			return false, malformed(err.Error(), fmt.Sprintf("messages[%d].content[%d]", group.InputIndex, group.container.toolResultIndex))
		}
		group.Tool = ctx
		ctx.Location = fmt.Sprintf("messages[%d].content[%d]", group.InputIndex, group.container.toolResultIndex)
		ctx.Tail = true
		group.Prompt = ctx.Focus
		active[callID] = struct{}{}
	}
	return allowReanalysis, validateActiveReanalysisCount(active)
}

func buildToolContext(call toolCall, inferredFocus string, maxFocus int) (*ToolContext, error) {
	ctx := &ToolContext{Name: call.name, CallID: call.callID, Focus: truncateRunes(strings.TrimSpace(inferredFocus), maxFocus), Detail: "high", CacheMode: "refresh", Active: true}
	if call.name == viewImageToolName {
		if args, err := toolArgumentsObject(call.arguments); err == nil {
			if detail, ok := args["detail"].(string); ok && (detail == "high" || detail == "original") {
				ctx.Detail = detail
			}
		}
		return ctx, nil
	}
	args, err := toolArgumentsObject(call.arguments)
	if err != nil {
		return nil, fmt.Errorf("deepseek_vision_reanalyze arguments must be a JSON object")
	}
	for key := range args {
		switch key {
		case "attachment_ids", "focus", "detail", "cache":
		default:
			return nil, fmt.Errorf("deepseek_vision_reanalyze arguments contain unsupported field %q", key)
		}
	}
	ids, ok := args["attachment_ids"].([]any)
	if !ok || len(ids) == 0 || len(ids) > 16 {
		return nil, fmt.Errorf("deepseek_vision_reanalyze attachment_ids must contain 1 to 16 strings")
	}
	for _, rawID := range ids {
		if id, ok := rawID.(string); !ok || strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("deepseek_vision_reanalyze attachment_ids must contain 1 to 16 strings")
		}
	}
	focus, ok := args["focus"].(string)
	if !ok || strings.TrimSpace(focus) == "" || len([]rune(focus)) > maxFocus {
		return nil, fmt.Errorf("deepseek_vision_reanalyze focus must contain 1 to %d characters", maxFocus)
	}
	ctx.Focus = strings.TrimSpace(focus)
	if detail, exists := args["detail"]; exists {
		value, ok := detail.(string)
		if !ok || (value != "high" && value != "original") {
			return nil, fmt.Errorf("deepseek_vision_reanalyze detail must be high or original")
		}
		ctx.Detail = value
	}
	if cache, exists := args["cache"]; exists {
		value, ok := cache.(string)
		if !ok || (value != "refresh" && value != "no_store") {
			return nil, fmt.Errorf("deepseek_vision_reanalyze cache must be refresh or no_store")
		}
		ctx.CacheMode = value
	}
	return ctx, nil
}

func toolArgumentsObject(raw any) (map[string]any, error) {
	if object, ok := raw.(map[string]any); ok {
		return object, nil
	}
	text, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("arguments are not JSON")
	}
	var object map[string]any
	if err := json.Unmarshal([]byte(text), &object); err != nil {
		return nil, err
	}
	return object, nil
}

func latestResponsesUserFocusBefore(items []any, before, maxFocus int) string {
	for i := before - 1; i >= 0; i-- {
		item, _ := items[i].(map[string]any)
		role, _ := item["role"].(string)
		if role != "user" {
			continue
		}
		content, _ := item["content"].([]any)
		var parts []string
		for _, raw := range content {
			block, _ := raw.(map[string]any)
			if typ, _ := block["type"].(string); typ != "input_text" {
				continue
			}
			if value, _ := block["text"].(string); strings.TrimSpace(value) != "" {
				parts = append(parts, strings.TrimSpace(value))
			}
		}
		return truncateRunes(strings.Join(parts, "\n\n"), maxFocus)
	}
	return ""
}

func latestChatUserFocusBefore(messages []any, before, maxFocus int) string {
	for i := before - 1; i >= 0; i-- {
		message, _ := messages[i].(map[string]any)
		role, _ := message["role"].(string)
		if role != "user" {
			continue
		}
		switch content := message["content"].(type) {
		case string:
			return truncateRunes(strings.TrimSpace(content), maxFocus)
		case []any:
			var parts []string
			for _, raw := range content {
				block, _ := raw.(map[string]any)
				if typ, _ := block["type"].(string); typ != "text" {
					continue
				}
				if value, _ := block["text"].(string); strings.TrimSpace(value) != "" {
					parts = append(parts, strings.TrimSpace(value))
				}
			}
			return truncateRunes(strings.Join(parts, "\n\n"), maxFocus)
		}
		return ""
	}
	return ""
}

func latestClaudeUserFocusBefore(messages []any, before, maxFocus int) string {
	for i := before - 1; i >= 0; i-- {
		message, _ := messages[i].(map[string]any)
		role, _ := message["role"].(string)
		if role != "user" {
			continue
		}
		switch content := message["content"].(type) {
		case string:
			return truncateRunes(strings.TrimSpace(content), maxFocus)
		case []any:
			var parts []string
			hasOrdinary := false
			for _, raw := range content {
				block, _ := raw.(map[string]any)
				typ, _ := block["type"].(string)
				if typ == "tool_result" {
					continue
				}
				hasOrdinary = true
				if typ == "text" {
					if value, _ := block["text"].(string); strings.TrimSpace(value) != "" {
						parts = append(parts, strings.TrimSpace(value))
					}
				}
			}
			if hasOrdinary {
				return truncateRunes(strings.Join(parts, "\n\n"), maxFocus)
			}
		}
	}
	return ""
}

func recognizedDeclaredTool(name string, declared map[string]bool) bool {
	return declared[name] && (name == viewImageToolName || name == reanalysisToolName)
}

func declaredResponsesTools(root map[string]any) map[string]bool {
	declared := make(map[string]bool)
	tools, _ := root["tools"].([]any)
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		name, _ := tool["name"].(string)
		if name == "" {
			if fn, ok := tool["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
		}
		declared[name] = name != ""
	}
	return declared
}

func declaredChatTools(root map[string]any) map[string]bool   { return declaredResponsesTools(root) }
func declaredClaudeTools(root map[string]any) map[string]bool { return declaredResponsesTools(root) }

func validateActiveReanalysisCount(active map[string]struct{}) error {
	if len(active) <= maxActiveReanalysisCalls {
		return nil
	}
	return limitExceeded(LimitReanalysisCount, len(active), maxActiveReanalysisCalls, "request contains too many active image reanalysis calls", "tools")
}
