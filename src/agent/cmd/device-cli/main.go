package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"aico-agent/internal/devicecli"
)

func main() {
	base := os.Getenv("AICO_DEVICE_CLI_URL")
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	flag.StringVar(&base, "url", base, "Agent HTTP base URL")
	flag.Usage = func() { fmt.Fprintln(os.Stderr, devicecli.Usage) }
	flag.Parse()
	if err := run(strings.TrimRight(base, "/"), flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(base string, args []string) error {
	client := &http.Client{Timeout: 5 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	scoped := base == os.Getenv("AICO_DEVICE_CLI_URL") && os.Getenv("AICO_DEVICE_CLI_TOKEN") != ""
	var look *devicecli.LookOptions
	if len(args) > 0 && args[0] == "look" {
		options, err := devicecli.ParseLookOptions(args[1:])
		if err != nil {
			return err
		}
		look = &options
	}
	if scoped {
		return runCall(client, base+"/command", args, os.Getenv("AICO_DEVICE_CLI_TOKEN"), look)
	}
	calls, err := devicecli.Calls(args)
	if err != nil {
		return err
	}
	if len(calls) > 1 || args[0] == "drag-start" {
		return runCall(client, base+"/api/device-cli/drag", args, "", nil)
	}
	for _, call := range calls {
		if err := runCall(client, base+"/api/tools/"+call.Tool, map[string]any{"input": call.Input}, "", look); err != nil {
			return err
		}
	}
	return nil
}

func runCall(client *http.Client, url string, input any, token string, look *devicecli.LookOptions) error {
	output, err := invoke(client, url, input, token)
	if err != nil {
		return err
	}
	var visual struct {
		Data          string `json:"data"`
		ActionOutput  string `json:"action_output"`
		ScreenChanged *bool  `json:"screen_changed"`
	}
	visualErr := json.Unmarshal([]byte(output), &visual)
	if look != nil {
		if visualErr != nil || visual.Data == "" {
			return fmt.Errorf("screenshot response has no image")
		}
		path, err := saveScreenshot(visual.Data, *look)
		if err != nil {
			return err
		}
		fmt.Println(path)
		return nil
	}
	if visualErr == nil && visual.Data != "" {
		output = visual.ActionOutput
		if visual.ScreenChanged != nil && !*visual.ScreenChanged {
			fmt.Fprintln(os.Stderr, "device-cli: no meaningful screen change")
		}
	}
	if output != "" {
		fmt.Println(output)
	}
	return nil
}

func invoke(client *http.Client, url string, input any, token string) (string, error) {
	body, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return "", fmt.Errorf("device CLI HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(raw)))
	}
	var result struct {
		Output  string `json:"output"`
		IsError bool   `json:"is_error"`
		Error   string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 24<<20)).Decode(&result); err != nil {
		return "", err
	}
	if result.IsError {
		if result.Error == "" {
			result.Error = result.Output
		}
		return "", fmt.Errorf("%s", result.Error)
	}
	return result.Output, nil
}
