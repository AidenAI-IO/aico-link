package configweb

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"aico-agent/internal/agent"
	"aico-agent/internal/netproxy"
)

type EnvAssignment struct {
	Key   string
	Value string
}

func parseSystemEnv(content string) ([]EnvAssignment, error) {
	assignments := make([]EnvAssignment, 0)
	scanner := bufio.NewScanner(strings.NewReader(content))
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(strings.TrimSuffix(scanner.Text(), "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line == "export" {
			return nil, fmt.Errorf("line %d: export requires assignments", lineNumber)
		}
		if strings.HasPrefix(line, "export ") || strings.HasPrefix(line, "export\t") {
			line = strings.TrimSpace(line[len("export"):])
		}
		for len(line) > 0 {
			line = strings.TrimLeftFunc(line, unicode.IsSpace)
			if line == "" || line[0] == '#' {
				break
			}
			keyEnd := 0
			for keyEnd < len(line) && isEnvNameChar(line[keyEnd], keyEnd == 0) {
				keyEnd++
			}
			if keyEnd == 0 || keyEnd >= len(line) || line[keyEnd] != '=' {
				return nil, fmt.Errorf("line %d: unsupported system env statement", lineNumber)
			}
			key := line[:keyEnd]
			line = line[keyEnd+1:]
			value, rest, err := parseEnvValue(line, lineNumber)
			if err != nil {
				return nil, err
			}
			assignments = append(assignments, EnvAssignment{Key: key, Value: value})
			line = rest
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	values := make(map[string]string, len(assignments))
	for _, assignment := range assignments {
		values[assignment.Key] = assignment.Value
	}
	for _, keys := range [][2]string{{"HTTP_PROXY", "http_proxy"}, {"HTTPS_PROXY", "https_proxy"}, {"ALL_PROXY", "all_proxy"}} {
		value := values[keys[0]]
		if value == "" {
			value = values[keys[1]]
		}
		if value != "" {
			if err := netproxy.Validate(value); err != nil {
				return nil, fmt.Errorf("%s: %v: %s", keys[1], err, value)
			}
		}
	}
	return assignments, nil
}

func isAllowedSystemEnvKey(key string) bool {
	switch key {
	case "http_proxy", "HTTP_PROXY", "https_proxy", "HTTPS_PROXY",
		"all_proxy", "ALL_PROXY", "no_proxy", "NO_PROXY",
		"ANTHROPIC_BASE_URL", "ANDROID_SERIAL", "AICO_ADB_SERIAL",
		"AICO_POINTER_MODE", "AICO_WIFI_PROXY_ENABLED":
		return true
	}
	return strings.HasSuffix(key, "_KEY") ||
		strings.HasSuffix(key, "_TOKEN") ||
		strings.HasSuffix(key, "_SECRET")
}

func isEnvNameChar(value byte, first bool) bool {
	if value == '_' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' {
		return true
	}
	return !first && value >= '0' && value <= '9'
}

func parseEnvValue(line string, lineNumber int) (string, string, error) {
	if line == "" {
		return "", "", nil
	}
	if line[0] == '\'' || line[0] == '"' {
		quote := line[0]
		var value strings.Builder
		for index := 1; index < len(line); index++ {
			ch := line[index]
			if ch == quote {
				rest := line[index+1:]
				if rest != "" && !unicode.IsSpace(rune(rest[0])) {
					return "", "", fmt.Errorf("line %d: expected whitespace after assignment", lineNumber)
				}
				return value.String(), rest, nil
			}
			if quote == '"' && (ch == '$' || ch == '`' || ch == '\\') {
				return "", "", fmt.Errorf("line %d: unsupported shell expansion in quoted value", lineNumber)
			}
			value.WriteByte(ch)
		}
		return "", "", fmt.Errorf("line %d: unterminated quoted value", lineNumber)
	}

	end := 0
	for end < len(line) && !unicode.IsSpace(rune(line[end])) {
		switch line[end] {
		case ';', '&', '|', '<', '>', '(', ')', '`', '$', '\\', '\'', '"':
			return "", "", fmt.Errorf("line %d: unsupported shell syntax in assignment value", lineNumber)
		}
		if line[end] >= 0x80 {
			return "", "", fmt.Errorf("line %d: unsupported shell syntax in assignment value", lineNumber)
		}
		end++
	}
	return line[:end], line[end:], nil
}

func readFileLimited(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return data, nil
}

func atomicWriteFile(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func (s *Server) handleGetSystemEnv(w http.ResponseWriter, r *http.Request) {
	content := ""
	data, err := readFileLimited(s.options.SystemEnvPath, maxSystemEnvSize)
	if err != nil && !os.IsNotExist(err) {
		writeJSONError(w, http.StatusServiceUnavailable, "system env file is unavailable")
		return
	}
	if err == nil {
		content = string(data)
	}
	restartRequired := s.systemEnvironmentRestartRequired(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                     true,
		"system_env":             content,
		"agent_restart_required": restartRequired,
		"apply":                  systemEnvironmentApply(restartRequired),
		"path":                   s.options.SystemEnvPath,
	})
}

func (s *Server) handleSystemEnv(w http.ResponseWriter, r *http.Request) {
	var request struct {
		SystemEnv *string `json:"system_env"`
	}
	if !readJSONBody(w, r, &request) {
		return
	}
	if request.SystemEnv == nil {
		writeJSONError(w, 400, "missing system_env string")
		return
	}
	if len(*request.SystemEnv) > maxSystemEnvSize {
		writeJSONError(w, 400, "system env is too large")
		return
	}
	assignments, err := parseSystemEnv(*request.SystemEnv)
	if err != nil {
		writeJSONError(w, 400, err.Error())
		return
	}
	for _, assignment := range assignments {
		if !isAllowedSystemEnvKey(assignment.Key) {
			writeJSONError(w, 400, "environment variable is not approved: "+assignment.Key)
			return
		}
	}
	s.systemEnvMu.Lock()
	defer s.systemEnvMu.Unlock()
	if err := atomicWriteFile(s.options.SystemEnvPath, []byte(*request.SystemEnv), 0o600); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	restartRequired := s.systemEnvironmentRestartRequired(r.Context())
	if restartRequired == nil {
		// A saved edit needs explicit application when Agent cannot confirm it.
		required := true
		restartRequired = &required
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "persisted": true, "system_env": *request.SystemEnv,
		"agent_restart_required": restartRequired,
		"apply":                  systemEnvironmentApply(restartRequired),
	})
}

