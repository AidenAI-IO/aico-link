package agent

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"
)

const (
	AttachmentKindImage = "image"
	AttachmentKindAudio = "audio"

	TurnModalityText  = "text"
	TurnModalitySTT   = "stt"
	TurnModalityAudio = "audio"

	voiceAudioInputPlaceholder = "Voice audio input"
)

type InputAttachment struct {
	Kind     string
	Name     string
	MIMEType string
	Width    int
	Height   int
	Data     []byte
}

type InputArtifact struct {
	Kind       string `json:"kind"`
	Name       string `json:"name,omitempty"`
	MIMEType   string `json:"mime_type,omitempty"`
	Path       string `json:"path,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Size       int64  `json:"size,omitempty"`
	Data       []byte `json:"-"`
}

type TurnInput struct {
	InputText       string
	OriginalText    string
	Modality        string
	Source          string
	Attachments     []InputAttachment
	Transcript      string
	Artifacts       []InputArtifact
	TelemetryEvents []TaskEpisodeEvent
}

type AudioInputResult = TurnInput

func NewSTTClientFromConfig(cfg Config) (STTClient, error) {
	provider := strings.ToLower(strings.TrimSpace(cfg.STT.Provider))
	if provider == "" {
		return nil, nil
	}

	proxyConfig := ProxyConfigFromEnvironment()
	if err := proxyConfig.Validate(); err != nil {
		return nil, fmt.Errorf("proxy environment: %w", err)
	}
	httpClient := newProxyHTTPClient(proxyConfig)
	language := strings.ToLower(strings.TrimSpace(cfg.STT.Language))

	definition, ok := lookupSTTProviderDefinition(provider)
	if !ok {
		return nil, fmt.Errorf("unsupported STT provider: %s", cfg.STT.Provider)
	}
	return definition.build(cfg, proxyConfig, httpClient, language)
}

func NewTextTurnInput(userText string, attachments []InputAttachment) TurnInput {
	trimmedText := strings.TrimSpace(userText)
	return normalizeTurnInput(TurnInput{
		InputText:    normalizeRunInput(trimmedText, attachments),
		OriginalText: trimmedText,
		Modality:     TurnModalityText,
		Attachments:  cloneInputAttachments(attachments),
		Artifacts:    artifactsFromAttachments(attachments),
	})
}

func PrepareAudioInput(mode string, sttClient STTClient, wavData []byte, transcriptHint, userText string, attachments []InputAttachment) (AudioInputResult, error) {
	return PrepareAudioInputWithContext(context.Background(), mode, sttClient, wavData, transcriptHint, userText, attachments)
}

// PrepareAudioInputWithContext is the cancellation-aware form used by active
// voice turns. The legacy PrepareAudioInput wrapper remains source-compatible.
func PrepareAudioInputWithContext(ctx context.Context, mode string, sttClient STTClient, wavData []byte, transcriptHint, userText string, attachments []InputAttachment) (AudioInputResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	resolvedMode := strings.ToLower(strings.TrimSpace(mode))
	if resolvedMode == "" {
		resolvedMode = TurnModalitySTT
	}

	finalAttachments := append([]InputAttachment{}, attachments...)
	trimmedText := strings.TrimSpace(userText)

	switch resolvedMode {
	case TurnModalitySTT:
		transcript := strings.TrimSpace(transcriptHint)
		if transcript == "" {
			if sttClient == nil {
				return AudioInputResult{}, fmt.Errorf("STT mode enabled but STT client is unavailable")
			}
			var err error
			transcript, err = TranscribeWAVWithContext(ctx, sttClient, wavData)
			if err != nil {
				return AudioInputResult{}, fmt.Errorf("STT transcription failed: %w", err)
			}
		}
		transcript = strings.TrimSpace(transcript)
		if transcript == "" {
			return AudioInputResult{}, fmt.Errorf("STT returned empty transcript")
		}

		return normalizeTurnInput(TurnInput{
			InputText:    normalizeRunInput(mergeUserTextAndTranscript(trimmedText, transcript), finalAttachments),
			OriginalText: trimmedText,
			Modality:     TurnModalitySTT,
			Source:       "voice",
			Attachments:  finalAttachments,
			Transcript:   transcript,
			Artifacts:    artifactsFromAttachments(finalAttachments),
		}), nil

	case TurnModalityAudio:
		audioAttachment := InputAttachment{
			Kind:     AttachmentKindAudio,
			Name:     "recording.wav",
			MIMEType: "audio/wav",
			Data:     wavData,
		}
		finalAttachments = append(finalAttachments, InputAttachment{
			Kind:     audioAttachment.Kind,
			Name:     audioAttachment.Name,
			MIMEType: audioAttachment.MIMEType,
			Data:     audioAttachment.Data,
		})
		inputText := trimmedText
		if inputText == "" {
			inputText = voiceAudioInputPlaceholder
		}
		artifacts := append(artifactsFromAttachments(attachments), audioArtifactFromWAV(audioAttachment.Name, audioAttachment.MIMEType, "", wavData))
		return normalizeTurnInput(TurnInput{
			InputText:    inputText,
			OriginalText: trimmedText,
			Modality:     TurnModalityAudio,
			Source:       "voice",
			Attachments:  finalAttachments,
			Artifacts:    artifacts,
		}), nil

	default:
		return AudioInputResult{}, fmt.Errorf("invalid audio input mode: %s", mode)
	}
}

func mergeUserTextAndTranscript(userText, transcript string) string {
	userText = strings.TrimSpace(userText)
	transcript = strings.TrimSpace(transcript)

	switch {
	case userText == "":
		return transcript
	case transcript == "":
		return userText
	case strings.EqualFold(userText, transcript):
		return userText
	default:
		return userText + "\n\nVoice transcript:\n" + transcript
	}
}

func normalizeRunInput(input string, attachments []InputAttachment) string {
	input = strings.TrimSpace(input)
	if input != "" {
		return input
	}

	if len(attachments) == 0 {
		return ""
	}

	hasImage := false
	hasAudio := false
	for _, attachment := range attachments {
		switch attachment.Kind {
		case AttachmentKindImage:
			hasImage = true
		case AttachmentKindAudio:
			hasAudio = true
		}
	}

	switch {
	case hasImage && hasAudio:
		return "Please analyze the attached image and audio."
	case hasImage:
		return "Please analyze the attached image."
	case hasAudio:
		return "Please analyze the attached audio."
	default:
		return "Please analyze the attached content."
	}
}

func normalizeTurnInput(input TurnInput) TurnInput {
	input.InputText = strings.TrimSpace(input.InputText)
	input.OriginalText = strings.TrimSpace(input.OriginalText)
	input.Transcript = strings.TrimSpace(input.Transcript)
	input.Modality = strings.ToLower(strings.TrimSpace(input.Modality))
	input.Source = strings.TrimSpace(input.Source)
	input.Attachments = cloneInputAttachments(input.Attachments)
	input.Artifacts = sanitizeInputArtifacts(input.Artifacts)
	input.TelemetryEvents = cloneTaskEpisodeEvents(input.TelemetryEvents)

	if input.Modality == "" {
		input.Modality = TurnModalityText
	}
	if input.Source == "" && (input.Modality == TurnModalitySTT || input.Modality == TurnModalityAudio) {
		input.Source = "voice"
	}
	if input.InputText == "" {
		switch {
		case input.Modality == TurnModalitySTT && input.Transcript != "":
			input.InputText = input.Transcript
		case input.Modality == TurnModalityAudio:
			input.InputText = voiceAudioInputPlaceholder
		case input.OriginalText != "":
			input.InputText = input.OriginalText
		default:
			input.InputText = normalizeRunInput("", input.Attachments)
		}
	}
	return input
}

func cloneTaskEpisodeEvents(events []TaskEpisodeEvent) []TaskEpisodeEvent {
	if len(events) == 0 {
		return nil
	}
	cloned := make([]TaskEpisodeEvent, 0, len(events))
	for _, event := range events {
		copyEvent := event
		if len(event.CompletionCriteria) > 0 {
			copyEvent.CompletionCriteria = append([]string(nil), event.CompletionCriteria...)
		}
		if len(event.Plan) > 0 {
			copyEvent.Plan = append([]string(nil), event.Plan...)
		}
		if event.DurationMs != nil {
			durationMs := *event.DurationMs
			copyEvent.DurationMs = &durationMs
		}
		if len(event.Metadata) > 0 {
			copyEvent.Metadata = make(map[string]interface{}, len(event.Metadata))
			for key, value := range event.Metadata {
				copyEvent.Metadata[key] = value
			}
		}
		cloned = append(cloned, copyEvent)
	}
	return cloned
}

func cloneInputAttachments(attachments []InputAttachment) []InputAttachment {
	if len(attachments) == 0 {
		return nil
	}
	cloned := make([]InputAttachment, 0, len(attachments))
	for _, attachment := range attachments {
		copyAttachment := attachment
		if len(attachment.Data) > 0 {
			copyAttachment.Data = append([]byte(nil), attachment.Data...)
		}
		cloned = append(cloned, copyAttachment)
	}
	return cloned
}

func sanitizeInputArtifacts(artifacts []InputArtifact) []InputArtifact {
	if len(artifacts) == 0 {
		return nil
	}
	sanitized := make([]InputArtifact, 0, len(artifacts))
	for _, artifact := range artifacts {
		artifact.Kind = strings.TrimSpace(artifact.Kind)
		artifact.Name = strings.TrimSpace(artifact.Name)
		artifact.MIMEType = strings.TrimSpace(artifact.MIMEType)
		artifact.Path = strings.TrimSpace(artifact.Path)
		artifact.Data = nil
		if artifact.Kind == "" && artifact.MIMEType == "" && artifact.Path == "" && artifact.Name == "" && artifact.Size == 0 && artifact.DurationMS == 0 {
			continue
		}
		sanitized = append(sanitized, artifact)
	}
	if len(sanitized) == 0 {
		return nil
	}
	return sanitized
}

func artifactsFromAttachments(attachments []InputAttachment) []InputArtifact {
	if len(attachments) == 0 {
		return nil
	}
	artifacts := make([]InputArtifact, 0, len(attachments))
	for _, attachment := range attachments {
		kind := strings.TrimSpace(attachment.Kind)
		mimeType := strings.TrimSpace(attachment.MIMEType)
		name := strings.TrimSpace(attachment.Name)
		if kind == "" && mimeType == "" && name == "" && len(attachment.Data) == 0 {
			continue
		}
		artifacts = append(artifacts, InputArtifact{
			Kind:     kind,
			Name:     name,
			MIMEType: mimeType,
			Size:     int64(len(attachment.Data)),
		})
	}
	return sanitizeInputArtifacts(artifacts)
}

func audioArtifactFromWAV(name, mimeType, path string, wavData []byte) InputArtifact {
	if strings.TrimSpace(name) == "" {
		name = "recording.wav"
	}
	if strings.TrimSpace(mimeType) == "" {
		mimeType = "audio/wav"
	}
	return InputArtifact{
		Kind:       AttachmentKindAudio,
		Name:       name,
		MIMEType:   mimeType,
		Path:       strings.TrimSpace(path),
		DurationMS: wavDurationMS(wavData),
		Size:       int64(len(wavData)),
	}
}

func withAudioArtifactPath(input TurnInput, path string, durationMS int64, size int64) TurnInput {
	input = normalizeTurnInput(input)
	path = strings.TrimSpace(path)
	if durationMS < 0 {
		durationMS = 0
	}
	if size < 0 {
		size = 0
	}
	for i := range input.Artifacts {
		if input.Artifacts[i].Kind != AttachmentKindAudio {
			continue
		}
		if path != "" {
			input.Artifacts[i].Path = path
		}
		if durationMS > 0 {
			input.Artifacts[i].DurationMS = durationMS
		}
		if size > 0 {
			input.Artifacts[i].Size = size
		}
		return normalizeTurnInput(input)
	}
	if path != "" || input.Modality == TurnModalityAudio {
		input.Artifacts = append(input.Artifacts, InputArtifact{
			Kind:       AttachmentKindAudio,
			Name:       "recording.wav",
			MIMEType:   "audio/wav",
			Path:       path,
			DurationMS: durationMS,
			Size:       size,
		})
	}
	return normalizeTurnInput(input)
}

func firstAudioArtifact(input TurnInput) (InputArtifact, bool) {
	for _, artifact := range input.Artifacts {
		if artifact.Kind == AttachmentKindAudio {
			return artifact, true
		}
	}
	return InputArtifact{}, false
}

func wavDurationMS(wavData []byte) int64 {
	if len(wavData) < 44 || string(wavData[0:4]) != "RIFF" || string(wavData[8:12]) != "WAVE" {
		return 0
	}
	channels := int(binary.LittleEndian.Uint16(wavData[22:24]))
	sampleRate := int(binary.LittleEndian.Uint32(wavData[24:28]))
	bitsPerSample := int(binary.LittleEndian.Uint16(wavData[34:36]))
	if channels <= 0 || sampleRate <= 0 || bitsPerSample <= 0 {
		return 0
	}
	dataSize := 0
	for offset := 12; offset+8 <= len(wavData); {
		chunkID := string(wavData[offset : offset+4])
		chunkSize := int(binary.LittleEndian.Uint32(wavData[offset+4 : offset+8]))
		dataStart := offset + 8
		if chunkID == "data" {
			dataSize = chunkSize
			if dataStart+dataSize > len(wavData) {
				dataSize = len(wavData) - dataStart
			}
			break
		}
		next := dataStart + chunkSize
		if chunkSize%2 == 1 {
			next++
		}
		if next <= offset {
			break
		}
		offset = next
	}
	if dataSize <= 0 {
		dataSize = len(wavData) - 44
	}
	bytesPerFrame := channels * bitsPerSample / 8
	if bytesPerFrame <= 0 {
		return 0
	}
	denominator := int64(bytesPerFrame * sampleRate)
	return (int64(dataSize)*1000 + denominator - 1) / denominator
}
