package agent

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

type stubSpeakerEmbedder struct {
	vectors map[string][]float32
	lastPCM int
}

func (s *stubSpeakerEmbedder) Embed(pcm []int16) ([]float32, error) {
	s.lastPCM = len(pcm)
	key := "default"
	if len(pcm) > 0 && pcm[0] == 1 {
		key = "owner"
	}
	if len(pcm) > 0 && pcm[0] == 2 {
		key = "other"
	}
	vec := s.vectors[key]
	if vec == nil {
		vec = unitSpeakerStub("owner")
	}
	return append([]float32(nil), vec...), nil
}

func unitSpeakerStub(seed string) []float32 {
	out := make([]float32, speakerEmbeddingDim)
	out[0] = 1
	if seed == "other" {
		out[0] = 0
		out[1] = 1
	}
	return out
}

func TestSpeakerGateDisabledAllowsEveryone(t *testing.T) {
	gate := &SpeakerGate{Enabled: false}
	got := gate.AllowUtterance([]int16{1, 2, 3})
	if !got.Allowed || got.Reason != "gate_disabled" {
		t.Fatalf("decision = %#v", got)
	}
}

func TestSpeakerGateStatusReportsProfilesWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	store := NewSpeakerProfileStore(dir, 3)
	if err := store.Save("owner", unitSpeakerStub("owner")); err != nil {
		t.Fatal(err)
	}
	gate := &SpeakerGate{Enabled: false, Threshold: 0.4, Store: store}
	enabled, count, threshold, err := gate.Status()
	if err != nil || enabled || count != 1 || threshold != 0.4 {
		t.Fatalf("status enabled=%v count=%d threshold=%v err=%v", enabled, count, threshold, err)
	}
}

func TestSpeakerGateRequiresProfiles(t *testing.T) {
	dir := t.TempDir()
	gate := &SpeakerGate{
		Enabled:   true,
		Threshold: 0.5,
		Embedder:  &stubSpeakerEmbedder{vectors: map[string][]float32{"owner": unitSpeakerStub("owner")}},
		Store:     NewSpeakerProfileStore(dir, 3),
	}
	got := gate.AllowUtterance([]int16{1})
	if got.Allowed || got.Reason != "no_profiles" {
		t.Fatalf("decision = %#v", got)
	}
}

func TestSpeakerGateSyncLiveSettingsUpdatesThreshold(t *testing.T) {
	dir := t.TempDir()
	store := NewSpeakerProfileStore(dir, 3)
	profile := make([]float32, speakerEmbeddingDim)
	profile[0] = 1
	if err := store.Save("owner", profile); err != nil {
		t.Fatal(err)
	}
	query := make([]float32, speakerEmbeddingDim)
	query[0] = 0.8
	query[1] = 0.6 // cosine vs [1,0,...] == 0.8
	gate := &SpeakerGate{
		Enabled:   true,
		Threshold: 0.90,
		Embedder:  &fixedSpeakerEmbedder{vec: query},
		Store:     store,
	}
	high := gate.AllowUtterance([]int16{1})
	if high.Allowed || high.Score < 0.79 || high.Score > 0.81 {
		t.Fatalf("expected reject near score 0.8 with threshold 0.90, got %#v", high)
	}
	enabled := true
	gate.SyncLiveSettings(Config{
		VoiceSpeakerGateEnabled: &enabled,
		VoiceSpeakerThreshold:   0.50,
	})
	low := gate.AllowUtterance([]int16{1})
	if !low.Allowed || low.Threshold != 0.50 {
		t.Fatalf("expected accept after live threshold sync, got %#v", low)
	}
}

type fixedSpeakerEmbedder struct {
	vec []float32
}

func (f *fixedSpeakerEmbedder) Embed(pcm []int16) ([]float32, error) {
	return append([]float32(nil), f.vec...), nil
}

func TestSpeakerGateMatchesOwnerRejectsOther(t *testing.T) {
	dir := t.TempDir()
	store := NewSpeakerProfileStore(dir, 3)
	if err := store.Save("owner", unitSpeakerStub("owner")); err != nil {
		t.Fatal(err)
	}
	gate := &SpeakerGate{
		Enabled:   true,
		Threshold: 0.5,
		Embedder: &stubSpeakerEmbedder{vectors: map[string][]float32{
			"owner": unitSpeakerStub("owner"),
			"other": unitSpeakerStub("other"),
		}},
		Store: store,
	}
	owner := gate.AllowUtterance([]int16{1})
	if !owner.Allowed || owner.Score < 0.99 {
		t.Fatalf("owner decision = %#v", owner)
	}
	other := gate.AllowUtterance([]int16{2})
	if other.Allowed || other.Score > 0.1 {
		t.Fatalf("other decision = %#v", other)
	}
}

