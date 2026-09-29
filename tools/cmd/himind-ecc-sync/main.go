package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MrBaoquan/himind-extensions-ecc/internal/eccsync"
)

// 这个命令是同步闭环的手动入口。同一条链路由插件（能力步）和
// Workflow（定时计划）复用，命令行只是它的第三次现身，方便人在本机复现。
func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	command := os.Args[1]
	flags := flag.NewFlagSet(command, flag.ExitOnError)
	repoRoot := flags.String("repo-root", ".", "本仓库根目录")
	sourceRoot := flags.String("source-root", "", "已解包的上游源码树")
	commit := flags.String("commit", "", "指定要搬运的上游提交；给了提交且缓存命中时全程不联网")
	repoURL := flags.String("repo-url", "", "覆盖上游地址，便于本地演练与非 GitHub 镜像")
	tool := flags.String("tool", "himind-ecc-sync/0.1.0", "写入锁文件的工具标识")
	dryRun := flags.Bool("dry-run", false, "只报告，不发布")
	repository := flags.String("repository", "", "覆盖仓库写法，prune 用它的发货地")
	limit := flags.Int("limit", 0, "单次最多发布几个技能，0 表示不限制")
	allowDerived := flags.Bool("allow-derived", false, "连元数据尚未人工确认的技能一起发布")
	format := flags.String("format", "json", "review-queue 的输出格式：json 或 md")
	status := flags.String("status", "", "review-queue 只看某一类状态：derived 或 stale")
	category := flags.String("category", "", "review-queue 只看某个 HiMind 分类")
	module := flags.String("module", "", "dispatch 只看某个上游模块")
	state := flags.String("state", "", "dispatch 只看某种分发状态：distributed、pending、held 或 excluded")
	keyword := flags.String("keyword", "", "dispatch 只按关键字过滤 ID、名称、模块与上游路径")
	excludeCategories := flags.String("exclude-category", "", "dispatch-exclude 要排除的 HiMind 分类，逗号分隔")
	excludeModules := flags.String("exclude-module", "", "dispatch-exclude 要排除的上游模块，逗号分隔")
	excludeSkills := flags.String("exclude-skill", "", "dispatch-exclude 要排除的单条技能 slug，逗号分隔")
	includeCategories := flags.String("include-category", "", "dispatch-include 要重新纳入分发的 HiMind 分类，逗号分隔")
	includeModules := flags.String("include-module", "", "dispatch-include 要重新纳入分发的上游模块，逗号分隔")
	includeSkills := flags.String("include-skill", "", "dispatch-include 要重新纳入分发的单条技能 slug，逗号分隔")
	reason := flags.String("reason", "", "写进策略的排除原因，缺省用兜底文案")
	decisions := flags.String("decisions", "", "review-apply 的校对结论文件")
	if err := flags.Parse(os.Args[2:]); err != nil {
		os.Exit(2)
	}
	absolute, err := filepath.Abs(*repoRoot)
	if err != nil {
		fail(err)
	}
	var result any
	switch command {
	case "probe":
		value, err := eccsync.Probe(absolute)
		if err != nil {
			fail(err)
		}
		result = value
	case "fetch":
		value, err := eccsync.Fetch(absolute, eccsync.FetchOptions{
			Commit:  *commit,
			RepoURL: *repoURL,
		})
		if err != nil {
			fail(err)
		}
		result = value
	case "generate":
		value, upstream, err := generate(absolute, *sourceRoot, *tool)
		if err != nil {
			fail(err)
		}
		result = withReportArtifact(value, eccsync.GenerateReport(absolute, upstream, value))
	case "gate":
		value, err := eccsync.Gate(absolute)
		if err != nil {
			fail(err)
		}
		result = value
		if !value.OK {
			print(result)
			os.Exit(1)
		}
	case "prune":
		value, err := eccsync.PruneReleases(absolute, eccsync.PruneOptions{
			DryRun: *dryRun, Repository: *repository,
		})
		if err != nil {
			fail(err)
		}
		result = value
	case "publish":
		value, err := eccsync.Publish(absolute, eccsync.PublishOptions{DryRun: *dryRun, Limit: *limit, AllowDerived: *allowDerived})
		if err != nil {
			fail(err)
		}
		result = withReportArtifact(value, eccsync.PublishReport(absolute, value))
	case "review-queue":
		value, err := eccsync.BuildReviewQueue(absolute, *sourceRoot)
		if err != nil {
			fail(err)
		}
		queue := value.Filter(*status, *category)
		if *limit > 0 && len(queue.Items) > *limit {
			queue.Items = queue.Items[:*limit]
		}
		if *format == "md" {
			fmt.Print(queue.Markdown())
			return
		}
		result = queue
	case "review-apply":
		path := strings.TrimSpace(*decisions)
		if path == "" {
			fmt.Fprintln(os.Stderr, "review-apply 需要 -decisions <file.json>")
			os.Exit(2)
		}
		entries, err := eccsync.LoadDecisions(path)
		if err != nil {
			fail(err)
		}
		value, err := eccsync.ApplyReviewDecisions(absolute, entries, time.Now())
		if err != nil {
			fail(err)
		}
		result = value
	case "dispatch":
		value, err := eccsync.BuildDistribution(absolute, eccsync.DistributionOptions{
			State: *state, Category: *category, Module: *module, Keyword: *keyword,
		})
		if err != nil {
			fail(err)
		}
		result = value
	case "dispatch-exclude":
		value, err := applyDispatchChange(absolute, dispatchChange{
			Categories: *excludeCategories, Modules: *excludeModules, Skills: *excludeSkills,
			Reason: *reason, Remove: false,
		})
		if err != nil {
			fail(err)
		}
		result = value
	case "dispatch-include":
		value, err := applyDispatchChange(absolute, dispatchChange{
			Categories: *includeCategories, Modules: *includeModules, Skills: *includeSkills,
			Remove: true,
		})
		if err != nil {
			fail(err)
		}
		result = value
	default:
		usage()
		os.Exit(2)
	}
	print(result)
}

