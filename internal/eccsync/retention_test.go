package eccsync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MrBaoquan/himind-extensions/tooling/catalog"
	"github.com/MrBaoquan/himind-extensions/tooling/distribution"
)

// releaseAt 造一条历史 Release 记录，只填回收判定用得到的字段。
func releaseAt(id int64, tag string) releaseRecord {
	return releaseRecord{ID: id, Tag: tag}
}

func TestPlanSupersededKeepsNewestVersions(t *testing.T) {
	records := []releaseRecord{
		releaseAt(1, "plugin/com.example.tool@1.0.0"),
		releaseAt(2, "plugin/com.example.tool@1.0.1"),
		releaseAt(3, "plugin/com.example.tool@1.0.2"),
		// 乱序给进来：回收判定不能依赖远端返回的顺序。
		releaseAt(4, "plugin/com.example.tool@1.0.10"),
		releaseAt(5, "plugin/com.example.tool@1.0.9"),
	}
	owners := map[string]bool{"plugin/com.example.tool": true}
	plan := planSuperseded(records, owners, map[string]int{distribution.KindPlugin: 2}, map[string]bool{})

	got := []string{}
	for _, item := range plan {
		got = append(got, item.Tag)
	}
	want := []string{
		"plugin/com.example.tool@1.0.0",
		"plugin/com.example.tool@1.0.1",
		"plugin/com.example.tool@1.0.2",
	}
	if len(got) != len(want) {
		t.Fatalf("要回收 %v，期望 %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("第 %d 条是 %q，期望 %q（10 必须排在 9 之后，否则会误删新版本）", index, got[index], want[index])
		}
	}
}

func TestPlanSupersededLeavesUnknownOwnersAlone(t *testing.T) {
	records := []releaseRecord{
		// 别人的仓库记录不该出现在这里，但真出现也不能删。
		releaseAt(1, "plugin/com.other.tool@1.0.0"),
		releaseAt(2, "plugin/com.other.tool@1.0.1"),
		// 自研的类型是受支持的，但归属不在扩展清单里。
		releaseAt(3, "skill/com.example.unknown@1.0.0"),
		releaseAt(4, "skill/com.example.unknown@1.0.1"),
		// 名字不像我们发的东西。
		releaseAt(5, "v1.2.3"),
		releaseAt(6, "docs/2026-09"),
	}
	owners := map[string]bool{"plugin/com.example.tool": true}
	plan := planSuperseded(records, owners, map[string]int{distribution.KindPlugin: 1, distribution.KindSkill: 1}, map[string]bool{})
	if len(plan) != 0 {
		t.Fatalf("不该动任何东西，却打算回收 %v", plan)
	}
}

func TestPlanSupersededNeverPrunesPinnedTags(t *testing.T) {
	records := []releaseRecord{
		releaseAt(1, "plugin/com.example.tool@1.0.0"),
		releaseAt(2, "plugin/com.example.tool@1.0.1"),
		releaseAt(3, "plugin/com.example.tool@1.0.2"),
	}
	owners := map[string]bool{"plugin/com.example.tool": true}
	pinned := map[string]bool{"plugin/com.example.tool@1.0.0": true}
	plan := planSuperseded(records, owners, map[string]int{distribution.KindPlugin: 1}, pinned)

	if len(plan) != 1 || plan[0].Tag != "plugin/com.example.tool@1.0.1" {
		t.Fatalf("被工作流钉住的 1.0.0 必须留着，只回收 1.0.1，实际 %v", plan)
	}
}

func TestPlanSupersededIgnoresDrafts(t *testing.T) {
	draft := releaseAt(2, "plugin/com.example.tool@1.0.1")
	draft.Draft = true
	records := []releaseRecord{
		releaseAt(1, "plugin/com.example.tool@1.0.0"),
		draft,
		releaseAt(3, "plugin/com.example.tool@1.0.2"),
	}
	owners := map[string]bool{"plugin/com.example.tool": true}
	// 保留窗口按版本数算：半成品没有对外版本，不能占掉一个名额，
	// 否则 1.0.1 的 Draft 会把窗口占满，把 1.0.0 留在仓库里、反而把 1.0.2 判成要回收。
	plan := planSuperseded(records, owners, map[string]int{distribution.KindPlugin: 1}, map[string]bool{})
	if len(plan) != 1 || plan[0].Tag != "plugin/com.example.tool@1.0.0" {
		t.Fatalf("Draft 不参与保留窗口，实际回收 %v", plan)
	}
}

