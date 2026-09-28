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
	// 只问一次「HEAD 是谁」。要不要跟着读 package.json，看提交到底变没变：
	// 提交没变时锁文件里的版本就是上游版本，为了显示它多打一跳 raw
	// 只会让每天那一次「没有变化」也在网络上多暴露一次失败机会。
	head, err := upstreamHead(policy)
	if err != nil {
		return ProbeResult{}, err
	}
	result := ProbeResult{
		Repository:     policy.Upstream.Repository,
		UpstreamCommit: head.Commit,
		CommitDate:     head.CommitDate,
		LockedCommit:   lock.Upstream.Commit,
		LockedVersion:  lock.Upstream.Version,
		UpstreamVer:    lock.Upstream.Version,
		LockedSkills:   len(lock.Skills),
	}
	if head.Commit != lock.Upstream.Commit {
		// 提交变了才值得读一次新版本号；读不到也不影响「有变化」这个结论——
		// 版本真正被用上是在 generate，那时它来自已经落地的源码树。
		result.UpstreamVer = ""
		if version, versionErr := upstreamVersion(policy, head.Commit); versionErr == nil {
			result.UpstreamVer = version
		}
	}
	switch {
	case lock.Upstream.Commit == "":
		result.Changed = true
		result.Reason = "本地还没有同步基线，需要首次全量搬运"
	case lock.Upstream.Commit != head.Commit:
		result.Changed = true
		result.Reason = fmt.Sprintf("上游提交由 %s 前进到 %s", shortSHA(lock.Upstream.Commit), shortSHA(head.Commit))
	default:
		result.Reason = "上游提交与本地基线一致，本次无需搬运"
	}
	// 顺手把刚问到的结论落在缓存目录里：紧随其后的 fetch 是同一轮同步的下一步，
	// 让它接着用，而不是把同一个问题再问一遍上游。
	// 这不是仓库内容（.cache 被忽略），写不进去也只是少省一次查询，
	// 所以这里不因为写不了缓存而让整条同步失败。
	_ = writeProbeFacts(repoRoot, head)
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
	head, err := upstreamCommit(policy, reference)
	if err != nil {
		return LockUpstream{}, err
	}
	return upstreamFactsForCommit(policy, head.Commit, head.CommitDate)
}

// upstreamHead 只查「默认分支 HEAD 是谁」。
//
// 与 UpstreamFacts 的差别：不再顺带读一次 package.json。缓存里已经有这棵树时，
// 版本与许可都在本地，那一跳 raw.githubusercontent.com 恰恰是这条链路上最容易
// 抖的一跳——定时任务每天要走的正是这条「命中缓存」的路。
func upstreamHead(policy Policy) (LockUpstream, error) {
	return upstreamCommit(policy, "HEAD")
}

// upstreamCommit 查一次提交，拿到完整 sha 与提交时间。
func upstreamCommit(policy Policy, reference string) (LockUpstream, error) {
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
	if strings.TrimSpace(document.SHA) == "" {
		return LockUpstream{}, fmt.Errorf("上游 %s 的提交查询没有返回 sha", policy.Upstream.Repository)
	}
	return LockUpstream{
		Repository:    policy.Upstream.Repository,
		Package:       policy.Upstream.Package,
		Commit:        document.SHA,
		CommitDate:    document.Commit.Committer.Date,
		License:       policy.Upstream.License,
		LicenseHolder: policy.Upstream.Holder,
	}, nil
}

// upstreamFactsForCommit 在已知提交的前提下补齐版本。
//
// 版本读不到就直接失败，不要返回一个空版本：空版本一路往下走，最后会变成
// 「无法从上游版本 "" 推出制品版本」这种把真正原因埋掉的报错。
func upstreamFactsForCommit(policy Policy, commit, date string) (LockUpstream, error) {
	version, err := upstreamVersion(policy, commit)
	if err != nil {
		return LockUpstream{}, fmt.Errorf("读取上游 %s 的版本失败: %w", shortSHA(commit), err)
	}
	if version == "" {
		return LockUpstream{}, fmt.Errorf("上游 %s 的 package.json 里没有 version 字段", shortSHA(commit))
	}
	return LockUpstream{
		Repository:    policy.Upstream.Repository,
		Package:       policy.Upstream.Package,
		Version:       version,
		Commit:        commit,
		CommitDate:    date,
		License:       policy.Upstream.License,
		LicenseHolder: policy.Upstream.Holder,
	}, nil
}

// upstreamVersion 读某个提交里 package.json 的版本号。
func upstreamVersion(policy Policy, commit string) (string, error) {
	raw, err := githubRaw(policy, commit, "package.json")
	if err != nil {
		return "", err
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return "", fmt.Errorf("解析 package.json 失败: %w", err)
	}
	return strings.TrimSpace(pkg.Version), nil
}