func TestSpeakerProfileStoreLimitAndReplace(t *testing.T) {
	dir := t.TempDir()
	store := NewSpeakerProfileStore(dir, 1)
	if store.MaxProfiles() != 1 {
		t.Fatalf("MaxProfiles() = %d, want 1", store.MaxProfiles())
	}
	if err := store.Save("a", unitSpeakerStub("owner")); err != nil {
		t.Fatal(err)
	}
	// Any enroll id collapses onto the canonical owner slot.
	if err := store.Save("b", unitSpeakerStub("owner")); err != nil {
		t.Fatalf("single-profile replace should succeed: %v", err)
	}
	list, err := store.List()
	if err != nil || len(list) != 1 || list[0].ID != speakerOwnerProfileID {
		t.Fatalf("list = %#v err=%v", list, err)
	}
	if err := store.Save(speakerOwnerProfileID, unitSpeakerStub("other")); err != nil {
		t.Fatalf("overwrite same id should succeed: %v", err)
	}
	owner, err := store.LoadOwner()
	if err != nil || owner == nil {
		t.Fatalf("LoadOwner = %v err=%v", owner, err)
	}
	if err := store.Delete(speakerOwnerProfileID); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("missing"); err == nil {
		t.Fatal("expected missing delete error")
	}
	if clamped := NewSpeakerProfileStore(dir, 5); clamped.MaxProfiles() != 1 {
		t.Fatalf("clamped MaxProfiles() = %d, want 1", clamped.MaxProfiles())
	}
	_ = filepath.Join(dir, "a.f32")
}

func TestSpeakerProfileStoreListSelectsNewestWithoutDeleting(t *testing.T) {
	dir := t.TempDir()
	store := NewSpeakerProfileStore(dir, 5)
	writeLegacySpeakerProfile(t, dir, speakerOwnerProfileID, unitSpeakerStub("owner"), time.Now().UTC().Add(-time.Hour))
	writeLegacySpeakerProfile(t, dir, "speaker3", unitSpeakerStub("other"), time.Now().UTC())

	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "speaker3" {
		t.Fatalf("list should surface newest legacy owner, got %#v", list)
	}
	// Read path must not delete extras.
	if _, err := os.Stat(filepath.Join(dir, speakerOwnerProfileID+".f32")); err != nil {
		t.Fatalf("stale canonical profile should remain until Save: %v", err)
	}
	owner, err := store.LoadOwner()
	if err != nil || owner == nil || owner[1] != 1 {
		t.Fatalf("LoadOwner should use newest (other) vector, got %v err=%v", owner, err)
	}
}

func TestSpeakerProfileStoreSaveConsolidatesLegacyExtras(t *testing.T) {
	dir := t.TempDir()
	store := NewSpeakerProfileStore(dir, 5)
	writeLegacySpeakerProfile(t, dir, "speaker3", unitSpeakerStub("other"), time.Now().UTC().Add(-time.Minute))
	writeLegacySpeakerProfile(t, dir, "guest", unitSpeakerStub("other"), time.Now().UTC().Add(-2*time.Minute))

	if err := store.Save("anything", unitSpeakerStub("owner")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if len(names) != 2 {
		t.Fatalf("after Save expected only speaker.{f32,json}, got %v", names)
	}
	owner, err := store.LoadOwner()
	if err != nil || owner == nil || owner[0] != 1 {
		t.Fatalf("LoadOwner after consolidate = %v err=%v", owner, err)
	}
}

func TestSpeakerProfileStoreSaveFailureKeepsExisting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory write permissions are not enforced the same way on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory mode bits")
	}
	dir := t.TempDir()
	store := NewSpeakerProfileStore(dir, 1)
	if err := store.Save(speakerOwnerProfileID, unitSpeakerStub("owner")); err != nil {
		t.Fatal(err)
	}
	// Make the store directory read-only so the next Save cannot write the tmp file.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	if err := store.Save(speakerOwnerProfileID, unitSpeakerStub("other")); err == nil {
		t.Fatal("expected Save to fail on read-only dir")
	}
	_ = os.Chmod(dir, 0o755)
	owner, err := store.LoadOwner()
	if err != nil || owner == nil || owner[0] != 1 {
		t.Fatalf("existing owner must survive failed Save, got %v err=%v", owner, err)
	}
}

