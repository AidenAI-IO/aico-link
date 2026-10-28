package speech

import (
	"strings"
	"testing"
	"unicode/utf8"
)

type resettableSpeechSink struct {
	strings.Builder
	resetCalls int
}

func (s *resettableSpeechSink) ResetBuffer() {
	s.resetCalls++
	s.Builder.Reset()
}

type flushingSpeechSink struct {
	strings.Builder
	flushCalls     int
	flushedContent []string
}

func (s *flushingSpeechSink) Flush() error {
	s.flushCalls++
	s.flushedContent = append(s.flushedContent, s.String())
	return nil
}

type validatingSpeechChunkSink struct {
	parts   []string
	invalid [][]byte
}

func (s *validatingSpeechChunkSink) Write(p []byte) (int, error) {
	if !utf8.Valid(p) {
		s.invalid = append(s.invalid, append([]byte(nil), p...))
	}
	s.parts = append(s.parts, string(p))
	return len(p), nil
}

func (s *validatingSpeechChunkSink) String() string {
	return strings.Join(s.parts, "")
}

func TestBuildSpeechTextExtractsTTSTag(t *testing.T) {
	output := strings.Join([]string{
		"已完成设置，当前音量是 42。",
		"",
		"详细信息如下：",
		"- 我先打开了设置。",
		"- 然后读取了音量状态。",
		"- 最后确认没有继续修改。",
		"",
		"<tts>",
		"已完成设置，请查收。",
		"</tts>",
	}, "\n")

	speech := BuildText(output)

	if speech != "已完成设置，请查收。" {
		t.Fatalf("speech = %q", speech)
	}
}

func TestBuildSpeechTextExtractsBracketAndMixedTTSTags(t *testing.T) {
	for _, tc := range []struct {
		output string
		want   string
	}{
		{output: "[tts]方括号标签。[/tts]", want: "方括号标签。"},
		{output: "[tts]混用结束标签。</tts>", want: "混用结束标签。"},
		{output: "<tts>混用结束标签。[/tts]", want: "混用结束标签。"},
	} {
		if got := BuildText(tc.output); got != tc.want {
			t.Fatalf("BuildSpeechText(%q) = %q, want %q", tc.output, got, tc.want)
		}
	}
}

func TestBuildSpeechTextNormalizesMarkdownInsideTTSTag(t *testing.T) {
	output := strings.Join([]string{
		"正文不进入播报。",
		"<tts>",
		"**重点**：已完成 `audio_service` 检查。",
		"- 当前音量是 **42**。",
		"详情见 [PR #237](https://github.com/AidenAI-IO/aico-link/pull/237)。",
		"</tts>",
	}, "\n")

	speech := BuildText(output)
	want := strings.Join([]string{
		"重点：已完成 audio_service 检查。",
		"当前音量是 42。",
		"详情见 PR #237（https://github.com/AidenAI-IO/aico-link/pull/237）。",
	}, "\n")

	if speech != want {
		t.Fatalf("speech = %q, want %q", speech, want)
	}
}

func TestBuildSpeechTextReturnsEmptyWithoutTTSTag(t *testing.T) {
	output := "<think>\n需要查当前时间。\n</think>"

	speech := BuildText(output)

	if speech != "" {
		t.Fatalf("speech = %q, want empty without tts tag", speech)
	}
}

func TestBuildSpeechTextJoinsMultipleTTSTags(t *testing.T) {
	output := strings.Join([]string{
		"<tts>第一句。</tts>",
		"中间正文。",
		"<tts>第二句。</tts>",
	}, "\n")

	speech := BuildText(output)
	want := strings.Join([]string{
		"第一句。",
		"第二句。",
	}, "\n")

	if speech != want {
		t.Fatalf("speech = %q, want %q", speech, want)
	}
}

func TestBuildSpeechTextTreatsResponseEndAsUnclosedTTSTagFallback(t *testing.T) {
	output := "<tts>已经完成。"

	if got := BuildText(output); got != "已经完成。" {
		t.Fatalf("BuildSpeechText() = %q, want unclosed TTS content", got)
	}
}

