package lanaccess

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// MCP access tokens are the only credential /mcp accepts, from every client.
// They are separate from session tokens: neither the device password nor a
// clearing it affects them.
// They work whether LAN access is on or off, do not expire, and are not
// revoked by a password change: each one is named when it is created and
// revoked on its own. They are accepted only on MCPPath, so a leaked token
// never reaches the settings page, backup, the terminal, SSH, or /api/tools/*
// (which would bypass the MCP tool allowlist, including the shell gate).
const (
	DefaultMCPTokensPath = "/userdata/system/mcp-tokens.json"

	MCPPath = "/mcp"

	MaxMCPTokenNameLen = 64

	mcpTokenPrefix   = "aico_mcp_"
	mcpTokenIDBytes  = 6
	mcpTokenKeyBytes = 32
	// The number of tokens is not limited. The read limit only guards
	// against a corrupt file; it holds tens of thousands of tokens.
	mcpTokenFileLimit = 8 << 20
)

var (
	ErrMCPTokenName     = errors.New("token name must be 1 to 64 characters")
	ErrMCPTokenNotFound = errors.New("MCP access token not found")
)

// MCPToken describes a stored token. The secret is never stored; only its
// SHA-256 hash is, which is enough for a 256-bit random key.
type MCPToken struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Hash      string    `json:"hash,omitempty"`
}

type mcpTokenFile struct {
	Tokens []MCPToken `json:"tokens"`
}

// MCPTokens lists stored tokens without their hashes, oldest first.
func (s *State) MCPTokens() ([]MCPToken, error) {
	tokens, err := s.readMCPTokens()
	if err != nil {
		return nil, err
	}
	for i := range tokens {
		tokens[i].Hash = ""
	}
	return tokens, nil
}

// CreateMCPToken stores a new named token and returns it with its secret,
// which is shown once and cannot be recovered.
func (s *State) CreateMCPToken(name string, now time.Time) (MCPToken, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > MaxMCPTokenNameLen || strings.ContainsAny(name, "\r\n\t") {
		return MCPToken{}, "", ErrMCPTokenName
	}
	s.mcpMu.Lock()
	defer s.mcpMu.Unlock()
	tokens, err := s.readMCPTokens()
	if err != nil {
		return MCPToken{}, "", err
	}
	idBytes := make([]byte, mcpTokenIDBytes)
	key := make([]byte, mcpTokenKeyBytes)
	if _, err := readRandom(idBytes); err != nil {
		return MCPToken{}, "", err
	}
	if _, err := readRandom(key); err != nil {
		return MCPToken{}, "", err
	}
	id := hex.EncodeToString(idBytes)
	secret := mcpTokenPrefix + id + "_" + base64.RawURLEncoding.EncodeToString(key)
	token := MCPToken{ID: id, Name: name, CreatedAt: now.UTC().Truncate(time.Second), Hash: hashMCPToken(secret)}
	if err := s.writeMCPTokens(append(tokens, token)); err != nil {
		return MCPToken{}, "", err
	}
	token.Hash = ""
	return token, secret, nil
}

// RevokeMCPToken deletes one token by ID.
func (s *State) RevokeMCPToken(id string) error {
	s.mcpMu.Lock()
	defer s.mcpMu.Unlock()
	tokens, err := s.readMCPTokens()
	if err != nil {
		return err
	}
	kept := tokens[:0]
	found := false
	for _, token := range tokens {
		if token.ID == id {
			found = true
			continue
		}
		kept = append(kept, token)
	}
	if !found {
		return ErrMCPTokenNotFound
	}
	return s.writeMCPTokens(kept)
}

// VerifyMCPToken reports whether secret is a stored MCP access token. A
// missing or damaged token file accepts nothing.
func (s *State) VerifyMCPToken(secret string) bool {
	secret = strings.TrimSpace(secret)
	rest, ok := strings.CutPrefix(secret, mcpTokenPrefix)
	if !ok {
		return false
	}
	id, _, ok := strings.Cut(rest, "_")
	if !ok || id == "" {
		return false
	}
	tokens, err := s.readMCPTokens()
	if err != nil {
		return false
	}
	want := hashMCPToken(secret)
	for _, token := range tokens {
		if token.ID == id {
			return subtle.ConstantTimeCompare([]byte(token.Hash), []byte(want)) == 1
		}
	}
	return false
}

// mcpTokenValid reports whether r is an MCP request carrying a stored MCP
// access token as its bearer.
func mcpTokenValid(state *State, r *http.Request) bool {
	if r.URL.Path != MCPPath {
		return false
	}
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(value) <= 7 || !strings.EqualFold(value[:7], "Bearer ") {
		return false
	}
	return state.VerifyMCPToken(value[7:])
}

func hashMCPToken(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func (s *State) readMCPTokens() ([]MCPToken, error) {
	file, err := os.Open(s.MCPTokensPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var stored mcpTokenFile
	if err := json.NewDecoder(io.LimitReader(file, mcpTokenFileLimit)).Decode(&stored); err != nil {
		return nil, err
	}
	return stored.Tokens, nil
}

func (s *State) writeMCPTokens(tokens []MCPToken) error {
	if tokens == nil {
		tokens = []MCPToken{}
	}
	data, err := json.MarshalIndent(mcpTokenFile{Tokens: tokens}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.MCPTokensPath, append(data, '\n'))
}
