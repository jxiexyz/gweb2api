package gemini

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var (
	reCodeExec    = regexp.MustCompile(`(?s)\x60\x60\x60(?:python|javascript|text)\?code_(?:reference|stdout)&code_event_index=\d+\n.*?\x60\x60\x60\n?`)
	reCardContent = regexp.MustCompile(`http://googleusercontent\.com/card_content/\d+\n?`)
	reBardError   = regexp.MustCompile(`BardErrorInfo\s*\[(\d+)\]`)
)

func CleanText(text string, strip bool) string {
	text = reCodeExec.ReplaceAllString(text, "")
	text = reCardContent.ReplaceAllString(text, "")
	if strip {
		return strings.TrimSpace(text)
	}
	return text
}

func ExtractTextsFromLine(line string) []string {
	if !strings.Contains(line, `"wrb.fr"`) || len(line) < 30 {
		return nil
	}

	var arr []any
	if err := json.Unmarshal([]byte(line), &arr); err != nil || len(arr) == 0 {
		return nil
	}

	firstElem, ok := arr[0].([]any)
	if !ok || len(firstElem) < 3 {
		return nil
	}

	innerStr, ok := firstElem[2].(string)
	if !ok || len(innerStr) < 10 {
		return nil
	}

	var inner []any
	if err := json.Unmarshal([]byte(innerStr), &inner); err != nil {
		return nil
	}

	if len(inner) <= 4 || inner[4] == nil {
		// Fallback for video generation status frames: [null, [conv, resp], {"7": ["I'm generating...", ...]}]
		if len(inner) > 2 && inner[2] != nil {
			if m, ok := inner[2].(map[string]any); ok {
				if v7, ok := m["7"].([]any); ok && len(v7) > 0 {
					if s, ok := v7[0].(string); ok && s != "" {
						return []string{s}
					}
				}
			}
		}
		return nil
	}

	parts, ok := inner[4].([]any)
	if !ok {
		return nil
	}

	var texts []string
	for _, part := range parts {
		partList, ok := part.([]any)
		if ok && len(partList) > 1 && partList[1] != nil {
			tList, ok := partList[1].([]any)
			if ok {
				for _, t := range tList {
					if str, ok := t.(string); ok && str != "" {
						texts = append(texts, str)
					}
				}
			}
		}
	}

	return texts
}

func IsBardError(raw string) (string, bool) {
	match := reBardError.FindStringSubmatch(raw)
	if len(match) > 1 {
		return match[1], true
	}
	return "", false
}

// ParseResult holds the extracted text and optional conversation ID from a Gemini response.
type ParseResult struct {
	Text           string
	ConversationID string
}

func ExtractResponseText(raw string) (string, error) {
	r := ExtractResponse(raw)
	if r.Text == "" {
		if code, ok := IsBardError(raw); ok {
			return "", fmt.Errorf("gemini upstream rejected request: BardErrorInfo [%s]", code)
		}
	}
	return r.Text, nil
}

// ExtractResponse parses the full Gemini StreamGenerate response,
// extracting both the text content and the conversation ID.
func ExtractResponse(raw string) ParseResult {
	var result ParseResult

	lines := strings.Split(raw, "\n")
	for _, line := range lines {
		// Extract conversation ID from inner[1] = [conv_id, resp_id]
		if result.ConversationID == "" {
			result.ConversationID = extractConversationID(line)
		}

		texts := ExtractTextsFromLine(line)
		for _, t := range texts {
			if len(t) > len(result.Text) {
				result.Text = t
			}
		}
	}

	result.Text = CleanText(result.Text, true)
	return result
}

// extractConversationID tries to pull the conversation ID from a wrb.fr response line.
// The Gemini response structure is: [[wrb.fr, ..., innerJSON, ...], ...]
// where innerJSON contains: [null, [conversation_id, response_id], ...]
func extractConversationID(line string) string {
	if !strings.Contains(line, `"wrb.fr"`) || len(line) < 30 {
		return ""
	}

	var arr []any
	if err := json.Unmarshal([]byte(line), &arr); err != nil || len(arr) == 0 {
		return ""
	}

	firstElem, ok := arr[0].([]any)
	if !ok || len(firstElem) < 3 {
		return ""
	}

	innerStr, ok := firstElem[2].(string)
	if !ok || len(innerStr) < 10 {
		return ""
	}

	var inner []any
	if err := json.Unmarshal([]byte(innerStr), &inner); err != nil {
		return ""
	}

	// inner[1] should be [conversation_id, response_id]
	if len(inner) < 2 || inner[1] == nil {
		return ""
	}

	pair, ok := inner[1].([]any)
	if !ok || len(pair) < 1 {
		return ""
	}

	convID, ok := pair[0].(string)
	if !ok || convID == "" {
		return ""
	}

	return convID
}
