package eccsync

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MrBaoquan/himind-extensions/tooling/distribution"
)

// 可重试的失败：退避后重发，成功就返回结果。
func TestRunWithRetryRecoversFromTransientFailure(t *testing.T) {
	attempts := 0
	value, err := runWithRetry(3, 0, func() (string, bool, error) {
		attempts++
		if attempts < 3 {
			return "", true, errors.New("TLS handshake timeout")
		}
		return "ok", false, nil
	})
	if err != nil {
		t.Fatalf("抖动之后应当成功，得到 %v", err)
	}
	if value != "ok" {
		t.Fatalf("应当返回第三次的结果，得到 %q", value)
	}
	if attempts != 3 {
		t.Fatalf("应当尝试三次，实际 %d 次", attempts)
	}
}

// 不可重试的失败：一次就返回，不要白等。
func TestRunWithRetryStopsOnNonRetryableFailure(t *testing.T) {
	attempts := 0
	_, err := runWithRetry(3, 0, func() (string, bool, error) {
		attempts++
		return "", false, errors.New("HTTP 404")
	})
	if err == nil {
		t.Fatal("确定性失败必须报错")
	}
	if attempts != 1 {
		t.Fatalf("不应重试，实际尝试 %d 次", attempts)
	}
	if strings.Contains(err.Error(), "已重试") {
		t.Fatalf("没有重试就不该标注重试次数，得到 %q", err.Error())
	}
}

// 重试用尽：错误里要能看出重试过，排查时不会把抖动误判成配置错。
func TestRunWithRetryAnnotatesExhaustedAttempts(t *testing.T) {
	attempts := 0
	_, err := runWithRetry(3, 0, func() (string, bool, error) {
		attempts++
		return "", true, errors.New("connection reset by peer")
	})
	if err == nil {
		t.Fatal("一直失败时必须报错")
	}
	if attempts != 3 {
		t.Fatalf("应当尝试三次，实际 %d 次", attempts)
	}
	if !strings.Contains(err.Error(), "已重试 2 次") {
		t.Fatalf("错误信息应标注重试次数，得到 %q", err.Error())
	}
}

// 退避间隔要真的生效，否则重试等于连着打三次，起不到等待恢复的作用。
func TestRunWithRetryWaitsBetweenAttempts(t *testing.T) {
	started := time.Now()
	_, _ = runWithRetry(2, 20*time.Millisecond, func() (string, bool, error) {
		return "", true, errors.New("i/o timeout")
	})
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond {
		t.Fatalf("两次尝试之间应当等待，实际只过了 %v", elapsed)
	}
}

// 退避要指数增长且有上限：连着的失败不能一直按同一个间隔重打。
func TestRetryDelayGrowsThenSaturates(t *testing.T) {
	base := 2 * time.Second
	// 抖动只加不减，所以下限就是「约定值」，上限是约定值 + 20%。
	check := func(attempt int, wantBase time.Duration) {
		t.Helper()
		got := retryDelay(base, attempt)
		if got < wantBase {
			t.Fatalf("第 %d 次尝试的等待不该小于 %v，得到 %v", attempt, wantBase, got)
		}
		if got > wantBase+wantBase/5+time.Millisecond {
			t.Fatalf("第 %d 次尝试的等待不该超过 %v 太多，得到 %v", attempt, wantBase, got)
		}
	}
	check(2, 2*time.Second)
	check(3, 4*time.Second)
	check(4, 8*time.Second)
	check(5, 16*time.Second)
	// 再往上翻就该被上限截住，别把定时任务拖成十几分钟。
	check(6, retryMaxBackoff)
	check(20, retryMaxBackoff)
	if got := retryDelay(base, 1); got != 0 {
		t.Fatalf("第一次尝试不该等，得到 %v", got)
	}
	if got := retryDelay(0, 3); got != 0 {
		t.Fatalf("没有配置退避就不该等，得到 %v", got)
	}
}

// 网络类报错文本要判成可重试，确定性报错要判成不可重试。
func TestIsTransientClassifiesRealWorldMessages(t *testing.T) {
	transient := []string{
		`Get "https://raw.githubusercontent.com/x/y/z/package.json": net/http: TLS handshake timeout`,
		"dial tcp 20.205.243.161:443: connectex: A connection attempt failed because the connected party did not properly respond",
		"error checking for existing release: net/http: TLS handshake timeout",
		"unexpected EOF",
		"HTTP 502: Bad Gateway",
		"secondary rate limit exceeded",
	}
	for _, text := range transient {
		if !isTransient(text) {
			t.Fatalf("应当判成可重试: %q", text)
		}
	}
	permanent := []string{
		"",
		"a release with the same tag name already exists",
		"HTTP 422: Validation Failed",
		"release not found",
		"gh: Not Found (HTTP 404)",
	}
	for _, text := range permanent {
		if isTransient(text) {
			t.Fatalf("不该判成可重试: %q", text)
		}
	}
}