// systemEnvironmentApply maps the restart flag onto the shared apply levels.
// The save handler has already resolved an unconfirmed state to "required", so
// nil here only means a read could not tell, which is not reported as pending.
func systemEnvironmentApply(restartRequired *bool) agent.ApplyLevel {
	if restartRequired != nil && *restartRequired {
		return agent.ApplyAgentRestart
	}
	return agent.ApplyLive
}

func (s *Server) systemEnvironmentRestartRequired(ctx context.Context) *bool {
	revision, err := agent.SystemEnvironmentRevision(s.options.SystemEnvPath)
	if err != nil {
		return nil
	}
	data, code, err := s.fetchAgentApplicationStatus(ctx)
	var live agent.ConfigApplyStatus
	if err != nil || code != http.StatusOK || json.Unmarshal(data, &live) != nil {
		return nil
	}
	if live.EnvironmentRevision == "" {
		return nil
	}
	required := live.EnvironmentRevision != revision
	return &required
}

func (s *Server) handleApplySystemEnv(w http.ResponseWriter, r *http.Request) {
	s.systemEnvMu.Lock()
	defer s.systemEnvMu.Unlock()
	if err := s.restartWiFiProxy(); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if err := s.scheduleAgentRestart(); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "agent_restart_scheduled": true})
}
