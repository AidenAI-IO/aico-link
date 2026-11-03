package ota

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"aico-agent/internal/logging"
)

type githubRelease struct {
	TagName string        `json:"tag_name"`
	Draft   bool          `json:"draft"`
	Assets  []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func FetchLatestReleaseAssets(ctx context.Context, baseAPIURL string, bearerToken string) (map[string]string, error) {
	return FetchLatestReleaseAssetsWithProxy(ctx, baseAPIURL, bearerToken, "")
}

func FetchLatestReleaseAssetsWithProxy(ctx context.Context, baseAPIURL string, bearerToken string, githubProxyURL string) (map[string]string, error) {
	// Apply GitHub proxy if configured
	apiURL := ApplyGitHubProxy(baseAPIURL, githubProxyURL)
	if apiURL != baseAPIURL {
		_ = logging.LogEvent(logging.Info, "ota", "github", "proxy_enabled",
			logging.Field{Key: "request", Value: "release_api"},
		)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := defaultOTAHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GitHub release status %d", resp.StatusCode)
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("decode GitHub release JSON: %w", err)
	}
	if len(release.Assets) == 0 {
		return nil, fmt.Errorf("no release assets in GitHub release")
	}
	assets := make(map[string]string, len(release.Assets))
	for _, asset := range release.Assets {
		if asset.Name == "" || asset.BrowserDownloadURL == "" {
			return nil, fmt.Errorf("GitHub release asset missing name or browser_download_url")
		}
		assets[asset.Name] = asset.BrowserDownloadURL
	}
	return assets, nil
}

func RequireReleaseAssets(assets map[string]string, names ...string) (map[string]string, error) {
	urls := make(map[string]string, len(names))
	for _, name := range names {
		url, ok := assets[name]
		if !ok || url == "" {
			return nil, fmt.Errorf("missing required release asset %s", name)
		}
		urls[name] = url
	}
	return urls, nil
}
