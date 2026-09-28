package eccsync

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// CacheDir 是上游源码与探测结果的本地缓存位置，落在仓库内但被 .gitignore 排除。
const CacheDir = ".cache/ecc-sync"

// ProbeResult 是一次上游探测的结论。
type ProbeResult struct {
	Repository     string `json:"repository"`
	UpstreamCommit string `json:"upstream_commit"`
	CommitDate     string `json:"commit_date,omitempty"`
	LockedCommit   string `json:"locked_commit,omitempty"`
	LockedVersion  string `json:"locked_version,omitempty"`
	// Changed 为 false 时整条链路应当零动作结束。
	Changed      bool   `json:"changed"`
	UpstreamVer  string `json:"upstream_version,omitempty"`
	LockedSkills int    `json:"locked_skills"`
	Reason       string `json:"reason"`
}

// Probe 比较上游当前 HEAD 与本地锁文件记录的提交。
//
// 这是闭环的第一个闸门：上游没动就什么都不做，避免每天跑一次同步
// 就在仓库里制造一批无意义的提交。
func Probe(repoRoot string) (ProbeResult, error) {
	policy, err := LoadPolicy(filepath.Join(repoRoot, PolicyFile))
	if err != nil {
		return ProbeResult{}, err
	}
	lock, err := LoadLock(filepath.Join(repoRoot, LockFile))
	if err != nil {
		return ProbeResult{}, err
	}
	facts, err := UpstreamFacts(policy, "")
	if err != nil {
		return ProbeResult{}, err
	}
	result := ProbeResult{
		Repository:     policy.Upstream.Repository,
		UpstreamCommit: facts.Commit,
		CommitDate:     facts.CommitDate,
		LockedCommit:   lock.Upstream.Commit,
		LockedVersion:  lock.Upstream.Version,
		UpstreamVer:    facts.Version,
		LockedSkills:   len(lock.Skills),
	}
	switch {
	case lock.Upstream.Commit == "":
		result.Changed = true
		result.Reason = "本地还没有同步基线，需要首次全量搬运"
	case lock.Upstream.Commit != facts.Commit:
		result.Changed = true
		result.Reason = fmt.Sprintf("上游提交由 %s 前进到 %s", shortSHA(lock.Upstream.Commit), shortSHA(facts.Commit))
	default:
		result.Reason = "上游提交与本地基线一致，本次无需搬运"
	}
	return result, nil
}

