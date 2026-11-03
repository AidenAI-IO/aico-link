package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"aico-agent/internal/wifiregion"
)

const defaultWiFiRegionConfigPath = "/userdata/debian/wifi/wpa_supplicant-wlan0.conf"

func runWiFiRegionApply(args []string) int {
	flags := flag.NewFlagSet("wifi-region-apply", flag.ContinueOnError)
	configPath := flags.String("config", defaultWiFiRegionConfigPath, "wpa_supplicant configuration file")
	interfaceName := flags.String("interface", "wlan0", "Wi-Fi network interface")
	requestedCountry := flags.String("country", "", "country code override for device diagnostics")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "wifi-region-apply: unexpected arguments")
		return 2
	}
	country := *requestedCountry
	if country == "" {
		var err error
		country, err = readWiFiConfigCountry(*configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "wifi-region-apply: %v\n", err)
			return 1
		}
	}
	code, ok := wifiregion.Normalize(country)
	if !ok || (*requestedCountry != "" && code == wifiregion.WorldCountry) {
		fmt.Fprintf(os.Stderr, "wifi-region-apply: unsupported country %q\n", country)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := wifiregion.ApplyDriverCountry(ctx, *interfaceName, code); err != nil {
		fmt.Fprintf(os.Stderr, "wifi-region-apply: %v\n", err)
		return 1
	}
	verifyCtx, cancelVerify := context.WithTimeout(ctx, 3*time.Second)
	defer cancelVerify()
	for {
		actual, err := wifiregion.ReadDriverCountry(verifyCtx, *interfaceName)
		if err == nil && actual == code {
			fmt.Printf("Wi-Fi phy country: %s\n", actual)
			return 0
		}
		select {
		case <-verifyCtx.Done():
			fmt.Fprintf(os.Stderr, "wifi-region-apply: phy country readback %q, expected %q: %v\n", actual, code, err)
			return 1
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func readWiFiConfigCountry(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, 1024*1024))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "network={" {
			break
		}
		if value, ok := strings.CutPrefix(line, "country="); ok {
			return strings.TrimSpace(value), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", errors.New("Wi-Fi configuration has no country")
}
