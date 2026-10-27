package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"aico-agent/internal/ota"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) < 2 {
		return usage()
	}
	switch args[0] {
	case "init":
		return initMisc(args[1:])
	case "read":
		return readMisc(args[1], out)
	case "set-active":
		return setActive(args[1:])
	case "mark-successful":
		return markSuccessful(args[1:])
	case "write":
		return writeMisc(args[1:])
	default:
		return usage()
	}
}

func usage() error {
	return fmt.Errorf("usage: abctl <init|read|set-active|mark-successful|write> <misc> [slot] [flags]")
}

func initMisc(args []string) error {
	if len(args) < 1 {
		return usage()
	}
	size := int64(ota.DefaultMiscSize)
	for i := 1; i < len(args); i++ {
		value, consumed, err := flagValue(args, &i, "--size")
		if err != nil {
			return err
		}
		if !consumed {
			return usage()
		}
		parsed, err := parseSize(value)
		if err != nil {
			return err
		}
		size = parsed
	}
	return ota.InitMisc(args[0], size)
}

func readMisc(path string, out io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	data, err := ota.ReadABData(f)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	fmt.Fprintf(out, "magic=\\0AB0 version=%d.%d crc32=%#08x\n", data.VersionMajor, data.VersionMinor, data.CRC32)
	fprintfSlot(out, "A", data.Slots[0])
	fprintfSlot(out, "B", data.Slots[1])
	fmt.Fprintf(out, "last_boot=%s\n", slotName(data.LastBoot))
	return nil
}

func fprintfSlot(out io.Writer, name string, slot ota.SlotData) {
	fmt.Fprintf(out, "slot %s: priority=%d tries=%d successful=%t\n", name, slot.Priority, slot.TriesRemaining, slot.SuccessfulBoot)
}

func setActive(args []string) error {
	if len(args) < 2 {
		return usage()
	}
	tries := uint64(ota.MaxTries)
	successful := uint64(0)
	for i := 2; i < len(args); i++ {
		name := flagName(args[i])
		value, consumed, err := flagValue(args, &i, name)
		if err != nil {
			return err
		}
		if !consumed {
			return usage()
		}
		switch name {
		case "--tries":
			parsed, err := strconv.ParseUint(value, 10, 8)
			if err != nil {
				return err
			}
			tries = parsed
		case "--successful":
			parsed, err := parseBoolFlag(value)
			if err != nil {
				return err
			}
			successful = parsed
		default:
			return usage()
		}
	}
	if tries > ota.MaxTries {
		return fmt.Errorf("tries %d exceeds %d", tries, ota.MaxTries)
	}

	return updateMisc(args[0], func(data *ota.ABData) error {
		slot, err := parseSlot(args[1])
		if err != nil {
			return err
		}
		return data.SetActive(slot, uint8(tries), successful == 1)
	})
}

func markSuccessful(args []string) error {
	if len(args) != 2 {
		return usage()
	}
	return updateMisc(args[0], func(data *ota.ABData) error {
		slot, err := parseSlot(args[1])
		if err != nil {
			return err
		}
		return data.MarkSuccessful(slot)
	})
}

