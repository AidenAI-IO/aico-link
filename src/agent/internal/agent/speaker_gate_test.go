package agent

import (
	"path/filepath"
	"testing"
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
	store := NewSpeakerProfileStore(dir, 2)
	if err := store.Save("a", unitSpeakerStub("owner")); err != nil {
		t.Fatal(err)
	}
	if err := store.Save("b", unitSpeakerStub("owner")); err != nil {
		t.Fatal(err)
	}
	if err := store.Save("c", unitSpeakerStub("owner")); err == nil {
		t.Fatal("expected profile limit error")
	}
	if err := store.Save("a", unitSpeakerStub("other")); err != nil {
		t.Fatalf("replace existing should succeed: %v", err)
	}
	list, err := store.List()
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %#v err=%v", list, err)
	}
	if _, err := store.LoadAll(); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("missing"); err == nil {
		t.Fatal("expected missing delete error")
	}
	_ = filepath.Join(dir, "a.f32")
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
