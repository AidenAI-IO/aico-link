package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"aico-agent/internal/ota"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	return runWithConfig(args, out, nil)
}

func runWithConfig(args []string, out io.Writer, configure func(*ota.UpdaterConfig)) error {
	return runWithDependencies(args, out, configure, ota.RunSelfCheck)
}

// runWithDependencies dispatches OTA commands with an injectable probe runner,
// so command outcomes can be tested independently of board hardware.
func runWithDependencies(args []string, out io.Writer, configure func(*ota.UpdaterConfig), selfCheck func(context.Context, ota.SelfCheckConfig) ota.SelfCheckReport) error {
	command, args := splitCommandAndFlags(args)
	positional := flagArgs(args)
	if (command == "health" || command == "mark-health" || command == "provision-identity" || command == "check" || command == "update" || command == "check-now" || command == "status" || command == "self-check" || command == "rollback" || command == "recover") && len(positional) != 0 {
		return usage()
	}
	config, err := parseConfigFlags(args)
	if err != nil {
		return err
	}
	if configure != nil {
		configure(&config)
	}
	updater, err := ota.NewUpdater(config, rebootForConfig(config))
	if err != nil {
		return err
	}
	ctx := context.Background()

	switch command {
	case "self-check":
		report := selfCheck(ctx, ota.DefaultSelfCheckConfig())
		if err := ota.SaveSelfCheckReport(filepath.Join(config.StateDir, "health", "current.json"), report); err != nil {
			return err
		}
		if err := json.NewEncoder(out).Encode(report); err != nil {
			return err
		}
		if report.Fatal() {
			return fmt.Errorf("self-check failed: %d required check(s)", report.Failures)
		}
		return nil
	case "rollback":
		if config.DryRun {
			return fmt.Errorf("--dry-run is not supported with rollback")
		}
		return updater.Rollback("manual rollback")
	case "recover":
		return updater.RecoverPendingData()
	case "health":
		return updater.ProcessPendingHealthOnce(ctx)
	case "mark-health":
		wrote, err := updater.MarkHealthIfPending()
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(struct {
			Written bool `json:"written"`
		}{Written: wrote})
	case "provision-identity":
		result, err := updater.ProvisionFactoryIdentity()
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(result)
	case "check":
		result, err := updater.CheckAvailable(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(result)
	case "update", "check-now":
		result, err := updater.CheckOnce(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(result)
	case "status":
		state, ab, err := updater.Status()
		if err != nil {
			return err
		}
		active, activeOK := ab.ActiveSlot()
		return json.NewEncoder(out).Encode(struct {
			State    ota.State  `json:"state"`
			ABData   ota.ABData `json:"ab_data"`
			Active   ota.Slot   `json:"active_slot"`
			ActiveOK bool       `json:"active_slot_ok"`
		}{State: state, ABData: ab, Active: active, ActiveOK: activeOK})
	case "verify-manifest":
		if len(positional) != 1 {
			return usage()
		}
		manifestPath := positional[0]
		manifest, err := updater.VerifyManifestFile(manifestPath)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "manifest ok: version=%s channel=%s build_time=%s\n", manifest.Version, manifest.Channel, manifest.BuildTime)
		return nil
	default:
		return usage()
	}
}

func rebootForConfig(config ota.UpdaterConfig) func() error {
	if config.DryRun {
		return func() error { return nil }
	}
	return platformReboot
}

func platformReboot() error {
	for _, path := range []string{"/sbin/reboot", "/usr/sbin/reboot"} {
		if _, err := os.Stat(path); err == nil {
			return exec.Command(path).Run()
		}
	}
	return fmt.Errorf("reboot binary not found in trusted paths")
}

func splitCommandAndFlags(args []string) (string, []string) {
	commands := map[string]bool{
		"health":             true,
		"mark-health":        true,
		"provision-identity": true,
		"check":              true,
		"update":             true,
		"check-now":          true,
		"status":             true,
		"verify-manifest":    true,
		"self-check":         true,
		"rollback":           true,
		"recover":            true,
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if len(arg) >= 2 && arg[0] == '-' {
			name := flagName(arg)
			if flagTakesValue(name) && !strings.Contains(arg, "=") && i+1 < len(args) {
				i++
			}
			continue
		}
		if commands[arg] {
			rest := make([]string, 0, len(args)-1)
			rest = append(rest, args[:i]...)
			rest = append(rest, args[i+1:]...)
			return arg, rest
		}
		return "health", args
	}
	return "health", args
}

func parseConfigFlags(args []string) (ota.UpdaterConfig, error) {
	fs := flag.NewFlagSet("ota", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", ota.DefaultOTAConfigPath, "config JSON path")
	stateDir := fs.String("state-dir", "", "OTA state directory")
	miscPath := fs.String("misc", "", "misc partition path")
	blockDir := fs.String("block-dir", "", "partition block-device directory")
	manifestURL := fs.String("manifest-url", "", "direct manifest URL (skips release API)")
	expectedVersion := fs.String("expected-version", "", "require the previously checked firmware version")
	expectedBuildTime := fs.String("expected-build-time", "", "require the previously checked firmware build time")
	publicKeyPath := fs.String("public-key", "", "Ed25519 public key PEM path")
	dryRun := fs.Bool("dry-run", false, "download and verify without switching misc or rebooting")
	testMode := fs.Bool("test", false, "use test-friendly short health timeout")
	healthTimeout := fs.Duration("health-timeout", 0, "pending health timeout")
	httpTimeout := fs.Duration("http-timeout", 0, "HTTP request timeout")
	switchTries := fs.Uint("switch-tries", 0, "tries remaining when switching slots")
	targetSlot := fs.String("target-slot", "", "test override for target slot: a or b")
	flags, _ := partitionFlagArgs(args)
	if err := fs.Parse(flags); err != nil {
		return ota.UpdaterConfig{}, err
	}
	config, err := ota.LoadUpdaterConfig(*configPath)
	if err != nil {
		return ota.UpdaterConfig{}, err
	}
	config.ExpectedVersion, config.ExpectedBuildTime = *expectedVersion, *expectedBuildTime
	if *stateDir != "" {
		config.StateDir = *stateDir
		config.DownloadDir = filepath.Join(*stateDir, "downloads")
		config.UpdateLockPath = filepath.Join(*stateDir, ota.DefaultOTAUpdateLockName)
	}
	if *miscPath != "" {
		config.MiscPath = *miscPath
	}
	if *blockDir != "" {
		config.BlockDir = *blockDir
	}
	if *manifestURL != "" {
		config.ManifestURL = *manifestURL
	}
	if *publicKeyPath != "" {
		config.PublicKeyPath = *publicKeyPath
	}
	if *healthTimeout != 0 {
		config.HealthTimeout = *healthTimeout
	}
	if *httpTimeout != 0 {
		config.HTTPTimeout = *httpTimeout
	}
	if *switchTries != 0 {
		if *switchTries > uint(ota.MaxTries) {
			return ota.UpdaterConfig{}, fmt.Errorf("switch-tries %d exceeds %d", *switchTries, ota.MaxTries)
		}
		config.SwitchTries = uint8(*switchTries)
	}
	if *targetSlot != "" {
		config.TargetSlotOverride = *targetSlot
	}
	config.DryRun = config.DryRun || *dryRun
	if *testMode {
		config.HealthTimeout = time.Second
	}
	return config, nil
}

func flagArgs(args []string) []string {
	_, positional := partitionFlagArgs(args)
	return positional
}

func partitionFlagArgs(args []string) ([]string, []string) {
	var flags []string
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if len(arg) < 2 || arg[0] != '-' {
			positional = append(positional, arg)
			continue
		}
		name := flagName(arg)
		if flagIsBool(name) || strings.Contains(arg, "=") {
			flags = append(flags, arg)
			continue
		}
		if flagTakesValue(name) && i+1 < len(args) {
			flags = append(flags, arg, args[i+1])
			i++
			continue
		}
		flags = append(flags, arg)
	}
	return flags, positional
}

func flagName(arg string) string {
	name := arg
	for len(name) > 0 && name[0] == '-' {
		name = name[1:]
	}
	if eq := strings.IndexByte(name, '='); eq >= 0 {
		name = name[:eq]
	}
	return name
}

func flagIsBool(name string) bool {
	return name == "dry-run" || name == "test"
}

func flagTakesValue(name string) bool {
	switch name {
	case "config", "state-dir", "misc", "block-dir", "manifest-url", "public-key", "health-timeout", "target-slot", "http-timeout", "switch-tries", "expected-version", "expected-build-time":
		return true
	default:
		return false
	}
}

func usage() error {
	return fmt.Errorf("usage: ota [flags] [health|mark-health|provision-identity|check|update|check-now|status|self-check|rollback|recover|verify-manifest <manifest>]")
}
