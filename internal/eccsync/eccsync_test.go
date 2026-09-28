package eccsync

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MrBaoquan/himind-extensions/tooling/catalog"
	"github.com/MrBaoquan/himind-extensions/tooling/distribution"
)

func TestVersionCoreKeepsOnlyTwoSegments(t *testing.T) {
	cases := []struct {
		input        string
		major, minor int
	}{
		{"2.2.2", 2, 2},
		{" v1.10.3 ", 1, 10},
		{"V2.0.0", 2, 0},
		{"1.0.0-beta.1", 1, 0},
		{"2.2", 2, 2},
		{"latest", 0, 0},
		{"", 0, 0},
	}
	for _, item := range cases {
		major, minor := versionCore(item.input)
		if major != item.major || minor != item.minor {
			t.Fatalf("versionCore(%q) = %d.%d, want %d.%d", item.input, major, minor, item.major, item.minor)
		}
	}
}

func TestClampDescriptionPrefersSentenceBoundary(t *testing.T) {
	sentence := strings.Repeat("a", 60) + "." + strings.Repeat("b", 80)
	clamped, truncated := clampDescription(sentence, 100)
	if !truncated {
		t.Fatal("长说明必须被判定为截断")
	}
	if clamped != strings.Repeat("a", 60)+"." {
		t.Fatalf("期望截到句末，得到 %q", clamped)
	}
	if got := len([]rune(clamped)); got > 100 {
		t.Fatalf("截断结果 %d 字超过上限", got)
	}

	short := "短说明"
	if value, truncated := clampDescription(short, 100); truncated || value != short {
		t.Fatalf("短说明不应被改动：%q %v", value, truncated)
	}
}

func TestShortenNameKeepsSemanticTail(t *testing.T) {
	// 英语名词短语的重心在末尾，缩名要先丢开头的限定词。
	if got := shortenName("Advanced Architecture Decision Records", 16); got != "Decision Records" {
		t.Fatalf("shortenName 结果 = %q", got)
	}
	// 括号里的补充说明直接丢掉。
	if got := shortenName("Skill Sync (upstream mirror)", 32); got != "Skill Sync" {
		t.Fatalf("shortenName 未去掉括号补充：%q", got)
	}
	if got := shortenName("短名", 18); got != "短名" {
		t.Fatalf("短名不应被改动：%q", got)
	}
}

func TestBlockedPathRejectsScriptsAndBinaryBundles(t *testing.T) {
	blocked := []string{
		"tools/install.ps1",
		"run.SH",
		"scripts/helper.py",
		"bin/tool.exe",
		"native/lib.dylib",
	}
	for _, path := range blocked {
		if !blockedPath(path) {
			t.Fatalf("%q 应被拦下", path)
		}
	}
	allowed := []string{
		"SKILL.md",
		"reference/checklist.md",
		"assets/diagram.png",
		"examples/config.yaml",
	}
	for _, path := range allowed {
		if blockedPath(path) {
			t.Fatalf("%q 不该被拦下", path)
		}
	}
}

func TestSplitByMetadataSourceHoldsDerivedEntries(t *testing.T) {
	metadata := Metadata{SchemaVersion: MetadataVersion, Skills: map[string]MetadataEntry{
		"reviewed-skill": {Name: "已校对", Source: "reviewed"},
		"derived-skill":  {Name: "待校对", Source: "derived"},
	}}
	targets := []publishTarget{
		{kind: distribution.KindSkill, id: "com.mrbaoquan.ecc.skill.reviewed-skill", slug: "reviewed-skill"},
		{kind: distribution.KindSkill, id: "com.mrbaoquan.ecc.skill.derived-skill", slug: "derived-skill"},
		{kind: distribution.KindSkill, id: "com.mrbaoquan.ecc.skill.unknown-skill", slug: "unknown-skill"},
		{kind: distribution.KindPlugin, id: "com.mrbaoquan.ecc-skill-sync"},
		{kind: distribution.KindWorkflow, id: "com.mrbaoquan.workflow.ecc-skill-sync"},
	}
	ready, held := splitByMetadataSource(targets, metadata)
	if len(ready) != 3 || ready[0].label() != "reviewed-skill" {
		t.Fatalf("只有 reviewed 技能可发布，插件与工作流不受闸门约束，得到 %v", ready)
	}
	if ready[1].kind != distribution.KindPlugin || ready[2].kind != distribution.KindWorkflow {
		t.Fatalf("插件与工作流必须直通，得到 %v", ready)
	}
	if len(held) != 2 || held[0] != "derived-skill" || held[1] != "unknown-skill" {
		t.Fatalf("derived 与缺失条目都必须挂起，得到 %v", held)
	}
}

