package ota

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testHashA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testHashB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const testHashC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

func TestCanonicalManifestSigningSuccessAndTamperFailure(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}

	manifest := validTestManifest()
	canonical, err := CanonicalManifestJSON(manifest)
	if err != nil {
		t.Fatalf("CanonicalManifestJSON() error = %v", err)
	}
	manifest.Signature = ManifestSignature{Algorithm: "ed25519", Value: hex.EncodeToString(ed25519.Sign(priv, canonical))}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal(manifest) error = %v", err)
	}

	if _, err := VerifyManifestJSON(manifestBytes, pub); err != nil {
		t.Fatalf("VerifyManifestJSON() error = %v", err)
	}

	tampered := []byte(strings.Replace(string(manifestBytes), testHashA, testHashC, 1))
	if _, err := VerifyManifestJSON(tampered, pub); err == nil {
		t.Fatal("VerifyManifestJSON() error = nil, want tamper failure")
	}
}

func TestCanonicalManifestJSONRemovesOnlySignatureValue(t *testing.T) {
	manifest := validTestManifest()
	manifest.Signature = ManifestSignature{Algorithm: "ed25519", Value: strings.Repeat("1", ed25519.SignatureSize*2)}

	canonical, err := CanonicalManifestJSON(manifest)
	if err != nil {
		t.Fatalf("CanonicalManifestJSON() error = %v", err)
	}
	if strings.Contains(string(canonical), "\"value\"") {
		t.Fatalf("canonical JSON contains signature value field: %s", canonical)
	}
	if !strings.Contains(string(canonical), "\"algorithm\":\"ed25519\"") {
		t.Fatalf("canonical JSON lost signature algorithm: %s", canonical)
	}
	if !json.Valid(canonical) {
		t.Fatalf("canonical JSON is invalid: %s", canonical)
	}
	if strings.HasSuffix(string(canonical), "\n") {
		t.Fatalf("canonical JSON has trailing newline: %q", canonical)
	}
}

func TestCanonicalManifestJSONBytesPreservesUnknownSignedFields(t *testing.T) {
	raw := []byte(`{"signature":{"value":"abc","algorithm":"ed25519"},"schema_version":2,"channel":"stable","version":"20260521-120000-abcdef0","build_time":"2026-05-21T12:00:00Z","compatibility":{"board":"aico-link"},"parts":[{"name":"boot","asset_a":{"name":"boot_a.img","size":1,"sha256":"` + testHashA + `"},"asset_b":{"name":"boot_b.img","size":2,"sha256":"` + testHashB + `"}}]}`)

	canonical, err := CanonicalManifestJSONBytes(raw)
	if err != nil {
		t.Fatalf("CanonicalManifestJSONBytes() error = %v", err)
	}
	if strings.Contains(string(canonical), "\"value\"") {
		t.Fatalf("canonical JSON contains signature value field: %s", canonical)
	}
	if !strings.Contains(string(canonical), "\"compatibility\":{") || !strings.Contains(string(canonical), "aico-link") {
		t.Fatalf("canonical JSON lost unknown signed field: %s", canonical)
	}
}

func TestVerifyManifestJSONPreservesUnknownSignedFields(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	rawTemplate := []byte(`{"schema_version":2,"channel":"stable","version":"20260521-120000-abcdef0","build_time":"2026-05-21T12:00:00Z","compatibility":{"board":"aico-link"},"parts":[{"name":"boot","asset_a":{"name":"boot_a.img","size":1,"sha256":"` + testHashA + `"},"asset_b":{"name":"boot_b.img","size":2,"sha256":"` + testHashB + `"}}],"signature":{"algorithm":"ed25519"}}`)
	canonical, err := CanonicalManifestJSONBytes(rawTemplate)
	if err != nil {
		t.Fatalf("CanonicalManifestJSONBytes() error = %v", err)
	}
	sig := hex.EncodeToString(ed25519.Sign(priv, canonical))
	signed := []byte(strings.Replace(string(rawTemplate), `"signature":{"algorithm":"ed25519"}`, `"signature":{"algorithm":"ed25519","value":"`+sig+`"}`, 1))

	manifest, err := VerifyManifestJSON(signed, pub)
	if err != nil {
		t.Fatalf("VerifyManifestJSON() error = %v", err)
	}
	if manifest.Version != "20260521-120000-abcdef0" {
		t.Fatalf("VerifyManifestJSON() version = %q", manifest.Version)
	}

	tampered := []byte(strings.Replace(string(signed), "aico-link", "other-board", 1))
	if _, err := VerifyManifestJSON(tampered, pub); err == nil {
		t.Fatal("VerifyManifestJSON() error = nil, want tamper failure")
	}
}

