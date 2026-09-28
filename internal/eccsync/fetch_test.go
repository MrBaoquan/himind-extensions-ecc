package eccsync

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 缓存命中的那一次 fetch 一次网络都不该发。
//
// 定时同步每天走的正是这条路：上游动了、但上次同步的树还在缓存里。原先这条路
// 也要先打两跳网络才能算出缓存目录名，于是一次几十秒的 TLS 抖动就能把整条
// 定时任务带走——而结论本来就在本地。
func TestFetchReusesCachedTreeForGivenCommitWithoutNetwork(t *testing.T) {
	repoRoot := t.TempDir()
	commit := "d3b8a3e908904e242ed2dbe66af62cca71131419"
	writeFetchFixture(t, repoRoot, commit)

	stub := withStubHTTPClient(t, func(request *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("缓存命中时不该发请求，却打了 %s", request.URL)
	})
	result, err := Fetch(repoRoot, FetchOptions{Commit: commit})
	if err != nil {
		t.Fatalf("缓存命中应当直接复用：%v", err)
	}
	if !result.Reused {
		t.Fatalf("应当报告复用缓存：%+v", result)
	}
	if result.Commit != commit {
		t.Fatalf("提交应当是调用方给的那个，得到 %q", result.Commit)
	}
	if seen := stub.seen(); len(seen) != 0 {
		t.Fatalf("缓存命中时不该发请求，实际打了 %v", seen)
	}
}

// 没给提交时，缓存命中只该多打一次 HEAD，不再顺带读 package.json。
//
// 那一跳 raw.githubusercontent.com 是本机最容易抖的出口；版本与提交时间
// 缓存树里都有，没必要为了它们再暴露一次失败面。
func TestFetchCacheHitOnlyAsksForHeadOnce(t *testing.T) {
	repoRoot := t.TempDir()
	commit := "d3b8a3e908904e242ed2dbe66af62cca71131419"
	writeFetchFixture(t, repoRoot, commit)

	stub := withStubHTTPClient(t, func(request *http.Request) (*http.Response, error) {
		if strings.HasSuffix(request.URL.Path, "/commits/HEAD") {
			return stubJSONResponse(request, http.StatusOK, fmt.Sprintf(
				`{"sha":%q,"commit":{"committer":{"date":"2026-09-28T00:38:49Z"}}}`, commit)), nil
		}
		return nil, fmt.Errorf("缓存命中时只该问一次 HEAD，却打了 %s", request.URL)
	})
	result, err := Fetch(repoRoot, FetchOptions{})
	if err != nil {
		t.Fatalf("缓存命中应当直接复用：%v", err)
	}
	if !result.Reused || result.Commit != commit {
		t.Fatalf("应当复用缓存树并回报上游提交：%+v", result)
	}
	seen := stub.seen()
	if len(seen) != 1 {
		t.Fatalf("缓存命中时应当恰好问一次 HEAD，实际打了 %v", seen)
	}
	if seen[0] != "https://api.github.com/repos/affaan-m/ECC/commits/HEAD" {
		t.Fatalf("唯一那次请求应当是 HEAD 查询，得到 %q", seen[0])
	}
}

