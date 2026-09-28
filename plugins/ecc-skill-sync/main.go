package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MrBaoquan/himind-extensions-ecc/internal/eccsync"
	"github.com/MrBaoquan/himind-extensions/sdk/jsonrpc"
)

// defaultTool 是写进锁文件的工具标识；能力与命令行共享同一份同步事实。
const defaultTool = "himind-ecc-sync/0.1.0"

// input 是五条能力共用的入参集合。
//
// 这里写成超集没关系，Agent 侧只回填 capability 的 input_schema 声明过的字段，
// 而 schema 那边是一个不落的子集。
type input struct {
	RepoRoot       string `json:"repo_root"`
	WorkspaceRoot  string `json:"workspace_root"`
	SourceRoot     string `json:"source_root"`
	Commit         string `json:"commit"`
	RepoURL        string `json:"repo_url"`
	Tool           string `json:"tool"`
	DryRun         bool   `json:"dry_run"`
	Limit          int    `json:"limit"`
	AllowDerived   bool   `json:"allow_derived"`
	Repository     string `json:"repository"`
	Channel        string `json:"channel"`
	PrivateKeyPath string `json:"private_key_path"`
	KeyID          string `json:"key_id"`
	DistDir        string `json:"dist_dir"`
	// TimeoutSeconds 由工作流或调用方传入，Agent 侧据此决定这次调用的等待上限。
	// 插件自己不拿它做取消：能力是同步执行的，能不能等由调用方负责。
	TimeoutSeconds int `json:"timeout_seconds"`
}

func main() {
	if err := jsonrpc.Serve(os.Stdin, os.Stdout, handle); err != nil {
		fmt.Fprintln(os.Stderr, "ecc skill sync stopped:", err)
	}
}

func handle(request jsonrpc.Request) (any, *jsonrpc.Error) {
	var in input
	if rpcError := jsonrpc.DecodeParams(request, &in); rpcError != nil {
		return nil, rpcError
	}
	switch request.Method {
	case "ecc.sync.probe":
		repoRoot, rpcError := resolveRepo(in)
		if rpcError != nil {
			return nil, rpcError
		}
		value, err := eccsync.Probe(repoRoot)
		if err != nil {
			return nil, jsonrpc.InternalError(err.Error())
		}
		return map[string]any{"ok": true, "probe": value}, nil
	case "ecc.sync.fetch":
		repoRoot, rpcError := resolveRepo(in)
		if rpcError != nil {
			return nil, rpcError
		}
		value, err := eccsync.Fetch(repoRoot, eccsync.FetchOptions{
			Commit:  strings.TrimSpace(in.Commit),
			RepoURL: strings.TrimSpace(in.RepoURL),
		})
		if err != nil {
			return nil, jsonrpc.InternalError(err.Error())
		}
		return map[string]any{"ok": true, "fetch": value}, nil
	case "ecc.sync.generate":
		return generate(in)
	case "ecc.sync.gate":
		repoRoot, rpcError := resolveRepo(in)
		if rpcError != nil {
			return nil, rpcError
		}
		value, err := eccsync.Gate(repoRoot)
		if err != nil {
			return nil, jsonrpc.InternalError(err.Error())
		}
		return map[string]any{"ok": value.OK, "gate": value}, nil
	case "ecc.sync.publish":
		return publish(in)
	default:
		return nil, jsonrpc.InvalidParams("unsupported ecc sync capability: " + request.Method)
	}
}

