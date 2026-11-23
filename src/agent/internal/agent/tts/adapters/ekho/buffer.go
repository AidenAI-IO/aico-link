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
	numberStart := -1
	for i := 0; i < len(text); {
		if !utf8.FullRuneInString(text[i:]) && !flush {
			return 0
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		end := i + size
		count++
		if unicode.IsDigit(r) {
			if numberStart < 0 {
				numberStart = i
			}
		} else if r != '.' {
			numberStart = -1
		}
		switch r {
		case '。', '！', '？', '；', '!', '?', ';', '\n':
			return end
		case '.':
			if end < len(text) && utf8.FullRuneInString(text[end:]) {
				next, _ := utf8.DecodeRuneInString(text[end:])
				if unicode.IsSpace(next) || unicode.Is(unicode.Han, next) || strings.ContainsRune("\"'”’)]}", next) {
					// Common abbreviations and initials are not sentence ends.
					words := strings.Fields(text[:i])
					word := ""
					if len(words) > 0 {
						word = strings.ToLower(words[len(words)-1])
					}
					initial, size := utf8.DecodeRuneInString(word)
					isInitial := size == len(word) && unicode.Is(unicode.Latin, initial)
					if !strings.Contains("|mr|mrs|ms|dr|prof|sr|jr|e.g|i.e|", "|"+word+"|") && !isInitial {
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
			// Keep a trailing number together even when its decimal point or
			// fractional digits have not arrived yet. A number occupying the
			// entire chunk still uses the hard limit to bound synthesis size.
			if numberStart > 0 {
				return numberStart
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
