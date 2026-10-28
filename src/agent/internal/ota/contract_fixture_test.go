package ota

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func contractFixtureRoot(t *testing.T) string {
	t.Helper()
	if configured := os.Getenv("AICO_CONTRACT_FIXTURES"); configured != "" {
		return configured
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "../../../../tests/contracts"))
}

func readContractFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(contractFixtureRoot(t), name))
	if err != nil {
		t.Fatalf("read contract fixture %s: %v", name, err)
	}
	return data
}

func TestSharedContractUDSFixture(t *testing.T) {
	var request map[string]string
	if err := json.Unmarshal(readContractFixture(t, "uds-health-request.json"), &request); err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"type": "request", "method": "health"}; !equalStringMap(request, want) {
		t.Fatalf("request fixture = %#v, want %#v", request, want)
	}

	encoded := readContractFixture(t, "uds-frame-response.bin")
	if len(encoded) < 12 {
		t.Fatalf("UDS fixture is shorter than the 12-byte prefix: %d", len(encoded))
	}
	headerSize := binary.LittleEndian.Uint32(encoded[:4])
	payloadSize := binary.LittleEndian.Uint64(encoded[4:12])
	payloadStart := uint64(12) + uint64(headerSize)
	if payloadStart+payloadSize != uint64(len(encoded)) {
		t.Fatalf("UDS fixture lengths are inconsistent: header=%d payload=%d total=%d", headerSize, payloadSize, len(encoded))
	}
	var header map[string]string
	if err := json.Unmarshal(encoded[12:payloadStart], &header); err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"type": "response", "method": "latest_frame", "seq": "7"}; !equalStringMap(header, want) {
		t.Fatalf("response fixture = %#v, want %#v", header, want)
	}
	if want := []byte{0, 1, 2, 3}; !bytes.Equal(encoded[payloadStart:], want) {
		t.Fatalf("payload = %v, want %v", encoded[payloadStart:], want)
	}
}

func TestSharedConfigAndOTAFixtures(t *testing.T) {
	var config map[string]any
	if err := json.Unmarshal(readContractFixture(t, "config-wire.json"), &config); err != nil {
		t.Fatal(err)
	}
	model, ok := config["model"].(map[string]any)
	if !ok || model["provider"] != "openai" || model["model"] != "gpt-4" {
		t.Fatalf("unexpected model fixture: %#v", config["model"])
	}
	search, ok := config["search"].(map[string]any)
	if !ok || search["provider"] != "duckduckgo" {
		t.Fatalf("unexpected search fixture: %#v", config["search"])
	}

	encoded := readContractFixture(t, "ota-manifest.json")
	var manifest Manifest
	if err := json.Unmarshal(encoded, &manifest); err != nil {
		t.Fatal(err)
	}
	if err := manifest.Validate(); err != nil {
		t.Fatalf("manifest fixture does not validate: %v", err)
	}
	if len(manifest.Parts) != 2 || manifest.Parts[0].Name != "boot" || manifest.Parts[1].Name != "rootfs" {
		t.Fatalf("unexpected manifest parts: %#v", manifest.Parts)
	}
	if _, err := base64.StdEncoding.DecodeString(manifest.Signature.Value); err != nil {
		t.Fatalf("manifest signature is not base64: %v", err)
	}
	canonical, err := CanonicalManifestJSONBytes(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(canonical, []byte(`"value"`)) {
		t.Fatalf("canonical manifest still contains the signature value: %s", canonical)
	}
}

func equalStringMap(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, want := range right {
		if left[key] != want {
			return false
		}
	}
	return true
}
