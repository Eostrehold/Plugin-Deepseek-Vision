package downstream

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
)

var codexAttachmentPathPattern = regexp.MustCompile(`(?i)(?:[a-z]:[\\/]|/)[^\x00-\x20<>"']*?[\\/]\.codex[\\/]attachments[\\/][a-z0-9_-]{1,128}[\\/][^\x00-\x20<>"']+`)

func sanitizeGroupPrompts(groups []PromptGroup) {
	for i := range groups {
		groups[i].Prompt = sanitizeAttachmentText(groups[i].Prompt, false)
		if groups[i].Tool != nil {
			groups[i].Tool.Focus = groups[i].Prompt
		}
	}
}

func sanitizeAttachmentTree(value any, allowCodexPaths bool) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if text, ok := child.(string); ok {
				if strings.EqualFold(key, "path") && looksLikeLocalPath(text) && !(allowCodexPaths && validCodexAttachmentPath(text)) {
					typed[key] = "[analyzed image attachment path omitted]"
					continue
				}
				if strings.EqualFold(key, "arguments") {
					if sanitizedJSON, ok := sanitizeJSONString(text, allowCodexPaths); ok {
						typed[key] = sanitizedJSON
						continue
					}
				}
				typed[key] = sanitizeAttachmentText(text, allowCodexPaths)
				continue
			}
			sanitizeAttachmentTree(child, allowCodexPaths)
		}
	case []any:
		for _, child := range typed {
			sanitizeAttachmentTree(child, allowCodexPaths)
		}
	}
}

func attachmentPathsToRedact(value any, allowCodexPaths bool) []string {
	seen := make(map[string]struct{})
	var collect func(any)
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" || allowCodexPaths && validCodexAttachmentPath(path) {
			return
		}
		variants := []string{path, strings.ReplaceAll(path, `\`, "/"), strings.ReplaceAll(path, "/", `\`)}
		variants = append(variants, strings.ReplaceAll(variants[2], `\`, `\\`))
		for _, variant := range variants {
			if variant != "" {
				seen[variant] = struct{}{}
			}
		}
	}
	collect = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				if text, ok := child.(string); ok {
					if strings.EqualFold(key, "path") && looksLikeLocalPath(text) {
						add(text)
					}
					inFilesBlock := false
					for _, line := range strings.Split(text, "\n") {
						trimmed := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
						if strings.EqualFold(trimmed, "# Files mentioned by the user:") {
							inFilesBlock = true
							continue
						}
						if inFilesBlock && strings.HasPrefix(trimmed, "# ") {
							inFilesBlock = false
						}
						if match := codexImageWrapperPattern.FindStringSubmatch(trimmed); len(match) == 2 {
							add(match[1])
						}
						if inFilesBlock && strings.HasPrefix(trimmed, "## ") {
							if colon := strings.Index(trimmed, ":"); colon >= 0 {
								candidate := strings.TrimSpace(trimmed[colon+1:])
								if looksLikeLocalPath(candidate) {
									add(candidate)
								}
							}
						}
					}
				}
				collect(child)
			}
		case []any:
			for _, child := range typed {
				collect(child)
			}
		}
	}
	collect(value)
	paths := make([]string, 0, len(seen))
	for path := range seen {
		paths = append(paths, path)
	}
	return paths
}

func sanitizeJSONString(text string, allowCodexPaths bool) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) < 2 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return "", false
	}
	var value any
	if json.Unmarshal([]byte(trimmed), &value) != nil {
		return "", false
	}
	sanitizeAttachmentTree(value, allowCodexPaths)
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", false
	}
	return string(encoded), true
}

func sanitizeAttachmentText(text string, allowCodexPaths bool) string {
	lines := strings.Split(text, "\n")
	inFilesBlock := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if strings.EqualFold(trimmed, "# Files mentioned by the user:") {
			inFilesBlock = true
			continue
		}
		if inFilesBlock && strings.HasPrefix(trimmed, "# ") {
			inFilesBlock = false
		}
		if match := codexImageWrapperPattern.FindStringSubmatch(trimmed); len(match) == 2 {
			if !(allowCodexPaths && validCodexAttachmentPath(match[1])) {
				lines[i] = "[Analyzed image attachment metadata omitted]"
			}
			continue
		}
		if strings.EqualFold(trimmed, "</image>") {
			lines[i] = "[End analyzed image attachment]"
			continue
		}
		if inFilesBlock && strings.HasPrefix(trimmed, "## ") {
			if colon := strings.Index(trimmed, ":"); colon >= 0 {
				path := strings.TrimSpace(trimmed[colon+1:])
				if looksLikeLocalPath(path) && !(allowCodexPaths && validCodexAttachmentPath(path)) {
					prefixBytes := strings.Index(line, ":") + 1
					if prefixBytes > 0 {
						lines[i] = line[:prefixBytes] + " [analyzed image attachment path omitted]"
					}
				}
			}
		}
	}
	result := strings.Join(lines, "\n")
	return codexAttachmentPathPattern.ReplaceAllStringFunc(result, func(path string) string {
		if allowCodexPaths && validCodexAttachmentPath(path) {
			return path
		}
		return "[analyzed image attachment path omitted]"
	})
}

func looksLikeLocalPath(path string) bool {
	path = strings.TrimSpace(path)
	return strings.HasPrefix(path, "/") || strings.HasPrefix(path, `\\`) ||
		(len(path) >= 3 && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) && path[1] == ':' && (path[2] == '/' || path[2] == '\\'))
}

func validCodexAttachmentPath(path string) bool {
	path = strings.TrimSpace(path)
	if len(path) == 0 || len(path) > 4096 || !looksLikeLocalPath(path) || strings.HasPrefix(path, `\\`) {
		return false
	}
	if strings.IndexFunc(path, func(r rune) bool { return unicode.IsControl(r) }) >= 0 {
		return false
	}
	normalized := strings.ReplaceAll(path, `\`, "/")
	segments := strings.Split(normalized, "/")
	for _, segment := range segments {
		if segment == ".." {
			return false
		}
	}
	for i := 0; i+3 < len(segments); i++ {
		if segments[i] != ".codex" || segments[i+1] != "attachments" {
			continue
		}
		id := segments[i+2]
		if id == "" || len(id) > 128 || segments[i+3] == "" {
			return false
		}
		for _, r := range id {
			if !(r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
				return false
			}
		}
		return true
	}
	return false
}
