package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MrBaoquan/himind-extensions-ecc/internal/eccsync"
)

// 命令行是同步闭环的第三次现身（插件能力、定时工作流、手动复现）。
// 这些测试盯住两件事：改策略的三种键都能加能删、写错的键必须在落盘前被拦下。

func TestSplitListAcceptsBothCommas(t *testing.T) {
	got := splitList(" framework-language, ,testing-quality，inner-ops ")
	want := []string{"framework-language", "testing-quality", "inner-ops"}
	if len(got) != len(want) {
		t.Fatalf("切分结果不对：%v", got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("切分结果不对：%v", got)
		}
	}
	if len(splitList("  ,  ")) != 0 {
		t.Fatal("只有空项时应当切出空列表，避免往策略里写空键")
	}
}

func TestEditDispatchPolicyAddsAndRemovesKeys(t *testing.T) {
	policy := eccsync.NewDispatchPolicy()
	editDispatchPolicy(&policy, dispatchChange{
		Categories: "testing-quality", Modules: "inner-ops", Skills: "alpha",
		Reason: "上游自用",
	})
	if policy.ExcludedCategories["testing-quality"] != "上游自用" ||
		policy.ExcludedModules["inner-ops"] != "上游自用" ||
		policy.ExcludedSkills["alpha"] != "上游自用" {
		t.Fatalf("三类键都要写上原因：%+v", policy)
	}

	// 不给原因时用兜底文案，策略里不能留下一条没头没尾的排除。
	editDispatchPolicy(&policy, dispatchChange{Modules: "another"})
	if policy.ExcludedModules["another"] != eccsync.DefaultDispatchReason {
		t.Fatalf("缺省原因要用兜底文案：%+v", policy.ExcludedModules)
	}

	editDispatchPolicy(&policy, dispatchChange{
		Categories: "testing-quality", Modules: "inner-ops,another", Skills: "alpha", Remove: true,
	})
	if policy.ExcludedCount() != 0 {
		t.Fatalf("摘掉之后策略应当回到全放行：%+v", policy)
	}
}

