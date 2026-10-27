package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLongTermMemoryAddWritesMarkdownAndSearchableIndex(t *testing.T) {
	ctx := context.Background()
	store := NewLongTermMemoryStore(filepath.Join(t.TempDir(), "long_term"))

	id, err := store.AddMemory(ctx, MemoryItem{
		ID:               "mem_test_login",
		Type:             "preference",
		Priority:         80,
		Confidence:       0.9,
		Tags:             []string{"登录", "验证码"},
		Entities:         []string{"某政务App"},
		Title:            "某政务 App 验证码登录偏好",
		Content:          "用户下次登录某政务 App 时，应先重新获取验证码。",
		EvidenceExcerpts: []string{"用户说：记一下，这个 App 下次登录要先重新获取验证码。"},
		SourceRefs: []MemorySourceRef{
			{Type: "chunk", ID: "chunk_test", EventIDs: []string{"evt_test"}},
		},
	})
	if err != nil {
		t.Fatalf("AddMemory() error = %v", err)
	}
	if id != "mem_test_login" {
		t.Fatalf("unexpected memory id: %q", id)
	}

	data, err := os.ReadFile(filepath.Join(store.RootDir(), "memories", "mem_test_login.md"))
	if err != nil {
		t.Fatalf("read memory markdown: %v", err)
	}
	markdown := string(data)
	for _, want := range []string{
		"id: mem_test_login",
		"type: preference",
		"status: active",
		"# 某政务 App 验证码登录偏好",
		"## 证据摘录",
		"用户说：记一下",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("expected markdown to contain %q, got:\n%s", want, markdown)
		}
	}

	results, err := store.Search(ctx, MemoryQuery{Tags: []string{"验证码"}, Limit: 5})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 search result, got %d", len(results))
	}
	if results[0].ID != "mem_test_login" || !strings.Contains(results[0].Content, "重新获取验证码") {
		t.Fatalf("unexpected search result: %#v", results[0])
	}
}

func TestLongTermMemorySearchReflectsUpdatedMarkdownAfterCache(t *testing.T) {
	ctx := context.Background()
	store := NewLongTermMemoryStore(filepath.Join(t.TempDir(), "long_term"))
	if _, err := store.AddMemory(ctx, MemoryItem{
		ID:               "mem_cached_update",
		Type:             "fact",
		Priority:         80,
		Confidence:       0.9,
		Tags:             []string{"cache-test"},
		Title:            "Cached update",
		Content:          "old content",
		EvidenceExcerpts: []string{"old evidence"},
	}); err != nil {
		t.Fatalf("AddMemory() error = %v", err)
	}

	if _, err := store.Search(ctx, MemoryQuery{Tags: []string{"cache-test"}, Limit: 5}); err != nil {
		t.Fatalf("initial Search() error = %v", err)
	}
	if err := store.UpdateMemory(ctx, "mem_cached_update", func(item *MemoryItem) {
		item.Content = "new content"
		item.EvidenceExcerpts = []string{"new evidence"}
	}); err != nil {
		t.Fatalf("UpdateMemory() error = %v", err)
	}

	results, err := store.Search(ctx, MemoryQuery{Tags: []string{"cache-test"}, Limit: 5})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 1 || !strings.Contains(results[0].Content, "new content") {
		t.Fatalf("Search() returned stale result: %#v", results)
	}
}

func TestLongTermMemorySearchReadsPlainMarkdownBody(t *testing.T) {
	ctx := context.Background()
	store := NewLongTermMemoryStore(filepath.Join(t.TempDir(), "long_term"))
	memoriesDir := filepath.Join(store.RootDir(), "memories")
	if err := os.MkdirAll(memoriesDir, 0o755); err != nil {
		t.Fatalf("create memories dir: %v", err)
	}
	markdown := `---
id: personamem_campfire_storytelling
type: fact
status: active
priority: 80
confidence: 0.9
tags:
  - family
  - campfire
  - storytelling
time_scope: long_term
created_at: "2026-05-29T00:00:00Z"
updated_at: "2026-05-29T00:00:00Z"
traceability: full
---

# Campfire storytelling discomfort

The user previously said storytelling around the campfire can feel awkward.
`
	if err := os.WriteFile(filepath.Join(memoriesDir, "personamem_campfire_storytelling.md"), []byte(markdown), 0o644); err != nil {
		t.Fatalf("write plain memory markdown: %v", err)
	}

	results, err := store.Search(ctx, MemoryQuery{Tags: []string{"campfire"}, Limit: 1})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 search result, got %d", len(results))
	}
	if !strings.Contains(results[0].Content, "feel awkward") {
		t.Fatalf("expected plain markdown body as content, got %#v", results[0])
	}
	if !strings.Contains(results[0].Summary, "feel awkward") {
		t.Fatalf("expected plain markdown body summary, got %#v", results[0])
	}
}