func TestManifestVerifyParsesPEMPublicKey(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey() error = %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})

	manifest := validTestManifest()
	canonical, err := CanonicalManifestJSON(manifest)
	if err != nil {
		t.Fatalf("CanonicalManifestJSON() error = %v", err)
	}
	manifest.Signature = ManifestSignature{Algorithm: "ed25519", Value: hex.EncodeToString(ed25519.Sign(priv, canonical))}

	parsed, err := ParseEd25519PublicKeyPEM(pemBytes)
	if err != nil {
		t.Fatalf("ParseEd25519PublicKeyPEM() error = %v", err)
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal(manifest) error = %v", err)
	}
	if _, err := VerifyManifestJSON(manifestBytes, parsed); err != nil {
		t.Fatalf("VerifyManifestJSON() with parsed key error = %v", err)
	}
}

func TestManifestValidationAllowsOnlyCanonicalPartNames(t *testing.T) {
	for _, name := range []string{"boot", "rootfs"} {
		manifest := validTestManifest()
		manifest.Parts = []ManifestPart{{Name: name, Asset: &ManifestAsset{Name: name + ".img", Size: 1, SHA256: testHashA}}}
		if name == "boot" {
			manifest.Parts[0].Asset = nil
			manifest.Parts[0].AssetA = &ManifestAsset{Name: "boot_a.img", Size: 1, SHA256: testHashA}
			manifest.Parts[0].AssetB = &ManifestAsset{Name: "boot_b.img", Size: 2, SHA256: testHashB}
		}
		if err := manifest.Validate(); err != nil {
			t.Fatalf("Validate() for %q error = %v", name, err)
		}
	}

	manifest := validTestManifest()
	manifest.Parts[0].Name = "kernel"
	if err := manifest.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want rejected part name")
	}
}

func TestManifestValidationAllowsTarGzCompressedImageAssets(t *testing.T) {
	manifest := validTestManifest()
	manifest.Parts[0].AssetA.Name = "boot_a.img.tar.gz"
	manifest.Parts[0].AssetA.ImageSHA256 = testHashA
	manifest.Parts[0].AssetB.Name = "boot_b.img.tar.gz"
	manifest.Parts[0].AssetB.ImageSHA256 = testHashB
	manifest.Parts[1].Asset.Name = "rootfs.img.tar.gz"
	manifest.Parts[1].Asset.ImageSHA256 = testHashC

	if err := manifest.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestManifestValidationRequiresImageSHA256ForTarGzAssets(t *testing.T) {
	manifest := validTestManifest()
	manifest.Parts[0].AssetB.Name = "boot_b.img.tar.gz"

	if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "image_sha256") {
		t.Fatalf("Validate() error = %v, want missing image_sha256 rejection", err)
	}
}

func TestManifestValidationRejectsDuplicateParts(t *testing.T) {
	manifest := validTestManifest()
	manifest.Parts = append(manifest.Parts, manifest.Parts[0])
	if err := manifest.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want duplicate part rejection")
	}
}

func TestManifestValidationRequiresBootSlotAssets(t *testing.T) {
	manifest := validTestManifest()
	manifest.Parts[0] = ManifestPart{Name: "boot", Asset: &ManifestAsset{Name: "boot.img", Size: 1, SHA256: testHashA}}
	if err := manifest.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want boot slot asset rejection")
	}
}

