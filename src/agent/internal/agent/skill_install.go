package agent

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxSkillInstallFileBytes     = 2 * 1024 * 1024
	maxSkillInstallTotalBytes    = 16 * 1024 * 1024
	maxSkillInstallArchiveBytes  = 32 * 1024 * 1024
	maxSkillInstallMetadataBytes = 8 * 1024 * 1024
	maxSkillInstallFiles         = 256
)

var lookupSkillInstallHostIPs = net.LookupIP

type skillInstallTargetGuard struct {
	next http.RoundTripper
}

func (g skillInstallTargetGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, fmt.Errorf("invalid skill source request")
	}
	addrs, err := lookupAllowedSkillInstallHostIPs(req.URL.Hostname())
	if err != nil {
		return nil, err
	}
	pinnedReq, next, err := pinSkillInstallProxyTarget(req, g.next, addrs[0])
	if err != nil {
		return nil, err
	}
	resp, err := next.RoundTrip(pinnedReq)
	if resp != nil {
		// Keep redirect and cookie handling anchored to the original hostname.
		resp.Request = req
	}
	return resp, err
}

func pinSkillInstallProxyTarget(req *http.Request, next http.RoundTripper, ip net.IP) (*http.Request, http.RoundTripper, error) {
	transport, ok := next.(*http.Transport)
	if !ok || transport.Proxy == nil {
		return req, next, nil
	}
	proxyURL, err := transport.Proxy(req)
	if err != nil {
		return nil, nil, err
	}
	if proxyURL == nil {
		return req, next, nil
	}

	host := req.URL.Hostname()
	port := req.URL.Port()
	if port == "" {
		if strings.EqualFold(req.URL.Scheme, "https") {
			port = "443"
		} else {
			port = "80"
		}
	}
	pinnedReq := req.Clone(req.Context())
	pinnedURL := *req.URL
	pinnedURL.Host = net.JoinHostPort(ip.String(), port)
	pinnedReq.URL = &pinnedURL
	pinnedReq.Host = req.URL.Host

	pinnedTransport := transport.Clone()
	pinnedTransport.Proxy = http.ProxyURL(proxyURL)
	if strings.EqualFold(req.URL.Scheme, "https") {
		tlsConfig := pinnedTransport.TLSClientConfig
		if tlsConfig == nil {
			tlsConfig = &tls.Config{}
		} else {
			tlsConfig = tlsConfig.Clone()
		}
		tlsConfig.ServerName = host
		pinnedTransport.TLSClientConfig = tlsConfig
	}
	return pinnedReq, pinnedTransport, nil
}

func newSkillInstallHTTPClient(proxy ProxyConfig) *http.Client {
	transport := newProxyTransport(proxy)
	if httpTransport, ok := transport.(*http.Transport); ok {
		baseDialContext := httpTransport.DialContext
		if baseDialContext == nil {
			baseDialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		}
		proxyAddressConfig := proxy
		if !proxyAddressConfig.HasProxyURL() {
			proxyAddressConfig = ProxyConfigFromEnvironment()
		}
		proxyAddresses := skillInstallProxyAddresses(proxyAddressConfig)
		httpTransport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			if _, isProxy := proxyAddresses[normalizeSkillInstallDialAddress(address)]; isProxy {
				return baseDialContext(ctx, network, address)
			}
			return dialAllowedSkillInstallAddress(ctx, baseDialContext, network, address)
		}
	}
	return &http.Client{
		Transport: skillInstallTargetGuard{next: transport},
		Timeout:   30 * time.Second,
	}
}

func skillInstallProxyAddresses(proxy ProxyConfig) map[string]struct{} {
	addresses := make(map[string]struct{})
	for _, raw := range []string{proxy.HTTPProxy, proxy.HTTPSProxy, proxy.AllProxy} {
		proxyURL, err := parseProxyURL(strings.TrimSpace(raw))
		if err != nil || proxyURL == nil {
			continue
		}
		port := proxyURL.Port()
		if port == "" {
			switch strings.ToLower(proxyURL.Scheme) {
			case "https":
				port = "443"
			case "socks5", "socks5h":
				port = "1080"
			default:
				port = "80"
			}
		}
		addresses[normalizeSkillInstallDialAddress(net.JoinHostPort(proxyURL.Hostname(), port))] = struct{}{}
	}
	return addresses
}

func normalizeSkillInstallDialAddress(address string) string {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(address))
	}
	return net.JoinHostPort(strings.ToLower(strings.TrimSpace(strings.Trim(host, "[]"))), port)
}

func dialAllowedSkillInstallAddress(ctx context.Context, dialContext func(context.Context, string, string) (net.Conn, error), network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addrs, err := lookupAllowedSkillInstallHostIPs(host)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, ip := range addrs {
		conn, err := dialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lastErr = err
	}
	return nil, lastErr
}

