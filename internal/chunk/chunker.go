package chunk

import (
	"strings"
	"unicode/utf8"
)

// Chunk splits text into overlapping windows of approximately size runes with
// overlap runes of overlap between consecutive windows. The split respects
// rune boundaries and prefers paragraph/sentence/word breaks when one is
// available within the trailing 10% of the window.
func Chunk(text string, size, overlap int) []string {
	if size <= 0 {
		size = 1200
	}
	if overlap < 0 || overlap >= size {
		overlap = size / 6
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	runes := []rune(text)
	if len(runes) <= size {
		return []string{strings.TrimSpace(text)}
	}

	out := make([]string, 0, len(runes)/size+1)
	pos := 0
	for pos < len(runes) {
		end := pos + size
		if end >= len(runes) {
			out = append(out, strings.TrimSpace(string(runes[pos:])))
			break
		}

		// Try to snap end backwards to a sensible break inside the last 15%.
		minSnap := pos + (size * 85 / 100)
		snap := -1
		for _, sep := range []string{"\n\n", "\n", ". ", "? ", "! ", "; ", " "} {
			if i := lastIndexRune(runes, sep, minSnap, end); i > snap {
				snap = i + utf8.RuneCountInString(sep)
			}
		}
		if snap > pos {
			end = snap
		}

		out = append(out, strings.TrimSpace(string(runes[pos:end])))
		pos = end - overlap
		if pos < 0 {
			pos = 0
		}
	}
	return out
}

func lastIndexRune(runes []rune, sep string, from, to int) int {
	if to > len(runes) {
		to = len(runes)
	}
	if from < 0 {
		from = 0
	}
	if from >= to {
		return -1
	}
	sub := string(runes[from:to])
	i := strings.LastIndex(sub, sep)
	if i < 0 {
		return -1
	}
	// Convert byte index back to rune index relative to the original slice.
	return from + utf8.RuneCountInString(sub[:i])
}