func TestLongTermMemorySearchRanksTopicalMatchesWhenFiltersAreTooNarrow(t *testing.T) {
	ctx := context.Background()
	store := NewLongTermMemoryStore(filepath.Join(t.TempDir(), "long_term"))

	if _, err := store.AddMemory(ctx, MemoryItem{
		ID:         "personamem_running_background",
		Type:       "profile",
		Priority:   100,
		Confidence: 0.9,
		Tags:       []string{"fitness", "running", "background"},
		Entities:   []string{"user"},
		Title:      "Running background",
		Content:    "The user has a background in running for fitness.",
		EvidenceExcerpts: []string{
			"The user mentioned running as a fitness background.",
		},
	}); err != nil {
		t.Fatalf("AddMemory() running error = %v", err)
	}
	if _, err := store.AddMemory(ctx, MemoryItem{
		ID:         "personamem_soccer_community",
		Type:       "preference",
		Priority:   80,
		Confidence: 0.9,
		Tags:       []string{"sports", "soccer", "community"},
		Title:      "Soccer and community fitness",
		Content:    "The user has positive experience with soccer and values team sports for fitness, camaraderie, teamwork, motivation, and lasting friendships.",
		EvidenceExcerpts: []string{
			"The user values soccer for fitness and community.",
		},
	}); err != nil {
		t.Fatalf("AddMemory() soccer error = %v", err)
	}

	results, err := store.Search(ctx, MemoryQuery{
		Tags:     []string{"fitness", "community"},
		Entities: []string{"user"},
		Types:    []string{"profile", "preference"},
		Limit:    2,
	})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("expected ranked search results")
	}
	if results[0].ID != "personamem_soccer_community" {
		t.Fatalf("expected soccer memory first, got %#v", results)
	}
}

func TestLongTermMemorySearchDoesNotReturnTypeOnlyMatchesForTopicalQuery(t *testing.T) {
	ctx := context.Background()
	store := NewLongTermMemoryStore(filepath.Join(t.TempDir(), "long_term"))

	if _, err := store.AddMemory(ctx, MemoryItem{
		ID:         "personamem_campfire_storytelling",
		Type:       "fact",
		Priority:   80,
		Confidence: 0.9,
		Tags:       []string{"family", "campfire", "storytelling"},
		Title:      "Campfire storytelling discomfort",
		Content:    "The user previously said storytelling around the campfire can feel awkward.",
		EvidenceExcerpts: []string{
			"The user said campfire storytelling can feel awkward.",
		},
	}); err != nil {
		t.Fatalf("AddMemory() error = %v", err)
	}

	results, err := store.Search(ctx, MemoryQuery{
		Tags:     []string{"travel", "activities", "blog"},
		Entities: []string{"travel blogs"},
		Types:    []string{"fact"},
		Limit:    3,
	})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected no unrelated type-only matches, got %#v", results)
	}
}

func TestLongTermMemorySearchTreatsWhitespaceOnlyTagsAsEmptyQuery(t *testing.T) {
	ctx := context.Background()
	store := NewLongTermMemoryStore(filepath.Join(t.TempDir(), "long_term"))

	if _, err := store.AddMemory(ctx, MemoryItem{
		ID:         "mem_general_preference",
		Type:       "preference",
		Priority:   80,
		Confidence: 0.9,
		Tags:       []string{"general"},
		Title:      "General preference",
		Content:    "The user prefers concise replies.",
		EvidenceExcerpts: []string{
			"The user asked for concise replies.",
		},
	}); err != nil {
		t.Fatalf("AddMemory() error = %v", err)
	}

	results, err := store.Search(ctx, MemoryQuery{Tags: []string{" \t"}, Limit: 1})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 1 || results[0].ID != "mem_general_preference" {
		t.Fatalf("expected whitespace-only tags to behave like empty query, got %#v", results)
	}
}

