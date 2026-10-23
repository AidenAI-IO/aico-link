package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"aico-agent/internal/agent"
	"aico-agent/internal/configupdate"
)

type webConfigDTO = configupdate.Config
type modelDTO = configupdate.Model
type modelProviderDTO = configupdate.ModelProvider
type ttsProviderDTO = configupdate.TTSProvider
type sttProviderDTO = configupdate.STTProvider
type ttsDTO = configupdate.TTS
type sttDTO = configupdate.STT
type audioDTO = configupdate.Audio
type audioArchiveDTO = configupdate.AudioArchive
type frameServiceDTO = configupdate.FrameService
type quickCaptureDTO = configupdate.QuickCapture
type storageDTO = configupdate.Storage
type storageDegradedModeDTO = configupdate.StorageDegradedMode
type storageCleanupDTO = configupdate.StorageCleanup
type deviceDTO = configupdate.Device
type voiceNotificationsDTO = configupdate.VoiceNotifications
type voiceNotificationResponseTailDTO = configupdate.VoiceNotificationResponseTail
type voiceNotificationExpirationDTO = configupdate.VoiceNotificationExpiration
type logDTO = configupdate.Log
type otaDTO = configupdate.OTA
type hidDTO = configupdate.HID
type searchDTO = configupdate.Search
type telemetryDTO = configupdate.Telemetry
type liveActivityDTO = configupdate.LiveActivity
type agentDTO = configupdate.Agent

func webConfigDTOFromAgentConfig(cfg agent.Config) webConfigDTO {
	return configupdate.FromAgentConfig(cfg)
}

type ValidationError = agent.ConfigValidationError

// ValidationResult is the output format for config-check command
type ValidationResult struct {
	Valid  bool              `json:"valid"`
	Errors []ValidationError `json:"errors"`
}

type ConfigTestResult struct {
	OK         bool              `json:"ok"`
	Results    []ConfigTestCheck `json:"results"`
	Transcript string            `json:"transcript,omitempty"`
}

