package downstream

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zesuy/Plugin-Deepseek-Vision/internal/safety"
)

// chatAdapter is the OpenAI Chat Completions downstream adapter. The adapter
// deliberately keeps the original Chat wire shape: the host remains
// responsible for translating Chat requests to whichever upstream protocol it
// uses after this plugin has replaced the image blocks.
type chatAdapter struct{}

func (chatAdapter) Protocol() Protocol { return ProtocolChat }

func (chatAdapter) Discover(body []byte, options ...Options) (Plan, error) {
	return discoverChat(body, options...)
}

type chatPlan struct {
	original                  []byte
	root                      any
	images                    []Image
	groups                    []PromptGroup
	options                   Options
	inputItems                int
	allowCodexAttachmentPaths bool
}

func (*chatPlan) Protocol() Protocol { return ProtocolChat }

type chatFocusCandidate struct {
	message int
	pos     int
	text    string
}

type chatPendingImage struct {
	Image
}

func discoverChat(body []byte, options ...Options) (*chatPlan, error) {
	opt := Options{}.normalized()
	if len(options) > 0 {
		opt = options[0].normalized()
	}
	if len(body) > opt.MaxBodyBytes {
		return nil, limitExceeded(LimitRequestBody, len(body), opt.MaxBodyBytes, "request body exceeds configured limit", "body")
	}

	root, err := decodeJSON(body)
	if err != nil {
		return nil, malformed("request body is not valid JSON", "body")
	}
	object, ok := root.(map[string]any)
	if !ok {
		return nil, malformed("Chat request must be a JSON object", "body")
	}
	plan := &chatPlan{
		original: append([]byte(nil), body...),
		root:     root,
		options:  opt,
	}

	rawMessages, exists := object["messages"]
	if !exists {
		return plan, nil
	}
	messages, ok := rawMessages.([]any)
	if !ok {
		return nil, malformed("messages must be an array", "messages")
	}
	plan.inputItems = len(messages)

	var pending []chatPendingImage
	var candidates []chatFocusCandidate
	messageText := make(map[int]string)
	messageKinds := make(map[int]locationKind)
	position := 0
	for messageIndex, rawMessage := range messages {
		messageObject, ok := rawMessage.(map[string]any)
		if !ok {
			return nil, malformed("messages entries must be objects", fmt.Sprintf("messages[%d]", messageIndex))
		}
		role := ""
		if rawRole, present := messageObject["role"]; present {
			var roleOK bool
			role, roleOK = rawRole.(string)
			if !roleOK {
				return nil, malformed("message.role must be a string", fmt.Sprintf("messages[%d].role", messageIndex))
			}
		}

		rawContent, present := messageObject["content"]
		if !present {
			position++
			continue
		}
		switch content := rawContent.(type) {
		case nil:
			// Assistant tool-call history commonly uses content:null while the
			// actual call is carried in tool_calls/function_call. It is valid Chat
			// history and cannot contain an image block, so preserve it unchanged.
			position++
		case string:
			if role == "user" && strings.TrimSpace(content) != "" {
				candidates = append(candidates, chatFocusCandidate{message: messageIndex, pos: position, text: content})
			}
			position++
		case []any:
			var textParts []string
			for blockIndex, rawBlock := range content {
				position++
				blockPosition := position
				blockObject, ok := rawBlock.(map[string]any)
				if !ok {
					return nil, malformed("message.content blocks must be objects", fmt.Sprintf("messages[%d].content[%d]", messageIndex, blockIndex))
				}
				typeName := ""
				if rawType, present := blockObject["type"]; present {
					var typeOK bool
					typeName, typeOK = rawType.(string)
					// Unknown blocks are intentionally left to the host. Only a
					// recognized text/image block has fields that this adapter must
					// validate, so an unknown block with a non-string type remains
					// opaque instead of widening this plugin's protocol contract.
					if !typeOK {
						typeName = ""
					}
				}
				switch typeName {
				case "text":
					rawText, present := blockObject["text"]
					if !present {
						return nil, malformed("text content block requires text", fmt.Sprintf("messages[%d].content[%d].text", messageIndex, blockIndex))
					}
					text, ok := rawText.(string)
					if !ok {
						return nil, malformed("text content block.text must be a string", fmt.Sprintf("messages[%d].content[%d].text", messageIndex, blockIndex))
					}
					if strings.TrimSpace(text) != "" {
						textParts = append(textParts, text)
						if role == "user" {
							candidates = append(candidates, chatFocusCandidate{message: messageIndex, pos: blockPosition, text: text})
						}
					}
				case "image_url":
					kind := locationContent
					source := "content"
					if role == "tool" {
						kind = locationFunctionOutput
						source = "function_call_output"
					}
					image, err := discoverChatImage(blockObject, imageLocation{
						kind: kind, input: messageIndex, block: blockIndex, globalPos: blockPosition,
					}, len(pending)+1, source, opt)
					if err != nil {
						return nil, err
					}
					pending = append(pending, image)
					messageKinds[messageIndex] = kind
				}
			}
			if len(textParts) > 0 {
				messageText[messageIndex] = truncateRunes(strings.TrimSpace(strings.Join(textParts, "\n\n")), opt.MaxFocusChars)
			}
		default:
			return nil, malformed("message.content must be a string or array", fmt.Sprintf("messages[%d].content", messageIndex))
		}
	}

	for i := range pending {
		pending[i].FocusHint = chooseChatFocus(pending[i].location, candidates, opt.MaxFocusChars)
	}
	uniqueReferences := make(map[string]struct{}, len(pending))
	for i := range pending {
		uniqueReferences[pending[i].Reference] = struct{}{}
	}
	if len(uniqueReferences) > opt.MaxImages {
		debugImages := chatPendingImageValues(pending)
		return nil, imageCountExceeded(len(uniqueReferences), opt.MaxImages, summarizeImages(debugImages, len(messages)), debugImages)
	}

	for i := range pending {
		plan.images = append(plan.images, pending[i].Image)
	}
	groupIndexes := make(map[int]int)
	for i := range plan.images {
		image := plan.images[i]
		groupIndex, exists := groupIndexes[image.InputIndex]
		if !exists {
			groupIndex = len(plan.groups)
			groupIndexes[image.InputIndex] = groupIndex
			prompt := messageText[image.InputIndex]
			if strings.TrimSpace(prompt) == "" {
				prompt = chooseChatFocus(image.location, candidates, opt.MaxFocusChars)
			}
			kind := messageKinds[image.InputIndex]
			if kind == 0 {
				kind = locationContent
			}
			plan.groups = append(plan.groups, PromptGroup{
				ID: len(plan.groups) + 1, InputIndex: image.InputIndex,
				Source: locationSource(kind), Prompt: truncateRunes(strings.TrimSpace(prompt), opt.MaxFocusChars),
				locationKind: kind,
			})
		}
		plan.groups[groupIndex].Images = append(plan.groups[groupIndex].Images, image)
	}
	// The group prompt is the same context sent to the VLM for every image in
	// the group. Keep Image.FocusHint consistent with it for diagnostics and
	// for any analyzer that chooses to inspect per-image metadata.
	for i := range plan.groups {
		for j := range plan.groups[i].Images {
			plan.groups[i].Images[j].FocusHint = plan.groups[i].Prompt
			for k := range plan.images {
				if plan.images[k].Number == plan.groups[i].Images[j].Number {
					plan.images[k].FocusHint = plan.groups[i].Prompt
				}
			}
		}
	}
	allowPaths, err := annotateChatReanalysis(object, plan.groups, opt)
	if err != nil {
		return nil, err
	}
	plan.allowCodexAttachmentPaths = allowPaths
	sanitizeGroupPrompts(plan.groups)
	return plan, nil
}

