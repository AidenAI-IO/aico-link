package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aico-agent/internal/ota"
)

func TestRunInitAndRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "misc.img")

	if err := run([]string{"init", path}, nil); err != nil {
		t.Fatalf("init error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat misc: %v", err)
	}
	if info.Size() != 4*1024*1024 {
		t.Fatalf("init size = %d, want 4 MiB", info.Size())
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open misc: %v", err)
	}
	data, err := ota.ReadABData(f)
	if err != nil {
		t.Fatalf("ReadABData() error = %v", err)
	}
	if data.Slots[0].Priority != 15 || !data.Slots[0].SuccessfulBoot {
		t.Fatalf("slot A = %+v", data.Slots[0])
	}

	out := new(bytes.Buffer)
	if err := run([]string{"read", path}, out); err != nil {
		t.Fatalf("read error = %v", err)
	}
	if got := out.String(); !strings.Contains(got, "slot A: priority=15 tries=0 successful=true") {
		t.Fatalf("read output = %q", got)
	}
	if got := out.String(); !strings.Contains(got, "last_boot=A") {
		t.Fatalf("read output = %q, want last_boot", got)
	}
}

func TestRunInitWithEqualsSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "misc.img")
	if err := run([]string{"init", path, "--size=4M"}, nil); err != nil {
		t.Fatalf("init error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat misc: %v", err)
	}
	if info.Size() != 4*1024*1024 {
		t.Fatalf("init size = %d, want 4 MiB", info.Size())
	}
}

func TestRunSetActive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "misc.img")
	if err := run([]string{"init", path}, nil); err != nil {
		t.Fatalf("init error = %v", err)
	}
	if err := run([]string{"set-active", path, "B", "--tries", "3"}, nil); err != nil {
		t.Fatalf("set-active error = %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open misc: %v", err)
	}
	data, err := ota.ReadABData(f)
	if err != nil {
		t.Fatalf("ReadABData() error = %v", err)
	}
	if data.Slots[1] != (ota.SlotData{Priority: 15, TriesRemaining: 3, SuccessfulBoot: false}) {
		t.Fatalf("slot B = %+v", data.Slots[1])
	}
}

func TestRunSetActiveRejectsSuccessfulWithTries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "misc.img")
	if err := run([]string{"init", path}, nil); err != nil {
		t.Fatalf("init error = %v", err)
	}
	if err := run([]string{"set-active", path, "B", "--tries", "3", "--successful=1"}, nil); err == nil {
		t.Fatal("set-active successful with tries error = nil, want error")
	}
}

func TestRunSetActivePreservesLastBoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "misc.img")
	if err := run([]string{"init", path}, nil); err != nil {
		t.Fatalf("init error = %v", err)
	}
	data := readTestABData(t, path)
	data.LastBoot = ota.SlotB
	writeTestABData(t, path, data)

	if err := run([]string{"set-active", path, "B", "--tries", "3"}, nil); err != nil {
		t.Fatalf("set-active error = %v", err)
	}
	if got := readTestABData(t, path).LastBoot; got != ota.SlotB {
		t.Fatalf("LastBoot = %d, want preserved SlotB", got)
	}
}

func TestRunMarkSuccessful(t *testing.T) {
	path := filepath.Join(t.TempDir(), "misc.img")
	if err := run([]string{"init", path}, nil); err != nil {
		t.Fatalf("init error = %v", err)
	}
	if err := run([]string{"set-active", path, "B", "--tries=3"}, nil); err != nil {
		t.Fatalf("set-active error = %v", err)
	}
	if err := run([]string{"mark-successful", path, "B"}, nil); err != nil {
		t.Fatalf("mark-successful error = %v", err)
	}

	data := readTestABData(t, path)
	if data.Slots[1] != (ota.SlotData{Priority: 15, TriesRemaining: 0, SuccessfulBoot: true}) {
		t.Fatalf("slot B = %+v", data.Slots[1])
	}
	if !data.Slots[0].SuccessfulBoot {
		t.Fatalf("slot A successful = false, want previous rollback slot still successful")
	}
}

