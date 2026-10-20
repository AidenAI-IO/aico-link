package configweb

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"aiden-agent/internal/backup"
	"github.com/google/uuid"
)

const testPassphrase = "correct horse battery staple"

var testAgentConfig = []byte("[basic_settings.language_timezone]\nlocale = \"en-US\"\ntimezone = \"UTC\"\n\n[basic_settings.device]\ndevice_type = \"iOS\"\n\n[model_settings.model]\nprovider = \"fake\"\nresponses = [\"restored\"]\n\n[conversation_settings.search]\nprovider = \"duckduckgo\"\n")

// buildTestArchive plans and encrypts an archive from a synthetic userdata
// root and returns the bytes plus the parsed public header.
func buildTestArchive(t *testing.T, source string, mode backup.Mode, components []backup.ComponentID, hardwareID string) ([]byte, backup.PublicHeader) {
	t.Helper()
	return buildTestArchiveWithProtection(t, source, mode, components, hardwareID, false)
}

func buildTestArchiveWithProtection(t *testing.T, source string, mode backup.Mode, components []backup.ComponentID, hardwareID string, plain bool) ([]byte, backup.PublicHeader) {
	t.Helper()
	plan, err := backup.NewPlanner(backup.Roots{Userdata: source, SD: filepath.Join(source, "sd")}).Plan(context.Background(), backup.PlanOptions{
		Mode: mode, Components: components, BackupID: "restore-test",
		Source: backup.SourceIdentity{HardwareID: hardwareID, MachineID: "0123456789abcdef0123456789abcdef"},
	})
	if err != nil {
		t.Fatal(err)
	}
	material := backup.NewPlainMaterial(plan.Manifest.CreatedAt)
	if !plain {
		material, err = backup.NewKeyMaterial([]byte(testPassphrase), plan.Manifest.CreatedAt)
		if err != nil {
			t.Fatal(err)
		}
	}
	defer material.Destroy()
	var archive bytes.Buffer
	if err := backup.WriteArchive(context.Background(), &archive, plan, material, nil); err != nil {
		t.Fatal(err)
	}
	header, _, err := backup.ReadPublicHeader(bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	return archive.Bytes(), header
}

func newRestoreTestServer(t *testing.T, target string) *Server {
	t.Helper()
	options := testOptions(t)
	options.BackupUserdataRoot = target
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

type restoreClient struct {
	t        *testing.T
	server   *Server
	token    string
	csrf     string
	transfer string
	jobID    string
}

func newRestoreClient(t *testing.T, server *Server) *restoreClient {
	t.Helper()
	token, csrf := createRestoreTestSession(t, server)
	return &restoreClient{t: t, server: server, token: token, csrf: csrf}
}

func (c *restoreClient) create(archive []byte, header backup.PublicHeader, passphrase string) *httptest.ResponseRecorder {
	protection := map[string]any{"mode": "passphrase", "passphrase": passphrase}
	if header.Protection.Algorithm == "sha256-chunked" {
		protection = map[string]any{"mode": "none"}
	}
	body, _ := json.Marshal(map[string]any{"format_version": 1, "archive_size": len(archive), "public_header": header, "protection": protection})
	resp := serveRestoreTestRequest(c.server, http.MethodPost, "/api/restore/jobs", body, c.token, c.csrf, true)
	if resp.Code == http.StatusAccepted {
		var created struct {
			JobID         string `json:"job_id"`
			TransferToken string `json:"transfer_token"`
		}
		if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil {
			c.t.Fatal(err)
		}
		c.jobID, c.transfer = created.JobID, created.TransferToken
	}
	return resp
}

func (c *restoreClient) chunk(index int, data []byte) *httptest.ResponseRecorder {
	digest := sha256.Sum256(data)
	req := httptest.NewRequest(http.MethodPut, "/api/restore/jobs/"+c.jobID+"/chunks/"+strconv.Itoa(index), bytes.NewReader(data))
	req.RemoteAddr = "127.0.0.2:1234"
	req.Header.Set("Authorization", "Bearer "+c.transfer)
	req.Header.Set("X-Aiden-Chunk-SHA256", hex.EncodeToString(digest[:]))
	resp := httptest.NewRecorder()
	c.server.ServeHTTP(resp, req)
	return resp
}

func (c *restoreClient) post(action string, body map[string]any) *httptest.ResponseRecorder {
	data, _ := json.Marshal(body)
	return serveRestoreTestRequest(c.server, http.MethodPost, "/api/restore/jobs/"+c.jobID+"/"+action, data, c.token, c.csrf, true)
}

func (c *restoreClient) status() map[string]any {
	resp := serveRestoreTestRequest(c.server, http.MethodGet, "/api/restore/jobs/"+c.jobID, nil, c.token, c.csrf, true)
	if resp.Code != http.StatusOK {
		c.t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	return decodeJSON(c.t, resp)
}

func decodeJSON(t *testing.T, resp *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode %q: %v", resp.Body.String(), err)
	}
	return payload
}

func chunked(archive []byte, size int) [][]byte {
	var chunks [][]byte
	for offset := 0; offset < len(archive); offset += size {
		end := offset + size
		if end > len(archive) {
			end = len(archive)
		}
		chunks = append(chunks, archive[offset:end])
	}
	return chunks
}

func TestRestoreJobEndToEnd(t *testing.T) {
	for _, plain := range []bool{false, true} {
		name := "legacy_encrypted"
		if plain {
			name = "password_free"
		}
		t.Run(name, func(t *testing.T) { testRestoreJobEndToEnd(t, plain) })
	}
}

func testRestoreJobEndToEnd(t *testing.T, plain bool) {
	source := t.TempDir()
	target := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "agent/agent.toml"), testAgentConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	archive, header := buildTestArchiveWithProtection(t, source, backup.ModeSameDevice, []backup.ComponentID{backup.ComponentAgentConfig}, "", plain)
	server := newRestoreTestServer(t, target)
	client := newRestoreClient(t, server)
	if resp := client.create(archive, header, testPassphrase); resp.Code != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", resp.Code, resp.Body.String())
	}
	resp := client.chunk(0, archive)
	if resp.Code != http.StatusAccepted {
		t.Fatalf("chunk status=%d body=%s", resp.Code, resp.Body.String())
	}
	// The single block carries the whole archive: the parser must already
	// have paused at manifest.json when the chunk response is written.
	if payload := decodeJSON(t, resp); payload["state"] != restoreJobAwaitingPlan || payload["manifest"] == nil {
		t.Fatalf("chunk response = %s", resp.Body.String())
	}
	if !server.maintenance.active() {
		t.Fatal("maintenance lock not held during ingest")
	}
	planResp := client.post("plan", map[string]any{"components": []string{"agent_config"}})
	if planResp.Code != http.StatusOK {
		t.Fatalf("plan status=%d body=%s", planResp.Code, planResp.Body.String())
	}
	planned := decodeJSON(t, planResp)
	digest, _ := planned["plan_digest"].(string)
	if digest == "" || planned["state"] != restoreJobValidating {
		t.Fatalf("plan response=%s", planResp.Body.String())
	}
	validateResp := client.post("validate", map[string]any{})
	if validateResp.Code != http.StatusOK {
		t.Fatalf("validate status=%d body=%s", validateResp.Code, validateResp.Body.String())
	}
	if _, err := os.Stat(backup.TransactionPath(target, client.jobID)); err != nil {
		t.Fatalf("prepared transaction log missing: %v", err)
	}
	if resp := client.post("apply", map[string]any{"plan_digest": "wrong", "confirm": "RESTORE"}); resp.Code != http.StatusConflict {
		t.Fatalf("apply with wrong digest status=%d body=%s", resp.Code, resp.Body.String())
	}
	applyResp := client.post("apply", map[string]any{"plan_digest": digest, "confirm": "RESTORE"})
	if applyResp.Code != http.StatusAccepted {
		t.Fatalf("apply status=%d body=%s", applyResp.Code, applyResp.Body.String())
	}
	if payload := decodeJSON(t, applyResp); payload["state"] != restoreJobCompleted {
		t.Fatalf("apply response=%s", applyResp.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(target, "agent/agent.toml"))
	if err != nil || !bytes.Equal(data, testAgentConfig) {
		t.Fatalf("restored data=%q err=%v", data, err)
	}
	if _, err := os.Stat(backup.TransactionDir(target, client.jobID)); !os.IsNotExist(err) {
		t.Fatalf("transaction directory not cleaned: %v", err)
	}
	if server.maintenance.active() {
		t.Fatal("maintenance lock still held after completion")
	}
}

func TestRestoreMultiChunkPausesForPlanAndStreamsToStaging(t *testing.T) {
	source := t.TempDir()
	target := t.TempDir()
	// Incompressible payload larger than two encrypted frames so the upload
	// takes several blocks and the manifest lands in the first one.
	uploadChunk := backup.UploadChunkSize(backup.DefaultChunkSize)
	payload := make([]byte, 2*uploadChunk+4096)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(source, "agent/memory/long_term"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "agent/memory/long_term/blob.bin"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "agent/memory/profile.md"), []byte("remember\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Existing target content must survive as a rollback copy until commit
	// completes, then disappear.
	if err := os.MkdirAll(filepath.Join(target, "agent/memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "agent/memory/stale.md"), []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive, header := buildTestArchive(t, source, backup.ModePortable, []backup.ComponentID{backup.ComponentAgentMemory}, "")
	chunks := chunked(archive, uploadChunk)
	if len(chunks) < 3 {
		t.Fatalf("expected at least 3 chunks, got %d", len(chunks))
	}
	server := newRestoreTestServer(t, target)
	client := newRestoreClient(t, server)
	if resp := client.create(archive, header, testPassphrase); resp.Code != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", resp.Code, resp.Body.String())
	}
	resp := client.chunk(0, chunks[0])
	if resp.Code != http.StatusAccepted {
		t.Fatalf("chunk 0 status=%d body=%s", resp.Code, resp.Body.String())
	}
	if payload := decodeJSON(t, resp); payload["state"] != restoreJobAwaitingPlan {
		t.Fatalf("chunk 0 response=%s", resp.Body.String())
	}
	// Further blocks are refused until the plan is submitted, and out-of-order
	// blocks are refused always.
	if resp := client.chunk(1, chunks[1]); resp.Code != http.StatusConflict {
		t.Fatalf("chunk before plan status=%d body=%s", resp.Code, resp.Body.String())
	}
	planResp := client.post("plan", map[string]any{})
	if planResp.Code != http.StatusOK {
		t.Fatalf("plan status=%d body=%s", planResp.Code, planResp.Body.String())
	}
	if state := decodeJSON(t, planResp)["state"]; state != restoreJobUploading {
		t.Fatalf("plan state=%v body=%s", state, planResp.Body.String())
	}
	if resp := client.chunk(2, chunks[2]); resp.Code != http.StatusConflict || !strings.Contains(resp.Body.String(), "chunk_out_of_order") {
		t.Fatalf("out-of-order chunk status=%d body=%s", resp.Code, resp.Body.String())
	}
	for index := 1; index < len(chunks); index++ {
		resp := client.chunk(index, chunks[index])
		if resp.Code != http.StatusAccepted {
			t.Fatalf("chunk %d status=%d body=%s job=%v", index, resp.Code, resp.Body.String(), client.status())
		}
		state := decodeJSON(t, resp)["state"]
		if index < len(chunks)-1 && state != restoreJobUploading {
			t.Fatalf("chunk %d state=%v", index, state)
		}
		if index == len(chunks)-1 && state != restoreJobValidating {
			t.Fatalf("final chunk state=%v body=%s", state, resp.Body.String())
		}
	}
	staged := filepath.Join(backup.TransactionDir(target, client.jobID), "new/agent-memory/long_term/blob.bin")
	if data, err := os.ReadFile(staged); err != nil || !bytes.Equal(data, payload) {
		t.Fatalf("staged payload mismatch: %v", err)
	}
	validateResp := client.post("validate", map[string]any{})
	if validateResp.Code != http.StatusOK {
		t.Fatalf("validate status=%d body=%s", validateResp.Code, validateResp.Body.String())
	}
	digest, _ := decodeJSON(t, validateResp)["plan_digest"].(string)
	applyResp := client.post("apply", map[string]any{"plan_digest": digest, "confirm": "restore"})
	if applyResp.Code != http.StatusAccepted {
		t.Fatalf("apply status=%d body=%s", applyResp.Code, applyResp.Body.String())
	}
	if data, err := os.ReadFile(filepath.Join(target, "agent/memory/long_term/blob.bin")); err != nil || !bytes.Equal(data, payload) {
		t.Fatalf("restored payload mismatch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "agent/memory/stale.md")); !os.IsNotExist(err) {
		t.Fatalf("stale target content survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, backup.TransactionDirName)); err == nil {
		entries, _ := os.ReadDir(filepath.Join(target, backup.TransactionDirName))
		if len(entries) != 0 {
			t.Fatalf("transaction directories left behind: %d", len(entries))
		}
	}
}

// The plan response alone must make the job accept the next chunk.  No parser
// goroutine runs here, which models the parser not yet being scheduled after
// the plan was accepted, so the outcome cannot depend on scheduling.
func TestRestorePlanResponseAcceptsChunksBeforeParserResumes(t *testing.T) {
	source := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "agent/agent.toml"), testAgentConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	archive, header := buildTestArchiveWithProtection(t, source, backup.ModeSameDevice, []backup.ComponentID{backup.ComponentAgentConfig}, "", true)
	server := newRestoreTestServer(t, t.TempDir())
	client := newRestoreClient(t, server)
	if resp := client.create(archive, header, ""); resp.Code != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", resp.Code, resp.Body.String())
	}
	job, ok := server.restoreJobs.get(client.jobID)
	if !ok {
		t.Fatal("restore job not registered")
	}
	t.Cleanup(func() { server.abortRestore(job, "cancelled", context.Canceled) })
	job.mu.Lock()
	material := job.material
	job.mu.Unlock()
	stream, err := backup.NewArchiveStream(bytes.NewReader(archive), material)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := stream.ReadManifest()
	if err != nil {
		t.Fatal(err)
	}
	// Where restoreParseLoop leaves a job once chunk 0 reached manifest.json.
	job.mu.Lock()
	job.manifest, job.manifestOK = manifest, true
	job.state, job.phase = restoreJobAwaitingPlan, restoreJobAwaitingPlan
	job.receivedChunkIndex = 1
	job.mu.Unlock()

	planResp := client.post("plan", map[string]any{"components": []string{"agent_config"}})
	if planResp.Code != http.StatusOK {
		t.Fatalf("plan status=%d body=%s", planResp.Code, planResp.Body.String())
	}
	if state := decodeJSON(t, planResp)["state"]; state != restoreJobUploading {
		t.Fatalf("plan state=%v body=%s", state, planResp.Body.String())
	}
	persisted, err := os.ReadFile(filepath.Join(server.options.BackupJobStateDir, client.jobID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(persisted, &record); err != nil || record["state"] != restoreJobUploading {
		t.Fatalf("persisted job=%s err=%v", persisted, err)
	}
	// Past the state gate, an out-of-order index is refused before the body is
	// queued, so this needs no parser.
	resp := client.chunk(2, archive)
	if resp.Code != http.StatusConflict || !strings.Contains(resp.Body.String(), "chunk_out_of_order") {
		t.Fatalf("chunk after plan status=%d body=%s", resp.Code, resp.Body.String())
	}
	if payload := decodeJSON(t, resp); payload["expected_index"] != float64(1) {
		t.Fatalf("chunk after plan body=%s", resp.Body.String())
	}
	if resp := client.post("plan", map[string]any{"components": []string{"agent_config"}}); resp.Code != http.StatusConflict || !strings.Contains(resp.Body.String(), "restore_plan_unavailable") {
		t.Fatalf("second plan status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func TestRestoreWrongPassphraseFailsOnFirstChunkAndReleasesMaintenance(t *testing.T) {
	source := t.TempDir()
	target := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "agent/agent.toml"), testAgentConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	archive, header := buildTestArchive(t, source, backup.ModePortable, []backup.ComponentID{backup.ComponentAgentConfig}, "")
	server := newRestoreTestServer(t, target)
	client := newRestoreClient(t, server)
	if resp := client.create(archive, header, "definitely not the passphrase"); resp.Code != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", resp.Code, resp.Body.String())
	}
	resp := client.chunk(0, archive)
	if resp.Code != http.StatusUnauthorized || !strings.Contains(resp.Body.String(), "wrong_passphrase") {
		t.Fatalf("wrong passphrase chunk status=%d body=%s", resp.Code, resp.Body.String())
	}
	if server.maintenance.active() {
		t.Fatal("maintenance lock still held after failed ingest")
	}
	if _, err := os.Stat(backup.TransactionDir(target, client.jobID)); !os.IsNotExist(err) {
		t.Fatalf("staging left behind after failure: %v", err)
	}
	if status := client.status(); status["state"] != restoreJobFailed {
		t.Fatalf("status=%v", status)
	}
}

func TestRestorePlanRejectsIdentityDuringPendingBootAndOnForeignDevice(t *testing.T) {
	source := t.TempDir()
	target := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "system"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "system/machine-id"), []byte("0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive, header := buildTestArchive(t, source, backup.ModeSameDevice, []backup.ComponentID{backup.ComponentDeviceIdentity}, "hw-serial-1")

	run := func(t *testing.T, prepare func(server *Server), body map[string]any) (*Server, *restoreClient, *httptest.ResponseRecorder) {
		t.Helper()
		server := newRestoreTestServer(t, target)
		prepare(server)
		client := newRestoreClient(t, server)
		if resp := client.create(archive, header, testPassphrase); resp.Code != http.StatusAccepted {
			t.Fatalf("create status=%d body=%s", resp.Code, resp.Body.String())
		}
		if resp := client.chunk(0, archive); resp.Code != http.StatusAccepted {
			t.Fatalf("chunk status=%d body=%s", resp.Code, resp.Body.String())
		}
		resp := client.post("plan", body)
		return server, client, resp
	}
	cleanup := func(client *restoreClient) {
		serveRestoreTestRequest(client.server, http.MethodDelete, "/api/restore/jobs/"+client.jobID, nil, client.token, client.csrf, true)
	}

	t.Run("pending boot", func(t *testing.T) {
		pending := filepath.Join(target, "ota/pending_boot.json")
		if err := os.MkdirAll(filepath.Dir(pending), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pending, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(pending)
		_, client, resp := run(t, func(server *Server) {
			_ = os.WriteFile(server.options.HardwareIDPath, []byte("hw-serial-1"), 0o600)
		}, map[string]any{})
		defer cleanup(client)
		if resp.Code != http.StatusConflict || !strings.Contains(resp.Body.String(), "ota_pending_boot") {
			t.Fatalf("plan status=%d body=%s", resp.Code, resp.Body.String())
		}
	})
	t.Run("foreign device", func(t *testing.T) {
		_, client, resp := run(t, func(server *Server) {
			_ = os.WriteFile(server.options.HardwareIDPath, []byte("hw-serial-2"), 0o600)
		}, map[string]any{})
		defer cleanup(client)
		if resp.Code != http.StatusConflict || !strings.Contains(resp.Body.String(), "source_device_mismatch") {
			t.Fatalf("plan status=%d body=%s", resp.Code, resp.Body.String())
		}
	})
	t.Run("unknown hardware requires confirmation", func(t *testing.T) {
		_, client, resp := run(t, func(*Server) {}, map[string]any{})
		defer cleanup(client)
		if resp.Code != http.StatusConflict || !strings.Contains(resp.Body.String(), "identity_confirmation_required") {
			t.Fatalf("plan status=%d body=%s", resp.Code, resp.Body.String())
		}
	})
	t.Run("matching device", func(t *testing.T) {
		_, client, resp := run(t, func(server *Server) {
			_ = os.WriteFile(server.options.HardwareIDPath, []byte("hw-serial-1"), 0o600)
		}, map[string]any{})
		defer cleanup(client)
		if resp.Code != http.StatusOK {
			t.Fatalf("plan status=%d body=%s", resp.Code, resp.Body.String())
		}
	})
}

func TestMaintenanceBlocksConflictingAPIsAndDefersAgentRestart(t *testing.T) {
	source := t.TempDir()
	target := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "agent/agent.toml"), testAgentConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	archive, header := buildTestArchive(t, source, backup.ModePortable, []backup.ComponentID{backup.ComponentAgentConfig}, "")
	server := newRestoreTestServer(t, target)
	client := newRestoreClient(t, server)
	if resp := client.create(archive, header, testPassphrase); resp.Code != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", resp.Code, resp.Body.String())
	}
	if resp := client.chunk(0, archive); resp.Code != http.StatusAccepted {
		t.Fatalf("chunk status=%d body=%s", resp.Code, resp.Body.String())
	}
	// Conflicting endpoints are locked while the job is awaiting its plan.
	req := httptest.NewRequest(http.MethodPost, "/api/storage/format", strings.NewReader(`{"fs":"ext4","confirm":"FORMAT"}`))
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, req)
	if resp.Code != http.StatusLocked || !strings.Contains(resp.Body.String(), "maintenance_in_progress") {
		t.Fatalf("format during maintenance status=%d body=%s", resp.Code, resp.Body.String())
	}
	// Status polling stays available and reports the running job.
	current := serveRestoreTestRequest(server, http.MethodGet, "/api/maintenance/current", nil, client.token, client.csrf, true)
	if current.Code != http.StatusOK {
		t.Fatalf("maintenance/current status=%d body=%s", current.Code, current.Body.String())
	}
	payload := decodeJSON(t, current)
	if payload["operation"] != "restore" || payload["job_id"] != client.jobID || payload["job"] == nil {
		t.Fatalf("maintenance/current=%s", current.Body.String())
	}
	// A restart requested during maintenance is queued, not executed, and
	// polling does not start it either.
	if err := server.scheduleAgentRestart(); err != nil {
		t.Fatal(err)
	}
	server.restartMu.Lock()
	deferred, running := server.restartDeferred, server.restartCommand != nil
	server.restartMu.Unlock()
	if !deferred || running {
		t.Fatalf("restart deferred=%v running=%v", deferred, running)
	}
	client.status()
	server.restartMu.Lock()
	deferred, running = server.restartDeferred, server.restartCommand != nil
	server.restartMu.Unlock()
	if !deferred || running {
		t.Fatalf("restart after poll deferred=%v running=%v", deferred, running)
	}
	// Cancelling releases the lock; the queued restart is then attempted.
	cancel := serveRestoreTestRequest(server, http.MethodDelete, "/api/restore/jobs/"+client.jobID, nil, client.token, client.csrf, true)
	if cancel.Code != http.StatusAccepted {
		t.Fatalf("cancel status=%d body=%s", cancel.Code, cancel.Body.String())
	}
	if server.maintenance.active() {
		t.Fatal("maintenance lock still held after cancel")
	}
	server.restartMu.Lock()
	deferred = server.restartDeferred
	server.restartMu.Unlock()
	if deferred {
		t.Fatal("deferred restart was not attempted after maintenance ended")
	}
	if _, err := os.Stat(backup.TransactionDir(target, client.jobID)); !os.IsNotExist(err) {
		t.Fatalf("staging left behind after cancel: %v", err)
	}
	after := serveRestoreTestRequest(server, http.MethodGet, "/api/maintenance/current", nil, client.token, client.csrf, true)
	if after.Code != http.StatusNoContent {
		t.Fatalf("maintenance/current after cancel status=%d", after.Code)
	}
}

func TestBackupJobStreamsVerifiableArchive(t *testing.T) {
	target := t.TempDir()
	for _, name := range []string{"agent/python/lib/package.py", "agent/log/events.log"} {
		path := filepath.Join(target, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("advanced data\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(target, "agent/memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "agent/agent.toml"), testAgentConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "agent/memory/profile.md"), []byte("remember\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := newRestoreTestServer(t, target)
	token, csrf := createRestoreTestSession(t, server)
	if err := os.WriteFile(server.options.HardwareIDPath, []byte("board-test"), 0o600); err != nil {
		t.Fatal(err)
	}
	capabilities := serveRestoreTestRequest(server, http.MethodGet, "/api/backup/capabilities", nil, token, csrf, true)
	if capabilities.Code != http.StatusOK || !strings.Contains(capabilities.Body.String(), `"estimated_size"`) {
		t.Fatalf("capabilities status=%d body=%s", capabilities.Code, capabilities.Body.String())
	}
	body, _ := json.Marshal(map[string]any{"format_version": 1, "protection": map[string]any{"mode": "none"}})
	created := serveRestoreTestRequest(server, http.MethodPost, "/api/backup/jobs", body, token, csrf, true)
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	payload := decodeJSON(t, created)
	jobID, _ := payload["job_id"].(string)
	archiveURL, _ := payload["archive_url"].(string)
	transfer, _ := payload["transfer_token"].(string)
	if jobID == "" || archiveURL == "" || transfer == "" {
		t.Fatalf("create response=%s", created.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, archiveURL, nil)
	req.RemoteAddr = "127.0.0.2:1234"
	req.Header.Set("Authorization", "Bearer "+transfer)
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || resp.Header().Get("Content-Type") != backup.ArchiveMIMEType {
		t.Fatalf("archive status=%d type=%q body=%s", resp.Code, resp.Header().Get("Content-Type"), resp.Body.String())
	}
	result, err := backup.VerifyArchive(context.Background(), bytes.NewReader(resp.Body.Bytes()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Manifest.Components) != 4 || result.Manifest.Mode != backup.ModeSameDevice || result.Header.Protection.Algorithm != "sha256-chunked" {
		t.Fatalf("manifest=%+v", result.Manifest)
	}
	job, _ := server.backupJobs.get(jobID)
	if len(job.components) != len(backup.DefaultComponents(backup.ModeSameDevice, false)) {
		t.Fatal("job did not include all available components")
	}
	for _, id := range []backup.ComponentID{backup.ComponentPythonEnvironment, backup.ComponentDiagnostics} {
		found := false
		for _, component := range result.Manifest.Components {
			if component.ID == id {
				found = true
			}
		}
		if !found {
			t.Fatalf("advanced component %s missing from archive", id)
		}
	}
	status := serveRestoreTestRequest(server, http.MethodGet, "/api/backup/jobs/"+jobID, nil, token, csrf, true)
	if status.Code != http.StatusOK || decodeJSON(t, status)["state"] != backupJobCompleted {
		t.Fatalf("status=%d body=%s", status.Code, status.Body.String())
	}
	if server.maintenance.active() {
		t.Fatal("maintenance lock still held after backup")
	}
	// The transfer token is single-use for the archive; a second download is
	// refused.
	again := httptest.NewRecorder()
	server.ServeHTTP(again, req)
	if again.Code == http.StatusOK {
		t.Fatal("archive streamed twice for the same job")
	}
}

func TestRestoreRecoveryAtStartupFinishesInterruptedCommit(t *testing.T) {
	target := t.TempDir()
	jobID := "rinterrupted"
	staged := filepath.Join(backup.TransactionDir(target, jobID), "new/agent-memory")
	if err := os.MkdirAll(staged, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "profile.md"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	txn := backup.Transaction{JobID: jobID, Layer: backup.LayerUserdata, RequiredLayers: []string{backup.LayerUserdata}, State: backup.TransactionCommitting,
		Units: []*backup.TransactionUnit{{Key: "userdata:agent-memory", Component: backup.ComponentAgentMemory, Layer: backup.LayerUserdata,
			Target: filepath.Join(target, "agent/memory"), Staged: staged, Old: filepath.Join(backup.TransactionDir(target, jobID), "old/agent-memory"), Directory: true}}}
	if err := backup.WriteTransaction(target, txn); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(backup.TransactionDir(target, "rorphan"), "new/agent-sessions")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	server := newRestoreTestServer(t, target)
	server.recoverRestoreTransactions()
	if data, err := os.ReadFile(filepath.Join(target, "agent/memory/profile.md")); err != nil || string(data) != "new\n" {
		t.Fatalf("recovered data=%q err=%v", data, err)
	}
	entries, _ := os.ReadDir(filepath.Join(target, backup.TransactionDirName))
	if len(entries) != 0 {
		t.Fatalf("transaction directories left behind: %d", len(entries))
	}
	token, csrf := createRestoreTestSession(t, server)
	capabilities := serveRestoreTestRequest(server, http.MethodGet, "/api/backup/capabilities", nil, token, csrf, true)
	if !strings.Contains(capabilities.Body.String(), `"last_recovery"`) || !strings.Contains(capabilities.Body.String(), `"committed"`) {
		t.Fatalf("capabilities=%s", capabilities.Body.String())
	}
}

func TestRollbackFailureKeepsMaintenanceAndSnapshotLeases(t *testing.T) {
	maintenance := newMaintenanceController(filepath.Join(t.TempDir(), "maintenance.lock"))
	maintenanceLease, err := maintenance.begin("restore", "rollback-failed", false)
	if err != nil {
		t.Fatal(err)
	}
	snapshotLease := &fakeStorageSnapshotLease{}
	job := &restoreJob{
		id: "rollback-failed", state: restoreJobRollingBack, phase: restoreJobRollingBack,
		maintenance: maintenanceLease, sdLease: snapshotLease, done: make(chan struct{}),
	}
	server := &Server{maintenance: maintenance}
	server.finishRestoreJob(job, restoreJobRollbackFailed, "rollback_failed", "rollback failed")
	if !maintenance.active() {
		t.Fatal("rollback failure released maintenance and allowed writers to restart")
	}
	if snapshotLease.released.Load() {
		t.Fatal("rollback failure released the SD snapshot lease")
	}
	job.mu.Lock()
	heldMaintenance, heldSnapshot := job.maintenance, job.sdLease
	job.mu.Unlock()
	if heldMaintenance == nil || heldSnapshot == nil {
		t.Fatal("rollback failure did not retain recovery leases")
	}
	maintenanceLease.Release()
	snapshotLease.Release()
}

func TestAbortDoesNotInterruptRestoreCommitPhases(t *testing.T) {
	for _, state := range []string{
		restoreJobQuiescing, restoreJobCommitting, restoreJobPostProcessing,
		restoreJobResuming, restoreJobRollingBack,
	} {
		t.Run(state, func(t *testing.T) {
			cancelled := false
			job := &restoreJob{id: "commit", state: state, phase: state, done: make(chan struct{}), cancel: func() { cancelled = true }}
			server := &Server{}
			server.abortRestore(job, "server_shutdown", context.Canceled)
			job.mu.Lock()
			gotState := job.state
			job.mu.Unlock()
			if gotState != state || cancelled {
				t.Fatalf("abort changed commit phase: state=%q cancelled=%v", gotState, cancelled)
			}
			if job.publicMap()["cancelable"] != false {
				t.Fatal("commit phase was exposed as cancelable")
			}
		})
	}
}

func createRestoreTestSession(t *testing.T, server *Server) (string, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/maintenance/sessions", nil)
	req.RemoteAddr = "127.0.0.2:1234"
	req.Header.Set("X-Aiden-Client", "test/1")
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("session status=%d body=%s", resp.Code, resp.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	return raw["token"].(string), raw["csrf_token"].(string)
}

func serveRestoreTestRequest(server *Server, method, path string, body []byte, token, csrf string, session bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.2:1234"
	if session {
		req.AddCookie(&http.Cookie{Name: "aiden_maintenance", Value: token})
		req.Header.Set("X-Aiden-CSRF-Token", csrf)
		req.Header.Set("X-Aiden-Request-ID", uuid.NewString())
	} else {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, req)
	return resp
}