func TestBuildSpeechTextDropsPartialClosingTagAtResponseEnd(t *testing.T) {
	output := "<tts>已经完成。</tt"

	if got := BuildText(output); got != "已经完成。" {
		t.Fatalf("BuildSpeechText() = %q, want partial closing tag removed", got)
	}
}

func TestBuildSpeechTextMatchesTTSTagsWithByteStableOffsets(t *testing.T) {
	output := "可见正文 İ K <TTS>K value</TTS> 尾部正文"

	speech := BuildText(output)

	if speech != "K value" {
		t.Fatalf("speech = %q, want byte-stable TTS extraction", speech)
	}
}

func TestSpeechStreamWriterExtractsPartialTTSTag(t *testing.T) {
	var sink strings.Builder
	writer := NewStreamWriter(&sink)

	chunks := []string{
		"\n<t",
		"ts>已完成",
		"，当前音量是 42。</tts>\n正文继续。",
	}
	for _, chunk := range chunks {
		if _, err := writer.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write(%q) error = %v", chunk, err)
		}
	}

	if got := sink.String(); got != "已完成，当前音量是 42。" {
		t.Fatalf("streamed speech = %q", got)
	}
}

func TestSpeechStreamWriterExtractsBracketAndMixedTTSTags(t *testing.T) {
	for _, tc := range []struct {
		payload string
		want    string
	}{
		{payload: "[tts]方括号标签。[/tts]", want: "方括号标签。"},
		{payload: "[tts]混用结束标签。</tts>", want: "混用结束标签。"},
		{payload: "<tts>混用结束标签。[/tts]", want: "混用结束标签。"},
	} {
		var sink strings.Builder
		writer := NewStreamWriter(&sink)
		if _, err := writer.Write([]byte(tc.payload)); err != nil {
			t.Fatalf("Write(%q) error = %v", tc.payload, err)
		}
		if got := sink.String(); got != tc.want {
			t.Fatalf("streamed speech = %q, want %q", got, tc.want)
		}
	}
}

func TestSpeechStreamWriterIgnoresTextOutsideTTSTag(t *testing.T) {
	var sink strings.Builder
	writer := NewStreamWriter(&sink)

	if _, err := writer.Write([]byte("<tts>播报这个。</tts>尾部不播报。")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	if got := sink.String(); got != "播报这个。" {
		t.Fatalf("streamed speech = %q", got)
	}
}

func TestSpeechStreamWriterLeavesTrailingTTSTagForToolEventSpeech(t *testing.T) {
	sink := &flushingSpeechSink{}
	writer := NewStreamWriter(sink)

	if _, err := writer.Write([]byte("正在检查音量。<tts>正在检查音量。</tts>")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if got := sink.String(); got != "" {
		t.Fatalf("streamed speech = %q, want trailing TTS block handled outside stream", got)
	}
	if sink.flushCalls != 0 {
		t.Fatalf("Flush() calls = %d, want 0 for trailing TTS block", sink.flushCalls)
	}
}

func TestSpeechStreamWriterFlushesAtClosingTagBeforeTrailingText(t *testing.T) {
	sink := &flushingSpeechSink{}
	writer := NewStreamWriter(sink)

	chunks := []string{
		"<tts>好的，",
		"已经完成。</tt",
		"s>下面是详细说明。",
	}
	for _, chunk := range chunks {
		if _, err := writer.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write(%q) error = %v", chunk, err)
		}
	}

	if got := sink.String(); got != "好的，已经完成。" {
		t.Fatalf("streamed speech = %q", got)
	}
	if sink.flushCalls != 1 {
		t.Fatalf("Flush() calls = %d, want 1", sink.flushCalls)
	}
	if got := sink.flushedContent[0]; got != "好的，已经完成。" {
		t.Fatalf("content at Flush() = %q", got)
	}
}

func TestSpeechStreamWriterHandlesSplitUTF8Rune(t *testing.T) {
	var sink strings.Builder
	writer := NewStreamWriter(&sink)
	payload := []byte(`<tts>好</tts>`)
	split := strings.Index(string(payload), "好")
	if split < 0 {
		t.Fatal("test payload missing split rune")
	}
	if _, err := writer.Write(payload[:split+1]); err != nil {
		t.Fatalf("Write(first) error = %v", err)
	}
	if _, err := writer.Write(payload[split+1:]); err != nil {
		t.Fatalf("Write(second) error = %v", err)
	}
	if got := sink.String(); got != "好" {
		t.Fatalf("streamed speech = %q", got)
	}
}

func TestSpeechStreamWriterKeepsUTF8BoundaryWhenHoldingEndTagSuffix(t *testing.T) {
	var sink validatingSpeechChunkSink
	writer := NewStreamWriter(&sink)
	chunks := []string{
		"<tts",
		">上海",
		"现在天气",
		"晴朗，",
		"气温3",
		"5.",
		"8度",
		"。</tts>",
	}
	for _, chunk := range chunks {
		if _, err := writer.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write(%q) error = %v", chunk, err)
		}
	}

	if len(sink.invalid) != 0 {
		t.Fatalf("streamed speech contains invalid UTF-8 chunks: %q", sink.invalid)
	}
	got := sink.String()
	if !utf8.ValidString(got) {
		t.Fatalf("streamed speech is not valid UTF-8: %q", got)
	}
	if got != "上海现在天气晴朗，气温35.8度。" {
		t.Fatalf("streamed speech = %q", got)
	}
}