// generate 复用 CLI 的组合方式，并在生成后落一份本次运行的报告。
//
// 报告不是给机器看的中间态，而是工作流能校验的产物：它带着本次的上游提交、
// 变了哪些技能、谁被隔离——用户看完这份报告就知道这次同步到底做了什么。
func generate(in input) (any, *jsonrpc.Error) {
	repoRoot, rpcError := resolveRepo(in)
	if rpcError != nil {
		return nil, rpcError
	}
	policy, err := eccsync.LoadPolicy(filepath.Join(repoRoot, eccsync.PolicyFile))
	if err != nil {
		return nil, jsonrpc.InternalError(err.Error())
	}
	eccsync.SetSkillIDPrefix(policy.SkillIDPrefix)

	sourceRoot := strings.TrimSpace(in.SourceRoot)
	if sourceRoot == "" {
		sourceRoot, err = eccsync.CachedSource(repoRoot)
		if err != nil {
			return nil, jsonrpc.InvalidParams("还没有可用的上游源码树，请先跑 ecc.sync.fetch：" + err.Error())
		}
	}
	upstream, err := eccsync.UpstreamFacts(policy, "")
	if err != nil {
		return nil, jsonrpc.InternalError(err.Error())
	}
	tool := strings.TrimSpace(in.Tool)
	if tool == "" {
		tool = defaultTool
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
		return nil, jsonrpc.InternalError(err.Error())
	}

	response := map[string]any{"ok": true, "generate": result}
	report, artifactErr := writeReport(repoRoot, "generate", map[string]any{
		"kind":      "generate",
		"repo_root": repoRoot,
		"upstream":  upstream,
		"result":    result,
	})
	if artifactErr != nil {
		response["artifact_error"] = artifactErr.Error()
		return response, nil
	}
	response["artifacts"] = []any{report}
	return response, nil
}

// publish 把本地已经生成、索引里还没有的版本推到远端。
//
// dry_run 是默认姿势：定时计划先用它确认「今天确实有东西要发」，
// 真要发时再把开关打开，避免无人值守的第一天就发错东西。
func publish(in input) (any, *jsonrpc.Error) {
	repoRoot, rpcError := resolveRepo(in)
	if rpcError != nil {
		return nil, rpcError
	}
	value, err := eccsync.Publish(repoRoot, eccsync.PublishOptions{
		DryRun:         in.DryRun,
		Limit:          in.Limit,
		Repository:     strings.TrimSpace(in.Repository),
		Channel:        strings.TrimSpace(in.Channel),
		PrivateKeyPath: strings.TrimSpace(in.PrivateKeyPath),
		KeyID:          strings.TrimSpace(in.KeyID),
		DistDir:        strings.TrimSpace(in.DistDir),
		AllowDerived:   in.AllowDerived,
	})
	if err != nil {
		return nil, jsonrpc.InternalError(err.Error())
	}
	return map[string]any{"ok": value.Failed == 0, "publish": value}, nil
}

// resolveRepo 找出这次要操作的仓库根目录。
//
// repo_root 优先，其次才用工作区；两者都必须真的指着同步仓库——
// 只认「目录存在」会让一次手滑把生成物写进业务项目里，
// 到时候 diff 里全是技能目录，很难看出是哪儿错了。
func resolveRepo(in input) (string, *jsonrpc.Error) {
	candidate := strings.TrimSpace(in.RepoRoot)
	if candidate == "" {
		candidate = strings.TrimSpace(in.WorkspaceRoot)
	}
	if candidate == "" {
		return "", jsonrpc.InvalidParams("需要 repo_root：指向 himind-extensions-ecc 仓库根目录（也可以用 workspace_root 指定）")
	}
	absolute, err := filepath.Abs(candidate)
	if err != nil {
		return "", jsonrpc.InvalidParams(err.Error())
	}
	if info, err := os.Stat(filepath.Join(absolute, eccsync.PolicyFile)); err != nil || info.IsDir() {
		return "", jsonrpc.InvalidParams("repo_root 不是 ECC 同步仓库（缺少 " + eccsync.PolicyFile + "）：" + absolute)
	}
	return absolute, nil
}

// writeReport 把一次运行的结论落成 JSON 文件，并返回 Artifact 信封。
//
// 落点选 .cache/ 而不是仓库根部：报告每次都不一样，它不是仓库内容的一部分；
// 写进工作树只会让「上游没变就零动作」这句话变成假的。
func writeReport(repoRoot, kind string, payload any) (map[string]any, error) {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	directory := filepath.Join(repoRoot, filepath.FromSlash(eccsync.CacheDir), "reports")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		directory = os.TempDir()
	}
	path := filepath.Join(directory, time.Now().UTC().Format("20060102T150405Z")+"-"+kind+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(data)
	return map[string]any{
		"artifact_id":   "ecc-sync-report",
		"artifact_type": "ecc_sync_report",
		"name":          "ECC 同步报告",
		"uri":           "file:///" + filepath.ToSlash(absolute),
		"sha256":        hex.EncodeToString(digest[:]),
		"size_bytes":    len(data),
	}, nil
}
