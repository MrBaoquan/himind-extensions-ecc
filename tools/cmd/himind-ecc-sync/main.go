package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
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
	tool := flags.String("tool", "himind-ecc-sync/0.1.0", "写入锁文件的工具标识")
	dryRun := flags.Bool("dry-run", false, "只报告，不发布")
	limit := flags.Int("limit", 0, "单次最多发布几个技能，0 表示不限制")
	allowDerived := flags.Bool("allow-derived", false, "连元数据尚未人工确认的技能一起发布")
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
		value, err := eccsync.Fetch(absolute, eccsync.FetchOptions{})
		if err != nil {
			fail(err)
		}
		result = value
	case "generate":
		value, err := generate(absolute, *sourceRoot, *tool)
		if err != nil {
			fail(err)
		}
		result = value
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
	case "publish":
		value, err := eccsync.Publish(absolute, eccsync.PublishOptions{DryRun: *dryRun, Limit: *limit, AllowDerived: *allowDerived})
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

func generate(repoRoot, sourceRoot, tool string) (eccsync.GenerateResult, error) {
	policy, err := eccsync.LoadPolicy(filepath.Join(repoRoot, eccsync.PolicyFile))
	if err != nil {
		return eccsync.GenerateResult{}, err
	}
	eccsync.SetSkillIDPrefix(policy.SkillIDPrefix)
	if sourceRoot == "" {
		sourceRoot, err = eccsync.CachedSource(repoRoot)
		if err != nil {
			return eccsync.GenerateResult{}, err
		}
	}
	upstream, err := eccsync.UpstreamFacts(policy, "")
	if err != nil {
		return eccsync.GenerateResult{}, err
	}
	return eccsync.Generate(eccsync.GenerateInput{
		RepoRoot:   repoRoot,
		SourceRoot: sourceRoot,
		Policy:     policy,
		Upstream:   upstream,
		Now:        time.Now(),
		Tool:       tool,
	})
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
	fmt.Fprintln(os.Stderr, "用法: himind-ecc-sync <probe|fetch|generate|gate|publish> [选项]")
}
