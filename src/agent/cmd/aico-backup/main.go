// aico-backup is the small host-side client for the Config Web backup API.
// It deliberately keeps archive bytes on the host and streams restore chunks
// directly from the input file; no complete archive is buffered in memory.
//
// Exit codes: 1 connection or usage error, 2 invalid or unsupported archive,
// 3 insufficient space, 4 device mismatch or blocked identity restore,
// 5 restore failed on the device.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"aico-agent/internal/backup"
	"github.com/google/uuid"
)

const defaultBaseURL = "http://192.168.42.1/api"

type client struct {
	baseURL string
	http    *http.Client
	token   string
	csrf    string
	jsonOut bool
	verbose io.Writer
}

type apiError struct {
	Status  int
	Code    string `json:"error"`
	Message string `json:"message"`
}

func (e *apiError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("HTTP %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Code)
}

type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitCode(err))
	}
}

func run(args []string, out, errOut io.Writer) error {
	if len(args) == 0 {
		return usage()
	}
	baseURL := strings.TrimRight(os.Getenv("AICO_BACKUP_URL"), "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	jsonFlag := false
	if index := indexOf(args, "--json"); index >= 0 {
		jsonFlag = true
		args = append(append([]string{}, args[:index]...), args[index+1:]...)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	c := &client{baseURL: baseURL, http: &http.Client{Transport: transport}, jsonOut: jsonFlag, verbose: errOut}
	switch args[0] {
	case "inspect":
		if len(args) != 2 {
			return usage()
		}
		return c.inspect(args[1], out)
	case "verify":
		if len(args) != 2 {
			return usage()
		}
		return c.verify(args[1], "", out)
	case "create":
		return c.create(args[1:], out)
	case "restore":
		return c.restore(args[1:], out, errOut)
	case "status":
		if len(args) != 2 {
			return usage()
		}
		return c.status(args[1], out)
	case "cancel":
		if len(args) != 2 {
			return usage()
		}
		return c.cancel(args[1], out)
	default:
		return usage()
	}
}

func usage() error {
	return &exitError{code: 1, err: errors.New(`usage: aico-backup [--json] <command>

  inspect <archive>                         print the public header
  verify <archive>                          decrypt and verify an archive locally
  create --output <path>
  restore <archive> [--components a,b,c] [--sd-strategy require_match|allow_different|emmc_fallback|skip]
                    [--confirm-identity] [--yes]
  status <job-id>
  cancel <job-id>

Environment: AICO_BACKUP_URL (default http://192.168.42.1/api)`)}
}

func indexOf(values []string, want string) int {
	for index, value := range values {
		if value == want {
			return index
		}
	}
	return -1
}

func (c *client) inspect(name string, out io.Writer) error {
	file, err := os.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	header, _, err := backup.ReadPublicHeader(file)
	if err != nil {
		return err
	}
	return c.print(out, map[string]any{"format": backup.FormatName, "header": header, "path": name, "size": info.Size()})
}

func (c *client) verify(name string, passphrase string, out io.Writer) error {
	result, err := verifyFile(name, passphrase)
	if err != nil {
		return err
	}
	return c.print(out, map[string]any{"ok": true, "archive": name, "bytes": result.Bytes, "manifest": result.Manifest})
}

func (c *client) create(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("create", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	output := flags.String("output", "", "destination archive path")
	if err := flags.Parse(args); err != nil || strings.TrimSpace(*output) == "" {
		return usage()
	}
	if _, err := os.Stat(*output); err == nil {
		return &exitError{code: 1, err: fmt.Errorf("refusing to overwrite existing output %s", *output)}
	}
	if err := c.openSession(); err != nil {
		return err
	}
	created, err := c.request(http.MethodPost, "/backup/jobs", map[string]any{
		"format_version": 1, "mode": backup.ModeSameDevice,
		"protection": map[string]any{"mode": "none"},
	}, true)
	if err != nil {
		return err
	}
	jobID, _ := created["job_id"].(string)
	archiveURL, _ := created["archive_url"].(string)
	transfer, _ := created["transfer_token"].(string)
	if jobID == "" || archiveURL == "" || transfer == "" {
		return errors.New("device returned an incomplete backup job")
	}
	fmt.Fprintf(c.verbose, "backup job %s created; the device stops the Agent while streaming\n", jobID)
	partial := *output + ".partial"
	file, err := os.OpenFile(partial, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	cleanup := func() { _ = os.Remove(partial) }
	response, err := c.do(http.MethodGet, c.apiPath(archiveURL), nil, transfer, false)
	if err != nil {
		_ = file.Close()
		cleanup()
		return err
	}
	written, copyErr := io.Copy(file, response.Body)
	closeErr := response.Body.Close()
	syncErr := file.Sync()
	fileCloseErr := file.Close()
	for _, err := range []error{copyErr, closeErr, syncErr, fileCloseErr} {
		if err != nil {
			cleanup()
			return err
		}
	}
	// The device reports the job outcome separately from the HTTP stream;
	// an interrupted stream can still look like a clean EOF to the client.
	// The transfer token is revoked once the stream ends, so poll with the
	// maintenance session.
	final, err := c.waitForTerminal("/backup/jobs/"+jobID, "")
	if err != nil {
		cleanup()
		return err
	}
	if final["state"] != "completed" {
		cleanup()
		return &exitError{code: 5, err: fmt.Errorf("backup job ended in state %v: %v", final["state"], final["error"])}
	}
	fmt.Fprintf(c.verbose, "received %d bytes; verifying\n", written)
	result, err := verifyFile(partial, "")
	if err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(partial, *output); err != nil {
		cleanup()
		return err
	}
	return c.print(out, map[string]any{"ok": true, "job_id": jobID, "path": *output, "bytes": written, "backup_id": result.Manifest.BackupID, "components": result.Manifest.Components})
}

// splitPositional extracts the single positional argument so flags may be
// written before or after it (Go's flag package stops at the first non-flag).
func splitPositional(args []string) (string, []string) {
	positional := ""
	var rest []string
	for index := 0; index < len(args); index++ {
		value := args[index]
		if strings.HasPrefix(value, "-") {
			rest = append(rest, value)
			if !strings.Contains(value, "=") && index+1 < len(args) && !strings.HasPrefix(args[index+1], "-") && !isBoolFlag(value) {
				rest = append(rest, args[index+1])
				index++
			}
			continue
		}
		if positional == "" {
			positional = value
			continue
		}
		rest = append(rest, value)
	}
	return positional, rest
}

func isBoolFlag(value string) bool {
	switch strings.TrimLeft(value, "-") {
	case "yes", "confirm-identity":
		return true
	}
	return false
}

func splitList(value string) []string {
	var result []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func (c *client) restore(args []string, out, errOut io.Writer) error {
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	componentList := flags.String("components", "", "comma-separated component ids (default: everything in the archive)")
	sdStrategy := flags.String("sd-strategy", "require_match", "SD handling: require_match, allow_different, emmc_fallback or skip")
	confirmIdentity := flags.Bool("confirm-identity", false, "restore device identity even when the device cannot verify the backup belongs to it")
	yes := flags.Bool("yes", false, "apply without the interactive RESTORE confirmation")
	name, flagArgs := splitPositional(args)
	if err := flags.Parse(flagArgs); err != nil || name == "" || flags.NArg() != 0 {
		return usage()
	}
	file, err := os.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	header, _, err := backup.ReadPublicHeader(file)
	if err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if header.Protection.Algorithm != "sha256-chunked" {
		return &exitError{code: 2, err: errors.New("encrypted legacy backups require the original password-capable client")}
	}
	if err := c.openSession(); err != nil {
		return err
	}
	created, err := c.request(http.MethodPost, "/restore/jobs", map[string]any{
		"format_version": 1, "archive_size": info.Size(), "public_header": header,
		"protection": map[string]any{"mode": "none"},
	}, true)
	if err != nil {
		return err
	}
	jobID, _ := created["job_id"].(string)
	transfer, _ := created["transfer_token"].(string)
	chunkSize := int64(numberValue(created["chunk_size"]))
	if jobID == "" || transfer == "" || chunkSize < 1 {
		return errors.New("device returned an incomplete restore job")
	}
	fmt.Fprintf(c.verbose, "restore job %s created; uploading in %d byte blocks\n", jobID, chunkSize)
	planSubmitted := false
	submitPlan := func(payload map[string]any) error {
		manifest, _ := payload["manifest"].(map[string]any)
		components := splitList(*componentList)
		if len(components) == 0 {
			components = manifestComponents(manifest)
		}
		if manifest != nil {
			fmt.Fprintf(errOut, "archive %v created %v mode %v components %v\n", manifest["backup_id"], manifest["created_at"], manifest["mode"], components)
		}
		plan, err := c.request(http.MethodPost, "/restore/jobs/"+jobID+"/plan", map[string]any{
			"components": components, "sd_strategy": *sdStrategy, "confirm_conflicts": true, "confirm_identity": *confirmIdentity,
		}, true)
		if err != nil {
			_ = c.deleteRestoreJob(jobID)
			return err
		}
		if warnings, ok := plan["warnings"].([]any); ok {
			for _, warning := range warnings {
				fmt.Fprintf(errOut, "warning: %v\n", warning)
			}
		}
		planSubmitted = true
		return nil
	}
	last, err := c.uploadRestoreChunks(file, info.Size(), jobID, transfer, chunkSize, submitPlan)
	if err != nil {
		_ = c.deleteRestoreJob(jobID)
		return err
	}
	if !planSubmitted {
		_ = c.deleteRestoreJob(jobID)
		return &exitError{code: 5, err: fmt.Errorf("device never reported the archive manifest (last state %v)", last["state"])}
	}
	validated, err := c.request(http.MethodPost, "/restore/jobs/"+jobID+"/validate", map[string]any{}, true)
	if err != nil {
		_ = c.deleteRestoreJob(jobID)
		return err
	}
	digest, _ := validated["plan_digest"].(string)
	if digest == "" {
		_ = c.deleteRestoreJob(jobID)
		return errors.New("device did not return a plan digest")
	}
	if !*yes {
		fmt.Fprintln(errOut, "Restore validated. The device stops its services and replaces the selected data. Type RESTORE to apply:")
		confirmation, err := readLine()
		if err != nil || strings.TrimSpace(confirmation) != "RESTORE" {
			_ = c.deleteRestoreJob(jobID)
			return &exitError{code: 1, err: errors.New("restore not applied")}
		}
	}
	applied, err := c.request(http.MethodPost, "/restore/jobs/"+jobID+"/apply", map[string]any{"plan_digest": digest, "confirm": "RESTORE"}, true)
	if err != nil {
		return err
	}
	final := applied
	if !isTerminalState(stringValue(applied["state"])) {
		final, err = c.waitForTerminal("/restore/jobs/"+jobID, "")
		if err != nil {
			return err
		}
	}
	state := stringValue(final["state"])
	if state != "completed" && state != "reboot_required" {
		return &exitError{code: 5, err: fmt.Errorf("restore ended in state %s: %v", state, final["error"])}
	}
	if state == "reboot_required" {
		fmt.Fprintln(errOut, "The device is rebooting to finish identity provisioning; reconnect once it is back on the USB link.")
	}
	return c.print(out, map[string]any{"ok": true, "job_id": jobID, "state": state, "warnings": final["warnings"]})
}

func manifestComponents(manifest map[string]any) []string {
	components := []string{}
	values, _ := manifest["components"].([]any)
	for _, value := range values {
		if item, ok := value.(map[string]any); ok {
			if id, ok := item["id"].(string); ok {
				components = append(components, id)
			}
		}
	}
	return components
}

// uploadRestoreChunks streams the archive block by block.  Every response is
// inspected: awaiting_plan means the device parsed manifest.json and the plan
// must be submitted before the next block is accepted.
func (c *client) uploadRestoreChunks(file *os.File, size int64, jobID, transfer string, chunkSize int64, onManifest func(map[string]any) error) (map[string]any, error) {
	buffer := make([]byte, chunkSize)
	var index, offset int64
	var last map[string]any
	for offset < size {
		count, err := io.ReadFull(file, buffer)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, err
		}
		if count == 0 {
			break
		}
		chunk := buffer[:count]
		digest := sha256.Sum256(chunk)
		// Chunks are authorized with the short-lived, job-scoped transfer
		// token so a raw archive upload can never reach another job.
		payload, err := c.requestWithBearer(http.MethodPut, "/restore/jobs/"+jobID+"/chunks/"+strconv.FormatInt(index, 10), nil, transfer, true, chunk, hex.EncodeToString(digest[:]))
		if err != nil {
			return nil, err
		}
		index++
		offset += int64(count)
		last = payload
		fmt.Fprintf(c.verbose, "uploaded %d/%d bytes (%s)\n", offset, size, stringValue(payload["state"]))
		if stringValue(payload["state"]) == "awaiting_plan" {
			if err := onManifest(payload); err != nil {
				return nil, err
			}
		}
		if errValue, ok := payload["error"].(map[string]any); ok && errValue != nil {
			return nil, &apiError{Status: http.StatusConflict, Code: stringValue(errValue["code"]), Message: stringValue(errValue["message"])}
		}
	}
	return last, nil
}

func (c *client) waitForTerminal(path, transfer string) (map[string]any, error) {
	for {
		var payload map[string]any
		var err error
		if transfer != "" {
			payload, err = c.requestWithBearer(http.MethodGet, path, nil, transfer, false)
		} else {
			payload, err = c.request(http.MethodGet, path, nil, false)
		}
		if err != nil {
			return nil, err
		}
		if isTerminalState(stringValue(payload["state"])) {
			return payload, nil
		}
		time.Sleep(time.Second)
	}
}

func isTerminalState(state string) bool {
	switch state {
	case "completed", "reboot_required", "cancelled", "failed", "rollback_failed":
		return true
	}
	return false
}

func (c *client) status(jobID string, out io.Writer) error {
	if err := c.openSession(); err != nil {
		return err
	}
	payload, err := c.request(http.MethodGet, "/backup/jobs/"+jobID, nil, false)
	if err != nil {
		payload, err = c.request(http.MethodGet, "/restore/jobs/"+jobID, nil, false)
	}
	if err != nil {
		return err
	}
	return c.print(out, payload)
}

func (c *client) cancel(jobID string, out io.Writer) error {
	if err := c.openSession(); err != nil {
		return err
	}
	if err := c.deleteBackupJob(jobID); err != nil {
		if err2 := c.deleteRestoreJob(jobID); err2 != nil {
			return err
		}
	}
	return c.print(out, map[string]any{"ok": true, "job_id": jobID, "cancelled": true})
}

func (c *client) deleteBackupJob(jobID string) error {
	_, err := c.request(http.MethodDelete, "/backup/jobs/"+jobID, nil, true)
	return err
}

func (c *client) deleteRestoreJob(jobID string) error {
	_, err := c.request(http.MethodDelete, "/restore/jobs/"+jobID, nil, true)
	return err
}

func (c *client) openSession() error {
	payload, err := c.requestWithClient(http.MethodPost, "/maintenance/sessions", map[string]any{}, "aico-backup-cli/1")
	if err != nil {
		return err
	}
	var ok bool
	c.token, ok = payload["token"].(string)
	if !ok || c.token == "" {
		return errors.New("maintenance session did not return a token")
	}
	c.csrf, _ = payload["csrf_token"].(string)
	return nil
}

func (c *client) request(method, path string, body any, mutate bool, extras ...any) (map[string]any, error) {
	return c.requestWithBearer(method, path, body, c.token, mutate, extras...)
}

// requestWithBearer is used for endpoints that accept a job-scoped transfer
// token as well as the normal maintenance-session bearer.  Keeping the token
// explicit at the call site prevents accidentally using the session token for
// a raw restore chunk upload.
func (c *client) requestWithBearer(method, path string, body any, bearer string, mutate bool, extras ...any) (map[string]any, error) {
	var data []byte
	var chunkHash string
	if len(extras) > 0 {
		if bytesValue, ok := extras[0].([]byte); ok {
			data = bytesValue
		}
		if len(extras) > 1 {
			chunkHash, _ = extras[1].(string)
		}
	} else if body != nil {
		data, _ = json.Marshal(body)
	}
	response, err := c.do(method, path, data, bearer, mutate, chunkHash)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var result map[string]any
	if response.ContentLength == 0 || response.StatusCode == http.StatusNoContent {
		return result, nil
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil && err != io.EOF {
		return nil, err
	}
	return result, nil
}

func (c *client) requestWithClient(method, path string, body any, clientName string) (map[string]any, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	response, err := c.doWithClient(method, path, data, "", false, clientName)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var result map[string]any
	if response.ContentLength == 0 {
		return result, nil
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil && err != io.EOF {
		return nil, err
	}
	return result, nil
}

func (c *client) do(method, path string, body []byte, bearer string, mutate bool, chunkHash ...string) (*http.Response, error) {
	return c.doWithClient(method, path, body, bearer, mutate, "", chunkHash...)
}

func (c *client) doWithClient(method, path string, body []byte, bearer string, mutate bool, clientName string, chunkHash ...string) (*http.Response, error) {
	url := path
	if strings.HasPrefix(path, "/") {
		url = c.baseURL + path
	}
	request, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	if clientName != "" {
		request.Header.Set("X-Aico-Client", clientName)
	}
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	if mutate {
		request.Header.Set("X-Aico-Request-ID", uuid.NewString())
	}
	if len(chunkHash) > 0 && chunkHash[0] != "" {
		request.Header.Set("Content-Type", "application/octet-stream")
		request.Header.Set("X-Aico-Chunk-SHA256", chunkHash[0])
	}
	// Client.Timeout includes reading the entire body. Apply it only to
	// bounded control/chunk requests, never to the full backup download.
	httpClient := *c.http
	if httpClient.Timeout == 0 {
		httpClient.Timeout = 10 * time.Minute
	}
	streaming := method == http.MethodGet && strings.HasSuffix(strings.SplitN(url, "?", 2)[0], "/archive")
	if streaming {
		httpClient.Timeout = 0
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, &exitError{code: 1, err: fmt.Errorf("device connection failed: %w", err)}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		var payload apiError
		_ = json.NewDecoder(response.Body).Decode(&payload)
		payload.Status = response.StatusCode
		return nil, &payload
	}
	if streaming {
		response.Body = newIdleStreamBody(response.Body, 2*time.Minute)
	}
	return response, nil
}

type idleStreamBody struct {
	io.ReadCloser
	timer *time.Timer
	idle  time.Duration
}

func newIdleStreamBody(body io.ReadCloser, idle time.Duration) *idleStreamBody {
	return &idleStreamBody{ReadCloser: body, idle: idle, timer: time.AfterFunc(idle, func() { _ = body.Close() })}
}

func (b *idleStreamBody) Read(buffer []byte) (int, error) {
	n, err := b.ReadCloser.Read(buffer)
	if err != nil {
		b.timer.Stop()
	} else if n > 0 {
		b.timer.Reset(b.idle)
	}
	return n, err
}

func (b *idleStreamBody) Close() error {
	b.timer.Stop()
	return b.ReadCloser.Close()
}

// apiPath accepts either a relative API path or the absolute URL returned by
// the device.  The CLI's base URL already includes the /api prefix.
func (c *client) apiPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return value
	}
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		return value
	}
	if strings.HasPrefix(value, "/api/") || value == "/api" {
		return strings.TrimPrefix(value, "/api")
	}
	return value
}

func (c *client) print(out io.Writer, value any) error {
	if c.jsonOut {
		return json.NewEncoder(out).Encode(value)
	}
	data, _ := json.MarshalIndent(value, "", "  ")
	_, err := fmt.Fprintln(out, string(data))
	return err
}

func verifyFile(name, passphrase string) (backup.VerifyResult, error) {
	file, err := os.Open(name)
	if err != nil {
		return backup.VerifyResult{}, err
	}
	defer file.Close()
	header, _, err := backup.ReadPublicHeader(file)
	if err != nil {
		return backup.VerifyResult{}, err
	}
	if header.Protection.Algorithm != "sha256-chunked" {
		return backup.VerifyResult{}, &exitError{code: 2, err: errors.New("encrypted legacy backups require the original password-capable client")}
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return backup.VerifyResult{}, err
	}
	return backup.VerifyArchive(context.Background(), file, []byte(passphrase))
}

func readLine() (string, error) {
	value, err := cliInput.ReadString('\n')
	if err != nil && value != "" {
		err = nil
	}
	return value, err
}

var cliInput = bufio.NewReader(os.Stdin)

func numberValue(value any) int64 {
	switch number := value.(type) {
	case float64:
		return int64(number)
	case json.Number:
		parsed, _ := number.Int64()
		return parsed
	case int64:
		return number
	case int:
		return int64(number)
	}
	return 0
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func exitCode(err error) int {
	var exit *exitError
	if errors.As(err, &exit) {
		return exit.code
	}
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case "insufficient_space":
			return 3
		case "source_device_mismatch", "device_mismatch", "ota_pending_boot", "identity_confirmation_required", "identity_not_portable", "sd_uuid_mismatch", "sd_missing":
			return 4
		case "commit_failed", "rollback_failed", "usb_required", "maintenance_in_progress":
			return 5
		case "wrong_passphrase", "unsupported_format", "manifest_invalid", "archive_truncated", "archive_authentication_failed", "hash_mismatch":
			return 2
		}
		return 5
	}
	if backup.ErrorCode(err) != "" {
		return 2
	}
	return 1
}