func TestManifestValidationEnforcesAssetNameCoherence(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{"boot asset_a", func(m *Manifest) { m.Parts[0].AssetA.Name = "boot_b.img" }},
		{"boot asset_b", func(m *Manifest) { m.Parts[0].AssetB.Name = "boot_a.img" }},
		{"rootfs neutral", func(m *Manifest) { m.Parts[1].Asset.Name = "rootfs_a.img" }},
		{"rootfs asset_a", func(m *Manifest) {
			m.Parts[1] = ManifestPart{Name: "rootfs", AssetA: &ManifestAsset{Name: "rootfs.img", Size: 1, SHA256: testHashA}, AssetB: &ManifestAsset{Name: "rootfs_b.img", Size: 2, SHA256: testHashB}}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := validTestManifest()
			tt.mutate(&manifest)
			if err := manifest.Validate(); err == nil {
				t.Fatal("Validate() error = nil, want asset name coherence rejection")
			}
		})
	}
}

func TestManifestValidationRejectsUppercaseSHA256(t *testing.T) {
	manifest := validTestManifest()
	manifest.Parts[0].AssetA.SHA256 = strings.ToUpper(manifest.Parts[0].AssetA.SHA256)
	if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "lowercase") {
		t.Fatalf("Validate() error = %v, want lowercase sha256 rejection", err)
	}
}

func TestDecodeSignatureValueFallsBackToBase64AfterShortHex(t *testing.T) {
	_, err := decodeSignatureValue("abcd")
	if err == nil || !strings.Contains(err.Error(), "signature length 3") {
		t.Fatalf("decodeSignatureValue() error = %v, want base64-decoded length failure", err)
	}
}

func TestAssetResolutionSelectsTargetSlotAssets(t *testing.T) {
	boot := ManifestPart{Name: "boot", AssetA: &ManifestAsset{Name: "boot_a.img", Size: 11, SHA256: testHashA}, AssetB: &ManifestAsset{Name: "boot_b.img", Size: 22, SHA256: testHashB}}

	asset, err := ResolveAsset(boot, SlotA)
	if err != nil {
		t.Fatalf("ResolveAsset(A) error = %v", err)
	}
	if asset.Name != "boot_a.img" || asset.Size != 11 || asset.SHA256 != testHashA {
		t.Fatalf("ResolveAsset(A) = %+v", asset)
	}

	asset, err = ResolveAsset(boot, SlotB)
	if err != nil {
		t.Fatalf("ResolveAsset(B) error = %v", err)
	}
	if asset.Name != "boot_b.img" || asset.Size != 22 || asset.SHA256 != testHashB {
		t.Fatalf("ResolveAsset(B) = %+v", asset)
	}
}

func TestAssetResolutionFallsBackToNeutralForRootfs(t *testing.T) {
	part := ManifestPart{Name: "rootfs", Asset: &ManifestAsset{Name: "rootfs.img", Size: 33, SHA256: testHashC}}

	asset, err := ResolveAsset(part, SlotB)
	if err != nil {
		t.Fatalf("ResolveAsset() error = %v", err)
	}
	if asset.Name != "rootfs.img" || asset.Size != 33 || asset.SHA256 != testHashC {
		t.Fatalf("ResolveAsset() = %+v", asset)
	}
}

func TestAssetResolutionRejectsIncoherentAssetName(t *testing.T) {
	part := ManifestPart{Name: "boot", AssetA: &ManifestAsset{Name: "boot_b.img", Size: 11, SHA256: testHashA}, AssetB: &ManifestAsset{Name: "boot_b.img", Size: 22, SHA256: testHashB}}
	if _, err := ResolveAsset(part, SlotA); err == nil {
		t.Fatal("ResolveAsset() error = nil, want asset name coherence rejection")
	}
}