func TestRunWriteExplicitState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "misc.img")
	if err := run([]string{"init", path}, nil); err != nil {
		t.Fatalf("init error = %v", err)
	}
	args := []string{
		"write", path,
		"--a-priority", "9", "--a-tries=2", "--a-successful", "0",
		"--b-priority=15", "--b-tries", "0", "--b-successful=1",
	}
	if err := run(args, nil); err != nil {
		t.Fatalf("write error = %v", err)
	}

	data := readTestABData(t, path)
	if data.Slots[0] != (ota.SlotData{Priority: 9, TriesRemaining: 2, SuccessfulBoot: false}) {
		t.Fatalf("slot A = %+v", data.Slots[0])
	}
	if data.Slots[1] != (ota.SlotData{Priority: 15, TriesRemaining: 0, SuccessfulBoot: true}) {
		t.Fatalf("slot B = %+v", data.Slots[1])
	}
}

func TestRunRejectsInvalidSetActiveFlags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "misc.img")
	if err := run([]string{"init", path}, nil); err != nil {
		t.Fatalf("init error = %v", err)
	}
	if err := run([]string{"set-active", path, "B", "--tries", "8"}, nil); err == nil {
		t.Fatal("set-active tries=8 error = nil, want error")
	}
	if err := run([]string{"set-active", path, "B", "--successful", "2"}, nil); err == nil {
		t.Fatal("set-active successful=2 error = nil, want error")
	}
}

func TestRunRejectsInvalidWriteValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "misc.img")
	if err := run([]string{"init", path}, nil); err != nil {
		t.Fatalf("init error = %v", err)
	}

	base := []string{
		"write", path,
		"--a-priority", "9", "--a-tries", "2", "--a-successful", "0",
		"--b-priority", "15", "--b-tries", "0", "--b-successful", "1",
	}
	tests := []struct {
		name  string
		flag  string
		value string
	}{
		{"priority", "--a-priority", "16"},
		{"tries", "--a-tries", "8"},
		{"successful", "--a-successful", "2"},
		{"successful with tries", "--b-tries", "1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string(nil), base...)
			for i := 2; i < len(args)-1; i += 2 {
				if args[i] == tt.flag {
					args[i+1] = tt.value
				}
			}
			if err := run(args, nil); err == nil {
				t.Fatalf("write %s=%s error = nil, want error", tt.flag, tt.value)
			}
		})
	}
}

func TestRunWritePreservesLastBoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "misc.img")
	if err := run([]string{"init", path}, nil); err != nil {
		t.Fatalf("init error = %v", err)
	}
	data := readTestABData(t, path)
	data.LastBoot = ota.SlotB
	writeTestABData(t, path, data)

	args := []string{
		"write", path,
		"--a-priority", "9", "--a-tries", "2", "--a-successful", "0",
		"--b-priority", "15", "--b-tries", "0", "--b-successful", "1",
	}
	if err := run(args, nil); err != nil {
		t.Fatalf("write error = %v", err)
	}
	if got := readTestABData(t, path).LastBoot; got != ota.SlotB {
		t.Fatalf("LastBoot = %d, want preserved SlotB", got)
	}
}

func readTestABData(t *testing.T, path string) ota.ABData {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open misc: %v", err)
	}
	defer f.Close()
	data, err := ota.ReadABData(f)
	if err != nil {
		t.Fatalf("ReadABData() error = %v", err)
	}
	return data
}

func writeTestABData(t *testing.T, path string, data ota.ABData) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open misc: %v", err)
	}
	defer f.Close()
	if err := ota.WriteABDataAt(f, data); err != nil {
		t.Fatalf("WriteABDataAt() error = %v", err)
	}
}