func TestPublishedVersionsKeepsEveryReleasedVersion(t *testing.T) {
	index := catalog.New("mrbaoquan/himind-extensions-ecc", "beta", "public")
	pluginID := "com.mrbaoquan.ecc-skill-sync"
	// 索引是版本历史：同一个插件会留着 1.0.0 与 1.0.1 两条。先写新的再写旧的，
	// 复现「新版本在数组前面」的真实排布，顺序不该影响判定。
	for _, version := range []string{"1.0.1", "1.0.0"} {
		if err := index.Upsert(distribution.KindPlugin, map[string]interface{}{
			"plugin_id": pluginID, "version": version,
		}); err != nil {
			t.Fatalf("写入索引失败：%v", err)
		}
	}
	published := publishedVersions(index)
	for _, version := range []string{"1.0.0", "1.0.1"} {
		if !published[distribution.KindPlugin+"/"+pluginID][version] {
			t.Fatalf("插件 %s 已发过，必须被判为不再待发", version)
		}
	}
	if published[distribution.KindPlugin+"/"+pluginID]["1.0.2"] {
		t.Fatal("没发过的 1.0.2 必须仍是待发")
	}
	if published[distribution.KindSkill+"/com.mrbaoquan.ecc.skill.search-first"]["2.2.2"] {
		t.Fatal("索引里没有的技能不该出现在已发集合里")
	}
}

func TestCheckIdentityRejectsBadSlugAndOversizedID(t *testing.T) {
	policy := Policy{SkillIDPrefix: "com.mrbaoquan.ecc.skill."}
	SetSkillIDPrefix(policy.SkillIDPrefix)
	t.Cleanup(func() { SetSkillIDPrefix("com.mrbaoquan.ecc.skill.") })

	if err := checkIdentity("good-slug-2", policy); err != nil {
		t.Fatalf("合规 slug 不该报错：%v", err)
	}
	if err := checkIdentity("Bad_Slug", policy); err == nil {
		t.Fatal("大写与下划线必须被拒")
	}
	if err := checkIdentity(strings.Repeat("a", 45), policy); err == nil {
		t.Fatal("稳定 ID 超过 64 字必须被拒")
	}
}

func TestParseFrontmatterReadsScalars(t *testing.T) {
	document := "\ufeff---\nname: react-patterns\ndescription: \"写 React 组件时参考\"\n  ignored: true\n---\n\n# 标题\n"
	fields := ParseFrontmatter(document)
	if fields["name"] != "react-patterns" {
		t.Fatalf("name = %q", fields["name"])
	}
	if fields["description"] != "写 React 组件时参考" {
		t.Fatalf("description 未去掉引号：%q", fields["description"])
	}
	if _, ok := fields["ignored"]; ok {
		t.Fatal("缩进续行不应被当成字段")
	}
	if len(ParseFrontmatter("# 没有 frontmatter")) != 0 {
		t.Fatal("没有 frontmatter 时应返回空表")
	}
}

func TestDigestFilesAndGeneratedDigestAreStable(t *testing.T) {
	files := []SourceFile{
		{RelativePath: "SKILL.md", Content: []byte("a")},
		{RelativePath: "reference/notes.md", Content: []byte("b")},
	}
	first := digestFiles(files, nil)
	if first != digestFiles(files, nil) {
		t.Fatal("同样的输入必须得到同样的摘要")
	}
	if first == digestFiles(files, []string{"scripts/x.py"}) {
		t.Fatal("被拦文件参与摘要，摘要必须变化")
	}
	changed := append([]SourceFile{{RelativePath: "SKILL.md", Content: []byte("c")}}, files[1])
	if first == digestFiles(changed, nil) {
		t.Fatal("内容变化必须改变摘要")
	}

	generated := map[string][]byte{"SKILL.md": []byte("a"), "skill.json": []byte("b")}
	if digestGenerated(generated) != digestGenerated(generated) {
		t.Fatal("map 遍历顺序不应影响生成摘要")
	}
	other := map[string][]byte{"SKILL.md": []byte("a"), "skill.json": []byte("c")}
	if digestGenerated(generated) == digestGenerated(other) {
		t.Fatal("内容变化必须改变生成摘要")
	}
}

