package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gocolly/colly"
	"github.com/tmc/langchaingo/tools/duckduckgo"
	"github.com/tmc/langchaingo/tools/wikipedia"
)

const defaultWebSearchMaxResults = 5
const defaultUserAgent = "aico/1.0 (+https://aico.link)"
const defaultWebToolTimeout = 15 * time.Second
const maxWebToolOutputBytes = 12_000
const braveSearchEndpoint = "https://api.search.brave.com/res/v1/web/search"

var lookupScrapeHostIPs = net.LookupIP
var webScraperRequestDelay = 3 * time.Second

// searchBackend abstracts the actual search call.
type searchBackend interface {
	Search(ctx context.Context, query string) (string, error)
}

// --- Web Search Tool ---

type WebSearchTool struct {
	backend  searchBackend
	provider string
}

func NewWebSearchTool(cfg SearchConfig, proxy ProxyConfig) *WebSearchTool {
	provider := cfg.ProviderOrDefault()
	var backend searchBackend

	httpClient := newProxyHTTPClient(proxy)
	httpClient.Timeout = defaultWebToolTimeout

	switch provider {
	case searchProviderDuckDuckGo:
		inner, err := duckduckgo.New(
			defaultWebSearchMaxResults,
			defaultUserAgent,
			duckduckgo.WithHTTPClient(httpClient),
		)
		if err == nil {
			backend = &duckduckgoBackend{inner: inner}
		}
	case searchProviderTavily:
		apiKey := searchAPIKeyOrEnv(cfg.APIKey)
		if apiKey != "" {
			backend = &tavilyBackend{
				apiKey: apiKey,
				client: *httpClient,
			}
		}
	case searchProviderBrave:
		apiKey := searchAPIKeyOrEnv(cfg.APIKey, braveSearchAPIKeyEnv)
		if apiKey != "" {
			backend = &braveBackend{
				apiKey:   apiKey,
				client:   *httpClient,
				endpoint: braveSearchEndpoint,
			}
		}
	}

	return &WebSearchTool{backend: backend, provider: provider}
}

func (t *WebSearchTool) Name() string { return "web_search" }

func (t *WebSearchTool) Description() string {
	return `Search the public web and return result snippets. ` +
		`Use when you need information that is not on the device, such as ` +
		`looking up product details, news, current events, or how a UI element should look.`
}

func (t *WebSearchTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"query": stringArgSchema("Search query."),
	}, "query")
}

func (t *WebSearchTool) Call(ctx context.Context, input string) (string, error) {
	if t.backend == nil {
		return toolErrorResultString(ctx, CodeModuleUnavailable, "web_search tool is not configured (provider="+t.provider+")"), nil
	}

	query := strings.TrimSpace(input)
	if query == "" {
		return toolErrorResultString(ctx, CodeInvalidArguments, "query is required"), nil
	}

	if strings.HasPrefix(query, "{") {
		var args struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal([]byte(query), &args); err == nil && strings.TrimSpace(args.Query) != "" {
			query = strings.TrimSpace(args.Query)
		}
	}

	callCtx, cancel := contextWithDefaultTimeout(ctx)
	defer cancel()

	result, err := t.backend.Search(callCtx, query)
	if err != nil {
		if contextErr := contextError(callCtx, err); contextErr != nil {
			return "", contextErr
		}
		return toolErrorResultf(ctx, CodeToolExecutionFailed, "%v", err), nil
	}
	return truncateToolOutput(result), nil
}

// --- DuckDuckGo backend ---

type duckduckgoBackend struct {
	inner *duckduckgo.Tool
}

func (b *duckduckgoBackend) Search(ctx context.Context, query string) (string, error) {
	return b.inner.Call(ctx, query)
}

// --- Tavily backend ---

type tavilyBackend struct {
	apiKey string
	client http.Client
}

