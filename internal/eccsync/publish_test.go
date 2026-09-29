package eccsync

import (
	"testing"

	"github.com/MrBaoquan/himind-extensions/tooling/distribution"
)

// 建 Release 必须显式把 tag 钉在来源提交上。
//
// 不钉的话 gh 会按默认分支当前的 HEAD 建 tag：本地提交还没推时，tag 就落到
// 别人的提交上，资产全对、源码对不上，而且没有任何一步会报错。这条测试把
// 「--target 必须在、必须等于来源提交」钉成契约。
func TestReleaseCreateArgsPinsTagToSourceCommit(t *testing.T) {
	const commit = "5881b2e0d9eb1cb8d762f24e6bdb1aa7d9de9f79"
	args := releaseCreateArgs(
		"MrBaoquan/himind-extensions-ecc",
		"plugin/com.mrbaoquan.ecc-skill-sync@1.1.6",
		PublishItem{ID: "com.mrbaoquan.ecc-skill-sync", Version: "1.1.6"},
		"dist/artifact.hmpkg",
		"dist/manifest.json",
		distribution.ReleaseManifest{SourceCommit: commit},
	)

	if args[0] != "release" || args[1] != "create" {
		t.Fatalf("应该建 Release：%v", args)
	}
	if args[2] != "plugin/com.mrbaoquan.ecc-skill-sync@1.1.6" {
		t.Fatalf("第三个参数应该是 tag：%v", args)
	}
	index := -1
	for i, arg := range args {
		if arg == "--target" {
			index = i
			break
		}
	}
	if index < 0 {
		t.Fatalf("缺 --target，tag 会落到默认分支 HEAD 上：%v", args)
	}
	if index+1 >= len(args) || args[index+1] != commit {
		t.Fatalf("--target 必须指向来源提交 %s：%v", commit, args)
	}
}

// 来源提交读不到时不能拼出一个空的 --target：gh 会当场报参数错，
// 而那时制品已经打包、签名已经做完，白跑一轮。
func TestReleaseCreateArgsOmitsTargetWithoutSourceCommit(t *testing.T) {
	args := releaseCreateArgs(
		"MrBaoquan/himind-extensions-ecc",
		"plugin/com.mrbaoquan.ecc-skill-sync@1.1.6",
		PublishItem{ID: "com.mrbaoquan.ecc-skill-sync", Version: "1.1.6"},
		"dist/artifact.hmpkg",
		"dist/manifest.json",
		distribution.ReleaseManifest{},
	)
	for _, arg := range args {
		if arg == "--target" {
			t.Fatalf("没有来源提交时不该带 --target：%v", args)
		}
	}
}

// 只有「远端不认识这个提交」才硬停。
//
// 这是回归测试的核心：1.1.1~1.1.5 五个 tag 全部落到了 1.1.0 那个提交上，
// 因为发布时分支还没推。限流、网络抖动只是这次问不出来，把它们也当成
// 「没推」会让无人值守的定时任务在自己家网络打嗝时整条停住。
func TestRevisionMissingReadsGitHubMiss(t *testing.T) {
	cases := map[string]bool{
		// 查提交接口实测回的就是这一句：422 加 No commit found for SHA，不是 404。
		`gh api repos/mrbaoquan/himind-extensions-ecc/commits/6dbb2ac --jq .sha: exit status 1: {"message":"No commit found for SHA: 6dbb2ac65074213aa8f938f5ce93167610f75782","status":"422"}: gh: No commit found for SHA: 6dbb2ac65074213aa8f938f5ce93167610f75782 (HTTP 422)`: true,
		"gh: Not Found (HTTP 404)":                                             true,
		"gh: API rate limit exceeded for installation. (HTTP 429)":             false,
		"gh api: exit status 1: dial tcp: lookup api.github.com: no such host": false,
	}
	for message, want := range cases {
		if got := revisionMissing(message); got != want {
			t.Fatalf("判错了一次：%q 应该判成 %v", message, want)
		}
	}
}

// 提交号读不到时必须停下，不能靠猜一个目标去建 tag。
func TestEnsureRevisionOnRemoteRefusesEmptyRevision(t *testing.T) {
	if err := ensureRevisionOnRemote("MrBaoquan/himind-extensions-ecc", "  "); err == nil {
		t.Fatal("读不到提交号时应该拦住，否则 tag 会落到默认分支 HEAD 上")
	}
}
