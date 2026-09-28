package eccsync

import (
	"strings"
	"testing"
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
	ready, held := splitByMetadataSource([]string{"reviewed-skill", "derived-skill", "unknown-skill"}, metadata)
	if len(ready) != 1 || ready[0] != "reviewed-skill" {
		t.Fatalf("只有 reviewed 条目可发布，得到 %v", ready)
	}
	if len(held) != 2 || held[0] != "derived-skill" || held[1] != "unknown-skill" {
		t.Fatalf("derived 与缺失条目都必须挂起，得到 %v", held)
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
