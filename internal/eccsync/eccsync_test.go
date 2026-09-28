package eccsync

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

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