func TestManifestGeneratorScriptOutputVerifies(t *testing.T) {
	requireCommand(t, "openssl")
	requireCommand(t, "jq")
	requireCommand(t, "stat")

	repoRoot := filepath.Clean(filepath.Join("..", "..", "..", ".."))
	repoRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		t.Fatalf("Abs(repoRoot) error = %v", err)
	}
	scriptPath := filepath.Join(repoRoot, "scripts", "generate_ota_manifest.sh")
	if _, err := os.Stat(scriptPath); err != nil {
		t.Fatalf("generator script stat error = %v", err)
	}

	tmpDir := t.TempDir()
	imageDir := filepath.Join(tmpDir, "images")
	if err := os.Mkdir(imageDir, 0o755); err != nil {
		t.Fatalf("Mkdir(imageDir) error = %v", err)
	}
	for name, content := range map[string]string{
		"boot_a.img":   "boot-a",
		"boot_b.img":   "boot-b",
		"rootfs_a.img": "rootfs-a",
		"rootfs_b.img": "rootfs-b",
	} {
		if err := os.WriteFile(filepath.Join(imageDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}

	keyPath := filepath.Join(tmpDir, "signing.pem")
	pubPath := filepath.Join(tmpDir, "pub.pem")
	runCommand(t, repoRoot, "openssl", "genpkey", "-algorithm", "ED25519", "-out", keyPath)
	runCommand(t, repoRoot, "openssl", "pkey", "-in", keyPath, "-pubout", "-out", pubPath)

	manifestPath := filepath.Join(tmpDir, "manifest.json")
	runCommand(t, repoRoot, scriptPath,
		"--version", "20260521-120000-abcdef0",
		"--channel", "stable",
		"--build-time", "2026-05-21T12:00:00Z",
		"--sign-key", keyPath,
		"--image-dir", imageDir,
		"--output", manifestPath,
	)

	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("ReadFile(manifest) error = %v", err)
	}
	publicKeyPEM, err := os.ReadFile(pubPath)
	if err != nil {
		t.Fatalf("ReadFile(public key) error = %v", err)
	}
	manifest, err := VerifyManifestJSONWithPublicKeyPEM(manifestBytes, publicKeyPEM)
	if err != nil {
		t.Fatalf("VerifyManifestJSONWithPublicKeyPEM() error = %v", err)
	}
	if len(manifest.Parts) != 2 {
		t.Fatalf("generated parts = %d, want 2", len(manifest.Parts))
	}
}

func TestManifestGeneratorRejectsDeviceInvalidMetadata(t *testing.T) {
	requireCommand(t, "openssl")
	requireCommand(t, "jq")

	repoRoot := filepath.Clean(filepath.Join("..", "..", "..", ".."))
	repoRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		t.Fatalf("Abs(repoRoot) error = %v", err)
	}
	scriptPath := filepath.Join(repoRoot, "scripts", "generate_ota_manifest.sh")
	tmpDir := t.TempDir()
	imageDir := filepath.Join(tmpDir, "images")
	if err := os.Mkdir(imageDir, 0o755); err != nil {
		t.Fatalf("Mkdir(imageDir) error = %v", err)
	}
	for _, name := range []string{"boot_a.img", "boot_b.img", "rootfs.img"} {
		if err := os.WriteFile(filepath.Join(imageDir, name), []byte(name), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
	keyPath := filepath.Join(tmpDir, "signing.pem")
	runCommand(t, repoRoot, "openssl", "genpkey", "-algorithm", "ED25519", "-out", keyPath)

	tests := []struct {
		name      string
		version   string
		channel   string
		buildTime string
	}{
		{"version", "bad version with spaces", "stable", "2026-05-21T12:00:00Z"},
		{"channel", "20260521-120000-abcdef0", "bad channel", "2026-05-21T12:00:00Z"},
		{"build_time", "20260521-120000-abcdef0", "stable", "May 21 2026"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command(scriptPath,
				"--version", tt.version,
				"--channel", tt.channel,
				"--build-time", tt.buildTime,
				"--sign-key", keyPath,
				"--image-dir", imageDir,
				"--output", filepath.Join(tmpDir, tt.name+".json"),
			)
			cmd.Dir = repoRoot
			if output, err := cmd.CombinedOutput(); err == nil {
				t.Fatalf("generator succeeded with invalid %s, output: %s", tt.name, output)
			}
		})
	}
}

func requireCommand(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not available: %v", name, err)
	}
}

func runCommand(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, output)
	}
}

