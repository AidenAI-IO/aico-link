package speech

import (
	"bytes"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	markdownImagePattern = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]*)\)`)
	markdownLinkPattern  = regexp.MustCompile(`\[([^\]]+)\]\(([^)]*)\)`)
)

var (
	ttsStartTags = []string{"<tts>", "[tts]"}
	ttsEndTags   = []string{"</tts>", "[/tts]"}
)

// BuildText returns only text explicitly marked for TTS, normalized for speech.
func BuildText(output string) string {
	speech := ExtractText(output)
	if speech == "" {
		return ""
	}
	return normalizeMarkdownForSpeech(speech)
}

// ExtractText returns text explicitly marked for TTS without speech normalization.
func ExtractText(output string) string {
	var parts []string
	remaining := output
	for {
		start, startTag := asciiIndexFoldAny(remaining, ttsStartTags)
		if start < 0 {
			break
		}
		remaining = remaining[start+len(startTag):]
		end, endTag := asciiIndexFoldAny(remaining, ttsEndTags)
		if end < 0 {
			// Treat the response boundary as an implicit closing tag. The model can
			// occasionally omit </tts>; keeping the explicit opening tag as the
			// authorization boundary avoids silently dropping that speech.
			if text := strings.TrimSpace(trimPartialTTSEndTagSuffix(remaining)); text != "" {
				parts = append(parts, text)
			}
			break
		}
		if text := strings.TrimSpace(remaining[:end]); text != "" {
			parts = append(parts, text)
		}
		remaining = remaining[end+len(endTag):]
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func normalizeMarkdownForSpeech(output string) string {
	lines := strings.Split(output, "\n")
	normalized := make([]string, 0, len(lines))
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if isMarkdownFenceLine(trimmed) {
			inFence = !inFence
			continue
		}
		if !inFence {
			if isMarkdownTableSeparatorLine(trimmed) {
				continue
			}
			trimmed = stripMarkdownLinePrefix(trimmed)
			trimmed = normalizeMarkdownTableRow(trimmed)
			trimmed = normalizeInlineMarkdown(trimmed)
		}
		normalized = append(normalized, trimmed)
	}
	return strings.TrimSpace(strings.Join(normalized, "\n"))
}

func isMarkdownFenceLine(line string) bool {
	return strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~")
}

func isMarkdownTableSeparatorLine(line string) bool {
	if !strings.Contains(line, "|") || !strings.Contains(line, "-") {
		return false
	}
	for _, r := range line {
		if r != '|' && r != '-' && r != ':' && r != ' ' && r != '\t' {
			return false
		}
	}
	return true
}

func stripMarkdownLinePrefix(line string) string {
	line = stripMarkdownBlockquotePrefix(line)
	line = stripMarkdownHeadingPrefix(line)
	line = stripMarkdownListPrefix(line)
	line = stripMarkdownTaskPrefix(line)
	return strings.TrimSpace(line)
}

func stripMarkdownBlockquotePrefix(line string) string {
	for strings.HasPrefix(line, ">") {
		line = strings.TrimSpace(strings.TrimPrefix(line, ">"))
	}
	return line
}

func stripMarkdownHeadingPrefix(line string) string {
	hashes := 0
	for hashes < len(line) && hashes < 6 && line[hashes] == '#' {
		hashes++
	}
	if hashes > 0 && hashes < len(line) && line[hashes] == ' ' {
		return strings.TrimSpace(line[hashes+1:])
	}
	return line
}

func stripMarkdownListPrefix(line string) string {
	if len(line) >= 2 && (line[0] == '-' || line[0] == '*' || line[0] == '+') && line[1] == ' ' {
		return strings.TrimSpace(line[2:])
	}
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i > 0 && i+1 < len(line) && (line[i] == '.' || line[i] == ')') && line[i+1] == ' ' {
		return strings.TrimSpace(line[i+2:])
	}
	return line
}

func stripMarkdownTaskPrefix(line string) string {
	if len(line) >= 4 && line[0] == '[' && line[2] == ']' && line[3] == ' ' {
		marker := line[1]
		if marker == ' ' || marker == 'x' || marker == 'X' {
			return strings.TrimSpace(line[4:])
		}
	}
	return line
}

func normalizeMarkdownTableRow(line string) string {
	if strings.Count(line, "|") < 2 {
		return line
	}
	parts := strings.Split(strings.Trim(line, "|"), "|")
	cells := make([]string, 0, len(parts))
	for _, part := range parts {
		cell := strings.TrimSpace(part)
		if cell != "" {
			cells = append(cells, cell)
		}
	}
	if len(cells) == 0 {
		return ""
	}
	return strings.Join(cells, "，")
}

func normalizeInlineMarkdown(text string) string {
	text = markdownImagePattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := markdownImagePattern.FindStringSubmatch(match)
		alt := strings.TrimSpace(parts[1])
		url := strings.TrimSpace(parts[2])
		if alt == "" && url == "" {
			return ""
		}
		if alt == "" {
			return "图片（" + url + "）"
		}
		if url == "" {
			return "图片：" + alt
		}
		return "图片：" + alt + "（" + url + "）"
	})
	text = markdownLinkPattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := markdownLinkPattern.FindStringSubmatch(match)
		label := strings.TrimSpace(parts[1])
		url := strings.TrimSpace(parts[2])
		if url == "" {
			return label
		}
		return label + "（" + url + "）"
	})
	text = strings.ReplaceAll(text, "~~", "")
	text = strings.ReplaceAll(text, "**", "")
	text = strings.ReplaceAll(text, "__", "")
	text = strings.ReplaceAll(text, "`", "")
	text = strings.ReplaceAll(text, "*", "")
	return strings.TrimSpace(text)
}