// ProbeFactsFile 记录最近一次 probe 问到的上游提交，供紧随其后的 fetch 复用。
//
// probe 与 fetch 是一轮同步里相邻的两步，问的是同一个问题（上游 HEAD 是谁）。
// 把结论落在缓存目录（.gitignore 已排除）里，fetch 就不必再问一遍。
const ProbeFactsFile = "last-probe.json"

// probeFactsTTL 是这条本地结论的有效期。
//
// 定时同步里 probe 到 fetch 只隔几秒；但 fetch 也可能被单独调起（人工复现、
// 换个 shell 跑命令行）。过了这个窗口就当结论陈旧，老老实实再问一次上游。
const probeFactsTTL = 30 * time.Minute

// probeRecord 是 last-probe.json 的内容：结论 + 记录时刻。
type probeRecord struct {
	Facts      LockUpstream `json:"facts"`
	RecordedAt string       `json:"recorded_at"`
}

// writeProbeFacts 把一次 probe 的结论记在缓存目录。
//
// 只认完整 sha：短 sha 或者是拼不齐的结论，记下来只会让 fetch 拿到一个
// 不足以定位提交的值。
func writeProbeFacts(repoRoot string, facts LockUpstream) error {
	if len(strings.TrimSpace(facts.Commit)) < 40 {
		return nil
	}
	payload, err := json.MarshalIndent(probeRecord{
		Facts:      facts,
		RecordedAt: time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return err
	}
	directory := filepath.Join(repoRoot, filepath.FromSlash(CacheDir))
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, ProbeFactsFile), append(payload, '\n'), 0o644)
}

// readProbeFacts 读回最近一次 probe 的结论；过期或缺字段就当作没有。
func readProbeFacts(repoRoot string) (LockUpstream, bool) {
	payload, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(CacheDir), ProbeFactsFile))
	if err != nil {
		return LockUpstream{}, false
	}
	var record probeRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		return LockUpstream{}, false
	}
	if len(strings.TrimSpace(record.Facts.Commit)) < 40 {
		return LockUpstream{}, false
	}
	recordedAt, err := time.Parse(time.RFC3339, record.RecordedAt)
	if err != nil || time.Since(recordedAt) > probeFactsTTL {
		return LockUpstream{}, false
	}
	return record.Facts, true
}

// SourceFactsFile 是随上游源码树一起落盘的「本次对齐事实」。
//
// 缓存目录名只是短 sha，光靠它推不回完整提交 sha、提交时间和许可信息，
// 所以 fetch 把当次拿到的事实原样记一份在树根，后续步骤读本地即可。
const SourceFactsFile = ".ecc-upstream-facts.json"

// writeSourceFacts 把这次 fetch 拿到的上游事实记在源码树旁边。
// 缓存树是派生物，多一个点文件不影响 Discover 的扫描结果，也不进版本库。
func writeSourceFacts(sourceRoot string, facts LockUpstream) error {
	if strings.TrimSpace(sourceRoot) == "" || strings.TrimSpace(facts.Commit) == "" {
		return nil
	}
	payload, err := json.MarshalIndent(facts, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(sourceRoot, SourceFactsFile), append(payload, '\n'), 0o644)
}

// SourceFacts 从已解包的上游源码树推出本次对齐的提交事实。
//
// 为什么 generate 不自己再问一次上游 HEAD：
//
//   - 每次同步会多打两次网络请求；api.github.com 或 raw.githubusercontent.com
//     抖一下，整条定时任务就整条失败，而这一步本来完全可以落在本地。
//   - HEAD 可能在 probe 与 generate 之间移动，那样生成的制品根本不是 probe
//     判定过的那份，「同一上游提交生成同一份字节」的确定性就没了。
//
// 所以事实只在 fetch 时取一次、随树落盘，后续步骤读本地。碰到更早版本留下的、
// 还没有事实文件的缓存，退回用目录名 + 树里的 package.json + 锁文件拼出来。
func SourceFacts(policy Policy, repoRoot, sourceRoot string) (LockUpstream, error) {
	if marker, err := os.ReadFile(filepath.Join(sourceRoot, SourceFactsFile)); err == nil {
		var recorded LockUpstream
		if json.Unmarshal(marker, &recorded) == nil &&
			strings.TrimSpace(recorded.Commit) != "" && strings.TrimSpace(recorded.Version) != "" {
			recorded.Repository = policy.Upstream.Repository
			recorded.Package = policy.Upstream.Package
			recorded.License = policy.Upstream.License
			recorded.LicenseHolder = policy.Upstream.Holder
			return recorded, nil
		}
	}

	facts := LockUpstream{
		Repository:    policy.Upstream.Repository,
		Package:       policy.Upstream.Package,
		Commit:        filepath.Base(filepath.Clean(sourceRoot)),
		License:       policy.Upstream.License,
		LicenseHolder: policy.Upstream.Holder,
	}
	// 目录名是短 sha，锁文件里存的是完整 sha：能对上就换成完整的，
	// 免得生成出来的锁文件留一个短 sha，让下一次 probe 永远判定成「有变化」。
	if lock, err := LoadLock(filepath.Join(repoRoot, LockFile)); err == nil {
		if facts.Commit != "" && strings.HasPrefix(lock.Upstream.Commit, facts.Commit) {
			facts.Commit = lock.Upstream.Commit
		}
		facts.CommitDate = lock.Upstream.CommitDate
	}
	if len(facts.Commit) < 40 {
		if full := expandCommit(repoRoot, facts.Commit); full != "" {
			facts.Commit = full
		}
	}
	if payload, err := os.ReadFile(filepath.Join(sourceRoot, "package.json")); err == nil {
		var pkg struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(payload, &pkg) == nil {
			facts.Version = strings.TrimSpace(pkg.Version)
		}
	}
	if strings.TrimSpace(facts.Version) == "" {
		return LockUpstream{}, fmt.Errorf(
			"源码树 %s 里读不到上游版本：既缺 %s，也没有可解析的 package.json；请重新执行一次 ecc.sync.fetch",
			sourceRoot, SourceFactsFile)
	}
	return facts, nil
}