func TestPlanSupersededKeepsAtLeastOneVersion(t *testing.T) {
	records := []releaseRecord{releaseAt(1, "skill/com.example.only@1.0.0")}
	owners := map[string]bool{"skill/com.example.only": true}
	for _, keep := range []int{0, -1} {
		plan := planSuperseded(records, owners, map[string]int{distribution.KindSkill: keep}, map[string]bool{})
		if len(plan) != 0 {
			t.Fatalf("keep=%d 时唯一版本被回收了: %v", keep, plan)
		}
	}
}

func TestKeepForFallsBackToDefaults(t *testing.T) {
	// 策略文件没写某一类时用缺省值，写了才覆盖；两者不能互相顶替。
	retention := ReleaseRetention{KeepVersions: map[string]int{distribution.KindSkill: 5}}
	if got := retention.KeepFor(distribution.KindSkill); got != 5 {
		t.Fatalf("技能保留数应为策略里的 5，实际 %d", got)
	}
	if got := retention.KeepFor(distribution.KindPlugin); got != DefaultKeepVersions[distribution.KindPlugin] {
		t.Fatalf("插件没写策略时应取缺省 %d，实际 %d", DefaultKeepVersions[distribution.KindPlugin], got)
	}
	keep := retention.KeepMap()
	if len(keep) != len(DefaultKeepVersions) {
		t.Fatalf("KeepMap 必须覆盖全部扩展类型，实际 %v", keep)
	}
}

func TestLoadReleaseRetention(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ReleasePolicyFile)

	// 文件不存在：按缺省策略走，首次落地不该因为少一个文件就失败。
	missing, err := LoadReleaseRetention(path)
	if err != nil {
		t.Fatalf("策略文件缺失不该报错: %v", err)
	}
	if missing.KeepFor(distribution.KindWorkflow) != DefaultKeepVersions[distribution.KindWorkflow] {
		t.Fatalf("缺省策略取错了: %v", missing)
	}

	if err := SaveReleaseRetention(path, ReleaseRetention{
		KeepVersions: map[string]int{distribution.KindPlugin: 2, distribution.KindSkill: 4, distribution.KindWorkflow: 2},
	}); err != nil {
		t.Fatalf("写策略: %v", err)
	}
	saved, err := LoadReleaseRetention(path)
	if err != nil {
		t.Fatalf("读策略: %v", err)
	}
	if saved.KeepFor(distribution.KindSkill) != 4 {
		t.Fatalf("策略没读回来: %v", saved)
	}

	// 0 或负数等于「把当前版本也删掉」，必须在读的时候拦下。
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"keep_versions":{"skill":0}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadReleaseRetention(path); err == nil {
		t.Fatal("keep_versions 为 0 应被拒绝")
	}

	if err := os.WriteFile(path, []byte(`{"schema_version":99}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadReleaseRetention(path); err == nil {
		t.Fatal("未知 schema_version 应被拒绝")
	}
}

func TestPinnedTagsReadsDependencyReferences(t *testing.T) {
	root := t.TempDir()
	indexPath := filepath.Join(root, "catalog.json")
	body := `{
	  "schema_version": 1,
	  "workflows": [
	    {"workflow_id": "com.example.flow", "version": "1.0.0",
	     "release_tag": "workflow/com.example.flow@1.0.0",
	     "dependencies": [
	       {"kind": "plugin", "id": "com.example.tool",
	        "source": {"reference": "plugin/com.example.tool@1.0.1"}},
	       {"kind": "plugin", "id": "com.example.plain"}
	     ]}
	  ]
	}`
	if err := os.WriteFile(indexPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	index, err := catalog.Load(indexPath)
	if err != nil {
		t.Fatalf("读索引: %v", err)
	}
	pinned := pinnedTags(index)
	if !pinned["plugin/com.example.tool@1.0.1"] {
		t.Fatalf("依赖 pin 没被收进来: %v", pinned)
	}
	if len(pinned) != 1 {
		t.Fatalf("没有 reference 的依赖不该被当成 pin: %v", pinned)
	}
}