func TestSameQuarantineIgnoresGeneratedAt(t *testing.T) {
	left := Quarantine{
		SchemaVersion: QuarantineVersion,
		GeneratedAt:   "2026-09-27T00:00:00Z",
		UpstreamSHA:   "abc",
		Entries:       []QuarantineEntry{{Slug: "x", Reason: "含脚本", Files: []string{"run.sh"}}},
	}
	right := left
	right.GeneratedAt = "2026-09-28T00:00:00Z"
	if !sameQuarantine(left, right) {
		t.Fatal("只有生成时间不同时必须判定为同一份隔离清单")
	}
	right.Entries = []QuarantineEntry{{Slug: "y", Reason: "含脚本", Files: []string{"run.sh"}}}
	if sameQuarantine(left, right) {
		t.Fatal("条目变化必须判定为不同")
	}
}

func TestDigestMetadataTracksReviewedCopy(t *testing.T) {
	base := MetadataEntry{
		Name:        "先搜后写",
		Description: "搜索现成方案再动手",
		Categories:  []string{"software-engineering"},
		Source:      "reviewed",
	}
	if digestMetadata(base) != digestMetadata(base) {
		t.Fatal("同样的元数据必须得到同样的摘要")
	}
	sameContent := base
	sameContent.Note = "这条注释只给人看，不该影响摘要"
	if digestMetadata(base) != digestMetadata(sameContent) {
		t.Fatal("推导备注不参与摘要")
	}
	edited := base
	edited.Description = "搜索现成方案再动手，找不到再自己写"
	if digestMetadata(base) == digestMetadata(edited) {
		t.Fatal("文案变化必须改变摘要，否则校对过的文案发不出去")
	}
}

// generate 用的上游事实必须来自 fetch 落地的源码树，不能再自己联网问一次 HEAD：
// 否则 api.github.com 或 raw.githubusercontent.com 抖一下，整条定时任务就会失败。
func TestSourceFactsPrefersRecordedFactsWithoutNetwork(t *testing.T) {
	repoRoot := t.TempDir()
	sourceRoot := filepath.Join(repoRoot, "source", "d3b8a3e")
	if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	recorded := LockUpstream{
		Version:       "2.2.2",
		Commit:        "d3b8a3e908904e242ed2dbe66af62cca71131419",
		CommitDate:    "2026-09-28T00:38:49Z",
		License:       "MIT",
		LicenseHolder: "Affaan Mustafa",
	}
	if err := writeSourceFacts(sourceRoot, recorded); err != nil {
		t.Fatal(err)
	}
	facts, err := SourceFacts(testPolicy(), repoRoot, sourceRoot)
	if err != nil {
		t.Fatalf("应当直接读事实文件，不该报错：%v", err)
	}
	if facts.Commit != recorded.Commit || facts.Version != recorded.Version {
		t.Fatalf("事实文件没有被采用：%+v", facts)
	}
	if facts.Repository != "affaan-m/ECC" || facts.LicenseHolder != "Affaan Mustafa" {
		t.Fatalf("仓库与许可信息应来自策略：%+v", facts)
	}
}