// Release 列表要按 `--slurp` 的「每页一个数组」结构解，且跨页都要认。
func TestParseReleasesSpansPages(t *testing.T) {
	output := []byte(`[
		[
			{"id": 1, "tag_name": "plugin/com.mrbaoquan.ecc-skill-sync@1.0.2", "draft": false, "html_url": "https://example.invalid/1.0.2", "assets": [{"name": "a"}]},
			{"id": 2, "tag_name": "plugin/com.mrbaoquan.ecc-skill-sync@1.0.3", "draft": false, "html_url": "https://example.invalid/1.0.3", "assets": [{"name": "b"}]}
		],
		[
			{"id": 3, "tag_name": "workflow/com.mrbaoquan.workflow.ecc-skill-sync@1.0.4", "draft": false, "html_url": "https://example.invalid/w", "assets": []},
			{"id": 4, "tag_name": "plugin/com.mrbaoquan.ecc-skill-sync@1.0.3", "draft": true, "html_url": "https://example.invalid/draft", "assets": [{"name": "b"}]}
		]
	]`)
	records, err := parseReleases(output)
	if err != nil {
		t.Fatalf("解析 Release 列表失败: %v", err)
	}
	if len(records) != 4 {
		t.Fatalf("跨页的记录不能丢，4 条都要在，实际 %d 条", len(records))
	}
	matched := recordsForTag(records, "plugin/com.mrbaoquan.ecc-skill-sync@1.0.3")
	if len(matched) != 2 {
		t.Fatalf("同 tag 的两条记录都要认出来（正式 + Draft），实际 %d 条", len(matched))
	}
	if matched[0].Draft || !matched[1].Draft {
		t.Fatalf("Draft 标记要保留，实际 draft=%v/%v", matched[0].Draft, matched[1].Draft)
	}
}

// 列表为空不是错误，是「这个 tag 还没发过」，上层据此去建 Release。
func TestParseReleasesEmptyListIsNotAnError(t *testing.T) {
	records, err := parseReleases([]byte(`[[]]`))
	if err != nil {
		t.Fatalf("空列表不该报错: %v", err)
	}
	if matched := recordsForTag(records, "plugin/x@1.0.0"); len(matched) != 0 {
		t.Fatalf("空列表不该有匹配，实际 %d 条", len(matched))
	}
}

// 输出不是 JSON 时必须报错：把解析失败吞成「没发过」，会去重复建 Release。
func TestParseReleasesRejectsGarbage(t *testing.T) {
	if _, err := parseReleases([]byte("gh: Not Found (HTTP 404)")); err == nil {
		t.Fatal("非 JSON 输出必须报错，不能当成尚未发布")
	}
}

// 本轮真实出现过的脏状态：同名 Draft 与正式 Release 并存。
// 正式那条决定「在不在、资产有哪些」，Draft 只标记待清理。
func TestReleaseStateFromRecordsTakesFormalAndFlagsDraft(t *testing.T) {
	state := releaseStateFromRecords([]releaseRecord{
		{ID: 398024431, Tag: "plugin/x@1.0.3", Draft: true,
			Assets: []releaseAsset{{Name: "x.hmpkg"}}},
		{ID: 398025657, Tag: "plugin/x@1.0.3", Draft: false, URL: "https://example.invalid/1.0.3",
			Assets: []releaseAsset{{Name: "x.hmpkg"}, {Name: "manifest.json"}}},
	})
	if !state.Exists {
		t.Fatal("有正式 Release 时必须判成已发布")
	}
	if state.URL != "https://example.invalid/1.0.3" {
		t.Fatalf("地址应取自正式那条，得到 %q", state.URL)
	}
	if !state.Assets["x.hmpkg"] || !state.Assets["manifest.json"] {
		t.Fatalf("资产应取自正式那条，得到 %v", state.Assets)
	}
	if !state.Drafts[398024431] {
		t.Fatalf("残留 Draft 必须被标记出来，得到 %v", state.Drafts)
	}
	if state.Drafts[398025657] {
		t.Fatal("正式 Release 不能被当成待清理的 Draft")
	}
}

// 只有 Draft（上次卡在挂资产那一步）时：不能算已发布，上线前要把这条半成品清掉。
func TestReleaseStateFromRecordsDraftOnlyMeansNotPublished(t *testing.T) {
	state := releaseStateFromRecords([]releaseRecord{
		{ID: 7, Tag: "plugin/x@1.0.3", Draft: true},
	})
	if state.Exists {
		t.Fatal("只有 Draft 不算已发布，否则索引会指向一条查不到的 Release")
	}
	if !state.Drafts[7] {
		t.Fatalf("半成品要标记出来，得到 %v", state.Drafts)
	}
}