func TestValidUTF8PrefixLenOnlyTruncatesIncompleteTrailingRune(t *testing.T) {
	tests := []struct {
		name string
		buf  []byte
		n    int
		want int
	}{
		{name: "ascii", buf: []byte("hello"), n: 5, want: 5},
		{name: "complete multibyte", buf: []byte("好"), n: len([]byte("好")), want: len([]byte("好"))},
		{name: "incomplete multibyte suffix", buf: []byte{'a', 0xe4, 0xb8}, n: 3, want: 1},
		{name: "incomplete lead byte suffix", buf: []byte{'a', 0xe4}, n: 2, want: 1},
		{name: "invalid byte in middle filtered out", buf: []byte{'a', 0xff}, n: 2, want: 1},
		{name: "invalid continuation filtered out", buf: []byte{'a', 0x80}, n: 2, want: 1},
		{name: "invalid middle byte filtered out", buf: []byte{'a', 0xff, 'b'}, n: 3, want: 1},
		{name: "multiple consecutive invalid bytes", buf: []byte{'a', 0xff, 0xfe, 0xfd}, n: 4, want: 1},
		{name: "incomplete 3-byte sequence", buf: []byte("hello世"), n: len([]byte("hello")) + 2, want: len([]byte("hello"))},
		{name: "incomplete 4-byte sequence", buf: []byte{0xf0, 0x9f, 0x98}, n: 3, want: 0},
		{name: "all invalid returns zero", buf: []byte{0xff, 0xfe, 0xfd}, n: 3, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validUTF8PrefixLen(tt.buf, tt.n); got != tt.want {
				t.Fatalf("validUTF8PrefixLen(%v, %d) = %d, want %d", tt.buf, tt.n, got, tt.want)
			}
		})
	}
}

func TestSpeechStreamWriterFiltersInvalidUTF8BeforeStreaming(t *testing.T) {
	var sink validatingSpeechChunkSink
	writer := NewStreamWriter(&sink)

	// Write chunk with invalid UTF-8 byte in the middle
	if _, err := writer.Write([]byte{'<', 't', 't', 's', '>', 'a', 'b', 0xff, 'c', 'd', 'e', 'f'}); err != nil {
		t.Fatalf("Write(first) error = %v", err)
	}
	// Write second chunk that should trigger emission of the first valid prefix
	if _, err := writer.Write([]byte("ghijk</tts>")); err != nil {
		t.Fatalf("Write(second) error = %v", err)
	}

	// All streamed chunks must be valid UTF-8
	if len(sink.invalid) != 0 {
		t.Fatalf("streamed speech contains invalid UTF-8 chunks: %q", sink.invalid)
	}

	// The invalid byte should be filtered out, but valid content should flow through
	got := sink.String()
	if !utf8.ValidString(got) {
		t.Fatalf("final streamed speech is not valid UTF-8: %q", got)
	}
	// Should contain the valid prefix before the invalid byte
	if !strings.Contains(got, "ab") {
		t.Fatalf("streamed speech missing valid prefix: %q", got)
	}
	// The valid content after the invalid byte should also be preserved
	if !strings.Contains(got, "cdefghijk") {
		t.Fatalf("streamed speech missing valid suffix: %q", got)
	}
}