func discoverChatImage(block map[string]any, location imageLocation, number int, source string, opt Options) (chatPendingImage, error) {
	rawImageURL, present := block["image_url"]
	if !present {
		return chatPendingImage{}, malformed("image_url content block requires image_url", locationPathChat(location))
	}
	imageURL, ok := rawImageURL.(map[string]any)
	if !ok {
		return chatPendingImage{}, malformed("image_url must be an object", locationPathChat(location))
	}
	rawReference, present := imageURL["url"]
	if !present {
		if _, hasFileID := imageURL["file_id"]; hasFileID {
			return chatPendingImage{}, unsupported("file_id image references are not supported", locationPathChat(location))
		}
		return chatPendingImage{}, malformed("image_url requires url", locationPathChat(location))
	}
	reference, ok := rawReference.(string)
	if !ok {
		return chatPendingImage{}, malformed("image_url.url must be a string", locationPathChat(location))
	}
	if err := safety.ValidateImageReference(reference, opt.MaxReferenceBytes); err != nil {
		if errors.Is(err, safety.ErrImageReferenceTooLarge) {
			return chatPendingImage{}, limitExceeded(LimitImageReference, len(reference), opt.MaxReferenceBytes, "image reference exceeds configured limit", locationPathChat(location))
		}
		return chatPendingImage{}, unsupported("image reference must be a valid HTTP(S) URL or data:image URI", locationPathChat(location))
	}
	return chatPendingImage{Image: Image{
		Number: number, Reference: reference, InputIndex: location.input, BlockIndex: location.block,
		Source: source, location: location,
	}}, nil
}