func TestLongTermMemorySearchSkipsCorruptIndexedMarkdown(t *testing.T) {
	ctx := context.Background()
	store := NewLongTermMemoryStore(filepath.Join(t.TempDir(), "long_term"))
	if _, err := store.AddMemory(ctx, MemoryItem{
		ID:         "mem_valid",
		Type:       "preference",
		Priority:   80,
		Confidence: 0.9,
		Tags:       []string{"general"},
		Title:      "Valid memory",
		Content:    "The user prefers concise replies.",
		EvidenceExcerpts: []string{
			"The user asked for concise replies.",
		},
	}); err != nil {
		t.Fatalf("AddMemory() error = %v", err)
	}

	corruptPath := filepath.Join(store.RootDir(), "memories", "mem_corrupt.md")
	if err := os.WriteFile(corruptPath, nil, 0o644); err != nil {
		t.Fatalf("write corrupt memory: %v", err)
	}
	index, err := store.loadIndex(ctx)
	if err != nil {
		t.Fatalf("loadIndex() error = %v", err)
	}
	index.Memories = append(index.Memories, memoryIndexEntry{
		ID:         "mem_corrupt",
		File:       filepath.ToSlash(filepath.Join("memories", "mem_corrupt.md")),
		Type:       "preference",
		Status:     "active",
		Priority:   100,
		Confidence: 1,
		Tags:       []string{"general"},
	})
	if err := store.writeIndex(index); err != nil {
		t.Fatalf("writeIndex() error = %v", err)
	}

	results, err := store.Search(ctx, MemoryQuery{Tags: []string{"general"}, Limit: 5})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 1 || results[0].ID != "mem_valid" {
		t.Fatalf("expected valid memory only, got %#v", results)
	}
}

func TestLongTermMemoryRebuildIndexSkipsCorruptMarkdown(t *testing.T) {
	ctx := context.Background()
	store := NewLongTermMemoryStore(filepath.Join(t.TempDir(), "long_term"))
	if _, err := store.AddMemory(ctx, MemoryItem{
		ID:         "mem_valid",
		Type:       "preference",
		Priority:   80,
		Confidence: 0.9,
		Tags:       []string{"general"},
		Title:      "Valid memory",
		Content:    "The user prefers concise replies.",
		EvidenceExcerpts: []string{
			"The user asked for concise replies.",
		},
	}); err != nil {
		t.Fatalf("AddMemory() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(store.RootDir(), "memories", "mem_corrupt.md"), nil, 0o644); err != nil {
		t.Fatalf("write corrupt memory: %v", err)
	}

	if err := store.RebuildIndex(ctx); err != nil {
		t.Fatalf("RebuildIndex() error = %v", err)
	}
	index, err := store.loadIndex(ctx)
	if err != nil {
		t.Fatalf("loadIndex() error = %v", err)
	}
	if len(index.Memories) != 1 || index.Memories[0].ID != "mem_valid" {
		t.Fatalf("expected valid memory only in rebuilt index, got %#v", index.Memories)
	}
}

func TestLongTermMemoryForgetExcludesMemoryFromSearch(t *testing.T) {
	ctx := context.Background()
	store := NewLongTermMemoryStore(filepath.Join(t.TempDir(), "long_term"))

	if _, err := store.AddMemory(ctx, MemoryItem{
		ID:               "mem_forget_me",
		Type:             "rule",
		Priority:         100,
		Confidence:       0.95,
		Tags:             []string{"支付"},
		Title:            "支付确认规则",
		Content:          "支付前必须确认。",
		EvidenceExcerpts: []string{"用户说：支付前先问我。"},
	}); err != nil {
		t.Fatalf("AddMemory() error = %v", err)
	}

	if err := store.Forget(ctx, "mem_forget_me", "user_request"); err != nil {
		t.Fatalf("Forget() error = %v", err)
	}
	forgottenMarkdown, err := os.ReadFile(filepath.Join(store.RootDir(), "memories", "mem_forget_me.md"))
	if err != nil {
		t.Fatalf("read forgotten markdown: %v", err)
	}
	if !strings.Contains(string(forgottenMarkdown), "用户说：支付前先问我。") {
		t.Fatalf("expected Forget to preserve evidence excerpt, got:\n%s", forgottenMarkdown)
	}
	results, err := store.Search(ctx, MemoryQuery{Tags: []string{"支付"}, Limit: 5})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected deleted memory to be excluded, got %#v", results)
	}

	tombstoneData, err := os.ReadFile(filepath.Join(store.RootDir(), "..", "lifecycle", "tombstones.jsonl"))
	if err != nil {
		t.Fatalf("read tombstones: %v", err)
	}
	if !strings.Contains(string(tombstoneData), "mem_forget_me") || !strings.Contains(string(tombstoneData), "user_request") {
		t.Fatalf("expected tombstone for forgotten memory, got %s", tombstoneData)
	}
}