func lookupAllowedSkillInstallHostIPs(host string) ([]net.IP, error) {
	host = strings.ToLower(strings.TrimSpace(strings.Trim(host, "[]")))
	if host == "" {
		return nil, fmt.Errorf("invalid skill source URL: empty host")
	}
	if host == "localhost" {
		return nil, fmt.Errorf("skill source URL host is not public")
	}
	if ip := net.ParseIP(host); ip != nil {
		if skillInstallIPIsDisallowed(ip) {
			return nil, fmt.Errorf("skill source URL host is not public")
		}
		return []net.IP{ip}, nil
	}
	addrs, err := lookupSkillInstallHostIPs(host)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("resolve skill source URL host: %w", err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve skill source URL host: no addresses")
	}
	for _, ip := range addrs {
		if skillInstallIPIsDisallowed(ip) {
			return nil, fmt.Errorf("skill source URL host resolves to a non-public address")
		}
	}
	return addrs, nil
}

func skillInstallIPIsDisallowed(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	return v4[0] == 100 && v4[1]&0xc0 == 0x40 || // 100.64.0.0/10 shared address space
		v4[0] == 198 && v4[1]&0xfe == 18 // 198.18.0.0/15 benchmark networks
}

type githubSkillSource struct {
	Owner     string
	Repo      string
	Ref       string
	SkillPath string
}

type jsDelivrPackage struct {
	Files []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	} `json:"files"`
}

func (t *SkillManageTool) install(ctx context.Context, req skillManageInput) (string, error) {
	if strings.TrimSpace(req.SourceURL) == "" {
		return "", fmt.Errorf("source_url is required for install")
	}
	files, canonicalSource, err := downloadSkillFiles(ctx, t.httpClient, req.SourceURL)
	if err != nil {
		return "", fmt.Errorf("download skill: %w", err)
	}
	skillMD, ok := files["SKILL.md"]
	if !ok {
		return "", fmt.Errorf("download skill: source does not contain SKILL.md")
	}
	if !utf8.Valid(skillMD) {
		return "", fmt.Errorf("invalid SKILL.md: content is not valid UTF-8")
	}
	skill, err := parseSkillFromContent(string(skillMD))
	if err != nil {
		return "", fmt.Errorf("invalid SKILL.md: %w", err)
	}
	name := req.Name
	if name == "" {
		name = skill.Name
	}
	if !isValidSkillName(name) {
		return "", fmt.Errorf("invalid skill name %q: must not contain path separators or '..'", name)
	}
	if err := validateSkillDefinition(skill, name); err != nil {
		return "", err
	}

	finalDir := filepath.Join(t.skillsDir, name)
	if fileExists(filepath.Join(finalDir, "SKILL.md")) {
		return "", fmt.Errorf("skill %q already exists", name)
	}
	if err := os.MkdirAll(filepath.Dir(t.skillsDir), 0o755); err != nil {
		return "", err
	}
	stagedDir, err := os.MkdirTemp(filepath.Dir(t.skillsDir), ".skill-install-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stagedDir)
	if err := writeInstalledSkillFiles(stagedDir, files); err != nil {
		return "", err
	}
	if err := os.MkdirAll(t.skillsDir, 0o755); err != nil {
		return "", err
	}

	skillFileMu.Lock()
	if fileExists(filepath.Join(finalDir, "SKILL.md")) {
		skillFileMu.Unlock()
		return "", fmt.Errorf("skill %q already exists", name)
	}
	err = os.Rename(stagedDir, finalDir)
	skillFileMu.Unlock()
	if err != nil {
		return "", fmt.Errorf("activate installed skill: %w", err)
	}

	t.updateManifestOnModify(name, true)
	t.recordModify(name)
	totalBytes := 0
	for _, data := range files {
		totalBytes += len(data)
	}
	return fmt.Sprintf(
		"Installed skill %q source=%s files=%d total_bytes=%d bytes=%d lines=%d sha256=%s",
		name,
		canonicalSource,
		len(files),
		totalBytes,
		len(skillMD),
		lineCount(skillMD),
		strings.TrimPrefix(hashContent(skillMD), "sha256:"),
	), nil
}

func writeInstalledSkillFiles(dir string, files map[string][]byte) error {
	paths := make([]string, 0, len(files))
	for rel := range files {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	for _, rel := range paths {
		if !isInstallableSkillPath(rel) {
			return fmt.Errorf("unsupported skill file path %q", rel)
		}
		fullPath := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(fullPath, files[rel], 0o644); err != nil {
			return err
		}
	}
	return nil
}

func downloadSkillFiles(ctx context.Context, client *http.Client, sourceURL string) (map[string][]byte, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(sourceURL))
	if err != nil {
		return nil, "", fmt.Errorf("invalid source_url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, "", fmt.Errorf("source_url must use http or https")
	}
	if parsed.Host == "" || parsed.User != nil {
		return nil, "", fmt.Errorf("source_url must include a host and no credentials")
	}
	if githubSource, ok, parseErr := parseGitHubSkillSource(parsed); parseErr != nil {
		return nil, "", parseErr
	} else if ok {
		files, err := downloadGitHubSkill(ctx, client, githubSource)
		return files, sourceURL, err
	}

	data, err := fetchSkillURL(ctx, client, parsed.String(), maxSkillInstallFileBytes)
	if err != nil {
		return nil, "", err
	}
	return map[string][]byte{"SKILL.md": data}, parsed.String(), nil
}

func normalizeSkillInstallURL(sourceURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(sourceURL))
	if err != nil {
		return "", fmt.Errorf("invalid source_url: %w", err)
	}
	githubSource, ok, err := parseGitHubSkillSource(parsed)
	if err != nil {
		return "", err
	}
	if !ok {
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return "", fmt.Errorf("source_url must use http or https")
		}
		return parsed.String(), nil
	}
	return fmt.Sprintf(
		"https://raw.githubusercontent.com/%s/%s/%s/%s/SKILL.md",
		url.PathEscape(githubSource.Owner),
		url.PathEscape(githubSource.Repo),
		url.PathEscape(githubSource.Ref),
		escapeURLPath(githubSource.SkillPath),
	), nil
}