// dispatchChange 是一次策略改动的输入。
//
// 收纳进一个结构是因为 CLI 与插件视图改的是同一件事：往三个集合里加条目、
// 或者把条目摘掉。分成两套写法，早晚会出现「界面加得进去、命令行删不掉」。
type dispatchChange struct {
	Categories string
	Modules    string
	Skills     string
	Reason     string
	// Remove 为真表示把这三组键从策略里摘掉，交回默认的「参与分发」。
	Remove bool
}

// applyDispatchChange 读策略、改策略、校验后落盘。
//
// 先校验后写：写错一个模块名不会报错，只会静默地什么都不排除，
// 那种错必须在落盘前拦下来。
func applyDispatchChange(repoRoot string, change dispatchChange) (eccsync.DispatchPolicy, error) {
	path := filepath.Join(repoRoot, eccsync.DispatchPolicyFile)
	policy, err := eccsync.LoadDispatchPolicy(path)
	if err != nil {
		return eccsync.DispatchPolicy{}, err
	}
	editDispatchPolicy(&policy, change)
	policy.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	modules, err := eccsync.LoadModuleMap(filepath.Join(repoRoot, filepath.FromSlash(eccsync.ModulesFile)))
	if err != nil {
		return eccsync.DispatchPolicy{}, err
	}
	lock, err := eccsync.LoadLock(filepath.Join(repoRoot, eccsync.LockFile))
	if err != nil {
		return eccsync.DispatchPolicy{}, err
	}
	if err := eccsync.ValidateDispatchTargets(policy, modules, lock); err != nil {
		return eccsync.DispatchPolicy{}, err
	}
	if err := eccsync.SaveDispatchPolicy(path, policy); err != nil {
		return eccsync.DispatchPolicy{}, err
	}
	return policy, nil
}