func TestLongTermMemoryParsedCacheEvictsBeyondCapacity(t *testing.T) {
	ctx := context.Background()
	const cap = 4
	store := NewLongTermMemoryStore(filepath.Join(t.TempDir(), "long_term"), withParsedCacheCapacity(cap))

	const total = cap + 3
	for i := 0; i < total; i++ {
		if _, err := store.AddMemory(ctx, MemoryItem{
			ID:               fmt.Sprintf("mem_cap_%02d", i),
			Type:             "fact",
			Priority:         50,
			Confidence:       0.8,
			Tags:             []string{fmt.Sprintf("tag_%02d", i)},
			Title:            fmt.Sprintf("memory %02d", i),
			Content:          fmt.Sprintf("content body %02d", i),
			EvidenceExcerpts: []string{fmt.Sprintf("evidence %02d", i)},
		}); err != nil {
			t.Fatalf("AddMemory(%d) error = %v", i, err)
		}
	}

	// A full match-all search reads every active memory markdown, which would
	// populate one parsed-cache entry per file without a bound.
	if _, err := store.Search(ctx, MemoryQuery{Limit: total}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	store.cacheMu.Lock()
	got := store.parsedCache.len()
	store.cacheMu.Unlock()
	if got != cap {
		t.Fatalf("parsed cache size = %d, want %d", got, cap)
	}
}

func TestLongTermMemoryParsedCacheDoesNotCacheExpiredEntries(t *testing.T) {
	ctx := context.Background()
	store := NewLongTermMemoryStore(filepath.Join(t.TempDir(), "long_term"), withParsedCacheCapacity(1))

	liveID := "a_live"
	if _, err := store.AddMemory(ctx, MemoryItem{
		ID:               liveID,
		Type:             "fact",
		Priority:         50,
		Confidence:       0.8,
		Tags:             []string{"durable"},
		Title:            "durable memory",
		Content:          "long lived content",
		EvidenceExcerpts: []string{"evidence live"},
	}); err != nil {
		t.Fatalf("AddMemory(live) error = %v", err)
	}

	expiredID := "z_expired"
	if _, err := store.AddMemory(ctx, MemoryItem{
		ID:               expiredID,
		Type:             "fact",
		Priority:         50,
		Confidence:       0.8,
		Tags:             []string{"ephemeral"},
		Title:            "ephemeral memory",
		Content:          "short lived content",
		ExpiresAt:        time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano),
		EvidenceExcerpts: []string{"evidence expired"},
	}); err != nil {
		t.Fatalf("AddMemory() error = %v", err)
	}

	if _, err := store.Search(ctx, MemoryQuery{Limit: 5}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	store.cacheMu.Lock()
	hasLive := store.parsedCache.has(store.memoryPath(liveID))
	hasExpired := store.parsedCache.has(store.memoryPath(expiredID))
	store.cacheMu.Unlock()
	if !hasLive {
		t.Fatal("expected live memory to remain cached")
	}
	if hasExpired {
		t.Fatal("expected expired memory not to be cached")
	}
}

func TestLongTermMemoryIndexUpgradeAddsExpiryMetadata(t *testing.T) {
	ctx := context.Background()
	store := NewLongTermMemoryStore(filepath.Join(t.TempDir(), "long_term"))
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)

	if _, err := store.AddMemory(ctx, MemoryItem{
		ID:               "mem_index_upgrade",
		Type:             "fact",
		Priority:         50,
		Confidence:       0.8,
		Content:          "memory with expiry metadata",
		ExpiresAt:        expiresAt,
		EvidenceExcerpts: []string{"evidence"},
	}); err != nil {
		t.Fatalf("AddMemory() error = %v", err)
	}

	if err := store.writeIndex(memoryIndex{
		Version:   1,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Memories: []memoryIndexEntry{{
			ID:     "mem_index_upgrade",
			File:   filepath.ToSlash(filepath.Join("memories", "mem_index_upgrade.md")),
			Type:   "fact",
			Status: "active",
		}},
	}); err != nil {
		t.Fatalf("writeIndex(v1) error = %v", err)
	}

	index, err := store.loadIndex(ctx)
	if err != nil {
		t.Fatalf("loadIndex() error = %v", err)
	}
	if index.Version != memoryIndexVersion {
		t.Fatalf("index version = %d, want %d", index.Version, memoryIndexVersion)
	}
	if len(index.Memories) != 1 || index.Memories[0].ExpiresAt != expiresAt {
		t.Fatalf("upgraded index missing expiry metadata: %#v", index.Memories)
	}
}

