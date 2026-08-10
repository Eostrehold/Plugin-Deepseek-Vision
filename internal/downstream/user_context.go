package downstream

import "strings"

// positionedUserText is one visible user-text fragment in protocol traversal
// order. Protocol adapters own position assignment because their wire
// containers differ, while userContextIndex owns selection semantics.
type positionedUserText struct {
	pos  int
	text string
}

type userContextTurn struct {
	item  int
	texts []positionedUserText
}

// userContextIndex is a request-local index of ordinary user turns. Empty turns
// are retained so latestBefore never leaks stale context across a newer user
// boundary. Image-container-local text remains protocol-specific.
type userContextIndex struct {
	turns      []userContextTurn
	turnByItem map[int]int
}

func (i *userContextIndex) recordTurn(item int, texts ...positionedUserText) {
	if i == nil {
		return
	}
	filtered := make([]positionedUserText, 0, len(texts))
	for _, text := range texts {
		if strings.TrimSpace(text.text) == "" {
			continue
		}
		filtered = append(filtered, text)
	}
	if turnIndex, exists := i.turnByItem[item]; exists {
		i.turns[turnIndex].texts = append(i.turns[turnIndex].texts, filtered...)
		return
	}
	if i.turnByItem == nil {
		i.turnByItem = make(map[int]int)
	}
	i.turnByItem[item] = len(i.turns)
	i.turns = append(i.turns, userContextTurn{item: item, texts: filtered})
}

func (i *userContextIndex) itemText(item, maxChars int) string {
	if i == nil {
		return ""
	}
	turnIndex, exists := i.turnByItem[item]
	if !exists {
		return ""
	}
	return joinPositionedUserText(i.turns[turnIndex].texts, maxChars)
}

func (i *userContextIndex) nearest(item, pos, maxChars int, preferSameItem bool) string {
	if i == nil {
		return ""
	}
	selectNearest := func(sameItemOnly bool) (positionedUserText, bool) {
		var best positionedUserText
		found := false
		bestDistance := 0
		for _, turn := range i.turns {
			if sameItemOnly && turn.item != item {
				continue
			}
			for _, candidate := range turn.texts {
				distance := abs(candidate.pos - pos)
				if !found || distance < bestDistance || (distance == bestDistance && candidate.pos < best.pos) {
					best = candidate
					bestDistance = distance
					found = true
				}
			}
		}
		return best, found
	}
	if preferSameItem {
		if best, found := selectNearest(true); found {
			return truncateRunes(strings.TrimSpace(best.text), maxChars)
		}
	}
	best, found := selectNearest(false)
	if !found {
		return ""
	}
	return truncateRunes(strings.TrimSpace(best.text), maxChars)
}

func (i *userContextIndex) latestBefore(item, maxChars int) string {
	if i == nil {
		return ""
	}
	latest := -1
	for turnIndex := range i.turns {
		if i.turns[turnIndex].item < item && (latest < 0 || i.turns[turnIndex].item > i.turns[latest].item) {
			latest = turnIndex
		}
	}
	if latest < 0 {
		return ""
	}
	return joinPositionedUserText(i.turns[latest].texts, maxChars)
}

func (i *userContextIndex) lastTurnIndex() int {
	if i == nil {
		return -1
	}
	last := -1
	for _, turn := range i.turns {
		if turn.item > last {
			last = turn.item
		}
	}
	return last
}

func joinPositionedUserText(texts []positionedUserText, maxChars int) string {
	parts := make([]string, 0, len(texts))
	for _, text := range texts {
		if trimmed := strings.TrimSpace(text.text); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return truncateRunes(strings.Join(parts, "\n\n"), maxChars)
}
