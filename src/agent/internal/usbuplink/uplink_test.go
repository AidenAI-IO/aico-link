package usbuplink

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	m, err := New(filepath.Join(t.TempDir(), "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

func macRequest() Request {
	return Request{ID: strings.Repeat("a", 32), Host: "mac", Peer: "127.0.0.1"}
}

func TestSwitchDefaultsOffAndOnlyPolicySurvivesRestart(t *testing.T) {
	m := testManager(t)
	if m.Status().Enabled {
		t.Fatal("enabled without opt-in")
	}
	if code, _ := m.Register(macRequest()); code != http.StatusForbidden {
		t.Fatal(code)
	}
	if err := m.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if code, err := m.Register(macRequest()); code != http.StatusOK {
		t.Fatal(code, err)
	}
	lease := m.Current()
	if _, err := m.Register(macRequest()); err != nil || lease != m.Current() {
		t.Fatal("renewal replaced active streams", err)
	}
	restarted, err := New(m.path)
	if err != nil {
		t.Fatal(err)
	}
	if !restarted.Status().Enabled || restarted.Current() != nil {
		t.Fatal("lease persisted across daemon restart")
	}
	if err := m.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	restarted, err = New(m.path)
	if err != nil || restarted.Status().Enabled || m.Current() != nil {
		t.Fatal("disable not persisted", err)
	}
}

func TestSessionValidationOwnershipAndCredentialRedaction(t *testing.T) {
	m := testManager(t)
	if err := m.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	for _, request := range []Request{
		{ID: "short", Host: "mac", Peer: "127.0.0.1"},
		{ID: strings.Repeat("a", 32), Host: "mac", Peer: "192.168.42.100"},
		{ID: strings.Repeat("a", 32), Host: "android", Peer: "127.0.0.1", Password: strings.Repeat("b", 32)},
		{ID: strings.Repeat("a", 32), Host: "android", Peer: "192.168.42.100"},
	} {
		if _, err := m.Register(request); err == nil {
			t.Fatal("accepted invalid request")
		}
	}
	r := Request{ID: strings.Repeat("a", 32), Host: "android", Peer: "192.168.42.100", Password: strings.Repeat("b", 32)}
	if _, err := m.Register(r); err != nil {
		t.Fatal(err)
	}
	status, _ := json.Marshal(m.Status())
	settings, _ := os.ReadFile(m.path)
	for _, value := range []string{r.ID, r.Password, r.Peer} {
		if strings.Contains(string(status), value) || strings.Contains(string(settings), value) {
			t.Fatal("session secret persisted or exposed")
		}
	}
	other := r
	other.ID = strings.Repeat("c", 32)
	if code, _ := m.Register(other); code != http.StatusConflict {
		t.Fatal(code)
	}
	if m.Release(r.ID, "192.168.42.101") || m.Release(other.ID, r.Peer) {
		t.Fatal("another owner released session")
	}
	if !m.Release(r.ID, r.Peer) || m.Current() != nil {
		t.Fatal("owner cannot release")
	}
}

func TestDisableAndExpiryCloseEstablishedConnections(t *testing.T) {
	for _, reason := range []string{"disable", "expiry", "release"} {
		t.Run(reason, func(t *testing.T) {
			m := testManager(t)
			if err := m.SetEnabled(true); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Register(macRequest()); err != nil {
				t.Fatal(err)
			}
			lease := m.Current()
			left, right := net.Pipe()
			defer right.Close()
			tracked, err := lease.Dial(context.Background(), func(context.Context) (net.Conn, error) { return left, nil })
			if err != nil {
				t.Fatal(err)
			}
			defer tracked.Close()
			switch reason {
			case "disable":
				if err := m.SetEnabled(false); err != nil {
					t.Fatal(err)
				}
			case "release":
				m.Release(macRequest().ID, macRequest().Peer)
			case "expiry":
				m.now = func() time.Time { return time.Now().Add(LeaseDuration + time.Second) }
				m.Current()
			}
			right.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := right.Read(make([]byte, 1)); err != io.EOF {
				t.Fatalf("stream still open: %v", err)
			}
			late, peer := net.Pipe()
			defer peer.Close()
			if conn, err := lease.Dial(context.Background(), func(context.Context) (net.Conn, error) { return late, nil }); err == nil {
				conn.Close()
				t.Fatal("late connect attached to revoked lease")
			}
		})
	}
}

func TestDisableCancelsPendingConnect(t *testing.T) {
	m := testManager(t)
	if err := m.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Register(macRequest()); err != nil {
		t.Fatal(err)
	}
	lease := m.Current()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := lease.Dial(context.Background(), func(ctx context.Context) (net.Conn, error) { close(started); <-ctx.Done(); return nil, ctx.Err() })
		done <- err
	}()
	<-started
	if err := m.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("connect succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("pending connect not canceled")
	}
}
