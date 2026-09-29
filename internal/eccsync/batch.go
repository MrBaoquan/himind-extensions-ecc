package eccsync

import (
	"fmt"
	"sort"
	"time"

	"github.com/MrBaoquan/himind-extensions/tooling/catalog"
	"github.com/MrBaoquan/himind-extensions/tooling/distribution"
	"github.com/MrBaoquan/himind-extensions/tooling/releaseplan"
)

// readyTarget 是一条已经打包完成、等着挂上 Release 的扩展。
//
// position 记它在本次目标列表里的位置：技能最后是整批发出去的，但报告必须按
// 「这次打算发什么」的顺序填，否则读报告的人对不上是哪个目标失败了。
type readyTarget struct {
	position     int
	target       publishTarget
	artifactPath string
	manifestPath string
	// plan 是这次发布的命名事实。批次发布时 Tag 会被改写成批次 tag：制品名与
	// 成员发布清单名照旧属于成员，只有「挂在哪条 Release 上」变成批次的。
	plan releaseplan.Plan
	// release 是成员自己的发布清单，装的是这批资产里属于它的那一份。
	// 批次 Release 提供容器，发布清单仍然一件一份：装谁、校验谁，都只看它。
	release distribution.ReleaseManifest
}

// batchVersion 推出这一批的版本号。
//
// 前两段直接取上游 package.json，第三段是同步序号。上游按自己的节奏发版，我们
// 按周对齐，同一个上游版本会被同步很多次（2.2.2 已经同步过三轮），所以上游的
// Release tag 不能拿来当批次名：同一版本第二次同步就没有名字可用了。带上同步
// 序号，批次名才唯一，上游版本与提交则写进批次清单留档。
//
// 成员版本高于序号推出来的版本时取成员版本：批次号必须盖得住它装的东西。
func batchVersion(lock Lock, members []readyTarget) (string, error) {
	major, minor := versionCore(lock.Upstream.Version)
	if major == 0 && minor == 0 {
		return "", fmt.Errorf("从上游版本 %q 推不出批次版本", lock.Upstream.Version)
	}
	sequence := lock.Sync.Sequence
	if sequence < 1 {
		sequence = 1
	}
	version := fmt.Sprintf("%d.%d.%d", major, minor, sequence)
	for _, member := range members {
		if catalog.CompareVersions(member.target.version, version) > 0 {
			version = member.target.version
		}
	}
	return version, nil
}

// batchUpstream 摘出这批的上游事实，写进批次清单留档。
//
// 溯源不靠 tag：tag 里是本仓库的批次号，上游是谁、哪次提交，只有清单说得清。
func batchUpstream(lock Lock) distribution.BatchUpstream {
	return distribution.BatchUpstream{
		Repository:    lock.Upstream.Repository,
		Package:       lock.Upstream.Package,
		Version:       lock.Upstream.Version,
		Commit:        lock.Upstream.Commit,
		CommitDate:    lock.Upstream.CommitDate,
		License:       lock.Upstream.License,
		LicenseHolder: lock.Upstream.LicenseHolder,
	}
}

// batchNotes 写批次 Release 的说明。
//
// 技能版本是逐件推出来的：上游这一轮动了 266 件，另外两件还停在老内容上。说明里
// 因此要给出覆盖到的版本区间，否则「这批是 2.2.3」就是一句不准的话。
func batchNotes(version string, lock Lock, members []readyTarget) string {
	low, high := versionSpan(members)
	notes := fmt.Sprintf("ECC 技能批次 %s：%d 件技能", version, len(members))
	if low != "" && high != "" && low != high {
		notes += fmt.Sprintf("，版本覆盖 %s 到 %s", low, high)
	}
	notes += fmt.Sprintf("\n\n上游：%s %s @%s", lock.Upstream.Repository, lock.Upstream.Version, shortSHA(lock.Upstream.Commit))
	if lock.Upstream.CommitDate != "" {
		notes += fmt.Sprintf("（%s）", lock.Upstream.CommitDate)
	}
	return notes
}

// versionSpan 取这批成员里最低与最高的制品版本。
func versionSpan(members []readyTarget) (string, string) {
	low, high := "", ""
	for _, member := range members {
		version := member.target.version
		if low == "" || catalog.CompareVersions(version, low) < 0 {
			low = version
		}
		if high == "" || catalog.CompareVersions(version, high) > 0 {
			high = version
		}
	}
	return low, high
}

// batchManifest 拼出批次 Release 的总清单。
//
// 成员按类型 + ID 排序：同一批内容重复发布时，清单必须逐字节一样，否则「这批跟
// 上一批有没有差别」就得靠人眼比对 JSON 顺序。
func batchManifest(
	tag, version, repository, channel, sourceCommit string,
	upstream distribution.BatchUpstream,
	members []distribution.BatchMember,
	now time.Time,
) distribution.BatchManifest {
	sort.SliceStable(members, func(i, j int) bool {
		if members[i].Kind != members[j].Kind {
			return members[i].Kind < members[j].Kind
		}
		return members[i].ID < members[j].ID
	})
	return distribution.BatchManifest{
		SchemaVersion: distribution.BatchManifestSchema,
		Repository:    repository,
		Tag:           tag,
		Version:       version,
		Channel:       channel,
		SourceCommit:  sourceCommit,
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Upstream:      upstream,
		Members:       members,
	}
}

// batchMemberVersions 把批次清单收成「成员键 → 版本」，键形如 skill/<id>。
//
// 回收要用它回答「这个旧批次的成员，在新批次里都还在吗」；只按成员键取版本，
// 是因为一件技能在一批里只能有一个版本，版本高低才是接管与否的判断依据。
func batchMemberVersions(manifest distribution.BatchManifest) map[string]string {
	out := make(map[string]string, len(manifest.Members))
	for _, member := range manifest.Members {
		out[member.Kind+"/"+member.ID] = member.Version
	}
	return out
}