func TestSpeakerProfileStoreSaveMetaFailureRestoresEmbed(t *testing.T) {
	dir := t.TempDir()
	store := NewSpeakerProfileStore(dir, 1)
	if err := store.Save(speakerOwnerProfileID, unitSpeakerStub("owner")); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(dir, speakerOwnerProfileID+".json")
	if err := os.Remove(metaPath); err != nil {
		t.Fatal(err)
	}
	// Occupy the meta path with a non-empty directory so the final rename fails
	// after the embedding has already been replaced.
	if err := os.Mkdir(metaPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metaPath, "blocker"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(metaPath)
	})

	if err := store.Save(speakerOwnerProfileID, unitSpeakerStub("other")); err == nil {
		t.Fatal("expected Save to fail when meta rename is blocked")
	}
	owner, err := store.LoadOwner()
	if err != nil || owner == nil || owner[0] != 1 {
		t.Fatalf("previous owner embedding must be restored, got %v err=%v", owner, err)
	}
	if _, err := os.Stat(filepath.Join(dir, speakerOwnerProfileID+".f32.bak")); !os.IsNotExist(err) {
		t.Fatalf("successful restore should consume the backup, stat err=%v", err)
	}
}

func TestSpeakerProfileStoreLoadOwnerUsesEmbedBackup(t *testing.T) {
	dir := t.TempDir()
	store := NewSpeakerProfileStore(dir, 1)
	if err := store.Save(speakerOwnerProfileID, unitSpeakerStub("owner")); err != nil {
		t.Fatal(err)
	}
	embedPath := filepath.Join(dir, speakerOwnerProfileID+".f32")
	bakPath := embedPath + ".bak"
	prev, err := os.ReadFile(embedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bakPath, prev, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(embedPath, []byte("truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner, err := store.LoadOwner()
	if err != nil || owner == nil || owner[0] != 1 {
		t.Fatalf("LoadOwner should recover from .f32.bak, got %v err=%v", owner, err)
	}
}

func TestSpeakerProfileStoreDeleteOwnerClearsLegacyExtras(t *testing.T) {
	dir := t.TempDir()
	store := NewSpeakerProfileStore(dir, 5)
	writeLegacySpeakerProfile(t, dir, speakerOwnerProfileID, unitSpeakerStub("owner"), time.Now().UTC().Add(-time.Hour))
	writeLegacySpeakerProfile(t, dir, "speaker3", unitSpeakerStub("other"), time.Now().UTC())

	if err := store.Delete("speaker3"); err != nil {
		t.Fatal(err)
	}
	list, err := store.List()
	if err != nil || len(list) != 0 {
		t.Fatalf("after owner delete expected empty list, got %#v err=%v", list, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("legacy files should be gone, got %v", entries)
	}
}

func TestSharedSpeakerProfileStoreReusesInstance(t *testing.T) {
	dir := t.TempDir()
	a := SharedSpeakerProfileStore(dir, 1)
	b := SharedSpeakerProfileStore(dir, 9)
	if a != b {
		t.Fatal("expected shared store instance for same dir")
	}
	if a.MaxProfiles() != 1 {
		t.Fatalf("MaxProfiles() = %d", a.MaxProfiles())
	}
}

func TestNormalizeSpeakerProfileIDAlwaysOwner(t *testing.T) {
	if got := NormalizeSpeakerProfileID("speaker3"); got != speakerOwnerProfileID {
		t.Fatalf("NormalizeSpeakerProfileID = %q", got)
	}
	if got := NormalizeSpeakerProfileID(""); got != speakerOwnerProfileID {
		t.Fatalf("NormalizeSpeakerProfileID empty = %q", got)
	}
}

func writeLegacySpeakerProfile(t *testing.T, dir, id string, embedding []float32, created time.Time) {
	t.Helper()
	raw := make([]byte, speakerEmbeddingDim*4)
	for i, v := range embedding {
		binary.LittleEndian.PutUint32(raw[i*4:], math.Float32bits(v))
	}
	embedPath := filepath.Join(dir, id+".f32")
	if err := os.WriteFile(embedPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(SpeakerProfileMeta{ID: id, CreatedAt: created.UTC()})
	if err := os.WriteFile(filepath.Join(dir, id+".json"), meta, 0o600); err != nil {
		t.Fatal(err)
	}
	atime := created.UTC()
	if err := os.Chtimes(embedPath, atime, atime); err != nil {
		t.Fatal(err)
	}
}

func TestEnrollmentWindowsSkipQuiet(t *testing.T) {
	pcm := make([]int16, speakerEnrollSamples)
	for i := range pcm {
		pcm[i] = 0
	}
	if windows := enrollmentWindows(pcm); len(windows) != 0 {
		t.Fatalf("quiet enrollment windows = %d", len(windows))
	}
	for i := 1000; i < 3000; i++ {
		pcm[i] = 12000
	}
	if windows := enrollmentWindows(pcm); len(windows) == 0 {
		t.Fatal("expected at least one loud window")
	}
}