func validTestManifest() Manifest {
	return Manifest{
		SchemaVersion: 2,
		Channel:       "stable",
		Version:       "20260521-120000-abcdef0",
		BuildTime:     time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC).Format(time.RFC3339),
		Parts: []ManifestPart{
			{Name: "boot", AssetA: &ManifestAsset{Name: "boot_a.img", Size: 11, SHA256: testHashA}, AssetB: &ManifestAsset{Name: "boot_b.img", Size: 22, SHA256: testHashB}},
			{Name: "rootfs", Asset: &ManifestAsset{Name: "rootfs.img", Size: 55, SHA256: testHashC}},
		},
		Signature: ManifestSignature{Algorithm: "ed25519"},
	}
}

func TestManifestAssetWithURL(t *testing.T) {
	manifest := validTestManifest()
	manifest.Parts[0].AssetA.URL = "https://example.com/boot_a.img"
	manifest.Parts[0].AssetB.URL = "https://example.com/boot_b.img"

	if err := manifest.Validate(); err != nil {
		t.Fatalf("Validate() with URLs error = %v", err)
	}
}

func TestManifestAssetURLValidation(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"valid https", "https://example.com/file.img", false},
		{"valid http", "http://example.com/file.img", false},
		{"invalid ftp", "ftp://example.com/file.img", true},
		{"invalid file", "file:///path/to/file.img", true},
		{"with whitespace", "https://example.com/file .img", true},
		{"empty is ok", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			asset := ManifestAsset{
				Name:   "boot_a.img",
				URL:    tt.url,
				Size:   12345,
				SHA256: testHashA,
			}
			err := validateManifestAsset("test", asset, "boot_a.img")
			if (err != nil) != tt.wantErr {
				t.Errorf("validateManifestAsset() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestManifestWithURLsSignatureVerification(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}

	manifest := validTestManifest()
	manifest.Parts[0].AssetA.URL = "https://cdn.example.com/firmware/boot_a.img"
	manifest.Parts[0].AssetB.URL = "https://cdn.example.com/firmware/boot_b.img"

	canonical, err := CanonicalManifestJSON(manifest)
	if err != nil {
		t.Fatalf("CanonicalManifestJSON() error = %v", err)
	}
	manifest.Signature = ManifestSignature{
		Algorithm: "ed25519",
		Value:     hex.EncodeToString(ed25519.Sign(priv, canonical)),
	}

	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal(manifest) error = %v", err)
	}

	verified, err := VerifyManifestJSON(manifestBytes, pub)
	if err != nil {
		t.Fatalf("VerifyManifestJSON() error = %v", err)
	}

	if verified.Parts[0].AssetA.URL != "https://cdn.example.com/firmware/boot_a.img" {
		t.Errorf("URL not preserved: got %q", verified.Parts[0].AssetA.URL)
	}
}

func TestManifestRejectsLegacyLayout(t *testing.T) {
	manifest := validTestManifest()
	manifest.SchemaVersion = 1
	if err := manifest.Validate(); err == nil {
		t.Fatal("legacy schema was accepted")
	}
	manifest = validTestManifest()
	manifest.Parts = append(manifest.Parts, ManifestPart{Name: "oem", Asset: &ManifestAsset{Name: "oem.img", Size: 1, SHA256: testHashA}})
	if err := manifest.Validate(); err == nil {
		t.Fatal("retired OEM partition was accepted")
	}
	if err := requireAtomicProductionManifest(validTestManifest()); err != nil {
		t.Fatal(err)
	}
	manifest = validTestManifest()
	manifest.Parts = manifest.Parts[:1]
	if err := requireAtomicProductionManifest(manifest); err == nil {
		t.Fatal("partial production manifest was accepted")
	}
}

func TestManifestRootFSSlotAssets(t *testing.T) {
	manifest := validTestManifest()
	manifest.Parts[1] = ManifestPart{Name: "rootfs", AssetA: &ManifestAsset{Name: "rootfs_a.img", Size: 1, SHA256: testHashA}, AssetB: &ManifestAsset{Name: "rootfs_b.img", Size: 2, SHA256: testHashB}}
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	asset, err := ResolveAsset(manifest.Parts[1], SlotB)
	if err != nil || asset.Name != "rootfs_b.img" {
		t.Fatalf("asset=%+v err=%v", asset, err)
	}
	manifest.Parts[1].AssetB.Name = "rootfs_a.img"
	if err := manifest.Validate(); err == nil {
		t.Fatal("wrong slot asset accepted")
	}
}
