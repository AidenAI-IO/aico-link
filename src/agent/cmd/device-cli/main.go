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
	if err := run(strings.TrimRight(base, "/"), flag.Args(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(base string, args []string, stdout io.Writer) error {
	client := &http.Client{Timeout: 5 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	scoped := base == strings.TrimRight(os.Getenv("AICO_DEVICE_CLI_URL"), "/") && os.Getenv("AICO_DEVICE_CLI_TOKEN") != ""
	var look *devicecli.LookOptions
	if len(args) > 0 && args[0] == "look" {
		options, err := devicecli.ParseLookOptions(args[1:])
		if err != nil {
			return err
		}
		look = &options
	}
	if !scoped && len(args) > 0 && args[0] == "wait" {
		delay, err := devicecli.ParseWaitDuration(args[1:])
		if err != nil {
			return err
		}
		time.Sleep(delay)
		return nil
	}
	if len(args) == 3 && args[0] == "app" && args[1] == "clipboard" && args[2] == "read" {
		path, token := "/api/device-cli/app", ""
		if scoped {
			path, token = "/command", os.Getenv("AICO_DEVICE_CLI_TOKEN")
		}
		return readClipboard(client, base+path, args, token, stdout)
	}
	if scoped {
		return runCall(client, base+"/command", args, os.Getenv("AICO_DEVICE_CLI_TOKEN"), look, stdout)
	}
	calls, err := devicecli.Calls(args)
	if err != nil {
		return err
	}
	if calls[0].AppData {
		return runCall(client, base+"/api/device-cli/app", args, "", nil, stdout)
	}
	if len(calls) > 1 || args[0] == "drag-start" {
		return runCall(client, base+"/api/device-cli/drag", args, "", nil, stdout)
	}
	for _, call := range calls {
		if err := runCall(client, base+"/api/tools/"+call.Tool, map[string]any{"input": call.Input}, "", look, stdout); err != nil {
			return err
		}
	}
	if args[0] == "tap" {
		time.Sleep(devicecli.TapSettleDelay)
	}
	return nil
}

// readClipboard writes the text verbatim, including empty values and trailing
// newlines, without exposing the Bridge's JSON envelope on stdout.
func readClipboard(client *http.Client, url string, args []string, token string, stdout io.Writer) error {
	output, err := invoke(client, url, args, token)
	if err != nil {
		return err
	}
	var result struct {
		OK   bool    `json:"ok"`
		Text *string `json:"text"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil || !result.OK || result.Text == nil {
		return fmt.Errorf("invalid clipboard read response")
	}
	_, err = io.WriteString(stdout, *result.Text)
	return err
}

func runCall(client *http.Client, url string, input any, token string, look *devicecli.LookOptions, stdout io.Writer) error {
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
		_, err = fmt.Fprintln(stdout, path)
		return err
	}
	if visualErr == nil && visual.Data != "" {
		output = visual.ActionOutput
		if visual.ScreenChanged != nil && !*visual.ScreenChanged {
			fmt.Fprintln(os.Stderr, "device-cli: no meaningful screen change")
		}
	}
	return writeResult(stdout, output)
}

// writeResult translates the Tools HTTP result to the CLI stdout contract.
// Use RawMessage so arrays, objects and numbers retain their JSON types.
func writeResult(stdout io.Writer, output string) error {
	switch strings.TrimSpace(output) {
	case "", "ok":
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(output), &fields); err != nil || fields == nil {
		_, err := fmt.Fprintln(stdout, output)
		return err
	}
	if status, exists := fields["ok"]; exists {
		switch string(status) {
		case "false":
			var message string
			if json.Unmarshal(fields["error"], &message) == nil && strings.TrimSpace(message) != "" {
				return fmt.Errorf("%s", message)
			}
			return fmt.Errorf("device command failed: %s", output)
		case "true":
			delete(fields, "ok")
		}
	}
	delete(fields, "restored_from_return_entry")
	switch len(fields) {
	case 0:
		return nil
	case 1:
		for _, value := range fields {
			if len(value) > 0 && value[0] == '"' {
				var text string
				if err := json.Unmarshal(value, &text); err != nil {
					return err
				}
				_, err := io.WriteString(stdout, text)
				return err
			}
			return json.NewEncoder(stdout).Encode(value)
		}
	}
	return json.NewEncoder(stdout).Encode(fields)
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
