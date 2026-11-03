package ble

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRequestDeadlineUsesCallerOrFallback(t *testing.T) {
	now := time.Unix(100, 0)
	if got := requestDeadline(context.Background(), now); !got.Equal(now.Add(5 * time.Second)) {
		t.Fatalf("fallback deadline = %s", got)
	}

	want := now.Add(time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), want)
	defer cancel()
	if got := requestDeadline(ctx, now); !got.Equal(want) {
		t.Fatalf("caller deadline = %s, want %s", got, want)
	}
}

func TestRequestEventsPreservesCursorAndMetadata(t *testing.T) {
	service := NewService(4)
	service.store.Append(NotificationEvent{
		NotificationUID:  42,
		Event:            "added",
		AppIdentifier:    "com.example.mail",
		Title:            "Hello",
		Message:          "World",
		MetadataComplete: true,
	})
	dir, err := os.MkdirTemp("/tmp", "aico-ble-client-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	server := NewUDSServer(filepath.Join(dir, "ble.sock"), service)
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	page, err := RequestEvents(context.Background(), server.path, "0", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].Title != "Hello" || page.Events[0].Message != "World" {
		t.Fatalf("unexpected page: %#v", page)
	}
	if page.LastID != "1" || page.Generation == "" || page.ResetRequired {
		t.Fatalf("cursor metadata lost: %#v", page)
	}
}
