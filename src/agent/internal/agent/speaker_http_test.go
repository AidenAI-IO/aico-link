package agent

import (
	"encoding/binary"
	"strings"
	"testing"
)

func TestPCM16FromWAVRejectsOverflowingChunkSize(t *testing.T) {
	// Minimal RIFF/WAVE header plus one chunk whose size would wrap int on arm.
	data := make([]byte, 20)
	copy(data[0:4], "RIFF")
	copy(data[8:12], "WAVE")
	copy(data[12:16], "data")
	binary.LittleEndian.PutUint32(data[16:20], 0x7FFFFFF8)

	_, err := pcm16FromWAV(data)
	if err == nil || !strings.Contains(err.Error(), "invalid wav chunk") {
		t.Fatalf("pcm16FromWAV() error = %v, want invalid wav chunk", err)
	}
}

func TestPCM16FromWAVAcceptsValidMonoPCM16(t *testing.T) {
	pcm := []byte{0x00, 0x10, 0xff, 0x7f}
	data := make([]byte, 44+len(pcm))
	copy(data[0:4], "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], uint32(36+len(pcm)))
	copy(data[8:12], "WAVE")
	copy(data[12:16], "fmt ")
	binary.LittleEndian.PutUint32(data[16:20], 16)
	binary.LittleEndian.PutUint16(data[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(data[22:24], 1) // mono
	binary.LittleEndian.PutUint32(data[24:28], speakerSampleRate)
	binary.LittleEndian.PutUint32(data[28:32], speakerSampleRate*2)
	binary.LittleEndian.PutUint16(data[32:34], 2)
	binary.LittleEndian.PutUint16(data[34:36], 16)
	copy(data[36:40], "data")
	binary.LittleEndian.PutUint32(data[40:44], uint32(len(pcm)))
	copy(data[44:], pcm)

	got, err := pcm16FromWAV(data)
	if err != nil {
		t.Fatalf("pcm16FromWAV() error = %v", err)
	}
	if len(got) != 2 || got[0] != 0x1000 || got[1] != 0x7fff {
		t.Fatalf("pcm16FromWAV() = %#v, want two PCM16 samples", got)
	}
}