type StreamWriter struct {
	target                  io.Writer
	pending                 []byte
	inTTS                   bool
	seenTTS                 bool
	streamTTS               bool
	outsideContent          bool
	emitted                 bool
	finishedResponseReady   bool
	finishedResponseEmitted bool
}

type ttsStreamFlusher interface {
	Flush() error
}

type ttsSpeechBlockStarter interface {
	BeginSpeechBlock()
}

type ttsBufferResetter interface {
	ResetBuffer()
}

func NewStreamWriter(target io.Writer) *StreamWriter {
	return &StreamWriter{target: target}
}

func (w *StreamWriter) ResetStreamState() {
	if w == nil {
		return
	}
	w.resetResponseState()
	w.finishedResponseReady = false
	w.finishedResponseEmitted = false
}

func (w *StreamWriter) resetResponseState() {
	if w == nil {
		return
	}
	w.pending = nil
	w.inTTS = false
	w.seenTTS = false
	w.streamTTS = false
	w.outsideContent = false
	w.emitted = false
}

func (w *StreamWriter) ResetBuffer() {
	if w == nil {
		return
	}
	if resetter, ok := w.target.(ttsBufferResetter); ok {
		resetter.ResetBuffer()
	}
	w.ResetStreamState()
}

func (w *StreamWriter) StreamEmitted() bool {
	return w != nil && w.emitted
}

// FinishResponse reports whether the just-completed LLM response streamed a
// leading TTS block, then resets parser state so the next tool iteration or
// final response can stream its own leading block. If the model omitted the
// closing tag, the response boundary acts as an implicit </tts>: held trailing
// bytes are emitted and the provider is flushed immediately.
func (w *StreamWriter) FinishResponse() bool {
	if w == nil {
		return false
	}
	if w.inTTS && w.streamTTS {
		pending := trimPartialTTSEndTagSuffixBytes(w.pending)
		for len(pending) > 0 {
			safeLen := validUTF8PrefixLen(pending, len(pending))
			if safeLen <= 0 {
				if len(pending) > 0 && utf8.RuneStart(pending[0]) {
					break
				}
				pending = pending[1:]
				continue
			}
			if err := w.writeTTSBytes(pending[:safeLen]); err != nil {
				w.ResetBuffer()
				return false
			}
			pending = pending[safeLen:]
		}
		if flusher, ok := w.target.(ttsStreamFlusher); ok {
			if err := flusher.Flush(); err != nil {
				w.ResetBuffer()
				return false
			}
		}
	}
	emitted := w.StreamEmitted()
	w.resetResponseState()
	w.finishedResponseReady = true
	w.finishedResponseEmitted = emitted
	return emitted
}