// 更早版本留下的缓存没有事实文件，这时退回用目录名 + package.json + 锁文件，
// 仍要能拼出完整提交 sha，避免写进锁文件的是短 sha 而让下一次 probe 永远报「有变化」。
func TestSourceFactsFallsBackToTreeAndLock(t *testing.T) {
	repoRoot := t.TempDir()
	sourceRoot := filepath.Join(repoRoot, "source", "d3b8a3e")
	if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "package.json"), []byte(`{"version":"2.2.2"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SaveLock(filepath.Join(repoRoot, LockFile), Lock{
		Upstream: LockUpstream{
			Commit:     "d3b8a3e908904e242ed2dbe66af62cca71131419",
			CommitDate: "2026-09-28T00:38:49Z",
		},
	}); err != nil {
		t.Fatal(err)
	}
	facts, err := SourceFacts(testPolicy(), repoRoot, sourceRoot)
	if err != nil {
		t.Fatalf("应当能从树和锁文件拼出事实：%v", err)
	}
	if facts.Version != "2.2.2" {
		t.Fatalf("版本应取自 package.json，得到 %q", facts.Version)
	}
	if facts.Commit != "d3b8a3e908904e242ed2dbe66af62cca71131419" {
		t.Fatalf("提交 sha 应补全为完整值，得到 %q", facts.Commit)
	}
	if facts.CommitDate != "2026-09-28T00:38:49Z" {
		t.Fatalf("提交时间应沿用锁文件，得到 %q", facts.CommitDate)
	}
}

// 版本读不到时必须明确失败并指出该重跑 fetch，而不是拿空版本继续往下生成。
func TestSourceFactsRejectsTreeWithoutVersion(t *testing.T) {
	repoRoot := t.TempDir()
	sourceRoot := filepath.Join(repoRoot, "source", "d3b8a3e")
	if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := SourceFacts(testPolicy(), repoRoot, sourceRoot)
	if err == nil {
		t.Fatal("没有版本信息时必须报错")
	}
	if !strings.Contains(err.Error(), SourceFactsFile) {
		t.Fatalf("错误信息应指出缺哪个文件，得到 %q", err.Error())
	}
}

// 传输抖动（TLS 握手超时、连接被重置）重发一次就能恢复：
// 不该让整条定时同步因为一次网络抖动失败。
func TestDoRequestRetriesTransportFailure(t *testing.T) {
	defer silenceBackoff()()

	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			// 第一轮直接断链，模拟传输层抖动。
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			connection, _, err := hijacker.Hijack()
			if err != nil {
				return
			}
			_ = connection.Close()
			return
		}
		_, _ = w.Write([]byte("2.2.2"))
	}))
	defer server.Close()

	request, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := doRequest(request, 1<<20)
	if err != nil {
		t.Fatalf("抖动之后应当自动重试成功，得到 %v", err)
	}
	if string(payload) != "2.2.2" {
		t.Fatalf("应当返回重试后的响应，得到 %q", payload)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("应当恰好请求两次，得到 %d", got)
	}
}

// 4xx 是确定性问题：重试只会白等，必须一次就返回。
func TestDoRequestDoesNotRetryClientError(t *testing.T) {
	defer silenceBackoff()()

	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	request, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := doRequest(request, 1<<20); err == nil {
		t.Fatal("404 必须返回错误")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("404 不应重试，实际请求 %d 次", got)
	}
}

// 5xx 值得重试，但要有上界；耗尽之后错误里要能看出重试过。
func TestDoRequestGivesUpAfterBoundedRetries(t *testing.T) {
	defer silenceBackoff()()

	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	request, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = doRequest(request, 1<<20)
	if err == nil {
		t.Fatal("一直 5xx 时必须失败")
	}
	if got := atomic.LoadInt32(&attempts); got != requestAttempts {
		t.Fatalf("应当尝试 %d 次，实际 %d 次", requestAttempts, got)
	}
	if !strings.Contains(err.Error(), "已重试") {
		t.Fatalf("错误信息应说明重试过，得到 %q", err.Error())
	}
}

// silenceBackoff 把重试间隔压到 0，免得单测为了等退避而变慢。
func silenceBackoff() func() {
	previous := requestBackoff
	requestBackoff = 0
	return func() { requestBackoff = previous }
}

func testPolicy() Policy {
	return Policy{
		Upstream: UpstreamRef{
			Repository: "affaan-m/ECC",
			Package:    "ecc-universal",
			License:    "MIT",
			Holder:     "Affaan Mustafa",
		},
		SkillIDPrefix: "com.mrbaoquan.ecc.skill",
	}
}

// ---- 校对链路 ----

// testPolicyJSON 是一份最小可用的 upstream-policy.json。
// 分类映射是必需的：技能的分类由上游模块推导，模块没映射就是「无法定分类」，
// 那样连校对队列都进不去。
const testPolicyJSON = `{
  "schema_version": 1,
  "upstream": {
    "repository": "affaan-m/ECC",
    "package": "ecc-universal",
    "license": "MIT",
    "holder": "Affaan Mustafa"
  },
  "module_categories": {"core": "software-engineering"},
  "excluded_skills": {},
  "file_policy": {"max_files_per_skill": 40, "max_file_bytes": 1048576, "text_extensions": [".md"]},
  "rewrites": [],
  "skill_id_prefix": "com.mrbaoquan.ecc.skill"
}
`

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeTestSkill(t *testing.T, sourceRoot, slug, name, description string) {
	t.Helper()
	document := "---\nname: " + name + "\ndescription: " + description + "\n---\n\n# " + name + "\n\n正文第一段。\n"
	writeTestFile(t, filepath.Join(sourceRoot, "skills", slug, "SKILL.md"), document)
}

// reviewFixture 造一份最小的「本仓库 + 上游源码树」。
//
// 只用三条技能：alpha 还没人读过，beta 校对过且与当前正文对得上，gamma 校对过
// 但校对之后上游又改过正文。真实仓库有 268 条，可校对链路的判据只有「锁文件里
// 的基线」和「上游正文摘要」两项，三种状态各一条就够把它们分开。
func reviewFixture(t *testing.T) (repoRoot, sourceRoot string) {
	t.Helper()
	repoRoot = t.TempDir()
	sourceRoot = t.TempDir()
	writeTestFile(t, filepath.Join(repoRoot, PolicyFile), testPolicyJSON)
	writeTestFile(t, filepath.Join(sourceRoot, "package.json"), `{"version":"2.2.3"}`)
	writeTestFile(t, filepath.Join(sourceRoot, "manifests", "install-modules.json"),
		`{"modules":[{"id":"core","kind":"skill","paths":["skills/alpha","skills/beta","skills/gamma"]}]}`)
	writeTestSkill(t, sourceRoot, "alpha", "alpha-helper", "Alpha helper for repetitive chores.")
	writeTestSkill(t, sourceRoot, "beta", "beta-helper", "Beta helper for repetitive chores.")
	writeTestSkill(t, sourceRoot, "gamma", "gamma-helper", "Gamma helper for repetitive chores.")
	return repoRoot, sourceRoot
}

func testPolicyOf(t *testing.T, repoRoot string) Policy {
	t.Helper()
	policy, err := LoadPolicy(filepath.Join(repoRoot, PolicyFile))
	if err != nil {
		t.Fatalf("读策略失败：%v", err)
	}
	return policy
}

// sourceDigestOf 直接问上游源码树「这条技能此刻的正文摘要」，避免在测试里
// 手写摘要——手写的摘要一旦对不上，测出来的就不是真实行为。
func sourceDigestOf(t *testing.T, repoRoot, sourceRoot, slug string) string {
	t.Helper()
	source, err := Discover(sourceRoot, testPolicyOf(t, repoRoot))
	if err != nil {
		t.Fatalf("扫描上游源码树失败：%v", err)
	}
	for _, skill := range source.Skills {
		if skill.Slug == slug {
			return skill.Digest
		}
	}
	t.Fatalf("上游源码树里没有 %s", slug)
	return ""
}

func TestReviewQueueSeparatesDerivedStaleAndReviewed(t *testing.T) {
	repoRoot, sourceRoot := reviewFixture(t)
	betaDigest := sourceDigestOf(t, repoRoot, sourceRoot, "beta")
	staleBaseline := "0000000000000000000000000000000000000000000000000000000000000000"
	if err := SaveMetadata(filepath.Join(repoRoot, filepath.FromSlash(MetadataFile)), Metadata{
		SchemaVersion: MetadataVersion,
		Skills: map[string]MetadataEntry{
			"alpha": {Name: "Alpha Helper", Description: "机械推导出来的说明", Source: "derived", Note: "显示名称由上游客名缩写而来"},
			"beta":  {Name: "乙工具", Description: "把乙这件事做完", Categories: []string{"software-engineering"}, Source: "reviewed"},
			"gamma": {Name: "丙工具", Description: "把丙这件事做完", Categories: []string{"software-engineering"}, Source: "reviewed"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := SaveLock(filepath.Join(repoRoot, LockFile), Lock{
		SchemaVersion: LockVersion,
		Upstream:      LockUpstream{Repository: "affaan-m/ECC", Version: "2.2.3", Commit: "d3b8a3e"},
		Skills: map[string]LockSkill{
			"alpha": {Version: "2.2.3", SourceDigest: sourceDigestOf(t, repoRoot, sourceRoot, "alpha")},
			"beta":  {Version: "2.2.3", ReviewedDigest: betaDigest},
			// gamma 的基线是「上一次校对时的正文」，与此刻正文不同 —— 这正是 stale。
			"gamma": {Version: "2.2.3", ReviewedDigest: staleBaseline},
		},
	}); err != nil {
		t.Fatal(err)
	}

	queue, err := BuildReviewQueue(repoRoot, sourceRoot)
	if err != nil {
		t.Fatalf("算校对队列失败：%v", err)
	}
	if queue.Total != 3 || queue.Pending != 1 || queue.Stale != 1 {
		t.Fatalf("待校对 1 条、待重校 1 条，得到 total=%d pending=%d stale=%d", queue.Total, queue.Pending, queue.Stale)
	}
	if len(queue.Items) != 2 {
		t.Fatalf("队列只该有 alpha 与 gamma，得到 %d 条：%+v", len(queue.Items), queue.Items)
	}
	if queue.Items[0].Slug != "alpha" || queue.Items[0].Status != ReviewStatusDerived {
		t.Fatalf("alpha 该是待校对，得到 %+v", queue.Items[0])
	}
	if queue.Items[0].CurrentName != "Alpha Helper" {
		t.Fatalf("待校对条目要显示机械推导出的文案，得到 %q", queue.Items[0].CurrentName)
	}
	if queue.Items[0].UpstreamName != "alpha-helper" {
		t.Fatalf("待校对条目要带上游原文供对照，得到 %q", queue.Items[0].UpstreamName)
	}
	stale := queue.Items[1]
	if stale.Slug != "gamma" || stale.Status != ReviewStatusStale {
		t.Fatalf("gamma 该是待重校，得到 %+v", stale)
	}
	if stale.ReviewedDigest != staleBaseline || stale.CurrentDigest == "" {
		t.Fatalf("待重校条目要同时给出校对基线与当前正文摘要，得到 %+v", stale)
	}
	if stale.CurrentName != "丙工具" {
		t.Fatalf("待重校条目仍要显示市场正在用的文案，得到 %q", stale.CurrentName)
	}
	if queue.ByCategory["software-engineering"] != 2 {
		t.Fatalf("分类统计要算上每个待办条目，得到 %v", queue.ByCategory)
	}

	// beta 校对过且与当前正文对得上：它不该出现在队列里。
	for _, item := range queue.Items {
		if item.Slug == "beta" {
			t.Fatal("已校对且未过期的技能不该出现在队列里")
		}
	}

	if filtered := queue.Filter(ReviewStatusStale, "software-engineering"); len(filtered.Items) != 1 || filtered.Pending != 0 || filtered.Stale != 1 {
		t.Fatalf("按分类收窄待重校应当只剩 gamma，得到 %+v", filtered)
	}
	if filtered := queue.Filter(ReviewStatusDerived, ""); len(filtered.Items) != 1 || filtered.Stale != 0 {
		t.Fatalf("按状态收窄待校对应当只剩 alpha，得到 %+v", filtered)
	}

	markdown := queue.Markdown()
	for _, want := range []string{"待校对 1，待重校 1", "## software-engineering（2）", "alpha-helper"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("清单里应当出现 %q：\n%s", want, markdown)
		}
	}
}

func TestApplyReviewDecisionsIsAllOrNothing(t *testing.T) {
	repoRoot, sourceRoot := reviewFixture(t)
	alphaReviewed := MetadataEntry{Name: "甲工具", Description: "把甲这件事做完", Categories: []string{"software-engineering"}, Source: "reviewed"}
	betaDerived := MetadataEntry{Name: "Beta Helper", Description: "机械推导出来的说明", Categories: []string{"software-engineering"}, Source: "derived", Note: "显示名称由上游客名缩写而来"}
	metadataPath := filepath.Join(repoRoot, filepath.FromSlash(MetadataFile))
	if err := SaveMetadata(metadataPath, Metadata{SchemaVersion: MetadataVersion, Skills: map[string]MetadataEntry{
		"alpha": alphaReviewed,
		"beta":  betaDerived,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := SaveLock(filepath.Join(repoRoot, LockFile), Lock{
		SchemaVersion: LockVersion,
		Skills: map[string]LockSkill{
			"alpha": {Version: "2.2.3"},
			"beta":  {Version: "2.2.3"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// 第 200 条不合法就中途停下，人不知道自己读到哪儿了；所以整批要么全落、要么一条不落。
	_, err := ApplyReviewDecisions(repoRoot, map[string]ReviewDecision{
		"alpha": {Name: "甲工具改名", Description: "把甲这件事做完，而且做得更稳"},
		"beta":  {Name: strings.Repeat("超", 19), Description: "名称超过 18 字上限"},
	}, time.Now())
	if err == nil {
		t.Fatal("名称超过 18 字必须整批拒绝")
	}
	if !strings.Contains(err.Error(), "beta") || !strings.Contains(err.Error(), "18") {
		t.Fatalf("错误信息要点名是哪条、超了什么，得到 %q", err.Error())
	}
	after, err := LoadMetadata(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after.Skills["alpha"], alphaReviewed) || !reflect.DeepEqual(after.Skills["beta"], betaDerived) {
		t.Fatal("整批拒绝时一条都不该落库")
	}

	// slug 写错也要当场拦住，而不是落进一份对不上的元数据表。
	if _, err := ApplyReviewDecisions(repoRoot, map[string]ReviewDecision{
		"ghost": {Name: "不存在的技能", Description: "把不存在这件事做完"},
	}, time.Now()); err == nil || !strings.Contains(err.Error(), "不在同步锁里") {
		t.Fatalf("锁里没有的 slug 必须报错，得到 %v", err)
	}

	result, err := ApplyReviewDecisions(repoRoot, map[string]ReviewDecision{
		"alpha": {Name: "甲工具改名", Description: "把甲这件事做完，而且做得更稳"},
		"beta":  {Name: "乙工具", Description: "把乙这件事做完"},
	}, time.Date(2026, 9, 28, 8, 39, 36, 0, time.UTC))
	if err != nil {
		t.Fatalf("合法的一批应当落库：%v", err)
	}
	if result.DecisionsOf != 2 || len(result.Applied) != 2 || result.Reviewed != 2 || result.Pending != 0 {
		t.Fatalf("落库摘要不对：%+v", result)
	}
	if result.ReviewedAt != "2026-09-28T08:39:36Z" {
		t.Fatalf("落库时间要写进元数据表，得到 %q", result.ReviewedAt)
	}
	after, err = LoadMetadata(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if after.Skills["beta"].Source != "reviewed" || after.Skills["beta"].Name != "乙工具" {
		t.Fatalf("落库后 beta 该是人工校对的文案，得到 %+v", after.Skills["beta"])
	}
	// 推导留痕必须清掉：留着会让下一个人以为这条还没读过。
	if after.Skills["beta"].Note != "" {
		t.Fatalf("校对过后不该再留推导备注，得到 %q", after.Skills["beta"].Note)
	}

	// 同一批再算一次队列：没有待办了。
	queue, err := BuildReviewQueue(repoRoot, sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if queue.Pending != 0 || queue.Stale != 0 || len(queue.Items) != 0 {
		t.Fatalf("两条都校对过之后队列该空，得到 %+v", queue)
	}
}

// 校对基线只在「文案刚被人读过」时刷新。用 SourceDigest 当基线的话，
// 上游一动就会被判成「刚重校过」，待重校标记永远亮不起来 —— 那这条链路的
// 唯一价值就没了。
func TestGenerateMovesReviewBaselineOnlyWhenTextWasRewritten(t *testing.T) {
	repoRoot, sourceRoot := reviewFixture(t)
	if err := SaveMetadata(filepath.Join(repoRoot, filepath.FromSlash(MetadataFile)), Metadata{
		SchemaVersion: MetadataVersion,
		Skills: map[string]MetadataEntry{
			"alpha": {Name: "甲工具", Description: "把甲这件事做完", Source: "reviewed"},
			"beta":  {Name: "乙工具", Description: "把乙这件事做完", Source: "reviewed"},
			"gamma": {Name: "丙工具", Description: "把丙这件事做完", Source: "reviewed"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	upstream := LockUpstream{
		Repository:    "affaan-m/ECC",
		Package:       "ecc-universal",
		Version:       "2.2.3",
		Commit:        "d3b8a3e908904e242ed2dbe66af62cca71131419",
		License:       "MIT",
		LicenseHolder: "Affaan Mustafa",
	}
	input := GenerateInput{
		RepoRoot:   repoRoot,
		SourceRoot: sourceRoot,
		Policy:     testPolicyOf(t, repoRoot),
		Upstream:   upstream,
		Now:        time.Date(2026, 9, 28, 8, 39, 36, 0, time.UTC),
		Tool:       "ecc-sync test",
	}

	first, err := Generate(input)
	if err != nil {
		t.Fatalf("首次生成失败：%v", err)
	}
	if len(first.StaleReviews) != 0 {
		t.Fatalf("刚校对过的文案不该被判成待重校，得到 %v", first.StaleReviews)
	}
	firstLock, err := LoadLock(filepath.Join(repoRoot, LockFile))
	if err != nil {
		t.Fatal(err)
	}
	betaInitialDigest := sourceDigestOf(t, repoRoot, sourceRoot, "beta")
	if firstLock.Skills["beta"].ReviewedDigest != betaInitialDigest {
		t.Fatalf("首次生成要把当前正文记成校对基线，得到 %q", firstLock.Skills["beta"].ReviewedDigest)
	}

	// 上游只动 beta 的正文：基线不该跟着动，标记要亮起来。
	writeTestSkill(t, sourceRoot, "beta", "beta-helper", "Beta helper now covers a different chore entirely.")
	second, err := Generate(input)
	if err != nil {
		t.Fatalf("上游改动后再生成失败：%v", err)
	}
	if len(second.StaleReviews) != 1 || second.StaleReviews[0] != "beta" {
		t.Fatalf("只有 beta 该被标成待重校，得到 %v", second.StaleReviews)
	}
	secondLock, err := LoadLock(filepath.Join(repoRoot, LockFile))
	if err != nil {
		t.Fatal(err)
	}
	if secondLock.Skills["beta"].ReviewedDigest != betaInitialDigest {
		t.Fatalf("上游改正文不该刷新校对基线，否则待重校永远不会亮：%q", secondLock.Skills["beta"].ReviewedDigest)
	}
	betaNewDigest := sourceDigestOf(t, repoRoot, sourceRoot, "beta")
	if secondLock.Skills["beta"].SourceDigest != betaNewDigest || betaNewDigest == betaInitialDigest {
		t.Fatalf("锁文件要跟上新的正文摘要：%+v", secondLock.Skills["beta"])
	}
	// alpha 与 gamma 一个字节没动，它们不该被牵连。
	if firstLock.Skills["alpha"].Version != secondLock.Skills["alpha"].Version {
		t.Fatalf("别的技能没动就不该涨版本：%q → %q", firstLock.Skills["alpha"].Version, secondLock.Skills["alpha"].Version)
	}

	// 人重新读过文案之后，基线跟到新正文，标记熄灭。
	if _, err := ApplyReviewDecisions(repoRoot, map[string]ReviewDecision{
		"beta": {Name: "乙工具", Description: "把乙这件事换成的新做法做完"},
	}, time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("重新校对落库失败：%v", err)
	}
	third, err := Generate(input)
	if err != nil {
		t.Fatalf("重新校对后再生成失败：%v", err)
	}
	if containsString(third.StaleReviews, "beta") {
		t.Fatalf("重新校对过之后不该再是待重校，得到 %v", third.StaleReviews)
	}
	thirdLock, err := LoadLock(filepath.Join(repoRoot, LockFile))
	if err != nil {
		t.Fatal(err)
	}
	if thirdLock.Skills["beta"].ReviewedDigest != betaNewDigest {
		t.Fatalf("重新校对后基线要跟到新正文，得到 %q", thirdLock.Skills["beta"].ReviewedDigest)
	}

	// 再来一次同样的输入：内容没变，同步序号与锁文件都该原地不动。
	before, err := os.ReadFile(filepath.Join(repoRoot, LockFile))
	if err != nil {
		t.Fatal(err)
	}
	fourth, err := Generate(input)
	if err != nil {
		t.Fatalf("重复生成失败：%v", err)
	}
	after, err := os.ReadFile(filepath.Join(repoRoot, LockFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || len(fourth.Changed) != 0 {
		t.Fatalf("上游与元数据都没变时必须是零动作：changed=%v", fourth.Changed)
	}
}