type ConfigTestCheck struct {
	Check  string `json:"check"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

// RunConfigCheck implements the `agent config-check` subcommand. It reads a
// config file with --config or a config_web payload from stdin, validates it, and
// outputs a structured JSON result. It is exported so the configuration-portal
// tests can drive the same entry point the CI and release gates run, instead of
// re-implementing its loader step.
func RunConfigCheck(args []string) int {
	fs := flag.NewFlagSet("config-check", flag.ExitOnError)
	formatFlag := fs.String("format", "json", "output format (only json supported)")
	stdinFlag := fs.Bool("stdin", false, "read config from stdin")
	configFlag := fs.String("config", "", "path to a TOML config file")

	if err := fs.Parse(args); err != nil {
		writeConfigCheckError("failed to parse flags: " + err.Error())
		return 1
	}

	if *formatFlag != "json" {
		writeConfigCheckError("only --format=json is supported")
		return 1
	}

	configPath := strings.TrimSpace(*configFlag)
	if *stdinFlag == (configPath != "") {
		writeConfigCheckError("exactly one of --stdin or --config is required")
		return 1
	}

	var result ValidationResult
	if *stdinFlag {
		var decodeErr error
		result, decodeErr = checkConfig(os.Stdin)
		if decodeErr != nil {
			writeConfigCheckError("invalid JSON input: " + decodeErr.Error())
			return 1
		}
	} else {
		result = checkConfigPath(configPath)
	}

	// Output result as JSON
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		fmt.Fprintf(os.Stderr, "failed to encode result: %v\n", err)
		return 1
	}

	if result.Valid {
		return 0
	}
	return 1
}

// checkConfigPath validates a persisted file for the CI and release gates, which
// run `config-check --config` before an image is assembled. It is deliberately the
// strict verdict: a config the runtime would paper over at boot (a stale
// input_mode=realtime whose credential was removed, where the Agent silently drops
// to an unconfigured voice mode) still has to fail here, or the gate would pass
// configurations that do not do what they say.
//
// The Config Web recovery portal reports a different, runtime-loader verdict,
// because the page has to describe the state the Agent actually boots in and the
// field to repair. The two are allowed to disagree; the portal is the repair
// surface, this is the release gate.
func checkConfigPath(path string) ValidationResult {
	if _, err := agent.LoadResolvedConfig(path); err != nil {
		return ValidationResult{Valid: false, Errors: parseValidationErrors(err)}
	}
	return ValidationResult{Valid: true, Errors: []ValidationError{}}
}

// checkConfig reads a config_web wire-format payload from r, maps it onto
// agent.Config, and runs the canonical Config.Validate(). It returns the
// structured result, or a non-nil error only when the input is not decodable
// JSON. Splitting this out of RunConfigCheck keeps the full
// decode -> map -> validate pipeline testable without driving os.Stdin/Stdout.
func checkConfig(r io.Reader) (ValidationResult, error) {
	// The payload is the config_web wire format defined by webConfigDTO:
	// snake_case keys, agent-level settings nested under an "agent" object, and
	// search exposing only a "has_api_key" boolean instead of the raw key.
	// agent.Config carries only TOML tags, so decoding straight
	// into it silently drops every snake_case / nested field and validates a
	// near-empty config. Decode into a DTO that mirrors the wire format, then
	// map it onto agent.Config before validating.
	var dto webConfigDTO
	if err := json.NewDecoder(r).Decode(&dto); err != nil {
		return ValidationResult{}, err
	}
	cfg := dto.ToAgentConfig()

	result := ValidationResult{
		Valid:  true,
		Errors: []ValidationError{},
	}
	if err := cfg.Validate(); err != nil {
		result.Valid = false
		result.Errors = parseValidationErrors(err)
		return result, nil
	}
	// Voice provider records are checked only here, on the save path. Boot stays
	// lenient: a TTS init failure is a warning and the agent still starts, so a
	// device whose provider reference went stale must keep booting. But a save
	// that stores a dangling reference silently loses voice on the next restart,
	// so it has to be rejected while the user is still looking at the form.
	if err := cfg.ValidateVoiceProviders(); err != nil {
		result.Valid = false
		result.Errors = parseValidationErrors(err)
	}
	return result, nil
}

// runConfigMeta implements the `agent config-meta` subcommand. It outputs the
// config field metadata (widget, enum, range, default, secret, visibility
// rules) as JSON for the config web UI to consume.
func runConfigMeta(args []string) int {
	fs := flag.NewFlagSet("config-meta", flag.ExitOnError)
	formatFlag := fs.String("format", "json", "output format (only json supported)")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "failed to parse flags: %v\n", err)
		return 1
	}

	if *formatFlag != "json" {
		fmt.Fprintln(os.Stderr, "only --format=json is supported")
		return 1
	}

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(agent.ConfigMeta()); err != nil {
		fmt.Fprintf(os.Stderr, "failed to encode metadata: %v\n", err)
		return 1
	}

	return 0
}

func resolvedWebConfigDTO(configPath string) (webConfigDTO, error) {
	// Keep the config page usable when a persisted config is invalid. The page
	// must be able to display and repair the bad field (for example, stale
	// input_mode=realtime without voice_model.api_key); config-update validates
	// the resulting candidate before persisting it.
	cfg, err := agent.LoadResolvedConfigForUpdate(configPath)
	if err != nil {
		return webConfigDTO{}, err
	}
	return configupdate.FromAgentConfig(cfg), nil
}

// runConfig implements the `agent config` subcommand. It reads the current
// agent.toml over the canonical defaults and emits the resolved config in the
// config_web wire format.
func runConfig(args []string) int {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	formatFlag := fs.String("format", "json", "output format (only json supported)")
	configFlag := fs.String("config", "", "path to a TOML config file")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "failed to parse flags: %v\n", err)
		return 1
	}

	if *formatFlag != "json" {
		fmt.Fprintln(os.Stderr, "only --format=json is supported")
		return 1
	}

	if strings.TrimSpace(*configFlag) == "" {
		fmt.Fprintln(os.Stderr, "--config is required")
		return 1
	}

	dto, err := resolvedWebConfigDTO(*configFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		return 1
	}

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(dto); err != nil {
		fmt.Fprintf(os.Stderr, "failed to encode config: %v\n", err)
		return 1
	}

	return 0
}

func runConfigUpdate(args []string) int {
	return runConfigUpdateIO(args, os.Stdin, os.Stdout, os.Stderr)
}

func runConfigUpdateIO(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("config-update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	formatFlag := fs.String("format", "json", "output format (only json supported)")
	stdinFlag := fs.Bool("stdin", false, "read JSON merge patch from stdin")
	configFlag := fs.String("config", "", "path to a TOML config file")
	dryRunFlag := fs.Bool("dry-run", false, "validate and report the apply level without writing the config")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		writeConfigUpdateError(stdout, err, configupdate.ErrorKindInvalidRequest)
		return 1
	}
	if *formatFlag != "json" || !*stdinFlag || strings.TrimSpace(*configFlag) == "" {
		writeConfigUpdateError(stdout, fmt.Errorf("--config, --stdin and --format=json are required"), configupdate.ErrorKindInvalidRequest)
		return 1
	}
	patch, err := io.ReadAll(stdin)
	if err != nil {
		writeConfigUpdateError(stdout, err, configupdate.ErrorKindInternal)
		return 1
	}
	update := configupdate.NewService().Update
	if *dryRunFlag {
		update = configupdate.NewService().Plan
	}
	result, err := update(strings.TrimSpace(*configFlag), patch)
	if err != nil {
		writeConfigUpdateError(stdout, err, configupdate.ErrorKind(err))
		return 1
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		fmt.Fprintf(stderr, "encode config-update result: %v\n", err)
		return 1
	}
	return 0
}

func writeConfigUpdateError(w io.Writer, err error, kind string) {
	_ = json.NewEncoder(w).Encode(configupdate.Result{
		OK:        false,
		Error:     err.Error(),
		ErrorKind: kind,
	})
}

type configTestInput struct {
	Section     string          `json:"section"`
	Values      json.RawMessage `json:"values"`
	Text        string          `json:"text"`
	AudioBase64 string          `json:"audio_base64"`
}

// runConfigTest implements provider checks through the same runtime registries
// and adapters used by the agent itself.
func runConfigTest(args []string) int {
	fs := flag.NewFlagSet("config-test", flag.ExitOnError)
	formatFlag := fs.String("format", "json", "output format (only json supported)")
	stdinFlag := fs.Bool("stdin", false, "read test request from stdin")
	sectionFlag := fs.String("section", "", "config section to test")
	configFlag := fs.String("config", "", "path to a TOML config file")
	timeoutFlag := fs.Duration("timeout", 45*time.Second, "test timeout")

	if err := fs.Parse(args); err != nil {
		writeConfigTestResult(configTestFailure("request", "failed to parse flags: "+err.Error()))
		return 1
	}
	if *formatFlag != "json" {
		writeConfigTestResult(configTestFailure("request", "only --format=json is supported"))
		return 1
	}
	if !*stdinFlag {
		writeConfigTestResult(configTestFailure("request", "--stdin flag is required"))
		return 1
	}
	if strings.TrimSpace(*configFlag) == "" {
		writeConfigTestResult(configTestFailure("request", "--config is required"))
		return 1
	}

	var input configTestInput
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		writeConfigTestResult(configTestFailure("request", "invalid JSON input: "+err.Error()))
		return 1
	}
	section := strings.TrimSpace(input.Section)
	if section == "" {
		section = strings.TrimSpace(*sectionFlag)
	}
	if section != "model" && section != "tts" && section != "stt" {
		writeConfigTestResult(configTestFailure("request", "unsupported section: "+section))
		return 1
	}

	// config-test checks the candidate values carried in the request, so the
	// persisted file only supplies surrounding context (proxy settings, provider
	// records). Requiring it to pass semantic validation would disable the section
	// Test buttons in exactly the recovery state they are needed in: the invalid
	// field is what the user is trying to test a replacement for. A file that
	// cannot be read or decoded still fails here.
	cfg, err := agent.LoadResolvedConfigForUpdate(*configFlag)
	if err != nil {
		writeConfigTestResult(configTestFailure("load_config", err.Error()))
		return 1
	}

	if len(input.Values) == 0 || string(input.Values) == "null" {
		writeConfigTestResult(configTestFailure("request", "missing values object"))
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeoutFlag)
	defer cancel()
	result := executeConfigTest(ctx, cfg, input, section)
	writeConfigTestResult(result)
	if result.OK {
		return 0
	}
	return 1
}

func executeConfigTest(ctx context.Context, cfg agent.Config, input configTestInput, section string) ConfigTestResult {
	if section == "model" {
		var modelValues modelDTO
		if err := json.Unmarshal(input.Values, &modelValues); err != nil {
			return configTestFailure("request", "invalid model values: "+err.Error())
		}

		result, err := agent.RunModelProviderTest(ctx, cfg, modelValues.ProviderTestRequest())
		if err != nil {
			detail := err.Error()
			if result.Provider != "" {
				detail = fmt.Sprintf("[provider=%s model=%s] %s", result.Provider, result.Model, detail)
			}
			return configTestFailure("provider_request", detail)
		}
		return ConfigTestResult{
			OK: true,
			Results: []ConfigTestCheck{{
				Check:  "provider_request",
				Passed: true,
				Detail: fmt.Sprintf("received a response from %s (model: %s)", result.Provider, result.Model),
			}},
		}
	}

	if section == "tts" {
		var ttsValues ttsDTO
		if err := json.Unmarshal(input.Values, &ttsValues); err != nil {
			return configTestFailure("request", "invalid tts values: "+err.Error())
		}

		playback, err := agent.RunTTSPlaybackTest(ctx, cfg, ttsValues.PlaybackTestRequest(input.Text))
		if err != nil {
			detail := err.Error()
			if ttsValues.Provider != "" {
				detail = fmt.Sprintf("[provider=%s model=%s voice=%s] %s", ttsValues.Provider, ttsValues.Model, ttsValues.VoiceID, detail)
			}
			return configTestFailure("tts_playback", detail)
		}
		return ConfigTestResult{
			OK: true,
			Results: []ConfigTestCheck{{
				Check:  "tts_playback",
				Passed: true,
				Detail: fmt.Sprintf("played %q with %s", playback.Text, playback.Provider),
			}},
		}
	}
	if section != "stt" {
		return configTestFailure("request", "unsupported section: "+section)
	}

	var sttValues sttDTO
	if err := json.Unmarshal(input.Values, &sttValues); err != nil {
		return configTestFailure("request", "invalid stt values: "+err.Error())
	}
	if strings.TrimSpace(input.AudioBase64) == "" {
		result, err := agent.RunSTTProviderTest(ctx, cfg, sttValues.TranscriptionTestRequest(nil))
		if err != nil {
			return configTestFailure("provider_config", err.Error())
		}
		return ConfigTestResult{
			OK: true,
			Results: []ConfigTestCheck{{
				Check:  "provider_config",
				Passed: true,
				Detail: fmt.Sprintf("created the %s STT client", result.Provider),
			}},
		}
	}
	wavData, err := base64.StdEncoding.DecodeString(strings.TrimSpace(input.AudioBase64))
	if err != nil {
		return configTestFailure("request", "invalid audio_base64: "+err.Error())
	}
	transcription, err := agent.RunSTTTranscriptionTest(ctx, cfg, sttValues.TranscriptionTestRequest(wavData))
	if err != nil {
		return configTestFailure("stt_transcription", err.Error())
	}
	return ConfigTestResult{
		OK:         true,
		Transcript: transcription.Transcript,
		Results: []ConfigTestCheck{{
			Check:  "stt_transcription",
			Passed: true,
			Detail: fmt.Sprintf("transcribed audio with %s", transcription.Provider),
		}},
	}
}

func configTestFailure(check, detail string) ConfigTestResult {
	return ConfigTestResult{
		OK: false,
		Results: []ConfigTestCheck{{
			Check:  check,
			Passed: false,
			Detail: detail,
		}},
	}
}

// parseValidationErrors converts a validation error into structured field errors.
// The mapping is shared with the Config Web recovery portal, which highlights the
// same field, so it lives in the agent package.
func parseValidationErrors(err error) []ValidationError {
	return agent.ParseConfigValidationErrors(err)
}

// writeConfigCheckError writes an error message as a JSON ValidationResult
func writeConfigCheckError(message string) {
	result := ValidationResult{
		Valid: false,
		Errors: []ValidationError{
			{
				Field:   "",
				Message: message,
			},
		},
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.Encode(result)
}

func writeConfigTestResult(result ConfigTestResult) {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(result)
}
