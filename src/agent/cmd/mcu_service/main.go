package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"aico-agent/internal/mcu"
)

func main() {
	if len(os.Args) == 3 && os.Args[1] == "shutdown-hook" {
		if err := mcu.ShutdownHook(os.Args[2], "/dev/ttyS5", mcu.IntentDirectory); err != nil {
			log.Fatal(err)
		}
		return
	}
	port := flag.String("port", "/dev/ttyS5", "MCU UART (115200 8N1)")
	socket := flag.String("socket", mcu.DefaultSocket, "application UDS socket")
	flag.Parse()
	if flag.NArg() == 1 && flag.Arg(0) == "poweroff" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		client := mcu.Client{Socket: *socket}
		state, err := client.GetState(ctx)
		if err != nil {
			log.Fatal(err)
		}
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			log.Fatal(err)
		}
		state, err = client.Poweroff(ctx, state.Generation, hex.EncodeToString(id[:]))
		if err != nil {
			log.Fatal(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(state); err != nil {
			log.Fatal(err)
		}
		return
	}
	if flag.NArg() != 0 {
		log.Fatal("unexpected arguments")
	}
	if os.Geteuid() != 0 {
		log.Fatal("mcu_service must run as root to coordinate shutdown")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdown, err := mcu.NewPoweroff(ctx, mcu.IntentDirectory, mcu.SystemPoweroff)
	if err != nil {
		log.Fatal(err)
	}
	service := mcu.NewService(shutdown)
	defer service.Close()
	server, err := mcu.Listen(*socket, service)
	if err != nil {
		log.Fatal(err)
	}
	defer server.Close()
	if shutdown.Pending() {
		shutdown.Commit()
	}
	for ctx.Err() == nil {
		uart, err := mcu.OpenSerial(*port)
		if err == nil {
			link := mcu.NewLink(uart)
			service.Attach(link)
			log.Printf("MCU UART connected: %s", *port)
			err = link.Run(ctx)
			service.Detach()
		}
		if ctx.Err() != nil {
			return
		}
		log.Printf("MCU UART unavailable: %v", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}