// editDispatchPolicy 把一次改动落到三个集合上，空键交给 SaveDispatchPolicy 清理。
func editDispatchPolicy(policy *eccsync.DispatchPolicy, change dispatchChange) {
	if policy.ExcludedModules == nil {
		policy.ExcludedModules = map[string]string{}
	}
	if policy.ExcludedCategories == nil {
		policy.ExcludedCategories = map[string]string{}
	}
	if policy.ExcludedSkills == nil {
		policy.ExcludedSkills = map[string]string{}
	}
	reason := strings.TrimSpace(change.Reason)
	if reason == "" {
		reason = eccsync.DefaultDispatchReason
	}
	applyKeys(policy.ExcludedCategories, splitList(change.Categories), change.Remove, reason)
	applyKeys(policy.ExcludedModules, splitList(change.Modules), change.Remove, reason)
	applyKeys(policy.ExcludedSkills, splitList(change.Skills), change.Remove, reason)
}

func applyKeys(target map[string]string, keys []string, remove bool, reason string) {
	for _, key := range keys {
		if remove {
			delete(target, key)
			continue
		}
		target[key] = reason
	}
}

// splitList 把「逗号分隔」的入参切成键；中英文逗号都认，空项丢掉。
func splitList(value string) []string {
	parts := strings.FieldsFunc(value, func(char rune) bool {
		return char == ',' || char == '，'
	})
	keys := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			keys = append(keys, trimmed)
		}
	}
	return keys
}

// generate 跑一次生成，并把上游锚点一并交回来：报告要写「对齐到哪个上游提交」，
// 这个事实本来就在生成过程中算出来了，没必要让调用方再读一遍标记文件。
func generate(repoRoot, sourceRoot, tool string) (eccsync.GenerateResult, eccsync.LockUpstream, error) {
	policy, err := eccsync.LoadPolicy(filepath.Join(repoRoot, eccsync.PolicyFile))
	if err != nil {
		return eccsync.GenerateResult{}, eccsync.LockUpstream{}, err
	}
	eccsync.SetSkillIDPrefix(policy.SkillIDPrefix)
	if sourceRoot == "" {
		sourceRoot, err = eccsync.CachedSource(repoRoot)
		if err != nil {
			return eccsync.GenerateResult{}, eccsync.LockUpstream{}, err
		}
	}
	// 与插件同一条路径：上游事实取自 fetch 落地的源码树，不再单独问一次 HEAD。
	upstream, err := eccsync.SourceFacts(policy, repoRoot, sourceRoot)
	if err != nil {
		return eccsync.GenerateResult{}, eccsync.LockUpstream{}, err
	}
	result, err := eccsync.Generate(eccsync.GenerateInput{
		RepoRoot:   repoRoot,
		SourceRoot: sourceRoot,
		Policy:     policy,
		Upstream:   upstream,
		Now:        time.Now(),
		Tool:       tool,
	})
	if err != nil {
		return eccsync.GenerateResult{}, eccsync.LockUpstream{}, err
	}
	return result, upstream, nil
}

// withReportArtifact 把报告落盘，再把信封挂到命令输出上。
//
// 报告写失败不能让命令本身变成失败：东西已经生成/发布了，丢的是留痕，不是结果。
// 输出保持原来的顶层字段不变，只多一个 artifacts，别破坏已有的解析习惯。
func withReportArtifact(value any, payload map[string]any) any {
	merged := map[string]any{}
	data, err := json.Marshal(value)
	if err != nil {
		return value
	}
	if err := json.Unmarshal(data, &merged); err != nil {
		return value
	}
	repoRoot, _ := payload["repo_root"].(string)
	kind, _ := payload["kind"].(string)
	artifact, err := eccsync.WriteRunReport(repoRoot, kind, payload)
	if err != nil {
		merged["artifact_error"] = err.Error()
		return merged
	}
	merged["artifacts"] = []any{artifact}
	return merged
}

func print(value any) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fail(err)
	}
	fmt.Println(string(data))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func usage() {
	fmt.Fprintln(os.Stderr, "用法: himind-ecc-sync <probe|fetch|generate|gate|publish|prune|review-queue|review-apply|dispatch|dispatch-exclude|dispatch-include> [选项]")
}