func (b *tavilyBackend) Search(ctx context.Context, query string) (string, error) {
	reqBody, _ := json.Marshal(map[string]interface{}{
		"api_key":             b.apiKey,
		"query":               query,
		"max_results":         defaultWebSearchMaxResults,
		"include_answer":      true,
		"include_raw_content": false,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.tavily.com/search", bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tavily API returned %d: %s", resp.StatusCode, string(body))
	}

	var result tavilyResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse tavily response: %w", err)
	}

	return result.Format(), nil
}

type tavilyResponse struct {
	Answer  string         `json:"answer"`
	Results []tavilyResult `json:"results"`
}

type tavilyResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

func (r *tavilyResponse) Format() string {
	var sb strings.Builder
	if r.Answer != "" {
		sb.WriteString("Answer: ")
		sb.WriteString(r.Answer)
		sb.WriteString("\n\n")
	}
	for i, res := range r.Results {
		sb.WriteString(fmt.Sprintf("[%d] %s\n%s\n%s\n\n", i+1, res.Title, res.URL, res.Content))
	}
	return strings.TrimSpace(sb.String())
}

// --- Brave Search backend ---

type braveBackend struct {
	apiKey   string
	client   http.Client
	endpoint string
}

func (b *braveBackend) Search(ctx context.Context, query string) (string, error) {
	endpoint := b.endpoint
	if endpoint == "" {
		endpoint = braveSearchEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}

	params := req.URL.Query()
	params.Set("q", query)
	params.Set("count", fmt.Sprintf("%d", defaultWebSearchMaxResults))
	req.URL.RawQuery = params.Encode()
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", b.apiKey)

	resp, err := b.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("brave search API returned %d: %s", resp.StatusCode, string(body))
	}

	var result braveSearchResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse brave search response: %w", err)
	}
	return result.Format(), nil
}

type braveSearchResponse struct {
	Web struct {
		Results []braveSearchResult `json:"results"`
	} `json:"web"`
}

type braveSearchResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
}

func (r *braveSearchResponse) Format() string {
	var sb strings.Builder
	position := 1
	for _, res := range r.Web.Results {
		title := strings.TrimSpace(res.Title)
		pageURL := strings.TrimSpace(res.URL)
		description := strings.TrimSpace(res.Description)
		if title == "" && pageURL == "" && description == "" {
			continue
		}
		sb.WriteString(fmt.Sprintf("[%d] %s\n%s\n%s\n\n", position, title, pageURL, description))
		position++
	}
	return strings.TrimSpace(sb.String())
}

// --- Wikipedia ---

type WikipediaTool struct {
	inner wikipedia.Tool
}

func NewWikipediaTool(proxy ProxyConfig) *WikipediaTool {
	httpClient := newProxyHTTPClient(proxy)
	httpClient.Timeout = defaultWebToolTimeout
	return &WikipediaTool{
		inner: wikipedia.New(
			defaultUserAgent,
			wikipedia.WithHTTPClient(httpClient),
		),
	}
}

func (t *WikipediaTool) Name() string { return "wikipedia" }

func (t *WikipediaTool) Description() string {
	return `Search Wikipedia for factual information about people, places, companies, ` +
		`historical events, or other subjects.`
}

func (t *WikipediaTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"query": stringArgSchema("Wikipedia search query."),
	}, "query")
}

func (t *WikipediaTool) Call(ctx context.Context, input string) (string, error) {
	query := strings.TrimSpace(input)
	if query == "" {
		return toolErrorResultString(ctx, CodeInvalidArguments, "query is required"), nil
	}

	if strings.HasPrefix(query, "{") {
		var args struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal([]byte(query), &args); err == nil && strings.TrimSpace(args.Query) != "" {
			query = strings.TrimSpace(args.Query)
		}
	}

	callCtx, cancel := contextWithDefaultTimeout(ctx)
	defer cancel()

	result, err := t.inner.Call(callCtx, query)
	if err != nil {
		if contextErr := contextError(callCtx, err); contextErr != nil {
			return "", contextErr
		}
		return toolErrorResultf(ctx, CodeToolExecutionFailed, "%v", err), nil
	}
	return truncateToolOutput(result), nil
}