// ConsumeFinishedResponse reports the speech result captured at the most
// recent LLM response boundary. The boolean result is false when no boundary
// has been finalized since the previous consume.
func (w *StreamWriter) ConsumeFinishedResponse() (bool, bool) {
	if w == nil || !w.finishedResponseReady {
		return false, false
	}
	emitted := w.finishedResponseEmitted
	w.finishedResponseReady = false
	w.finishedResponseEmitted = false
	return emitted, true
}

// FinalizeResponse returns the speech result for the current response, or the
// result already captured when the runtime finalized that response boundary.
func (w *StreamWriter) FinalizeResponse() bool {
	if w == nil {
		return false
	}
	if emitted, ok := w.ConsumeFinishedResponse(); ok {
		return emitted
	}
	return w.FinishResponse()
}

func (w *StreamWriter) Write(p []byte) (int, error) {
	if w == nil || w.target == nil || len(p) == 0 {
		return len(p), nil
	}
	w.pending = append(w.pending, p...)
	for len(w.pending) > 0 {
		if !w.inTTS {
			idx, startTag := asciiIndexFoldBytesAny(w.pending, ttsStartTags)
			if idx < 0 {
				searchSuffixLen := longestTagLen(ttsStartTags) - 1
				if discardLen := len(w.pending) - searchSuffixLen; discardLen > 0 {
					w.markOutsideContent(w.pending[:discardLen])
				}
				w.pending = keepTagSearchSuffix(w.pending, searchSuffixLen)
				return len(p), nil
			}
			w.markOutsideContent(w.pending[:idx])
			w.pending = w.pending[idx+len(startTag):]
			w.inTTS = true
			// A new leading block owns a fresh speech output. Tool-call completion
			// resets response state before the next model iteration.
			w.streamTTS = !w.outsideContent
			w.seenTTS = true
			if w.streamTTS {
				if starter, ok := w.target.(ttsSpeechBlockStarter); ok {
					starter.BeginSpeechBlock()
				}
			}
			continue
		}

		idx, endTag := asciiIndexFoldBytesAny(w.pending, ttsEndTags)
		if idx >= 0 {
			if w.streamTTS {
				// Find the longest valid UTF-8 prefix before the end tag.
				skipBytes := 0
				for idx > skipBytes {
					safeIdx := validUTF8PrefixLen(w.pending[skipBytes:], idx-skipBytes)
					if safeIdx > 0 {
						if err := w.writeTTSBytes(w.pending[skipBytes : skipBytes+safeIdx]); err != nil {
							return 0, err
						}
						break
					}
					// Skip this invalid leading byte.
					skipBytes++
				}
			}
			w.pending = w.pending[idx+len(endTag):]
			w.inTTS = false
			if w.streamTTS {
				if flusher, ok := w.target.(ttsStreamFlusher); ok {
					if err := flusher.Flush(); err != nil {
						return 0, err
					}
				}
			}
			w.streamTTS = false
			w.outsideContent = false
			continue
		}

		safeLen := len(w.pending) - (longestTagLen(ttsEndTags) - 1)
		if safeLen <= 0 {
			return len(p), nil
		}
		if !w.streamTTS {
			w.pending = append(w.pending[:0], w.pending[safeLen:]...)
			return len(p), nil
		}
		safeLen = validUTF8PrefixLen(w.pending, safeLen)
		if safeLen <= 0 {
			// Check if the first byte is a valid rune start
			if len(w.pending) > 0 && utf8.RuneStart(w.pending[0]) {
				// Incomplete multi-byte sequence; keep it and wait for more data
				return len(p), nil
			}
			// Invalid byte; drop it to avoid stalling
			w.pending = w.pending[1:]
			continue
		}
		if err := w.writeTTSBytes(w.pending[:safeLen]); err != nil {
			return 0, err
		}
		w.pending = append(w.pending[:0], w.pending[safeLen:]...)
		return len(p), nil
	}
	return len(p), nil
}

func (w *StreamWriter) markOutsideContent(p []byte) {
	if w.outsideContent {
		return
	}
	for _, b := range p {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			w.outsideContent = true
			return
		}
	}
}

