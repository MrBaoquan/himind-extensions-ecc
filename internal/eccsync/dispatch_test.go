package eccsync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MrBaoquan/himind-extensions/tooling/catalog"
	"github.com/MrBaoquan/himind-extensions/tooling/distribution"
)

// 分发策略管的是「已经搬进仓库的技能还要不要往外发」。
//
// 这套测试盯住三件事：缺文件时必须默认全放行（老仓库升级不能停摆）、
// 判定必须按「单条 > 模块 > 分类」给出稳定的原因、被排除的技能必须出现在
// 跳过清单里而插件与工作流不受影响。少盯住任何一条，都会变成「策略说跳过
// 一批，但没人知道跳过了哪一批」。

func TestLoadDispatchPolicyMissingFileIsPermissive(t *testing.T) {
	policy, err := LoadDispatchPolicy(filepath.Join(t.TempDir(), DispatchPolicyFile))
	if err != nil {
		t.Fatalf("策略文件不存在时必须默认全部分发，而不是报错：%v", err)
	}
	if !policy.Empty() {
		t.Fatalf("缺省策略必须是空策略，得到 %+v", policy)
	}
	if policy.SchemaVersion != DispatchPolicyVersion {
		t.Fatalf("缺省策略要带上结构版本，得到 %d", policy.SchemaVersion)
	}
	decision := policy.Decide(DispatchSubject{Slug: "anything", Module: "any", Categories: []string{"software-engineering"}})
	if decision.Excluded {
		t.Fatal("空策略不该排除任何技能")
	}
}

func TestLoadDispatchPolicyRejectsUnknownSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), DispatchPolicyFile)
	writeTestFile(t, path, `{"schema_version": 99, "excluded_skills": {"alpha": "旧结构"}}`)
	if _, err := LoadDispatchPolicy(path); err == nil {
		t.Fatal("读到读不懂的结构版本必须报错，静默忽略等于悄悄改了分发范围")
	}
}

func TestSaveDispatchPolicyIsByteStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), DispatchPolicyFile)
	value := DispatchPolicy{
		ExcludedModules:    map[string]string{"zeta": "内部流程", "alpha": "内部流程"},
		ExcludedCategories: map[string]string{"testing-quality": "上游自用"},
		ExcludedSkills:     map[string]string{},
	}
	if err := SaveDispatchPolicy(path, value); err != nil {
		t.Fatalf("写策略失败：%v", err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 同一份意图再写一次必须逐字节一致：否则每次同步的 diff 里全是抖动，
	// 真正改了哪条规则反而看不出来。
	if err := SaveDispatchPolicy(path, value); err != nil {
		t.Fatalf("二次写策略失败：%v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("同一份策略两次写出的字节必须一致：\n%s\n---\n%s", first, second)
	}

	loaded, err := LoadDispatchPolicy(path)
	if err != nil {
		t.Fatalf("读回策略失败：%v", err)
	}
	if loaded.ExcludedModules["zeta"] != "内部流程" || loaded.ExcludedCategories["testing-quality"] != "上游自用" {
		t.Fatalf("往返之后策略内容变了：%+v", loaded)
	}
	if loaded.SchemaVersion != DispatchPolicyVersion {
		t.Fatalf("保存时必须补上结构版本，得到 %d", loaded.SchemaVersion)
	}
}

func TestDispatchPolicyDecidePrefersMostSpecificRule(t *testing.T) {
	subject := DispatchSubject{Slug: "alpha", Module: "framework-language", Categories: []string{"testing-quality"}}
	full := DispatchPolicy{
		SchemaVersion:      DispatchPolicyVersion,
		ExcludedSkills:     map[string]string{"alpha": "单条排除"},
		ExcludedModules:    map[string]string{"framework-language": "模块排除"},
		ExcludedCategories: map[string]string{"testing-quality": "分类排除"},
	}
	decision := full.Decide(subject)
	if !decision.Excluded || decision.Rule != DispatchRuleSkill || decision.Keyword != "alpha" || decision.Reason != "单条排除" {
		t.Fatalf("三个规则同时命中时要显示最具体的那条：%+v", decision)
	}

	withoutSkill := full
	withoutSkill.ExcludedSkills = map[string]string{}
	decision = withoutSkill.Decide(subject)
	if decision.Rule != DispatchRuleModule || decision.Keyword != "framework-language" {
		t.Fatalf("没有单条规则时应该落到模块规则：%+v", decision)
	}

	onlyCategory := full
	onlyCategory.ExcludedSkills = map[string]string{}
	onlyCategory.ExcludedModules = map[string]string{}
	decision = onlyCategory.Decide(subject)
	if decision.Rule != DispatchRuleCategory || decision.Keyword != "testing-quality" {
		t.Fatalf("只剩分类规则时应该落到分类规则：%+v", decision)
	}

	// 没有模块归属的技能不该因为模块为空而被误判，也不该 panic。
	decision = onlyCategory.Decide(DispatchSubject{Slug: "beta", Categories: []string{"software-engineering"}})
	if decision.Excluded {
		t.Fatalf("没命中任何规则时不该被排除：%+v", decision)
	}
}

func TestDispatchPolicyFillsBlankReason(t *testing.T) {
	policy := DispatchPolicy{
		SchemaVersion:      DispatchPolicyVersion,
		ExcludedModules:    map[string]string{"  framework-language  ": "   "},
		ExcludedCategories: map[string]string{"": "空键要丢掉"},
		ExcludedSkills:     map[string]string{"alpha": " 需要\n换行 收敛 "},
	}
	policy.normalize()
	if _, ok := policy.ExcludedCategories[""]; ok {
		t.Fatal("空键必须被丢掉，否则策略里会留下一条排除不了的规则")
	}
	if reason := policy.ExcludedModules["framework-language"]; reason != DefaultDispatchReason {
		t.Fatalf("空白原因要补兜底文案，得到 %q", reason)
	}
	if reason := policy.ExcludedSkills["alpha"]; reason != "需要 换行 收敛" {
		t.Fatalf("原因里的换行与多空格要收敛成单空格，得到 %q", reason)
	}
}

func TestValidateDispatchTargetsRejectsUnknownKeys(t *testing.T) {
	modules := ModuleMap{
		Categories: map[string]string{"framework-language": "software-engineering"},
		Skills:     map[string]string{"alpha": "framework-language"},
	}
	lock := Lock{SchemaVersion: LockVersion, Skills: map[string]LockSkill{"alpha": {Version: "1.0.0"}}}

	good := DispatchPolicy{
		SchemaVersion:      DispatchPolicyVersion,
		ExcludedModules:    map[string]string{"framework-language": "内部流程"},
		ExcludedCategories: map[string]string{"testing-quality": "上游自用"},
		ExcludedSkills:     map[string]string{"alpha": "单条排除"},
	}
	if err := ValidateDispatchTargets(good, modules, lock); err != nil {
		t.Fatalf("合法策略不该被拦下：%v", err)
	}

	bad := DispatchPolicy{
		SchemaVersion:      DispatchPolicyVersion,
		ExcludedModules:    map[string]string{"framework-langauge": "拼错了"},
		ExcludedCategories: map[string]string{"quality": "不是 11 个子集之一"},
		ExcludedSkills:     map[string]string{"alhpa": "slug 拼错了"},
	}
	err := ValidateDispatchTargets(bad, modules, lock)
	if err == nil {
		t.Fatal("写错的键必须在落盘前被发现，否则会静默地什么都不排除")
	}
	message := err.Error()
	for _, want := range []string{"framework-langauge", "quality", "alhpa"} {
		if !strings.Contains(message, want) {
			t.Fatalf("报错要一次性汇总三处写错，缺了 %q：%s", want, message)
		}
	}
}

// TestPendingTargetsSkipsExcludedSkillsAndKeepsTools 同时盯住两件事：
// 被策略挡下的技能要进跳过清单，本仓库自有的插件与工作流要照常发。
func TestPendingTargetsSkipsExcludedSkillsAndKeepsTools(t *testing.T) {
	repoRoot := t.TempDir()
	writeTestFile(t, filepath.Join(repoRoot, "plugins", "ecc-skill-sync", "plugin.json"),
		`{"id":"com.mrbaoquan.ecc-skill-sync","name":"ECC 技能同步","author":"马宝全","version":"1.0.9"}`)
	writeTestFile(t, filepath.Join(repoRoot, "workflows", "ecc-skill-sync", "workflow.json"),
		`{"id":"com.mrbaoquan.workflow.ecc-skill-sync","name":"ECC 同步","version":"1.0.12"}`)

	extensions := extensionsDocument{
		SchemaVersion: 1,
		Extensions: []extensionEntry{
			{Type: distribution.KindPlugin, ID: "com.mrbaoquan.ecc-skill-sync", Path: "plugins/ecc-skill-sync"},
			{Type: distribution.KindWorkflow, ID: "com.mrbaoquan.workflow.ecc-skill-sync", Path: "workflows/ecc-skill-sync"},
		},
	}
	lock := Lock{SchemaVersion: LockVersion, Skills: map[string]LockSkill{
		"alpha": {Version: "1.0.0"},
		"beta":  {Version: "1.0.0"},
		"gamma": {Version: "1.0.0"},
		"delta": {Version: "1.0.0"},
	}}
	// beta 属于被排除的上游模块，gamma 的分类被整片排除，alpha 单条排除，
	// delta 完全干净——三种规则各命中一条，干净的必须照发。
	policy := DispatchPolicy{
		SchemaVersion:      DispatchPolicyVersion,
		ExcludedSkills:     map[string]string{"alpha": "单条排除"},
		ExcludedModules:    map[string]string{"internal-ops": "上游内部流程"},
		ExcludedCategories: map[string]string{"testing-quality": "上游自用"},
	}
	context := DispatchContext{
		Policy: policy,
		Modules: ModuleMap{
			Categories: map[string]string{"internal-ops": "software-engineering"},
			Skills:     map[string]string{"beta": "internal-ops"},
		},
	}
	metadata := Metadata{SchemaVersion: MetadataVersion, Skills: map[string]MetadataEntry{
		"gamma": {Name: "gamma-helper", Source: "reviewed", Categories: []string{"testing-quality"}},
	}}

	SetSkillIDPrefix("com.mrbaoquan.ecc.skill.")
	t.Cleanup(func() { SetSkillIDPrefix("com.mrbaoquan.ecc.skill.") })
	index := catalog.New("mrbaoquan/himind-extensions-ecc", "beta", "public")

	targets, excluded, err := pendingTargets(repoRoot, lock, extensions, index, context, metadata)
	if err != nil {
		t.Fatalf("算待发布项失败：%v", err)
	}
	if len(excluded) != 3 {
		t.Fatalf("三条被排除的技能都要进跳过清单，得到 %+v", excluded)
	}
	// 锁是 map，遍历顺序随机，跳过清单必须自排序，这里顺带把它钉住。
	wantExcluded := []struct{ slug, rule, keyword string }{
		{"alpha", DispatchRuleSkill, "alpha"},
		{"beta", DispatchRuleModule, "internal-ops"},
		{"gamma", DispatchRuleCategory, "testing-quality"},
	}
	for i, want := range wantExcluded {
		got := excluded[i]
		if got.Slug != want.slug || got.Rule != want.rule || got.Keyword != want.keyword {
			t.Fatalf("跳过清单第 %d 条不对：want %+v got %+v", i, want, got)
		}
	}
	if len(targets) != 3 {
		t.Fatalf("插件、工作流与干净的 delta 都要发，得到 %+v", targets)
	}
	if targets[0].kind != distribution.KindPlugin {
		t.Fatalf("插件必须排在最前：%+v", targets)
	}
	if targets[1].kind != distribution.KindSkill || targets[1].slug != "delta" {
		t.Fatalf("干净的技能要照发，得到 %+v", targets[1])
	}
	if targets[2].kind != distribution.KindWorkflow {
		t.Fatalf("工作流必须排在最后：%+v", targets)
	}
}

// TestBuildDistributionSeparatesStates 说明界面拿到的状态是哪来的：
// 已发、待发、文案没过闸门、被策略排除，四类各一条，互不重叠。
func TestBuildDistributionSeparatesStates(t *testing.T) {
	repoRoot := t.TempDir()
	writeTestFile(t, filepath.Join(repoRoot, PolicyFile), testPolicyJSON)
	writeTestFile(t, filepath.Join(repoRoot, filepath.FromSlash(ModulesFile)),
		`{"schema_version":1,"module_categories":{"core":"software-engineering","ops":"testing-quality"},"skill_modules":{"gamma":"ops"}}`)
	writeTestFile(t, filepath.Join(repoRoot, LockFile), `{
  "schema_version": 1,
  "upstream": {"commit": "abcdef0123456789", "commit_date": "2026-09-01"},
  "sync": {"sequence": 7},
  "skills": {
    "alpha": {"version": "1.0.0", "source_path": "skills/alpha", "tier": "A"},
    "beta": {"version": "1.0.0", "source_path": "skills/beta", "tier": "A"},
    "gamma": {"version": "1.0.0", "source_path": "skills/gamma", "tier": "B"},
    "delta": {"version": "1.0.0", "source_path": "skills/delta", "tier": "A"}
  }
}`)
	// 元数据表：alpha 已校对，beta 是机械推导（过不了闸门），delta 已校对。
	// 索引里的 skill_id 必须和 BuildDistribution 读到的策略前缀一致，
	// 否则会被判成「这条技能从没发过」。
	SetSkillIDPrefix("com.mrbaoquan.ecc.skill")
	t.Cleanup(func() { SetSkillIDPrefix("com.mrbaoquan.ecc.skill.") })
	if err := SaveMetadata(filepath.Join(repoRoot, filepath.FromSlash(MetadataFile)), Metadata{
		SchemaVersion: MetadataVersion,
		Skills: map[string]MetadataEntry{
			"alpha": {Name: "alpha-helper", Source: "reviewed"},
			"beta":  {Name: "beta-helper", Source: "derived"},
			"delta": {Name: "delta-helper", Source: "reviewed"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// 索引里 alpha 的 1.0.0 已经发过，其余都没发过。
	index := catalog.New("mrbaoquan/himind-extensions-ecc", "beta", "public")
	if err := index.Upsert(distribution.KindSkill, map[string]interface{}{
		"skill_id": SkillID("alpha"), "version": "1.0.0",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".himind"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := index.Save(filepath.Join(repoRoot, filepath.FromSlash(CatalogFile))); err != nil {
		t.Fatal(err)
	}
	// gamma 的分类被整片排除。
	if err := SaveDispatchPolicy(filepath.Join(repoRoot, DispatchPolicyFile), DispatchPolicy{
		SchemaVersion:      DispatchPolicyVersion,
		ExcludedCategories: map[string]string{"testing-quality": "上游自用"},
	}); err != nil {
		t.Fatal(err)
	}

	report, err := BuildDistribution(repoRoot, DistributionOptions{})
	if err != nil {
		t.Fatalf("算分发状态失败：%v", err)
	}
	if report.Totals.Skills != 4 {
		t.Fatalf("四条技能都要算进来，得到 %d", report.Totals.Skills)
	}
	if report.Totals.Distributed != 1 || report.Totals.Pending != 1 || report.Totals.Held != 1 || report.Totals.Excluded != 1 {
		t.Fatalf("四种状态各一条：%+v", report.Totals)
	}
	states := map[string]string{}
	for _, item := range report.Items {
		states[item.Slug] = item.State
	}
	want := map[string]string{
		"alpha": DistributionDistributed,
		"beta":  DistributionHeld,
		"gamma": DistributionExcluded,
		"delta": DistributionPending,
	}
	for slug, state := range want {
		if states[slug] != state {
			t.Fatalf("%s 的状态应为 %s，得到 %s", slug, state, states[slug])
		}
	}
	// 被排除的那条要能回溯到是哪条规则排的。
	for _, item := range report.Items {
		if item.Slug == "gamma" && (item.Rule != DispatchRuleCategory || item.Keyword != "testing-quality") {
			t.Fatalf("被排除的技能要指出命中的规则：%+v", item)
		}
	}
	// 分类分组要覆盖模块映射里的分类，且被排除的那组要标出原因。
	found := false
	for _, group := range report.Categories {
		if group.Key == "testing-quality" {
			found = true
			if group.Rule != DispatchRuleCategory || group.Reason == "" {
				t.Fatalf("被整体排除的分类要标出规则与原因：%+v", group)
			}
		}
	}
	if !found {
		t.Fatalf("分类分组里必须出现 testing-quality：%+v", report.Categories)
	}
}