// --- Web Scraper ---

type WebScraperTool struct {
	proxy ProxyConfig
}

func NewWebScraperTool(proxy ProxyConfig) *WebScraperTool {
	return &WebScraperTool{proxy: proxy}
}

func (t *WebScraperTool) Name() string { return "web_scraper" }

func (t *WebScraperTool) Description() string {
	return `Fetch and extract the text content of a web page. ` +
		`Returns page title, headers, paragraphs, and links. ` +
		`Use when you need the full content of a specific page rather than search snippets.`
}

func (t *WebScraperTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"url": stringArgSchema("HTTP or HTTPS URL to fetch."),
	}, "url")
}

func (t *WebScraperTool) Call(ctx context.Context, input string) (string, error) {
	rawURL := strings.TrimSpace(input)
	if rawURL == "" {
		return toolErrorResultString(ctx, CodeInvalidArguments, "url is required"), nil
	}

	if strings.HasPrefix(rawURL, "{") {
		var args struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal([]byte(rawURL), &args); err == nil && strings.TrimSpace(args.URL) != "" {
			rawURL = strings.TrimSpace(args.URL)
		}
	}

	if err := validateScrapeURL(rawURL); err != nil {
		return toolErrorResultf(ctx, CodeInvalidArguments, "%v", err), nil
	}

	callCtx, cancel := contextWithDefaultTimeout(ctx)
	defer cancel()

	result, err := t.scrape(callCtx, rawURL)
	if err != nil {
		if contextErr := contextError(callCtx, err); contextErr != nil {
			return "", contextErr
		}
		return toolErrorResultf(ctx, CodeToolExecutionFailed, "%v", err), nil
	}
	return truncateToolOutput(result), nil
}

func (t *WebScraperTool) scrape(ctx context.Context, targetURL string) (string, error) {
	c := colly.NewCollector(
		colly.MaxDepth(1),
		colly.Async(false),
	)
	c.WithTransport(newScrapeTransport(t.proxy))

	if err := c.Limit(&colly.LimitRule{
		DomainGlob:  "*",
		Parallelism: 2,
		Delay:       webScraperRequestDelay,
	}); err != nil {
		return "", err
	}

	var siteData strings.Builder
	scrapedLinks := make(map[string]bool)
	scrapedLinksMutex := sync.RWMutex{}
	pageCount := 0
	pageCountMutex := sync.Mutex{}
	var requestErr error
	const maxPages = 1

	blacklist := []string{"login", "signup", "signin", "register", "logout", "download", "redirect"}

	c.OnRequest(func(r *colly.Request) {
		if ctx.Err() != nil {
			r.Abort()
			return
		}
		if err := validateScrapeURL(r.URL.String()); err != nil {
			requestErr = err
			r.Abort()
			return
		}
		pageCountMutex.Lock()
		if pageCount >= maxPages {
			r.Abort()
			pageCountMutex.Unlock()
			return
		}
		pageCount++
		pageCountMutex.Unlock()
	})

	c.OnHTML("html", func(e *colly.HTMLElement) {
		currentURL := e.Request.URL.String()

		scrapedLinksMutex.Lock()
		if scrapedLinks[currentURL] {
			scrapedLinksMutex.Unlock()
			return
		}
		scrapedLinks[currentURL] = true
		scrapedLinksMutex.Unlock()

		siteData.WriteString("\n\nPage URL: ")
		siteData.WriteString(currentURL)

		if title := e.ChildText("title"); title != "" {
			siteData.WriteString("\nPage Title: ")
			siteData.WriteString(title)
		}
		if desc := e.ChildAttr("meta[name=description]", "content"); desc != "" {
			siteData.WriteString("\nPage Description: ")
			siteData.WriteString(desc)
		}

		siteData.WriteString("\nHeaders:")
		e.ForEach("h1, h2, h3, h4, h5, h6", func(_ int, el *colly.HTMLElement) {
			siteData.WriteString("\n")
			siteData.WriteString(el.Text)
		})

		siteData.WriteString("\nContent:")
		e.ForEach("p", func(_ int, el *colly.HTMLElement) {
			siteData.WriteString("\n")
			siteData.WriteString(el.Text)
		})

		if currentURL == targetURL {
			e.ForEach("a", func(_ int, el *colly.HTMLElement) {
				link := el.Attr("href")
				if link != "" {
					siteData.WriteString("\nLink: ")
					siteData.WriteString(link)
				}
			})
		}
	})

	c.OnHTML("a[href]", func(e *colly.HTMLElement) {
		link := e.Request.AbsoluteURL(e.Attr("href"))
		u, err := url.Parse(link)
		if err != nil || u.Hostname() != e.Request.URL.Hostname() {
			return
		}
		for _, item := range blacklist {
			if strings.Contains(u.Path, item) {
				return
			}
		}
		if u.Path == "/index.html" || u.Path == "" {
			u.Path = "/"
		}
		scrapedLinksMutex.RLock()
		visited := scrapedLinks[u.String()]
		scrapedLinksMutex.RUnlock()
		if !visited {
			_ = c.Visit(u.String())
		}
	})

	if err := c.Visit(targetURL); err != nil {
		if requestErr != nil {
			return "", requestErr
		}
		return "", err
	}

	done := make(chan struct{})
	go func() {
		c.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-done:
	}
	if requestErr != nil {
		return "", requestErr
	}

	return siteData.String(), nil
}

func validateScrapeURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid url: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid url scheme: only http/https allowed")
	}
	_, err = lookupAllowedScrapeHostIPs(u.Hostname())
	return err
}

func newScrapeTransport(proxy ProxyConfig) http.RoundTripper {
	transport := newProxyTransport(proxy)
	httpTransport, ok := transport.(*http.Transport)
	if !ok {
		return transport
	}

	baseDialContext := httpTransport.DialContext
	if baseDialContext == nil {
		baseDialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	httpTransport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialAllowedScrapeAddress(ctx, baseDialContext, network, address)
	}
	return httpTransport
}

func dialAllowedScrapeAddress(ctx context.Context, dialContext func(context.Context, string, string) (net.Conn, error), network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addrs, err := lookupAllowedScrapeHostIPs(host)
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

func lookupAllowedScrapeHostIPs(host string) ([]net.IP, error) {
	host = strings.ToLower(strings.TrimSpace(strings.Trim(host, "[]")))
	if host == "" {
		return nil, fmt.Errorf("invalid url: empty host")
	}
	if host == "localhost" {
		return nil, fmt.Errorf("url host is not allowed")
	}
	if ip := net.ParseIP(host); ip != nil {
		if scrapeIPIsDisallowed(ip) {
			return nil, fmt.Errorf("url host is not allowed")
		}
		return []net.IP{ip}, nil
	}
	addrs, err := lookupScrapeHostIPs(host)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("resolve url host: %w", err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve url host: no addresses")
	}
	for _, ip := range addrs {
		if scrapeIPIsDisallowed(ip) {
			return nil, fmt.Errorf("url host resolves to a private or link-local address")
		}
	}
	return addrs, nil
}

func scrapeIPIsDisallowed(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

func contextWithDefaultTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, defaultWebToolTimeout)
}

func truncateToolOutput(output string) string {
	if len(output) <= maxWebToolOutputBytes {
		return output
	}

	cut := maxWebToolOutputBytes
	for cut > 0 && !utf8.RuneStart(output[cut]) {
		cut--
	}
	if cut == 0 {
		cut = maxWebToolOutputBytes
	}
	return output[:cut] + fmt.Sprintf("\n\n[output truncated to %d bytes]", cut)
}