func parseGitHubSkillSource(parsed *url.URL) (githubSkillSource, bool, error) {
	host := strings.ToLower(parsed.Hostname())
	if host != "github.com" && host != "www.github.com" {
		return githubSkillSource{}, false, nil
	}
	parts := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	if len(parts) < 5 || (parts[2] != "tree" && parts[2] != "blob") {
		return githubSkillSource{}, false, fmt.Errorf("GitHub source_url must point to a skill directory or SKILL.md")
	}
	decoded := make([]string, len(parts))
	for i, part := range parts {
		value, err := url.PathUnescape(part)
		if err != nil {
			return githubSkillSource{}, false, fmt.Errorf("invalid GitHub source_url path: %w", err)
		}
		decoded[i] = value
	}
	skillPath := strings.Join(decoded[4:], "/")
	if parts[2] == "blob" {
		if path.Base(skillPath) != "SKILL.md" {
			return githubSkillSource{}, false, fmt.Errorf("GitHub blob source_url must point to SKILL.md")
		}
		skillPath = path.Dir(skillPath)
	}
	skillPath = strings.Trim(path.Clean("/"+skillPath), "/")
	if skillPath == "" || skillPath == "." || strings.Contains(skillPath, "..") {
		return githubSkillSource{}, false, fmt.Errorf("invalid GitHub skill path")
	}
	return githubSkillSource{Owner: decoded[0], Repo: decoded[1], Ref: decoded[3], SkillPath: skillPath}, true, nil
}

func downloadGitHubSkill(ctx context.Context, client *http.Client, source githubSkillSource) (map[string][]byte, error) {
	files, cdnErr := downloadGitHubSkillFromJSDelivr(ctx, client, source)
	if cdnErr == nil {
		return files, nil
	}
	files, archiveErr := downloadGitHubSkillArchive(ctx, client, source)
	if archiveErr == nil {
		return files, nil
	}
	rawURL, rawURLErr := normalizeSkillInstallURL(fmt.Sprintf(
		"https://github.com/%s/%s/tree/%s/%s",
		source.Owner, source.Repo, source.Ref, source.SkillPath,
	))
	if rawURLErr == nil {
		if data, rawErr := fetchSkillURL(ctx, client, rawURL, maxSkillInstallFileBytes); rawErr == nil {
			return map[string][]byte{"SKILL.md": data}, nil
		}
	}
	return nil, fmt.Errorf("jsDelivr: %v; GitHub archive: %v", cdnErr, archiveErr)
}