func validUTF8PrefixLen(buf []byte, n int) int {
	if n > len(buf) {
		n = len(buf)
	}
	if n <= 0 {
		return 0
	}

	// Fast path: if already valid UTF-8, return immediately
	if utf8.Valid(buf[:n]) {
		return n
	}

	// Slow path: find the last complete rune boundary within the last UTFMax bytes
	start := n - 1
	lower := n - utf8.UTFMax
	if lower < 0 {
		lower = 0
	}
	for start >= lower && !utf8.RuneStart(buf[start]) {
		start--
	}

	// If we found a rune start and there's valid content before it, truncate there
	if start >= lower {
		if start > 0 && utf8.Valid(buf[:start]) {
			return start
		}
		// The rune start we found is at position 0 or itself part of invalid sequence
		// Keep scanning backwards to find a valid boundary (skip position 0)
		for i := start - 1; i > 0; i-- {
			if utf8.Valid(buf[:i]) {
				return i
			}
		}
		// If we're here, start is 0 or all positions checked are invalid
		// Check if position 0 itself is valid
		if start == 0 && utf8.Valid(buf[:1]) {
			return 1
		}
		return 0
	}

	// No rune start found in scan window - scan entire buffer for last valid boundary
	for i := n - 1; i > 0; i-- {
		if utf8.Valid(buf[:i]) {
			return i
		}
	}
	// Last resort: check if first byte is valid
	if utf8.Valid(buf[:1]) {
		return 1
	}
	return 0
}

func (w *StreamWriter) writeTTSBytes(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	n, err := w.target.Write(p)
	if n > 0 {
		w.emitted = true
	}
	return err
}

func keepTagSearchSuffix(buf []byte, max int) []byte {
	if max <= 0 || len(buf) <= max {
		return buf
	}
	return append(buf[:0], buf[len(buf)-max:]...)
}

func trimPartialTTSEndTagSuffix(text string) string {
	trimmed := trimPartialTTSEndTagSuffixBytes([]byte(text))
	return string(trimmed)
}

func trimPartialTTSEndTagSuffixBytes(buf []byte) []byte {
	for _, tag := range ttsEndTags {
		maxLen := len(tag) - 1
		if len(buf) < maxLen {
			maxLen = len(buf)
		}
		for suffixLen := maxLen; suffixLen > 0; suffixLen-- {
			start := len(buf) - suffixLen
			if asciiIndexFold(string(buf[start:]), tag[:suffixLen]) == 0 {
				return buf[:start]
			}
		}
	}
	return buf
}

func asciiIndexFoldAny(s string, tags []string) (int, string) {
	for i := 0; i < len(s); i++ {
		for _, tag := range tags {
			if i+len(tag) <= len(s) && asciiIndexFold(s[i:i+len(tag)], tag) == 0 {
				return i, tag
			}
		}
	}
	return -1, ""
}

func asciiIndexFoldBytesAny(buf []byte, tags []string) (int, string) {
	lower := asciiLowerBytes(buf)
	for i := 0; i < len(lower); i++ {
		for _, tag := range tags {
			if i+len(tag) <= len(lower) && bytes.Equal(lower[i:i+len(tag)], []byte(tag)) {
				return i, tag
			}
		}
	}
	return -1, ""
}

func longestTagLen(tags []string) int {
	longest := 0
	for _, tag := range tags {
		if len(tag) > longest {
			longest = len(tag)
		}
	}
	return longest
}

func asciiIndexFold(s, substr string) int {
	if substr == "" {
		return 0
	}
	last := len(s) - len(substr)
	for i := 0; i <= last; i++ {
		matched := true
		for j := 0; j < len(substr); j++ {
			if asciiLowerByte(s[i+j]) != asciiLowerByte(substr[j]) {
				matched = false
				break
			}
		}
		if matched {
			return i
		}
	}
	return -1
}

func asciiLowerBytes(buf []byte) []byte {
	lower := make([]byte, len(buf))
	for i, b := range buf {
		lower[i] = asciiLowerByte(b)
	}
	return lower
}

func asciiLowerByte(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}
