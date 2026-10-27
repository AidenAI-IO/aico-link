package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestEnsureADBReverseUsesResolvedSerial(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test shell script uses /bin/sh")
	}

	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "adb.log")
	adbPath := filepath.Join(tmpDir, "adb")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"$AICO_TEST_ADB_LOG\"\n" +
		"if [ \"$1\" = \"devices\" ] && [ \"$2\" = \"-l\" ]; then\n" +
		"  printf 'List of devices attached\\nserial123\\tdevice product:panther model:Pixel_7_Pro device:panther transport_id:1\\n'\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = \"-s\" ] && [ \"$2\" = \"serial123\" ] && [ \"$3\" = \"reverse\" ] && [ \"$4\" = \"tcp:8080\" ] && [ \"$5\" = \"tcp:8080\" ]; then\n" +
		"  exit 0\n" +
		"fi\n" +
		"echo \"unexpected args: $@\" >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(adbPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", adbPath, err)
	}

	t.Setenv("AICO_ADB_PATH", adbPath)
	t.Setenv("AICO_TEST_ADB_LOG", logPath)
	t.Setenv("AICO_ADB_SERIAL", "")
	t.Setenv("ANDROID_SERIAL", "")
	defer clearAutoConfiguredADBSerial("serial123")

	if err := EnsureADBReverse(context.Background(), "8080", "8080"); err != nil {
		t.Fatalf("EnsureADBReverse() error = %v", err)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", logPath, err)
	}
	logText := string(logData)
	if !strings.Contains(logText, "devices -l") {
		t.Fatalf("adb log missing devices call: %q", logText)
	}
	if !strings.Contains(logText, "-s serial123 reverse tcp:8080 tcp:8080") {
		t.Fatalf("adb log missing reverse call: %q", logText)
	}
}
