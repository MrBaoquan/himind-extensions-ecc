package eccsync

import (
	"strings"
	"testing"
	"time"

	"github.com/MrBaoquan/himind-extensions/tooling/catalog"
	"github.com/MrBaoquan/himind-extensions/tooling/distribution"
)

// batchLock 造一份只填批次版本推导会用到的字段的锁文件。
func batchLock(upstreamVersion string, sequence int) Lock {
	return Lock{
		Upstream: LockUpstream{Repository: "affaan-m/ECC", Package: "ecc", Version: upstreamVersion, Commit: "0b9a1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b"},
		Sync:     LockSync{Sequence: sequence, Tool: "ecc-sync"},
	}
}

// batchSkill 造一个已打包完成、等着挂上批次 Release 的技能成员。
func batchSkill(slug, version string) readyTarget {
	return readyTarget{
		target: publishTarget{
			kind:    distribution.KindSkill,
			id:      "com.mrbaoquan.ext.ecc." + slug,
			version: version,
			slug:    slug,
		},
	}
}

// 批次版本的四段是「上游 major.minor.patch + 同步序号」。
//
// 这条是整套命名的地基：归类看前两段（对应上游哪一版），唯一性看第三、四段。
// 上游 2.2.2 会被同步很多次（实际已经三轮），没有序号，第二次同步就没有名字可用。
func TestBatchVersionFollowsUpstreamSequence(t *testing.T) {
	cases := []struct {
		upstream string
		sequence int
		want     string
	}{
		{upstream: "2.2.2", sequence: 1, want: "2.2.2.1"},
		{upstream: "2.2.2", sequence: 3, want: "2.2.2.3"},
		// 上游喜欢写 v2.2.2，前缀不能带进批次版本。
		{upstream: "v2.2.2", sequence: 2, want: "2.2.2.2"},
		// 序号没写（老锁文件）时按下限 1 算，不能推出版本 0。
		{upstream: "2.2.2", sequence: 0, want: "2.2.2.1"},
		// 上游换版时前两段跟着换，patch 落在第三段。
		{upstream: "2.3.0", sequence: 4, want: "2.3.0.4"},
		// 上游只写两段时 patch 按 0 算，批次名仍是四段。
		{upstream: "2.3", sequence: 4, want: "2.3.0.4"},
	}
	for _, item := range cases {
		got, err := batchVersion(batchLock(item.upstream, item.sequence))
		if err != nil {
			t.Fatalf("上游 %s 序号 %d 推版本失败：%v", item.upstream, item.sequence, err)
		}
		if got != item.want {
			t.Fatalf("上游 %s 序号 %d 应推出 %s，实际 %s", item.upstream, item.sequence, item.want, got)
		}
	}
}

// 批次名之间必须能排序：回收按批次版本挑保留窗口，倒序就会把新批次当旧的回收掉。
//
// 同步序号会涨到两位数，所以比的必须是数字段而不是字符串：按字符串排，"10" 会
// 排到 "9" 前面。
func TestBatchVersionsStayOrdered(t *testing.T) {
	older, err := batchVersion(batchLock("2.2.2", 9))
	if err != nil {
		t.Fatalf("推版本失败：%v", err)
	}
	newer, err := batchVersion(batchLock("2.2.2", 10))
	if err != nil {
		t.Fatalf("推版本失败：%v", err)
	}
	if catalog.CompareVersions(older, newer) >= 0 {
		t.Fatalf("第 10 次同步的批次应排在第 9 次之后：%s / %s", older, newer)
	}
	next, err := batchVersion(batchLock("2.3.0", 1))
	if err != nil {
		t.Fatalf("推版本失败：%v", err)
	}
	if catalog.CompareVersions(newer, next) >= 0 {
		t.Fatalf("上游升版后的批次应排在上一上游版本之后：%s / %s", newer, next)
	}
}

// 上游版本读不出来时不能猜一个批次版本：批次 tag 一旦落下去就是永久的。
func TestBatchVersionRejectsUnparsableUpstream(t *testing.T) {
	if _, err := batchVersion(batchLock("nightly", 1)); err == nil {
		t.Fatal("上游版本不是 X.Y 形式时应报错，而不是推一个批次版本出来")
	}
}

