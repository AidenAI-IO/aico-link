package configweb

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// wifiAuthMonitor listens only during a candidate connection attempt. The
// supplicant's status command does not expose why authentication failed.
type wifiAuthMonitor struct {
	ssid          string
	wrongPassword atomic.Bool
	conn          *net.UnixConn
	directory     string
	done          chan struct{}
}

func newWiFiAuthMonitor(ssid string) *wifiAuthMonitor {
	return &wifiAuthMonitor{ssid: ssid}
}

func (m *wifiAuthMonitor) start(ctx context.Context, interfaceName string) {
	m.startAt(ctx, filepath.Join("/var/run/wpa_supplicant", interfaceName))
}

func (m *wifiAuthMonitor) startAt(ctx context.Context, socketPath string) {
	if m.conn != nil {
		return
	}
	directory, err := os.MkdirTemp("", "aico-wpa-")
	if err != nil {
		return
	}
	local := &net.UnixAddr{Name: filepath.Join(directory, "events"), Net: "unixgram"}
	conn, err := net.ListenUnixgram("unixgram", local)
	if err != nil {
		_ = os.RemoveAll(directory)
		return
	}
	remote := &net.UnixAddr{Name: socketPath, Net: "unixgram"}
	if _, err = conn.WriteToUnix([]byte("ATTACH"), remote); err == nil {
		var response [64]byte
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		var count int
		count, _, err = conn.ReadFromUnix(response[:])
		if err == nil && strings.TrimSpace(string(response[:count])) != "OK" {
			err = net.ErrClosed
		}
	}
	if err != nil || ctx.Err() != nil {
		_ = conn.Close()
		_ = os.RemoveAll(directory)
		return
	}
	m.conn, m.directory, m.done = conn, directory, make(chan struct{})
	go func() {
		defer close(m.done)
		var event [4096]byte
		for ctx.Err() == nil {
			_ = conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
			count, _, err := conn.ReadFromUnix(event[:])
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				continue
			}
			if err != nil {
				return
			}
			if wifiWrongKeyEvent(string(event[:count]), m.ssid) {
				m.wrongPassword.Store(true)
			}
		}
	}()
}

func (m *wifiAuthMonitor) stop() bool {
	if m.conn != nil {
		_ = m.conn.Close()
		<-m.done
		_ = os.RemoveAll(m.directory)
	}
	return m.wrongPassword.Load()
}

func wifiWrongKeyEvent(event, ssid string) bool {
	if !strings.Contains(event, "CTRL-EVENT-SSID-TEMP-DISABLED ") ||
		!strings.Contains(event, "reason=WRONG_KEY") {
		return false
	}
	start := strings.Index(event, `ssid="`)
	if start < 0 {
		return false
	}
	quoted := event[start+len("ssid="):]
	var value strings.Builder
	escaped := false
	for index := 1; index < len(quoted); index++ {
		character := quoted[index]
		if escaped {
			value.WriteByte(character)
			escaped = false
			continue
		}
		if character == '\\' {
			escaped = true
			continue
		}
		if character == '"' {
			return value.String() == ssid
		}
		value.WriteByte(character)
	}
	return false
}
