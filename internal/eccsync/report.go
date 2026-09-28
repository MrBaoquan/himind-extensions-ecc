package eccsync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 报告种类。generate 回答「这次搬进来了什么」，publish 回答
// 「搬进来的这些，哪些真的发出去了、哪些被分发策略挡下」。
const (
	ReportKindGenerate = "generate"
	ReportKindPublish  = "publish"
)

// 报告 Artifact 的稳定标识。三个入口（命令行、插件能力、工作流步骤）
// 引用的是同一类产物，标识必须一致，否则同一份报告会被当成三样东西。
const (
	ReportArtifactID    = "ecc-sync-report"
	ReportArtifactType  = "ecc_sync_report"
	ReportArtifactTitle = "ECC 同步报告"
)

// ReportsDir 是报告的落点：.cache/ecc-sync/reports。
//
// 不写仓库根部是有原因的：报告每次都不一样，它不是仓库内容的一部分；
// 写进工作树只会让「上游没变就零动作」这句话变成假的。
func ReportsDir(repoRoot string) string {
	return filepath.Join(repoRoot, filepath.FromSlash(CacheDir), "reports")
}

// WriteRunReport 把一次运行的结论落成 JSON 文件，并返回 Artifact 信封。
//
// 同一条链路上有三个入口：命令行、插件能力、工作流步骤。任何一次运行没落文件，
// 事后复盘就会缺一块——尤其是定时任务安安静静少发一批的时候。所以写入逻辑
// 只留这一份，谁跑都留痕。
func WriteRunReport(repoRoot, kind string, payload any) (map[string]any, error) {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	directory := ReportsDir(repoRoot)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		// 缓存目录建不出来（只读挂载、权限异常）时退到临时目录，
		// 报告本身还能拿到，比整次运行失败有用。
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
		"artifact_id":   ReportArtifactID,
		"artifact_type": ReportArtifactType,
		"name":          ReportArtifactTitle,
		"uri":           "file:///" + filepath.ToSlash(absolute),
		"sha256":        hex.EncodeToString(digest[:]),
		"size_bytes":    len(data),
	}, nil
}

// GenerateReport 组装一份生成报告。
//
// 上游锚点跟报告一起走：只写「搬了多少条」而不写「对齐到哪个上游提交」，
// 过两周没人说得清这份报告对应的是上游什么状态。
func GenerateReport(repoRoot string, upstream LockUpstream, result GenerateResult) map[string]any {
	return map[string]any{
		"kind":      ReportKindGenerate,
		"repo_root": repoRoot,
		"upstream":  upstream,
		"result":    result,
	}
}

// PublishReport 组装一份发布报告。
//
// 上游锚点取自锁文件，跟着报告一起走：生成报告说「这次对齐到哪个上游提交」，
// 发布报告说「在这个提交上，谁发了、谁被哪条规则挡下」——两份报告对得上，
// 才谈得上回头复盘某一条技能是哪一步开始不更新的。
func PublishReport(repoRoot string, value PublishResult) map[string]any {
	report := map[string]any{
		"kind":      ReportKindPublish,
		"repo_root": repoRoot,
		"publish":   value,
	}
	// 读不到锁文件不该让整次发布变成失败：发布本身已经成功了，
	// 报告里少一块锚点，比丢掉整份报告要好。
	//
	// 但也不能把空锚点当成锚点：LoadLock 在文件缺失时返回的是空锁而不是错误，
	// 原样写进去就成了「upstream: {commit: ""}」——既过不了 schema 的必填校验，
	// 又会在复盘时假装自己知道对齐到哪个上游。
	if lock, err := LoadLock(filepath.Join(repoRoot, LockFile)); err == nil && strings.TrimSpace(lock.Upstream.Commit) != "" {
		report["upstream"] = lock.Upstream
	}
	return report
}
