package mcp

import (
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
)

// messageMatch is the unified excerpt shape for search_in_message,
// search_message_bodies, and vector/hybrid search_messages. char_offset
// is a byte offset into raw body_text for get_message center_at.
// score is set for vector chunk matches only.
type messageMatch struct {
	CharOffset int      `json:"char_offset"`
	Snippet    string   `json:"snippet"`
	Line       int      `json:"line"`
	Score      *float64 `json:"score,omitempty"`
}

func embedPreprocessConfig(cfg vector.Config) embed.PreprocessConfig {
	return embed.PreprocessConfig{
		StripQuotes:        cfg.Preprocess.StripQuotesEnabled(),
		StripSignatures:    cfg.Preprocess.StripSignaturesEnabled(),
		StripHTML:          cfg.Preprocess.StripHTMLEnabled(),
		StripBase64:        cfg.Preprocess.StripBase64Enabled(),
		StripURLTracking:   cfg.Preprocess.StripURLTrackingEnabled(),
		CollapseWhitespace: cfg.Preprocess.CollapseWhitespaceEnabled(),
	}
}

func preprocessedEmbedText(subject, body string, cfg vector.Config) string {
	txt, _ := embed.Preprocess(subject, body, 0, embedPreprocessConfig(cfg))
	return txt
}

func subjectPrefixRuneCount(subject string) int {
	if subject == "" {
		return 0
	}
	return utf8.RuneCountInString("Subject: " + subject + "\n\n")
}

func runeSliceByOffsets(s string, startRune, endRune int) string {
	if s == "" || startRune < 0 || endRune <= startRune {
		return ""
	}
	startByte := runeOffsetToByteOffset(s, startRune)
	endByte := runeOffsetToByteOffset(s, endRune)
	if startByte >= len(s) {
		return ""
	}
	if endByte > len(s) {
		endByte = len(s)
	}
	return s[startByte:endByte]
}

func runeOffsetToByteOffset(s string, runeOffset int) int {
	if runeOffset <= 0 {
		return 0
	}
	walked := 0
	for i := range s {
		if walked >= runeOffset {
			return i
		}
		walked++
	}
	return len(s)
}

func chunkHitsToMatches(
	preprocessed, body string,
	prefixRunes int,
	hits []vector.ChunkHit,
	minScore float64,
	maxMatches int,
) ([]messageMatch, bool) {
	if len(hits) == 0 || maxMatches <= 0 {
		return nil, false
	}
	var matches []messageMatch
	for _, h := range hits {
		if h.Score < minScore {
			continue
		}
		chunkText := runeSliceByOffsets(preprocessed, h.ChunkCharStart, h.ChunkCharEnd)
		if chunkText == "" {
			continue
		}
		start, end := contextWindow(len(chunkText), 0, 0, searchContextChars)
		snip := bodyByteSlice(chunkText, start, end)

		bodyRuneStart := h.ChunkCharStart - prefixRunes
		charOff := 0
		if bodyRuneStart >= 0 {
			charOff = runeOffsetToByteOffset(body, bodyRuneStart)
		}

		score := h.Score
		matches = append(matches, messageMatch{
			CharOffset: charOff,
			Snippet:    snip,
			Line:       lineNumberAt(body, charOff),
			Score:      &score,
		})
		if len(matches) >= maxMatches {
			qualifying := countAboveMin(hits, minScore)
			return matches, qualifying > maxMatches
		}
	}
	qualifying := countAboveMin(hits, minScore)
	return matches, qualifying > len(matches)
}

func countAboveMin(hits []vector.ChunkHit, minScore float64) int {
	n := 0
	for _, h := range hits {
		if h.Score >= minScore {
			n++
		}
	}
	return n
}

func extractContextMatches(body string, terms []string, contextChars int) []messageMatch {
	snippets := extractContextChar(body, terms, contextChars)
	if len(snippets) == 0 {
		return nil
	}
	// Re-walk terms to attach char_offset/line for each merged snippet window.
	lowerBody := strings.ToLower(body)
	var matches []messageMatch
	seen := make(map[int]struct{})
	for _, term := range terms {
		if len(term) < 2 {
			continue
		}
		lowerTerm := strings.ToLower(term)
		termLen := len(term)
		searchFrom := 0
		for {
			idx := strings.Index(lowerBody[searchFrom:], lowerTerm)
			if idx < 0 {
				break
			}
			pos := searchFrom + idx
			searchFrom = pos + 1
			start, end := contextWindow(len(body), pos, termLen, contextChars)
			if _, ok := seen[start]; ok {
				continue
			}
			seen[start] = struct{}{}
			matches = append(matches, messageMatch{
				CharOffset: pos,
				Snippet:    bodyByteSlice(body, start, end),
				Line:       lineNumberAt(body, pos),
			})
		}
	}
	if len(matches) == 0 {
		for _, snip := range snippets {
			matches = append(matches, messageMatch{Snippet: snip, Line: 1})
		}
	}
	return matches
}

func floatArg(args map[string]any, key string, def float64) float64 {
	v, ok := args[key].(float64)
	if !ok {
		return def
	}
	return v
}
