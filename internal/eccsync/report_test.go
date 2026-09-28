package eccsync

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 报告必须落在缓存目录里，而不是仓库根部。
//
// 定时任务每天都会跑一次，报告一旦写进工作树，「上游没变就零动作」这句话
// 立刻变成假的：每次运行都留下一堆无意义 diff。这条测试把这个位置钉住。
func TestWriteRunReportLandsInCacheDir(t *testing.T) {
	repoRoot := t.TempDir()
	report := map[string]any{"kind": ReportKindPublish, "repo_root": repoRoot}

	artifact, err := WriteRunReport(repoRoot, ReportKindPublish, report)
	if err != nil {
		t.Fatalf("发布报告没落盘：%v", err)
	}
	uri, ok := artifact["uri"].(string)
	if !ok || !strings.HasPrefix(uri, "file:///") {
		t.Fatalf("报告信封应给出本地文件 URI：%+v", artifact)
	}
	path := filepath.FromSlash(strings.TrimPrefix(uri, "file:///"))
	want := filepath.Join(repoRoot, filepath.FromSlash(CacheDir), "reports")
	if !strings.HasPrefix(path, want) {
		t.Fatalf("报告应落在 %s 下，实际是 %s", want, path)
	}
	if !strings.HasSuffix(path, "-publish.json") {
		t.Fatalf("报告文件名应带上种类，便于两份报告并排看：%s", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("按信封找不到报告文件：%v", err)
	}
	// 报告是给人回看的产物，必须能直接被别的工具读，不能被工作树状态顺走。
	if _, err := os.Stat(filepath.Join(repoRoot, ".gitignore")); err == nil {
		ignored, err := os.ReadFile(filepath.Join(repoRoot, ".gitignore"))
		if err == nil && !strings.Contains(string(ignored), ".cache") {
			t.Fatalf("报告落点必须在 .gitignore 里：%s", ignored)
		}
	}
}

// 发布报告要同时说清三件事：发了什么、挡下了谁、挡它的是哪条规则。
//
// 少任何一件，定时任务事后都没法复盘：只报条数只能知道「少了东西」，
// 不知道是分类、模块还是单条技能被排除，也就不知道该找谁改。
func TestPublishReportCarriesExclusionsAndUpstreamAnchor(t *testing.T) {
	repoRoot := t.TempDir()
	if err := SaveLock(filepath.Join(repoRoot, LockFile), Lock{
		SchemaVersion: LockVersion,
		Upstream:      LockUpstream{Repository: "affaan-m/ECC", Commit: "deadbeef", Version: "2.2.3"},
		Skills:        map[string]LockSkill{},
	}); err != nil {
		t.Fatal(err)
	}
	value := PublishResult{
		Repository: "MrBaoquan/himind-extensions-ecc",
		Channel:    "beta",
		DryRun:     true,
		Pending:    1,
		Excluded:   2,
		ExcludedItems: []ExcludedItem{{
			Slug: "seo", ID: "com.mrbaoquan.ecc.skill.seo", Version: "1.0.1",
			Rule: DispatchRuleSkill, Keyword: "seo", Reason: "团队不用",
		}},
		Items: []PublishItem{},
	}

	artifact, err := WriteRunReport(repoRoot, ReportKindPublish, PublishReport(repoRoot, value))
	if err != nil {
		t.Fatalf("发布报告没落盘：%v", err)
	}
	data, err := os.ReadFile(filepath.FromSlash(strings.TrimPrefix(artifact["uri"].(string), "file:///")))
	if err != nil {
		t.Fatal(err)
	}
	var written map[string]any
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatalf("报告不是合法 JSON：%v", err)
	}
	if written["kind"] != ReportKindPublish {
		t.Fatalf("报告应标明这是发布报告：%+v", written["kind"])
	}
	publishValue, ok := written["publish"].(map[string]any)
	if !ok {
		t.Fatalf("报告应带上发布结论：%+v", written["publish"])
	}
	if publishValue["excluded"] != float64(2) {
		t.Fatalf("跳过条数应落进报告：%+v", publishValue["excluded"])
	}
	items, ok := publishValue["excluded_items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("跳过明细应落进报告：%+v", publishValue["excluded_items"])
	}
	item := items[0].(map[string]any)
	if item["rule"] != DispatchRuleSkill || item["reason"] != "团队不用" {
		t.Fatalf("跳过明细要写清被哪条规则挡下、为什么：%+v", item)
	}
	// 上游锚点跟发布报告一起走，两份报告才对得上。
	upstream, ok := written["upstream"].(map[string]any)
	if !ok || upstream["commit"] != "deadbeef" {
		t.Fatalf("发布报告应带上上游锚点：%+v", written["upstream"])
	}
}

// 锁文件缺失不该让报告整体丢掉：发布已经成功了，少一块锚点比丢整份报告好。
func TestPublishReportSurvivesMissingLock(t *testing.T) {
	repoRoot := t.TempDir()
	report := PublishReport(repoRoot, PublishResult{Repository: "x/y", Channel: "beta", Items: []PublishItem{}})
	if _, ok := report["upstream"]; ok {
		t.Fatalf("没有锁文件时不该编造上游锚点：%+v", report)
	}
	if report["kind"] != ReportKindPublish || report["repo_root"] != repoRoot {
		t.Fatalf("报告的基本字段必须齐全：%+v", report)
	}
}

// 同一秒里的两次运行不能互相覆盖。
//
// 真发一次、紧接着干跑一次复核，两次都落在当前这一秒；覆盖掉的是先写的那份，
// 也就是唯一一次真发布的凭据。撞名时加序号，命名习惯保持不变。
func TestWriteRunReportDoesNotOverwriteSameSecond(t *testing.T) {
	repoRoot := t.TempDir()
	first, err := WriteRunReport(repoRoot, ReportKindPublish, map[string]any{"kind": ReportKindPublish, "round": 1})
	if err != nil {
		t.Fatal(err)
	}
	second, err := WriteRunReport(repoRoot, ReportKindPublish, map[string]any{"kind": ReportKindPublish, "round": 2})
	if err != nil {
		t.Fatal(err)
	}
	if first["uri"] == second["uri"] {
		t.Fatalf("两次运行写到了同一个文件：%s", first["uri"])
	}
	if _, err := os.Stat(filepath.FromSlash(strings.TrimPrefix(second["uri"].(string), "file:///"))); err != nil {
		t.Fatalf("第二份报告没有真正落盘：%v", err)
	}
	entries, err := os.ReadDir(ReportsDir(repoRoot))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("两份报告都应该在场，实际 %d 份", len(entries))
	}
}