func chooseChatFocus(location imageLocation, candidates []chatFocusCandidate, maxChars int) string {
	var best *chatFocusCandidate
	bestDistance := int(^uint(0) >> 1)
	for i := range candidates {
		candidate := &candidates[i]
		distance := abs(candidate.pos - location.globalPos)
		if distance < bestDistance || (distance == bestDistance && candidate.pos < best.pos) {
			best, bestDistance = candidate, distance
		}
	}
	if best == nil {
		return ""
	}
	return truncateRunes(strings.TrimSpace(best.text), maxChars)
}

func chatPendingImageValues(images []chatPendingImage) []Image {
	values := make([]Image, len(images))
	for i := range images {
		values[i] = images[i].Image
	}
	return values
}

func locationPathChat(location imageLocation) string {
	return fmt.Sprintf("messages[%d].content[%d]", location.input, location.block)
}

func (p *chatPlan) InputItemCount() int {
	if p == nil {
		return 0
	}
	return p.inputItems
}

func (p *chatPlan) ImageCountDetails() ImageCountDetails {
	if p == nil {
		return ImageCountDetails{LastImageItemIndex: -1}
	}
	return summarizeImages(p.images, p.inputItems)
}

func (p *chatPlan) Images() []Image {
	if p == nil || len(p.images) == 0 {
		return nil
	}
	images := make([]Image, len(p.images))
	copy(images, p.images)
	for i := range images {
		images[i].location = imageLocation{}
	}
	return images
}

func (p *chatPlan) Groups() []PromptGroup {
	if p == nil || len(p.groups) == 0 {
		return nil
	}
	groups := make([]PromptGroup, len(p.groups))
	for i := range p.groups {
		groups[i] = p.groups[i]
		if p.groups[i].Tool != nil {
			tool := *p.groups[i].Tool
			groups[i].Tool = &tool
		}
		groups[i].locationKind = 0
		groups[i].Images = append([]Image(nil), p.groups[i].Images...)
		for j := range groups[i].Images {
			groups[i].Images[j].location = imageLocation{}
		}
	}
	return groups
}

func (p *chatPlan) HasImages() bool { return p != nil && len(p.images) > 0 }