// 缓存里的树缺 package.json 时不能就地返回一棵 generate 读不出版本的树，
// 而要落到联网补齐——这里用「出口全断」反证它确实试图补齐。
func TestFetchDoesNotReuseTreeWithoutVersion(t *testing.T) {
	repoRoot := t.TempDir()
	commit := "d3b8a3e908904e242ed2dbe66af62cca71131419"
	writeFetchPolicy(t, repoRoot)
	tree := sourceDir(repoRoot, commit)
	if err := os.MkdirAll(filepath.Join(tree, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}

	stub := withStubHTTPClient(t, func(request *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("出口不可用")
	})
	defer silenceBackoff()()
	if _, err := Fetch(repoRoot, FetchOptions{Commit: commit}); err == nil {
		t.Fatal("读不出版本的缓存树不该被当成可用缓存")
	}
	if seen := stub.seen(); len(seen) == 0 {
		t.Fatal("缺版本时应当联网补齐，而不是就地返回")
	}
}

// probe 刚问过 HEAD，紧随其后的 fetch 不该再问一遍：同一轮同步里的这两步
// 问的是同一个问题。
func TestFetchReusesProbeRecordWithoutNetwork(t *testing.T) {
	repoRoot := t.TempDir()
	commit := "d3b8a3e908904e242ed2dbe66af62cca71131419"
	writeFetchFixture(t, repoRoot, commit)
	if err := writeProbeFacts(repoRoot, LockUpstream{Commit: commit, CommitDate: "2026-09-28T00:38:49Z"}); err != nil {
		t.Fatal(err)
	}

	stub := withStubHTTPClient(t, func(request *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("同一轮里 probe 已经问过 HEAD，不该再打 %s", request.URL)
	})
	result, err := Fetch(repoRoot, FetchOptions{})
	if err != nil {
		t.Fatalf("有 probe 结论且缓存命中时应当零网络：%v", err)
	}
	if !result.Reused || result.Commit != commit {
		t.Fatalf("应当复用缓存树并回报 probe 的提交：%+v", result)
	}
	if seen := stub.seen(); len(seen) != 0 {
		t.Fatalf("不该发任何请求，实际打了 %v", seen)
	}
}

// 结论过期（比如昨天跑过 probe、今天单独调 fetch）就必须重新问上游，
// 否则会把旧提交当成当前 HEAD。
func TestFetchIgnoresStaleProbeRecord(t *testing.T) {
	repoRoot := t.TempDir()
	commit := "d3b8a3e908904e242ed2dbe66af62cca71131419"
	writeFetchFixture(t, repoRoot, commit)
	writeStaleProbeRecord(t, repoRoot, commit)

	stub := withStubHTTPClient(t, func(request *http.Request) (*http.Response, error) {
		if strings.HasSuffix(request.URL.Path, "/commits/HEAD") {
			return stubJSONResponse(request, http.StatusOK, fmt.Sprintf(
				`{"sha":%q,"commit":{"committer":{"date":"2026-09-28T00:38:49Z"}}}`, commit)), nil
		}
		return nil, fmt.Errorf("树在本地，不该再读 package.json：%s", request.URL)
	})
	if _, err := Fetch(repoRoot, FetchOptions{}); err != nil {
		t.Fatalf("陈旧结论应当退回联网，而不是失败：%v", err)
	}
	seen := stub.seen()
	if len(seen) != 1 {
		t.Fatalf("陈旧结论下应当只重新问一次 HEAD，实际打了 %v", seen)
	}
}

// 短 sha 或者缺时间戳的结论记下来没有意义，写的时候就不该落盘。
func TestWriteProbeFactsRejectsShortCommit(t *testing.T) {
	repoRoot := t.TempDir()
	if err := writeProbeFacts(repoRoot, LockUpstream{Commit: "d3b8a3e"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := readProbeFacts(repoRoot); ok {
		t.Fatal("短 sha 不该被当成可用的 probe 结论")
	}
}

func writeStaleProbeRecord(t *testing.T, repoRoot, commit string) {
	t.Helper()
	payload, err := json.MarshalIndent(probeRecord{
		Facts:      LockUpstream{Commit: commit, CommitDate: "2026-09-27T00:00:00Z"},
		RecordedAt: time.Now().UTC().Add(-2 * probeFactsTTL).Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(repoRoot, filepath.FromSlash(CacheDir))
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, ProbeFactsFile), payload, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeFetchFixture 造一个最小的同步仓库：策略文件 + 一棵带事实文件的缓存树。
func writeFetchFixture(t *testing.T, repoRoot, commit string) {
	t.Helper()
	writeFetchPolicy(t, repoRoot)

	tree := sourceDir(repoRoot, commit)
	if err := os.MkdirAll(filepath.Join(tree, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "package.json"), []byte(`{"version":"2.2.2"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeSourceFacts(tree, LockUpstream{
		Version:       "2.2.2",
		Commit:        commit,
		CommitDate:    "2026-09-28T00:38:49Z",
		License:       "MIT",
		LicenseHolder: "Affaan Mustafa",
	}); err != nil {
		t.Fatal(err)
	}
}

// writeFetchPolicy 在临时仓库里放一份能通过校验的策略文件。
func writeFetchPolicy(t *testing.T, repoRoot string) {
	t.Helper()
	policy := testPolicy()
	policy.SchemaVersion = PolicyVersion
	policy.FilePolicy = FilePolicy{MaxFilesPerSkill: 40, MaxFileBytes: 1 << 20, TextExtensions: []string{".md"}}
	policy.ModuleCategories = map[string]string{}
	policy.ExcludedSkills = map[string]string{}
	payload, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, PolicyFile), payload, 0o644); err != nil {
		t.Fatal(err)
	}
}

// stubTransport 是一次性的假出口：拦下请求并记账。
type stubTransport struct {
	mutex    sync.Mutex
	requests []string
	respond  func(*http.Request) (*http.Response, error)
}

func (s *stubTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	s.mutex.Lock()
	s.requests = append(s.requests, request.URL.String())
	s.mutex.Unlock()
	return s.respond(request)
}

func (s *stubTransport) seen() []string {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return append([]string(nil), s.requests...)
}

// withStubHTTPClient 把包的出口换成假出口，测试结束自动还原。
func withStubHTTPClient(t *testing.T, respond func(*http.Request) (*http.Response, error)) *stubTransport {
	t.Helper()
	stub := &stubTransport{respond: respond}
	previous := httpClient
	httpClient = &http.Client{Transport: stub, Timeout: previous.Timeout}
	t.Cleanup(func() { httpClient = previous })
	return stub
}

func stubJSONResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
		Request:    request,
	}
}