// expandCommit 用本地缓存的裸仓库把短 sha 补成完整 sha。
//
// 只有在读不到事实文件、锁文件也对不上时才需要它；失败就返回空串，
// 由调用方决定是留着短 sha 还是报错——联网补全不在这一步的职责里。
func expandCommit(repoRoot, short string) string {
	if strings.TrimSpace(short) == "" {
		return ""
	}
	cache := filepath.Join(repoRoot, filepath.FromSlash(CacheDir), "git")
	if _, err := os.Stat(filepath.Join(cache, "HEAD")); err != nil {
		return ""
	}
	output, err := runGit(cache, "rev-parse", "--verify", short+"^{commit}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(output)
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
// 顺序是「先本地、后网络」：缓存里已经有这次要的那棵树时，一次请求都不发。
// 定时同步每天走的正是这条路——上游没动时 probe 就拦下了，上游动了也常常
// 只是因为上一次同步的树还在缓存里。原先不论缓存有没有命中都要先打两跳网络
// 才能算出目录名，于是本机出口一抖（几十秒的 TLS 握手超时），明明结论就在
// 本地，整条定时任务还是跟着失败。
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
	commit := strings.TrimSpace(options.Commit)

	// 调用方给定提交时，缓存目录名由短 sha 唯一决定：能读出一棵完整的树
	// 就直接复用，连带把事实文件补齐，供 generate 走纯本地路径。
	if commit != "" {
		if target, ok := cachedSourceTree(repoRoot, commit); ok {
			if facts, factsErr := SourceFacts(policy, repoRoot, target); factsErr == nil {
				if len(commit) >= 40 {
					// 调用方给的是完整 sha，比目录名与锁文件更权威。
					facts.Commit = commit
				}
				if err := writeSourceFacts(target, facts); err != nil {
					return FetchResult{}, err
				}
				return FetchResult{
					Commit:     facts.Commit,
					SourceRoot: target,
					Reused:     true,
					Seconds:    int(time.Since(started).Seconds()),
				}, nil
			}
			// 事实拼不齐（老缓存缺 package.json 之类）：落到下面联网补齐，
			// 而不是就地返回一棵 generate 读不出版本的树。
		}
	}

	facts, err := resolveFetchFacts(policy, repoRoot, commit)
	if err != nil {
		return FetchResult{}, err
	}
	target, cached := cachedSourceTree(repoRoot, facts.Commit)
	if !cached {
		target = sourceDir(repoRoot, facts.Commit)
	}
	if cached {
		// 更早版本留下的缓存没有事实文件，这里补写一次，
		// 让 generate 不用再自己联网问一遍 HEAD。
		if err := writeSourceFacts(target, facts); err != nil {
			return FetchResult{}, err
		}
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
	if err := writeSourceFacts(target, facts); err != nil {
		return FetchResult{}, err
	}
	return FetchResult{
		Commit:     facts.Commit,
		SourceRoot: target,
		Transport:  transport,
		Seconds:    int(time.Since(started).Seconds()),
	}, nil
}

// resolveFetchFacts 定下这次要搬的是哪个提交，并尽量少打网络。
//
// 提交由调用方指定时直接问那个提交；否则先看同一轮 probe 留下的结论，
// 实在没有再问一次 HEAD——树要是在本地，版本与提交时间就都从本地取，
// 连 package.json 那一跳 raw 也省掉。
func resolveFetchFacts(policy Policy, repoRoot, commit string) (LockUpstream, error) {
	if commit != "" {
		return UpstreamFacts(policy, commit)
	}
	head, err := resolveFetchHead(policy, repoRoot)
	if err != nil {
		return LockUpstream{}, err
	}
	if target, ok := cachedSourceTree(repoRoot, head.Commit); ok {
		if facts, factsErr := SourceFacts(policy, repoRoot, target); factsErr == nil {
			// HEAD 的 sha 与提交时间是刚问来的，比缓存里的更新；版本、
			// 许可沿用本地事实，省掉 raw.githubusercontent.com 那一跳。
			facts.Commit = head.Commit
			if date := strings.TrimSpace(head.CommitDate); date != "" {
				facts.CommitDate = date
			}
			return facts, nil
		}
	}
	return upstreamFactsForCommit(policy, head.Commit, head.CommitDate)
}

// resolveFetchHead 找出这次要搬的提交。
//
// 优先用同一轮 probe 刚落下的本地结论：那个结论是这次变更判定的依据，
// 用它同时也更确定——中途 HEAD 再往前走，生成的制品也还是 probe 判定过的那份。
// 结论过期（单独调 fetch）时才真去问上游。
func resolveFetchHead(policy Policy, repoRoot string) (LockUpstream, error) {
	if recorded, ok := readProbeFacts(repoRoot); ok {
		return recorded, nil
	}
	return upstreamHead(policy)
}

// sourceDir 给出某个提交在缓存里对应的源码树目录。
func sourceDir(repoRoot, commit string) string {
	return filepath.Join(repoRoot, filepath.FromSlash(CacheDir), "source", shortSHA(commit))
}

// cachedSourceTree 判断缓存里是否已经有一棵解包完整、可供 generate 读取的源码树。
//
// 认的是树里有没有 skills 目录：生成器整条链路都从 skills 起步，
// 缺了它这棵树等于没用，得重新拉。
func cachedSourceTree(repoRoot, commit string) (string, bool) {
	if strings.TrimSpace(commit) == "" {
		return "", false
	}
	target := sourceDir(repoRoot, commit)
	marker, err := os.Stat(filepath.Join(target, "skills"))
	if err != nil || !marker.IsDir() {
		return "", false
	}
	return target, true
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

// httpClient 复用连接池：一次同步会连着打好几个小请求（查提交、读 package.json、
// 取源码），复用连接比每次重新握手省事，也少给 TLS 抖动留机会。
var httpClient = &http.Client{Timeout: 10 * time.Minute}

// requestAttempts 是一次请求的最多尝试次数。
//
// 5 次而不是 3 次：本机出口到 GitHub 出现过持续几十秒的断流，3 次尝试只覆盖
// 十来秒，一次这样的断流就能把当天的定时同步整条带走。
const requestAttempts = 5

// requestBackoff 是重试间隔，第 n 次重试前等 (n-1)*requestBackoff。
// 真实抖动基本几秒内自愈；连续三次都失败就该把错误交给上层，而不是无限拖。
var requestBackoff = 2 * time.Second

// doRequest 发请求，并对「值得重试」的失败做有界重试。
//
// 本机出口到 api.github.com / raw.githubusercontent.com 是偶发 TLS 握手超时，
// 典型表现是同一轮里 probe 成功、generate 失败，一整天的定时同步就没了。
// 幂等 GET 重发没有副作用，所以重试传输错误、5xx 与 429；4xx（除 429）重试
// 没有意义，直接失败。次数与退避交给 runWithRetry，与 gh 发布共用一套策略。
func doRequest(request *http.Request, limit int64) ([]byte, error) {
	return runWithRetry(requestAttempts, requestBackoff, func() ([]byte, bool, error) {
		return doRequestOnce(request, limit)
	})
}

// doRequestOnce 发一次请求，并说明这次失败值不值得重试。
func doRequestOnce(request *http.Request, limit int64) ([]byte, bool, error) {
	response, err := httpClient.Do(request)
	if err != nil {
		// 带 body 且不可重放的请求不能重发。
		replayable := request.Body == nil || request.GetBody != nil
		return nil, replayable, fmt.Errorf("请求 %s 失败: %w", request.URL.Redacted(), err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		retryable := response.StatusCode >= 500 || response.StatusCode == http.StatusTooManyRequests
		return nil, retryable, fmt.Errorf("请求 %s 返回 HTTP %d", request.URL.Redacted(), response.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, limit))
	if err != nil {
		// 读响应途中断流也当抖动处理。
		return nil, true, fmt.Errorf("读取 %s 的响应失败: %w", request.URL.Redacted(), err)
	}
	return payload, false, nil
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
