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

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
