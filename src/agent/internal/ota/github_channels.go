package ota

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/Masterminds/semver/v3"
)

var channelVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var releaseRepoPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func managedChannel(channel string) bool {
	return channel == "dev" || channel == "staging" || channel == "prod"
}

func releaseEndpoint(config UpdaterConfig) (string, error) {
	if config.Channel != "" && !managedChannel(config.Channel) && config.Channel != "local" &&
		config.Channel != "stable" && config.Channel != "debian-stable" && !strings.HasPrefix(config.Channel, "debian-dev-") {
		return "", fmt.Errorf("unknown release channel %q", config.Channel)
	}
	if config.ReleaseURL != "" {
		return config.ReleaseURL, nil
	}
	repo := config.Repo
	if repo == "" {
		repo = "AidenAI-IO/aico-link"
	}
	if !releaseRepoPattern.MatchString(repo) {
		return "", fmt.Errorf("invalid OTA repository %q", repo)
	}
	endpoint := "https://api.github.com/repos/" + repo + "/releases"
	if !managedChannel(config.Channel) {
		endpoint += "/latest"
	}
	return endpoint, nil
}

func requireManifestChannel(channel string, manifest Manifest) error {
	if managedChannel(channel) && (manifest.Channel != channel ||
		!strings.HasPrefix(manifest.Version, channel+"-v") ||
		!channelVersionPattern.MatchString(strings.TrimPrefix(manifest.Version, channel+"-v"))) {
		return fmt.Errorf("signed manifest does not belong to configured channel %q", channel)
	}
	return nil
}

// Business releases share the channel timeline but carry no firmware manifest.
// Scan the complete list, since publication order need not match version order.
func FetchChannelReleaseAssets(ctx context.Context, endpoint, channel, token, proxy string) (map[string]string, error) {
	if !managedChannel(channel) {
		return nil, fmt.Errorf("unknown release channel %q", channel)
	}
	var best *semver.Version
	var selected map[string]string
	for page := 1; page <= 100; page++ {
		apiURL, err := url.Parse(endpoint)
		if err != nil {
			return nil, err
		}
		query := apiURL.Query()
		query.Set("per_page", "100")
		query.Set("page", strconv.Itoa(page))
		apiURL.RawQuery = query.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ApplyGitHubProxy(apiURL.String(), proxy), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := defaultOTAHTTPClient.Do(req)
		if err != nil {
			return nil, err
		}
		const limit = 4 << 20
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GitHub release status %d", resp.StatusCode)
		}
		if readErr != nil {
			return nil, readErr
		}
		if len(data) > limit {
			return nil, fmt.Errorf("GitHub release page exceeds %d bytes", limit)
		}
		var releases []githubRelease
		if err := json.Unmarshal(data, &releases); err != nil {
			return nil, fmt.Errorf("decode GitHub release list: %w", err)
		}
		for _, release := range releases {
			if release.Draft || !strings.HasPrefix(release.TagName, channel+"-v") {
				continue
			}
			text := strings.TrimPrefix(release.TagName, channel+"-v")
			if !channelVersionPattern.MatchString(text) {
				continue
			}
			version, err := semver.StrictNewVersion(text)
			if err != nil || (best != nil && !version.GreaterThan(best)) {
				continue
			}
			assets := make(map[string]string)
			for _, asset := range release.Assets {
				assets[asset.Name] = asset.BrowserDownloadURL
			}
			if assets["manifest.json"] == "" {
				continue
			}
			if _, err := RequireReleaseAssets(assets, "boot_a.img.tar.gz", "boot_b.img.tar.gz", "rootfs.img.tar.gz"); err != nil {
				return nil, fmt.Errorf("incomplete channel OTA %s: %w", release.TagName, err)
			}
			best, selected = version, assets
		}
		if len(releases) < 100 {
			if selected == nil {
				return nil, fmt.Errorf("no published OTA for channel %q", channel)
			}
			return selected, nil
		}
	}
	return nil, fmt.Errorf("GitHub release pagination limit exceeded")
}