func TestLongTermMemoryProfileSkipsExpiredEntries(t *testing.T) {
	ctx := context.Background()
	profileCalls := 0
	rootDir := filepath.Join(t.TempDir(), "long_term")
	store := NewLongTermMemoryStore(rootDir, WithStoreProfileFn(func(context.Context, []ProfileEntry) string {
		profileCalls++
		return "unexpected"
	}))

	if _, err := store.AddMemory(ctx, MemoryItem{
		ID:               "mem_expired_profile",
		Type:             "profile",
		Priority:         90,
		Confidence:       0.9,
		Content:          "expired profile content",
		ExpiresAt:        time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
		EvidenceExcerpts: []string{"evidence"},
	}); err != nil {
		t.Fatalf("AddMemory() error = %v", err)
	}
	memoryPath := store.memoryPath("mem_expired_profile")
	parsed, err := readMemoryMarkdown(memoryPath)
	if err != nil {
		t.Fatalf("readMemoryMarkdown() error = %v", err)
	}
	parsed.Item.ExpiresAt = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	if err := writeFileAtomic(memoryPath, []byte(formatMemoryMarkdown(parsed.Item)), 0o644); err != nil {
		t.Fatalf("expire memory without rebuilding index: %v", err)
	}
	profilePath := filepath.Join(rootDir, "profile.md")
	if err := os.WriteFile(profilePath, []byte("stale expired profile"), 0o644); err != nil {
		t.Fatalf("write stale profile: %v", err)
	}
	if err := store.RegenerateProfileMD(ctx); err != nil {
		t.Fatalf("RegenerateProfileMD() error = %v", err)
	}
	if profileCalls != 0 {
		t.Fatalf("profile function calls = %d, want 0", profileCalls)
	}
	if _, err := os.Stat(profilePath); !os.IsNotExist(err) {
		t.Fatalf("profile file still exists after all sources expired: %v", err)
	}
}

func TestRegenerateProfileDoesNotHoldStoreLockDuringProfileFn(t *testing.T) {
	ctx := context.Background()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	store := NewLongTermMemoryStore(filepath.Join(t.TempDir(), "long_term"), WithStoreProfileFn(func(context.Context, []ProfileEntry) string {
		once.Do(func() { close(started) })
		<-release
		return "# Profile"
	}))
	if _, err := store.AddMemory(ctx, MemoryItem{
		ID: "mem_profile_lock", Type: "profile", Content: "profile content", EvidenceExcerpts: []string{"evidence"},
	}); err != nil {
		t.Fatal(err)
	}
	regenerated := make(chan error, 1)
	go func() { regenerated <- store.RegenerateProfileMD(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("profile function did not start")
	}
	searched := make(chan error, 1)
	go func() {
		_, err := store.Search(ctx, MemoryQuery{Limit: 1})
		searched <- err
	}()
	select {
	case err := <-searched:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		close(release)
		t.Fatal("Search blocked behind the profile model call")
	}
	close(release)
	if err := <-regenerated; err != nil {
		t.Fatal(err)
	}
}

func TestLongTermMemoryProfilePromptListsProfileEntriesWithinBudget(t *testing.T) {
	ctx := context.Background()
	store := NewLongTermMemoryStore(filepath.Join(t.TempDir(), "long_term"))
	for _, item := range []MemoryItem{
		{ID: "mem_job", Type: "profile", Priority: 90, Confidence: 0.9, Title: "职业", Content: "用户是一名护士，经常上夜班。", EvidenceExcerpts: []string{"我是护士"}},
		{ID: "mem_brevity", Type: "preference", Priority: 80, Confidence: 0.9, Title: "回复长度", Content: "用户希望回复尽量简短。", EvidenceExcerpts: []string{"说短一点"}},
		{ID: "mem_fact", Type: "fact", Priority: 95, Confidence: 0.9, Title: "车牌", Content: "车牌号浙A12345。", EvidenceExcerpts: []string{"车牌"}},
		{ID: "mem_pin", Type: "profile", Priority: 99, Confidence: 0.9, Title: "PIN", Content: "保险箱密码 5093。", Sensitive: true, EvidenceExcerpts: []string{"帮我记住"}},
	} {
		if _, err := store.AddMemory(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	prompt, err := store.ProfilePrompt(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	want := "- (profile) 用户是一名护士，经常上夜班。\n- (preference) 用户希望回复尽量简短。"
	if prompt != want {
		t.Fatalf("ProfilePrompt() = %q, want %q", prompt, want)
	}
	short, err := store.ProfilePrompt(ctx, len("- (profile) 用户是一名护士，经常上夜班。\n"))
	if err != nil {
		t.Fatal(err)
	}
	if short != "- (profile) 用户是一名护士，经常上夜班。" {
		t.Fatalf("ProfilePrompt(short) = %q, want only the highest-priority entry", short)
	}
}
