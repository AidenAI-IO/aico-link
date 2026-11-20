package ekho

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"aico-agent/internal/agent/tts"
)

// writeWAVPCM accepts Ekho's PCM16 WAV, skips metadata chunks (including RIFF
// padding), and copies aligned PCM in small blocks instead of loading the WAV.
func writeWAVPCM(ctx context.Context, r io.Reader, sink tts.AudioSink) error {
	var header [12]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return err
	}
	if string(header[:4]) != "RIFF" || string(header[8:]) != "WAVE" {
		return fmt.Errorf("invalid WAV header")
	}
	// Restrict reads to the RIFF payload, including all chunk headers.
	length := int64(binary.LittleEndian.Uint32(header[4:8])) - 4
	if length < 0 {
		return fmt.Errorf("invalid RIFF length")
	}
	r = io.LimitReader(r, length)
	validFormat := false
	for {
		var chunk [8]byte
		if _, err := io.ReadFull(r, chunk[:]); err != nil {
			return fmt.Errorf("missing WAV data: %w", err)
		}
		size := int64(binary.LittleEndian.Uint32(chunk[4:]))
		switch string(chunk[:4]) {
		case "fmt ":
			if size < 16 {
				return fmt.Errorf("short WAV format")
			}
			var format [16]byte
			if _, err := io.ReadFull(r, format[:]); err != nil {
				return err
			}
			if binary.LittleEndian.Uint16(format[:2]) != 1 ||
				binary.LittleEndian.Uint16(format[2:4]) != 1 ||
				binary.LittleEndian.Uint32(format[4:8]) != sampleRate ||
				binary.LittleEndian.Uint16(format[12:14]) != 2 ||
				binary.LittleEndian.Uint16(format[14:16]) != 16 {
				return fmt.Errorf("WAV must be 22050 Hz mono PCM16")
			}
			validFormat = true
			size -= 16
		case "data":
			if !validFormat || size == 0 || size%2 != 0 {
				return fmt.Errorf("invalid WAV PCM data")
			}
			var pcm [sampleRate / 10 * 2]byte // 100 ms PCM16 per write.
			for size > 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
				n := int64(len(pcm))
				if size < n {
					n = size
				}
				if _, err := io.ReadFull(r, pcm[:n]); err != nil {
					return fmt.Errorf("truncated PCM: %w", err)
				}
				if err := sink.WritePCM(pcm[:n]); err != nil {
					return err
				}
				size -= n
			}
			return nil
		}
		if _, err := io.CopyN(io.Discard, r, size+size%2); err != nil {
			return err
		}
	}
}
