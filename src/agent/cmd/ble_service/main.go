package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"aico-agent/internal/ble"
	"aico-agent/internal/logging"
)

func main() {
	socketPath := flag.String("socket", "/run/ble_service/ble_service.sock", "Unix domain socket path")
	deviceName := flag.String("device-name", "Aico Link", "BLE advertised device name")
	eventCapacity := flag.Int("event-capacity", 512, "maximum in-memory ANCS events")
	pairingWindow := flag.Duration("pairing-window", 5*time.Minute, "first-device BLE pairing window")
	clearAdvHelper := flag.String(
		"clear-adv-helper", "/usr/lib/aico/aico-bluetooth-clear-adv",
		"helper that clears stale controller advertising sets; empty disables it",
	)
	flag.Parse()
	if *eventCapacity <= 0 {
		logging.Fatalf("ble_service", "startup", "event-capacity must be positive")
	}
	if *pairingWindow <= 0 {
		logging.Fatalf("ble_service", "startup", "pairing-window must be positive")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	service := ble.NewService(*eventCapacity)
	uds := ble.NewUDSServer(*socketPath, service)
	if err := uds.Start(); err != nil {
		logging.Fatalf("ble_service", "startup", "start BLE UDS service: %v", err)
	}
	defer uds.Close()
	logging.Infof("ble_service", "startup", "ble_service listening on %s", *socketPath)

	for ctx.Err() == nil {
		err := service.RunBlueZ(ctx, *deviceName, *pairingWindow)
		if ctx.Err() != nil {
			break
		}
		logging.Warnf("ble_service", "main", "BlueZ backend stopped: %v", err)
		// The unit's ExecStartPre clears stale sets once, but this loop never
		// exits, so a cleanup that was skipped or came too early would leave
		// the board invisible. Clear again before every retry that needs it.
		if errors.Is(err, ble.ErrAdvertisementRegistration) {
			clearAdvertisingSets(ctx, *clearAdvHelper)
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}
	}
	logging.Infof("ble_service", "main", "ble_service stopped")
}

func clearAdvertisingSets(ctx context.Context, helper string) {
	if helper == "" {
		return
	}
	if _, err := os.Stat(helper); err != nil {
		return
	}
	runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	output, err := exec.CommandContext(runCtx, helper).CombinedOutput()
	if err != nil || len(output) > 0 {
		logging.Warnf("ble_service", "main", "clear advertising sets: err=%v output=%q", err, output)
	}
}