// UpstreamFacts 读取上游当前提交与包版本。
//
// sha 为空时取默认分支 HEAD。这里走 GitHub API，只有公开读取，
// 有 token 就用 token（提高速率上限），没有也能跑。
func UpstreamFacts(policy Policy, sha string) (LockUpstream, error) {
	reference := strings.TrimSpace(sha)
	if reference == "" {
		reference = "HEAD"
	}
	body, err := githubGet(policy, fmt.Sprintf("/repos/%s/commits/%s", policy.Upstream.Repository, reference))
	if err != nil {
		return LockUpstream{}, err
	}
	var document struct {
		SHA    string `json:"sha"`
		Commit struct {
			Committer struct {
				Date string `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return LockUpstream{}, fmt.Errorf("解析上游提交失败: %w", err)
	}
	version := ""
	if raw, err := githubRaw(policy, document.SHA, "package.json"); err == nil {
		var pkg struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(raw, &pkg) == nil {
			version = strings.TrimSpace(pkg.Version)
		}
	}
	return LockUpstream{
		Repository:    policy.Upstream.Repository,
		Package:       policy.Upstream.Package,
		Version:       version,
		Commit:        document.SHA,
		CommitDate:    document.Commit.Committer.Date,
		License:       policy.Upstream.License,
		LicenseHolder: policy.Upstream.Holder,
	}, nil
}

// FetchOptions 控制下载行为。
type FetchOptions struct {
	// Commit 为空时先探测上游 HEAD。
	Commit string
	// RepoURL 覆盖上游地址，便于本地演练与非 GitHub 镜像。
	RepoURL string
}

// FetchResult 是一次源码下载的结论。
type FetchResult struct {
	Commit     string `json:"commit"`
	SourceRoot string `json:"source_root"`
	Reused     bool   `json:"reused"`
	// Transport 记录这次是用什么手段取到的源码：git / tarball。
	Transport string `json:"transport,omitempty"`
	Seconds   int    `json:"seconds"`
}

// Fetch 把上游源码树落到本地缓存。
//
// 缓存按提交 sha 分目录：同一个提交重复跑不会重新下载，也不会互相覆盖。
//
// 取源码走 git 浅拉取而不是源码压缩包：同一个网络里 codeload 经常只有
// 几十 KB/s，一个五十兆的仓库能把超时耗光；git 协议通常十几秒就回来了。
// 两者给出的是同一个提交的同一棵树，换传输手段不影响生成物的确定性。
func Fetch(repoRoot string, options FetchOptions) (FetchResult, error) {
	started := time.Now()
	policy, err := LoadPolicy(filepath.Join(repoRoot, PolicyFile))
	if err != nil {
		return FetchResult{}, err
	}
	facts, err := UpstreamFacts(policy, options.Commit)
	if err != nil {
		return FetchResult{}, err
	}
	target := filepath.Join(repoRoot, filepath.FromSlash(CacheDir), "source", shortSHA(facts.Commit))
	if marker, err := os.Stat(filepath.Join(target, "skills")); err == nil && marker.IsDir() {
		return FetchResult{Commit: facts.Commit, SourceRoot: target, Reused: true, Seconds: int(time.Since(started).Seconds())}, nil
	}
	if err := os.RemoveAll(target); err != nil {
		return FetchResult{}, err
	}
	repository := strings.TrimSpace(options.RepoURL)
	if repository == "" {
		repository = "https://github.com/" + policy.Upstream.Repository + ".git"
	}
	transport := "git"
	if err := fetchWithGit(repoRoot, repository, facts.Commit, target); err != nil {
		if _, lookupErr := exec.LookPath("git"); lookupErr == nil {
			return FetchResult{}, err
		}
		// 机器上没有 git 时才退回压缩包：慢，但至少还能把闭环走完。
		transport = "tarball"
		archive, archiveErr := githubArchive(policy, facts.Commit)
		if archiveErr != nil {
			return FetchResult{}, archiveErr
		}
		if err := extractTarGz(archive, target); err != nil {
			return FetchResult{}, err
		}
	}
	return FetchResult{
		Commit:     facts.Commit,
		SourceRoot: target,
		Transport:  transport,
		Seconds:    int(time.Since(started).Seconds()),
	}, nil
}

// fetchWithGit 用浅拉取把某个提交导成目录。
//
// 缓存里保留一个裸工作区复用：反复同步只增量取包，不从零开始。
// 导出用 `git archive`，这样拿到的是提交里的原始字节，不带工作区状态。
func fetchWithGit(repoRoot, repository, commit, target string) error {
	cache := filepath.Join(repoRoot, filepath.FromSlash(CacheDir), "git")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(cache, "HEAD")); err != nil {
		if _, err := runGit(cache, "init", "-q"); err != nil {
			return err
		}
	}
	current, err := runGit(cache, "remote", "get-url", "origin")
	switch {
	case err == nil && strings.TrimSpace(current) != repository:
		if _, err := runGit(cache, "remote", "set-url", "origin", repository); err != nil {
			return err
		}
	case err != nil:
		if _, err := runGit(cache, "remote", "add", "origin", repository); err != nil {
			return err
		}
	}
	if _, err := runGit(cache, "fetch", "--depth", "1", "--no-tags", "origin", commit); err != nil {
		return fmt.Errorf("浅拉取上游提交 %s 失败: %w", shortSHA(commit), err)
	}
	archive, err := gitArchive(cache, commit)
	if err != nil {
		return err
	}
	return extractGitArchive(archive, target)
}

// runGit 跑一条 git 子命令，把输出合起来返回，便于失败时把原因写清楚。
func runGit(directory string, args ...string) (string, error) {
	command := exec.Command("git", args...)
	command.Dir = directory
	configureHiddenCommand(command)
	// 拉取失败时不要弹出凭据提示，否则无人值守的定时任务会卡在提示上。
	command.Env = append(command.Environ(), "GIT_TERMINAL_PROMPT=0")
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		return output.String(), fmt.Errorf("git %s 失败: %w: %s", strings.Join(args, " "), err, tail(output.String(), 400))
	}
	return output.String(), nil
}

// gitArchive 取某个提交的 tar 字节。
func gitArchive(directory, commit string) ([]byte, error) {
	command := exec.Command("git", "archive", "--format=tar", commit)
	command.Dir = directory
	configureHiddenCommand(command)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("导出上游提交 %s 失败: %w: %s", shortSHA(commit), err, tail(stderr.String(), 400))
	}
	return stdout.Bytes(), nil
}

// CachedSource 返回最近一次解包好的源码树，供 generate 复用。
func CachedSource(repoRoot string) (string, error) {
	root := filepath.Join(repoRoot, filepath.FromSlash(CacheDir), "source")
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("本地没有缓存的上游源码，请先执行 fetch: %w", err)
	}
	latest := ""
	var latestTime time.Time
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(latestTime) {
			latestTime = info.ModTime()
			latest = entry.Name()
		}
	}
	if latest == "" {
		return "", fmt.Errorf("本地没有缓存的上游源码，请先执行 fetch")
	}
	return filepath.Join(root, latest), nil
}

// githubGet 调用 GitHub REST API。
func githubGet(policy Policy, path string) ([]byte, error) {
	request, err := http.NewRequest(http.MethodGet, "https://api.github.com"+path, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "himind-ecc-sync")
	if token := githubToken(); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return doRequest(request, 32<<20)
}

// githubRaw 读取上游某个提交里的单个文件。
func githubRaw(policy Policy, commit, name string) ([]byte, error) {
	url := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s", policy.Upstream.Repository, commit, name)
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "himind-ecc-sync")
	return doRequest(request, 4<<20)
}

// githubArchive 下载上游某个提交的源码压缩包。
func githubArchive(policy Policy, commit string) ([]byte, error) {
	url := fmt.Sprintf("https://codeload.github.com/%s/tar.gz/%s", policy.Upstream.Repository, commit)
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "himind-ecc-sync")
	if token := githubToken(); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return doRequest(request, 512<<20)
}

func doRequest(request *http.Request, limit int64) ([]byte, error) {
	client := &http.Client{Timeout: 10 * time.Minute}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("请求 %s 失败: %w", request.URL.Redacted(), err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("请求 %s 返回 HTTP %d", request.URL.Redacted(), response.StatusCode)
	}
	return io.ReadAll(io.LimitReader(response.Body, limit))
}

// githubToken 按约定顺序找 token。找不到就匿名访问：
// 公开仓库的读操作本来就不需要凭据，缺 token 不该让同步停摆。
func githubToken() string {
	for _, name := range []string{"HIMIND_GITHUB_TOKEN", "GITHUB_TOKEN", "GH_TOKEN"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}
