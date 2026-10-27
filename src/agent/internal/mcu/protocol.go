// Package mcu implements the MCU/RV1106 V1 link and application API.
package mcu

import (
	"encoding/binary"
	"errors"
	"time"
)

const (
	REQ             byte = 1
	RESP            byte = 2
	EVENT           byte = 3
	GetState        byte = 0x01
	GetBattery      byte = 0x02
	SetTimer        byte = 0x03
	GetTimer        byte = 0x04
	CancelTimer     byte = 0x05
	RVReady         byte = 0x06
	Wakeup          byte = 0x07
	PluginChanged   byte = 0x08
	ShutdownRequest byte = 0x09
	ShutdownReady   byte = 0x0a
	SetLED          byte = 0x0b
	GetLED          byte = 0x0c
	Success         byte = 0
	Busy            byte = 1
	Invalid         byte = 2
	NotReady        byte = 3
	Denied          byte = 4
	InternalError   byte = 5
	Timeout         byte = 6
	Booting         byte = 1
	Ready           byte = 2
	ShuttingDown    byte = 3
	MaxPayload           = 64
)

type Frame struct {
	Type, Seq, Command byte
	Payload            []byte
}

func CRC16(data []byte) uint16 {
	crc := uint16(0xffff)
	for _, b := range data {
		crc ^= uint16(b) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func Encode(f Frame) ([]byte, error) {
	if f.Type < REQ || f.Type > EVENT || len(f.Payload) > MaxPayload || (f.Type == RESP && (len(f.Payload) == 0 || f.Payload[0] > Timeout)) {
		return nil, errors.New("invalid MCU frame")
	}
	wire := make([]byte, 10+len(f.Payload))
	copy(wire, []byte{0xaa, 0x55, 1, f.Type, f.Seq, f.Command})
	binary.LittleEndian.PutUint16(wire[6:8], uint16(len(f.Payload)))
	copy(wire[8:], f.Payload)
	binary.LittleEndian.PutUint16(wire[len(wire)-2:], CRC16(wire[2:len(wire)-2]))
	return wire, nil
}

type Parser struct {
	buffer  []byte
	last    time.Time
	Invalid uint64
}

func (p *Parser) Feed(data []byte, now time.Time) []Frame {
	if len(p.buffer) > 0 && now.Sub(p.last) >= 100*time.Millisecond {
		p.buffer = nil
		p.Invalid++
	}
	var frames []Frame
	for _, b := range data {
		p.buffer = append(p.buffer, b)
		p.last = now
		for len(p.buffer) > 0 {
			v := p.buffer
			if v[0] != 0xaa || (len(v) > 1 && v[1] != 0x55) || (len(v) > 2 && v[2] != 1) || (len(v) > 3 && (v[3] < REQ || v[3] > EVENT)) {
				p.buffer = v[1:]
				p.Invalid++
				continue
			}
			if len(v) < 8 {
				break
			}
			n := int(binary.LittleEndian.Uint16(v[6:8]))
			if n > MaxPayload || (v[3] == RESP && n == 0) {
				p.buffer = v[1:]
				p.Invalid++
				continue
			}
			if len(v) < n+10 {
				break
			}
			if CRC16(v[2:8+n]) != binary.LittleEndian.Uint16(v[8+n:10+n]) || (v[3] == RESP && v[8] > Timeout) {
				p.buffer = v[1:]
				p.Invalid++
				continue
			}
			frames = append(frames, Frame{v[3], v[4], v[5], append([]byte(nil), v[8:8+n]...)})
			p.buffer = v[n+10:]
		}
	}
	return frames
}