// 说明要写全「上游哪一版、第几次同步」，并给出覆盖到的版本区间。
//
// 上游版本会被同步很多轮，只写版本号分不出这批是第几次；成员版本是逐件推的，
// 一批里本来就可能有高有低，不写区间「这批是 2.2.3」就是一句不准的话。
func TestBatchNotesReportVersionSpan(t *testing.T) {
	lock := batchLock("2.2.2", 3)
	lock.Upstream.CommitDate = "2026-09-28"
	notes := batchNotes("2.2.2.3", lock, []readyTarget{
		batchSkill("search-first", "2.2.1"),
		batchSkill("verification-loop", "2.2.3"),
		batchSkill("data-quality", "2.2.2"),
	})
	for _, want := range []string{
		"2.2.2.3", "上游 2.2.2", "本仓第 3 次同步", "3 件技能",
		"2.2.1 到 2.2.3", "affaan-m/ECC", "2026-09-28",
	} {
		if !strings.Contains(notes, want) {
			t.Fatalf("批次说明缺少 %q：\n%s", want, notes)
		}
	}
}

// 版本一致时不写区间：写「2.2.3 到 2.2.3」只会占位置。
func TestBatchNotesOmitSpanWhenUniform(t *testing.T) {
	notes := batchNotes("2.2.2.3", batchLock("2.2.2", 3), []readyTarget{
		batchSkill("search-first", "2.2.3"),
		batchSkill("verification-loop", "2.2.3"),
	})
	if strings.Contains(notes, " 到 ") {
		t.Fatalf("成员版本一致时不该写区间：\n%s", notes)
	}
}

// 成员顺序按类型 + ID 定死：同一批内容重复发布时清单必须逐字节一样，
// 否则「这批跟上一批有没有差别」只能靠人眼比对 JSON 顺序。
func TestBatchManifestSortsMembersByKindThenID(t *testing.T) {
	members := []distribution.BatchMember{
		{Kind: distribution.KindSkill, ID: "com.example.zeta", Version: "2.2.3"},
		{Kind: distribution.KindPlugin, ID: "com.example.beta", Version: "1.1.0"},
		{Kind: distribution.KindSkill, ID: "com.example.alpha", Version: "2.2.3"},
	}
	manifest := batchManifest("batch/2.2.2.3", "2.2.2.3", "MrBaoquan/himind-extensions-ecc", "beta",
		"0b9a1c2", distribution.BatchUpstream{Version: "2.2.2", Commit: "0b9a1c2"}, members,
		time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC))
	got := make([]string, 0, len(manifest.Members))
	for _, member := range manifest.Members {
		got = append(got, member.Kind+"/"+member.ID)
	}
	want := []string{
		distribution.KindPlugin + "/com.example.beta",
		distribution.KindSkill + "/com.example.alpha",
		distribution.KindSkill + "/com.example.zeta",
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("成员应排成 %v，实际 %v", want, got)
		}
	}
}

// 批次清单里的成员版本按「类型/ID」取，一件技能在一批里只能有一个版本。
func TestBatchMemberVersionsKeyedByKindAndID(t *testing.T) {
	manifest := distribution.BatchManifest{Members: []distribution.BatchMember{
		{Kind: distribution.KindSkill, ID: "com.example.alpha", Version: "2.2.1"},
		{Kind: distribution.KindSkill, ID: "com.example.beta", Version: "2.2.3"},
	}}
	versions := batchMemberVersions(manifest)
	if versions[distribution.KindSkill+"/com.example.alpha"] != "2.2.1" {
		t.Fatalf("成员版本表少了 alpha：%v", versions)
	}
	if versions[distribution.KindSkill+"/com.example.beta"] != "2.2.3" {
		t.Fatalf("成员版本表少了 beta：%v", versions)
	}
	if len(versions) != 2 {
		t.Fatalf("成员版本表应只有两条，实际 %v", versions)
	}
}