// RewriteGroupsText replaces each image_url block in a fresh JSON tree. All
// result validation happens before cloning, so a bad VLM result cannot leave a
// partially rewritten request behind.
func (p *chatPlan) RewriteGroupsText(results []string) ([]byte, error) {
	if p == nil {
		return nil, plannerError(ErrorMalformedRequest, 400, "nil chat plan", "body")
	}
	if len(p.groups) == 0 {
		return append([]byte(nil), p.original...), nil
	}
	if len(results) != len(p.groups) {
		return nil, plannerError(ErrorMissingResult, 502, "one VLM result is required for each prompt group", "messages")
	}

	type groupResult struct {
		group PromptGroup
		text  string
	}
	byMessage := make(map[int]groupResult, len(p.groups))
	for i := range p.groups {
		text := strings.TrimSpace(redactGroupText(results[i], p.images))
		if text == "" {
			return nil, plannerError(ErrorInvalidResult, 502, fmt.Sprintf("VLM group result %d is empty", i+1), "messages")
		}
		if p.options.MaxResultChars > 0 && runeLen(text) > p.options.MaxResultChars {
			return nil, limitExceeded(LimitVLMResult, runeLen(text), p.options.MaxResultChars, "VLM result exceeds configured limit", "messages")
		}
		byMessage[p.groups[i].InputIndex] = groupResult{group: p.groups[i], text: text}
	}

	root, err := cloneJSON(p.root)
	if err != nil {
		return nil, plannerError(ErrorRewriteVerification, 500, "failed to clone request body", "body")
	}
	object, ok := root.(map[string]any)
	if !ok {
		return nil, plannerError(ErrorRewriteVerification, 500, "Chat request must be a JSON object", "body")
	}
	messages, ok := object["messages"].([]any)
	if !ok {
		return nil, plannerError(ErrorRewriteVerification, 500, "rewritten Chat request has no messages array", "messages")
	}

	replaced := 0
	for messageIndex, rawMessage := range messages {
		result, hasGroup := byMessage[messageIndex]
		if !hasGroup {
			continue
		}
		messageObject, ok := rawMessage.(map[string]any)
		if !ok {
			return nil, plannerError(ErrorRewriteVerification, 500, "message changed before rewrite", fmt.Sprintf("messages[%d]", messageIndex))
		}
		content, ok := messageObject["content"].([]any)
		if !ok {
			return nil, plannerError(ErrorRewriteVerification, 500, "image-bearing message content changed before rewrite", fmt.Sprintf("messages[%d].content", messageIndex))
		}
		localNumber := 0
		for blockIndex, rawBlock := range content {
			blockObject, ok := rawBlock.(map[string]any)
			if !ok {
				return nil, plannerError(ErrorRewriteVerification, 500, "message content block changed before rewrite", fmt.Sprintf("messages[%d].content[%d]", messageIndex, blockIndex))
			}
			typeName, _ := blockObject["type"].(string)
			if typeName != "image_url" {
				continue
			}
			if localNumber >= len(result.group.Images) {
				return nil, plannerError(ErrorRewriteVerification, 500, "message image count changed before rewrite", fmt.Sprintf("messages[%d].content[%d]", messageIndex, blockIndex))
			}
			localNumber++
			content[blockIndex] = map[string]any{
				"type": "text",
				"text": imageReplacementMarker(result.group, localNumber),
			}
			replaced++
		}
		if localNumber != len(result.group.Images) {
			return nil, plannerError(ErrorRewriteVerification, 500, "message image locations changed before rewrite", fmt.Sprintf("messages[%d].content", messageIndex))
		}
		content = append(content, map[string]any{
			"type": "text",
			"text": RenderGroupResult(result.group, result.text),
		})
		messageObject["content"] = content
	}
	if replaced != len(p.images) {
		return nil, plannerError(ErrorRewriteVerification, 500, "not all discovered images were rewritten", "messages")
	}
	redactAnalyzedAttachmentPaths(root, attachmentPathsToRedact(root, p.allowCodexAttachmentPaths))
	sanitizeAttachmentTree(root, p.allowCodexAttachmentPaths)
	body, err := json.Marshal(root)
	if err != nil {
		return nil, plannerError(ErrorRewriteVerification, 500, "rewritten request could not be encoded", "body")
	}
	check, err := discoverChat(body, p.options)
	if err != nil || check.HasImages() {
		return nil, plannerError(ErrorRewriteVerification, 500, "rewritten request still contains an image_url", "body")
	}
	return body, nil
}
