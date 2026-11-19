package ekho

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxChunkRunes = 160

// nextBoundary returns a byte boundary, preserving incomplete UTF-8 and the
// trailing fragment until more text arrives or the caller flushes. English
// periods wait for lookahead to avoid splitting streamed decimal numbers.
func nextBoundary(text string, flush bool) int {
	count, soft := 0, 0
	for i := 0; i < len(text); {
		if !utf8.FullRuneInString(text[i:]) && !flush {
			return 0
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		end := i + size
		count++
		switch r {
		case '。', '！', '？', '；', '!', '?', ';', '\n':
			return end
		case '.':
			if end < len(text) && utf8.FullRuneInString(text[end:]) {
				next, _ := utf8.DecodeRuneInString(text[end:])
				if unicode.IsSpace(next) || strings.ContainsRune("\"'”’)]}", next) {
					// Common abbreviations and initials are not sentence ends.
					words := strings.Fields(text[:i])
					word := ""
					if len(words) > 0 {
						word = strings.ToLower(words[len(words)-1])
					}
					if !strings.Contains("|mr|mrs|ms|dr|prof|sr|jr|e.g|i.e|", "|"+word+"|") && utf8.RuneCountInString(word) != 1 {
						return end
					}
				}
			}
		case ',', '，', '、', ':', '：', ' ', '\t':
			soft = end
		}
		if count >= maxChunkRunes {
			if soft > 0 {
				return soft
			}
			return end
		}
		i = end
	}
	if flush {
		return len(text)
	}
	return 0
}