func TestSpeechStreamWriterStreamsOnlyFirstLeadingTTSTag(t *testing.T) {
	sink := &flushingSpeechSink{}
	writer := NewStreamWriter(sink)
	payload := `<tts>第一句。</tts> <tts>第二句。</tts>正文`
	if _, err := writer.Write([]byte(payload)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if got := sink.String(); got != "第一句。" {
		t.Fatalf("streamed speech = %q", got)
	}
	if sink.flushCalls != 1 {
		t.Fatalf("Flush() calls = %d, want 1", sink.flushCalls)
	}
}

func TestSpeechStreamWriterFinishResponseAllowsNextLeadingTTSTag(t *testing.T) {
	sink := &flushingSpeechSink{}
	writer := NewStreamWriter(sink)

	if _, err := writer.Write([]byte("<tts>工具进度。</tts>正文")); err != nil {
		t.Fatalf("Write(tool progress) error = %v", err)
	}
	if !writer.FinishResponse() {
		t.Fatal("FinishResponse() = false, want streamed tool progress")
	}
	if writer.StreamEmitted() {
		t.Fatal("StreamEmitted() = true after FinishResponse")
	}

	if _, err := writer.Write([]byte("<tts>最终回复。</tts>正文")); err != nil {
		t.Fatalf("Write(final response) error = %v", err)
	}
	if got := sink.String(); got != "工具进度。最终回复。" {
		t.Fatalf("streamed speech = %q", got)
	}
	if sink.flushCalls != 2 {
		t.Fatalf("Flush() calls = %d, want 2", sink.flushCalls)
	}
}

func TestSpeechStreamWriterFinishResponseFlushesUnclosedLeadingTTS(t *testing.T) {
	sink := &flushingSpeechSink{}
	writer := NewStreamWriter(sink)

	if _, err := writer.Write([]byte("<tts>好的，已经完成。")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if !writer.FinishResponse() {
		t.Fatal("FinishResponse() = false, want unclosed leading TTS streamed")
	}
	if got := sink.String(); got != "好的，已经完成。" {
		t.Fatalf("streamed speech = %q", got)
	}
	if sink.flushCalls != 1 {
		t.Fatalf("Flush() calls = %d, want 1", sink.flushCalls)
	}
}

func TestSpeechStreamWriterFinishResponseDropsPartialClosingTag(t *testing.T) {
	sink := &flushingSpeechSink{}
	writer := NewStreamWriter(sink)

	if _, err := writer.Write([]byte("<tts>好的，已经完成。</tt")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if !writer.FinishResponse() {
		t.Fatal("FinishResponse() = false, want partial closing tag fallback streamed")
	}
	if got := sink.String(); got != "好的，已经完成。" {
		t.Fatalf("streamed speech = %q", got)
	}
	if sink.flushCalls != 1 {
		t.Fatalf("Flush() calls = %d, want 1", sink.flushCalls)
	}
}

func TestSpeechStreamWriterResetBufferClearsParserState(t *testing.T) {
	sink := &resettableSpeechSink{}
	writer := NewStreamWriter(sink)

	if _, err := writer.Write([]byte("<tts>stale speech already emitted")); err != nil {
		t.Fatalf("Write(stale) error = %v", err)
	}
	if !writer.StreamEmitted() {
		t.Fatal("StreamEmitted() = false before reset, want true")
	}

	writer.ResetBuffer()

	if sink.resetCalls != 1 {
		t.Fatalf("ResetBuffer() calls = %d, want 1", sink.resetCalls)
	}
	if writer.StreamEmitted() {
		t.Fatal("StreamEmitted() = true after reset, want false")
	}
	if _, err := writer.Write([]byte("<tts>fresh</tts>")); err != nil {
		t.Fatalf("Write(fresh) error = %v", err)
	}
	if got := sink.String(); got != "fresh" {
		t.Fatalf("streamed speech after reset = %q, want %q", got, "fresh")
	}
}
