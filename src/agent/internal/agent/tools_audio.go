package agent

import (
	"context"
	"encoding/json"
	"strings"
)

type playbackVolumeClient interface {
	SetPlaybackVolume(volume int) error
	GetPlaybackVolume() (int, error)
}

// AudioVolumeTool gets or sets audio_service playback volume.
type AudioVolumeTool struct {
	client playbackVolumeClient
}

func NewAudioVolumeTool(socketPath string) *AudioVolumeTool {
	return &AudioVolumeTool{client: NewAudioServiceClient(socketPath)}
}

func (t *AudioVolumeTool) Name() string { return "audio_volume" }

func (t *AudioVolumeTool) Description() string {
	return `Get or set audio playback volume for audio_service/TTS playback. Omit volume to read the current value. This controls Aico playback/TTS volume, not phone system UI volume.`
}

func (t *AudioVolumeTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"volume": rangedIntegerArgSchema("Optional playback volume to set before reading the current value.", 0, 100),
	})
}

func (t *AudioVolumeTool) Call(ctx context.Context, input string) (string, error) {
	var args struct {
		Volume *int `json:"volume"`
	}

	trimmed := strings.TrimSpace(input)
	if trimmed != "" {
		if err := json.Unmarshal([]byte(trimmed), &args); err != nil {
			return toolErrorResultf(ctx, CodeInvalidArguments, "invalid input: %v. Expected JSON format: {\"volume\": 70} or {} to read current volume. Volume must be a number between 0 and 100", err), nil
		}
	}

	if args.Volume != nil {
		if *args.Volume < 0 || *args.Volume > 100 {
			return toolErrorResultString(ctx, CodeInvalidArguments, "volume must be between 0 and 100"), nil
		}
		if err := t.client.SetPlaybackVolume(*args.Volume); err != nil {
			return toolErrorResultf(ctx, CodeToolExecutionFailed, "%v", err), nil
		}
	}

	volume, err := t.client.GetPlaybackVolume()
	if err != nil {
		return toolErrorResultf(ctx, CodeToolExecutionFailed, "%v", err), nil
	}

	result := struct {
		Volume int `json:"volume"`
	}{
		Volume: volume,
	}
	out, _ := json.Marshal(result)
	return string(out), nil
}