// Draft 的清理次序要确定：map 遍历顺序随机，日志对不上就没法排查。
func TestSortedDraftIDsIsAscending(t *testing.T) {
	got := sortedDraftIDs(map[int64]bool{398025657: true, 12: true, 398024431: true})
	want := []int64{12, 398024431, 398025657}
	if len(got) != len(want) {
		t.Fatalf("条数不符，得到 %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("应按 id 升序，得到 %v", got)
		}
	}
	if ids := sortedDraftIDs(map[int64]bool{}); len(ids) != 0 {
		t.Fatalf("没有 Draft 时应当为空，得到 %v", ids)
	}
}

// tag 的归属键要剥掉版本，且只认本仓库发的那三种类型。
func TestManagedTagRecognizesOwnedNamespaces(t *testing.T) {
	cases := map[string]string{
		"plugin/com.mrbaoquan.ecc-skill-sync@1.0.3":            "plugin/com.mrbaoquan.ecc-skill-sync",
		"workflow/com.mrbaoquan.workflow.ecc-skill-sync@1.0.5": "workflow/com.mrbaoquan.workflow.ecc-skill-sync",
		"skill/com.mrbaoquan.ecc.skill.search-first@2.2.2":     "skill/com.mrbaoquan.ecc.skill.search-first",
		"skill/com.mrbaoquan.ecc.skill.a@b@1.0.0":              "skill/com.mrbaoquan.ecc.skill.a@b",
		// 批次 tag 不属于任何单个扩展，归属键就是 tag 自己。
		"batch/2.2.3": "batch/2.2.3",
	}
	for tag, want := range cases {
		got, ok := managedTag(tag)
		if !ok {
			t.Fatalf("%q 应当认得出来", tag)
		}
		if got != want {
			t.Fatalf("%q 的归属键应为 %q，得到 %q", tag, want, got)
		}
	}
	strangers := []string{
		"v1.0.0",
		"release-2026",
		"plugin/",
		"@1.0.0",
		"docs/com.mrbaoquan.ecc-skill-sync@1.0.0",
		"",
	}
	for _, tag := range strangers {
		if _, ok := managedTag(tag); ok {
			t.Fatalf("%q 不是本仓库的发版名字，不该认", tag)
		}
	}
}

// 清扫范围要精确到扩展 ID：别人发的 Release、本仓库不存在这个扩展的 tag 都不碰。
func TestStaleDraftIDsOnlyTouchesManagedOwners(t *testing.T) {
	managed := map[string]bool{
		"plugin/com.mrbaoquan.ecc-skill-sync":            true,
		"workflow/com.mrbaoquan.workflow.ecc-skill-sync": true,
	}
	records := []releaseRecord{
		// 本仓库名下、已经不会再发一次的旧版本半成品：要清。
		{ID: 398024431, Tag: "plugin/com.mrbaoquan.ecc-skill-sync@1.0.3", Draft: true},
		// 正式发布不是半成品，别动。
		{ID: 398025657, Tag: "plugin/com.mrbaoquan.ecc-skill-sync@1.0.3", Draft: false},
		// 同名扩展但不在名单里（这条链条之外的 ID）：不碰。
		{ID: 5, Tag: "plugin/com.other.thing@0.1.0", Draft: true},
		// 认不出归属的名字：不碰。
		{ID: 6, Tag: "v2.0.0", Draft: true},
		// 本仓库名下、当前版本的半成品：要清（重跑时它会和正式那条撞车）。
		{ID: 7, Tag: "workflow/com.mrbaoquan.workflow.ecc-skill-sync@1.0.5", Draft: true},
	}
	got := staleDraftIDs(records, managed, map[string]bool{})
	want := []int64{7, 398024431}
	if len(got) != len(want) {
		t.Fatalf("应清 %v，得到 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("应清 %v（升序），得到 %v", want, got)
		}
	}
}

// 清扫名单来自 extensions.json，插件/工作流/技能三类都要进名单。
func TestManagedOwnersCoversAllThreeKinds(t *testing.T) {
	owners := managedOwners(extensionsDocument{Extensions: []extensionEntry{
		{Type: distribution.KindPlugin, ID: "p"},
		{Type: distribution.KindWorkflow, ID: "w"},
		{Type: distribution.KindSkill, ID: "s"},
	}})
	for _, want := range []string{"plugin/p", "workflow/w", "skill/s"} {
		if !owners[want] {
			t.Fatalf("名单里缺少 %q，得到 %v", want, owners)
		}
	}
}