func TestApplyDispatchChangeValidatesBeforeWriting(t *testing.T) {
	repoRoot := t.TempDir()
	writeFile(t, filepath.Join(repoRoot, eccsync.PolicyFile), `{
  "schema_version": 1,
  "upstream": {"repository": "affaan-m/ECC", "package": "ecc-universal", "license": "MIT", "holder": "Affaan Mustafa"},
  "module_categories": {"core": "software-engineering"},
  "excluded_skills": {},
  "file_policy": {"max_files_per_skill": 40, "max_file_bytes": 1048576, "text_extensions": [".md"]},
  "rewrites": [],
  "skill_id_prefix": "com.mrbaoquan.ecc.skill"
}`)
	writeFile(t, filepath.Join(repoRoot, filepath.FromSlash(eccsync.ModulesFile)),
		`{"schema_version":1,"module_categories":{"core":"software-engineering"},"skill_modules":{"alpha":"core"}}`)
	writeFile(t, filepath.Join(repoRoot, eccsync.LockFile),
		`{"schema_version":1,"upstream":{},"sync":{},"skills":{"alpha":{"version":"1.0.0"}}}`)

	policy, err := applyDispatchChange(repoRoot, dispatchChange{Modules: "core", Reason: "内部流程"})
	if err != nil {
		t.Fatalf("合法改动应当写盘成功：%v", err)
	}
	if policy.ExcludedModules["core"] != "内部流程" {
		t.Fatalf("写回的策略不对：%+v", policy)
	}
	if policy.UpdatedAt == "" {
		t.Fatal("改动必须记下时间，界面上要能显示「最后一次修改」")
	}
	policyPath := filepath.Join(repoRoot, eccsync.DispatchPolicyFile)
	if _, err := os.Stat(policyPath); err != nil {
		t.Fatalf("策略文件应当已经落盘：%v", err)
	}

	// 写错的模块名必须被拦下，而且不能把上一份好策略改坏。
	if _, err := applyDispatchChange(repoRoot, dispatchChange{Modules: "coree", Reason: "拼错了"}); err == nil {
		t.Fatal("未知模块必须在落盘前被发现")
	} else if !strings.Contains(err.Error(), "coree") {
		t.Fatalf("报错要指出是哪个键写错了：%v", err)
	}
	reloaded, err := eccsync.LoadDispatchPolicy(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.ExcludedModules["coree"]; ok {
		t.Fatal("写错的键不该出现在策略文件里")
	}
}

// 命令行是真实发版走的那条路，它也得留报告。
//
// 发布本身是成功还是失败，看的是「东西有没有推上去」；报告回答的是
// 「这一批里谁被分发策略挡下了」。两者不能互相顶替：报告没写成，
// 命令照样算成功，但输出里要能看出报告丢了。
func TestWithReportArtifactKeepsResultFields(t *testing.T) {
	repoRoot := t.TempDir()
	value := eccsync.PublishResult{
		Repository: "MrBaoquan/himind-extensions-ecc",
		Channel:    "beta",
		DryRun:     true,
		Pending:    1,
		Excluded:   1,
		ExcludedItems: []eccsync.ExcludedItem{{
			Slug: "taste", ID: "com.mrbaoquan.ecc.skill.taste", Version: "1.0.0",
			Rule: eccsync.DispatchRuleSkill, Keyword: "taste", Reason: "团队不用",
		}},
		Items: []eccsync.PublishItem{},
	}

	merged, ok := withReportArtifact(value, eccsync.PublishReport(repoRoot, value)).(map[string]any)
	if !ok {
		t.Fatal("命令输出应当是对象，原来的顶层字段不能被包进新层级")
	}
	// 原有的顶层字段一个都不能少、不能改名：脚本和习惯都指着它们。
	for _, key := range []string{"repository", "channel", "dry_run", "pending", "excluded", "items"} {
		if _, present := merged[key]; !present {
			t.Fatalf("原有字段 %q 丢了：%+v", key, merged)
		}
	}
	artifacts, ok := merged["artifacts"].([]any)
	if !ok || len(artifacts) != 1 {
		t.Fatalf("输出里应当带上报告信封：%+v", merged["artifacts"])
	}
	envelope := artifacts[0].(map[string]any)
	path := filepath.FromSlash(strings.TrimPrefix(envelope["uri"].(string), "file:///"))
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("按信封找不到刚写的报告：%v", err)
	}
	// 报告要能自己说清「被挡下的是哪一条」，而不是只有一个数字。
	if !strings.Contains(string(payload), `"kind": "publish"`) ||
		!strings.Contains(string(payload), `"rule": "skill"`) {
		t.Fatalf("报告内容不完整：%s", payload)
	}
}

// 缓存目录建不出来时，报告退到临时目录，但记录本身不能丢。
//
// 发布已经成功了，这时候让命令报错只会把「发出去了」说成「失败了」。
// 代价是路径可能不在仓库里——信封里带着真实路径，照样找得到。
func TestWithReportArtifactFallsBackToTempDir(t *testing.T) {
	value := eccsync.PublishResult{Repository: "x/y", Channel: "beta", Items: []eccsync.PublishItem{}}
	// 盘符里带非法字符，仓库内的缓存目录必然建不出来。
	broken := filepath.Join(t.TempDir(), "bad:dir")
	merged, ok := withReportArtifact(value, eccsync.PublishReport(broken, value)).(map[string]any)
	if !ok {
		t.Fatal("报告落点异常时也应保持原来的输出形状")
	}
	if merged["repository"] != "x/y" {
		t.Fatalf("报告落点异常不该抹掉发布结论：%+v", merged)
	}
	artifacts, ok := merged["artifacts"].([]any)
	if !ok || len(artifacts) != 1 {
		t.Fatalf("退到临时目录也要给出报告信封：%+v", merged["artifacts"])
	}
	envelope := artifacts[0].(map[string]any)
	path := filepath.FromSlash(strings.TrimPrefix(envelope["uri"].(string), "file:///"))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("信封里的路径必须真的存在：%v", err)
	}
	_ = os.Remove(path)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