func writeMisc(args []string) error {
	if len(args) < 1 {
		return usage()
	}
	set := map[string]uint64{}
	for i := 1; i < len(args); i++ {
		name := flagName(args[i])
		value, consumed, err := flagValue(args, &i, name)
		if err != nil {
			return err
		}
		if !consumed {
			return usage()
		}
		parsed, err := strconv.ParseUint(value, 10, 8)
		if err != nil {
			return err
		}
		set[name] = parsed
	}

	required := []string{"--a-priority", "--a-tries", "--a-successful", "--b-priority", "--b-tries", "--b-successful"}
	for _, name := range required {
		if _, ok := set[name]; !ok {
			return fmt.Errorf("missing %s", name)
		}
	}
	for _, name := range []string{"--a-priority", "--b-priority"} {
		if set[name] > ota.MaxPriority {
			return fmt.Errorf("%s %d exceeds %d", name, set[name], ota.MaxPriority)
		}
	}
	for _, name := range []string{"--a-tries", "--b-tries"} {
		if set[name] > ota.MaxTries {
			return fmt.Errorf("%s %d exceeds %d", name, set[name], ota.MaxTries)
		}
	}
	for _, name := range []string{"--a-successful", "--b-successful"} {
		if set[name] > 1 {
			return fmt.Errorf("%s must be 0 or 1", name)
		}
	}
	if set["--a-successful"] == 1 && set["--a-tries"] > 0 {
		return fmt.Errorf("slot A successful cannot have nonzero tries")
	}
	if set["--b-successful"] == 1 && set["--b-tries"] > 0 {
		return fmt.Errorf("slot B successful cannot have nonzero tries")
	}

	data := ota.ABData{VersionMajor: 1, VersionMinor: 0, LastBoot: ota.SlotA}
	if existing, err := readABDataPath(args[0]); err == nil {
		data.LastBoot = existing.LastBoot
	}
	data.Slots[0] = ota.SlotData{Priority: uint8(set["--a-priority"]), TriesRemaining: uint8(set["--a-tries"]), SuccessfulBoot: set["--a-successful"] == 1}
	data.Slots[1] = ota.SlotData{Priority: uint8(set["--b-priority"]), TriesRemaining: uint8(set["--b-tries"]), SuccessfulBoot: set["--b-successful"] == 1}
	return updateMisc(args[0], func(existing *ota.ABData) error {
		*existing = data
		return nil
	})
}

func readABDataPath(path string) (ota.ABData, error) {
	f, err := os.Open(path)
	if err != nil {
		return ota.ABData{}, err
	}
	defer f.Close()
	return ota.ReadABData(f)
}

func updateMisc(path string, update func(*ota.ABData) error) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}

	data, err := ota.ReadABData(f)
	if err != nil {
		_ = f.Close()
		return err
	}
	if err := update(&data); err != nil {
		_ = f.Close()
		return err
	}
	writeErr := ota.WriteABDataAt(f, data)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func parseSlot(s string) (ota.Slot, error) {
	switch strings.ToUpper(s) {
	case "A", "0":
		return ota.SlotA, nil
	case "B", "1":
		return ota.SlotB, nil
	default:
		if n, err := strconv.Atoi(s); err == nil {
			return ota.Slot(n), fmt.Errorf("invalid slot %d", n)
		}
		return ota.SlotA, fmt.Errorf("invalid slot %q", s)
	}
}

func slotName(slot ota.Slot) string {
	if slot == ota.SlotB {
		return "B"
	}
	return "A"
}

func flagName(arg string) string {
	if i := strings.IndexByte(arg, '='); i >= 0 {
		return arg[:i]
	}
	return arg
}

func flagValue(args []string, index *int, want string) (string, bool, error) {
	arg := args[*index]
	if strings.HasPrefix(arg, want+"=") {
		return strings.TrimPrefix(arg, want+"="), true, nil
	}
	if arg != want {
		return "", false, nil
	}
	if *index+1 >= len(args) {
		return "", false, fmt.Errorf("missing value for %s", want)
	}
	(*index)++
	return args[*index], true, nil
}

func parseBoolFlag(value string) (uint64, error) {
	parsed, err := strconv.ParseUint(value, 10, 8)
	if err != nil {
		return 0, err
	}
	if parsed > 1 {
		return 0, fmt.Errorf("successful must be 0 or 1")
	}
	return parsed, nil
}

func parseSize(value string) (int64, error) {
	if value == "" {
		return 0, fmt.Errorf("size is empty")
	}
	multiplier := int64(1)
	last := value[len(value)-1]
	switch last {
	case 'K', 'k':
		multiplier = 1024
		value = value[:len(value)-1]
	case 'M', 'm':
		multiplier = 1024 * 1024
		value = value[:len(value)-1]
	case 'G', 'g':
		multiplier = 1024 * 1024 * 1024
		value = value[:len(value)-1]
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, fmt.Errorf("size must be positive")
	}
	return n * multiplier, nil
}
