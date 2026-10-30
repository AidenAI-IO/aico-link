package tts

import "testing"

func TestTextGranularityValuesAreStable(t *testing.T) {
	if TextGranularityIncremental != "incremental" || TextGranularitySentence != "sentence" || TextGranularityBatch != "batch" {
		t.Fatalf("unexpected text granularity values: %q %q %q", TextGranularityIncremental, TextGranularitySentence, TextGranularityBatch)
	}
}

func TestCapabilitiesDistinguishTextAndAudioGranularity(t *testing.T) {
	caps := Capabilities{
		TextGranularity:          TextGranularitySentence,
		SupportsIncrementalAudio: true,
	}
	if caps.TextGranularity != TextGranularitySentence {
		t.Fatalf("TextGranularity = %q, want sentence", caps.TextGranularity)
	}
	if !caps.SupportsIncrementalAudio {
		t.Fatal("SupportsIncrementalAudio = false, want true")
	}
}