func downloadGitHubSkillFromJSDelivr(ctx context.Context, client *http.Client, source githubSkillSource) (map[string][]byte, error) {
	packageURL := fmt.Sprintf(
		"https://data.jsdelivr.com/v1/packages/gh/%s/%s@%s?structure=flat",
		url.PathEscape(source.Owner), url.PathEscape(source.Repo), url.PathEscape(source.Ref),
	)
	metadata, err := fetchSkillURL(ctx, client, packageURL, maxSkillInstallMetadataBytes)
	if err != nil {
		return nil, err
	}
	var pkg jsDelivrPackage
	if err := json.Unmarshal(metadata, &pkg); err != nil {
		return nil, fmt.Errorf("parse jsDelivr metadata: %w", err)
	}
	prefix := strings.Trim(source.SkillPath, "/") + "/"
	files := make(map[string][]byte)
	total := 0
	for _, file := range pkg.Files {
		repoPath := strings.TrimPrefix(file.Name, "/")
		if !strings.HasPrefix(repoPath, prefix) {
			continue
		}
		rel := strings.TrimPrefix(repoPath, prefix)
		if !isInstallableSkillPath(rel) {
			continue
		}
		if len(files) >= maxSkillInstallFiles {
			return nil, fmt.Errorf("skill contains more than %d supported files", maxSkillInstallFiles)
		}
		if file.Size > maxSkillInstallFileBytes || total+int(file.Size) > maxSkillInstallTotalBytes {
			return nil, fmt.Errorf("skill files exceed install size limit")
		}
		fileURL := fmt.Sprintf(
			"https://cdn.jsdelivr.net/gh/%s/%s@%s/%s",
			url.PathEscape(source.Owner), url.PathEscape(source.Repo), url.PathEscape(source.Ref), escapeURLPath(repoPath),
		)
		data, err := fetchSkillURL(ctx, client, fileURL, maxSkillInstallFileBytes)
		if err != nil {
			return nil, fmt.Errorf("download %s: %w", rel, err)
		}
		total += len(data)
		if total > maxSkillInstallTotalBytes {
			return nil, fmt.Errorf("skill files exceed install size limit")
		}
		files[rel] = data
	}
	if _, ok := files["SKILL.md"]; !ok {
		return nil, fmt.Errorf("skill directory does not contain SKILL.md")
	}
	return files, nil
}

func downloadGitHubSkillArchive(ctx context.Context, client *http.Client, source githubSkillSource) (map[string][]byte, error) {
	archiveURL := fmt.Sprintf(
		"https://codeload.github.com/%s/%s/zip/%s",
		url.PathEscape(source.Owner), url.PathEscape(source.Repo), url.PathEscape(source.Ref),
	)
	data, err := fetchSkillURL(ctx, client, archiveURL, maxSkillInstallArchiveBytes)
	if err != nil {
		return nil, err
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("read GitHub archive: %w", err)
	}
	prefix := strings.Trim(source.SkillPath, "/") + "/"
	files := make(map[string][]byte)
	total := 0
	for _, archived := range reader.File {
		archivePath := strings.TrimPrefix(path.Clean(archived.Name), "/")
		parts := strings.SplitN(archivePath, "/", 2)
		if len(parts) != 2 || !strings.HasPrefix(parts[1], prefix) || archived.FileInfo().IsDir() {
			continue
		}
		rel := strings.TrimPrefix(parts[1], prefix)
		if !isInstallableSkillPath(rel) {
			continue
		}
		if len(files) >= maxSkillInstallFiles || archived.UncompressedSize64 > maxSkillInstallFileBytes {
			return nil, fmt.Errorf("skill exceeds install limits")
		}
		file, err := archived.Open()
		if err != nil {
			return nil, err
		}
		fileData, readErr := readLimited(file, maxSkillInstallFileBytes)
		closeErr := file.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read %s: %w", rel, readErr)
		}
		if closeErr != nil {
			return nil, closeErr
		}
		total += len(fileData)
		if total > maxSkillInstallTotalBytes {
			return nil, fmt.Errorf("skill files exceed install size limit")
		}
		files[rel] = fileData
	}
	if _, ok := files["SKILL.md"]; !ok {
		return nil, fmt.Errorf("skill directory does not contain SKILL.md")
	}
	return files, nil
}

func fetchSkillURL(ctx context.Context, client *http.Client, sourceURL string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/markdown, application/json, application/zip, application/octet-stream;q=0.9, */*;q=0.1")
	req.Header.Set("User-Agent", "Aico-skill-installer")
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return readLimited(resp.Body, limit)
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("download exceeds %d byte limit", limit)
	}
	return data, nil
}

func isInstallableSkillPath(rel string) bool {
	rel = strings.TrimSpace(strings.ReplaceAll(rel, "\\", "/"))
	if rel == "SKILL.md" {
		return true
	}
	if rel == "" || strings.HasPrefix(rel, "/") {
		return false
	}
	segments := strings.Split(rel, "/")
	if len(segments) < 2 {
		return false
	}
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return allowedSubDirs[segments[0]]
}

func escapeURLPath(value string) string {
	parts := strings.Split(strings.Trim(value, "/"), "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func lineCount(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	lines := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		lines++
	}
	return lines
}
