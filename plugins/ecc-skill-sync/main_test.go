package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MrBaoquan/himind-extensions-ecc/internal/eccsync"
)

// reviewQueueOf 造一份交错着待校对与待重校的队列，条数与状态计数都算好。
func reviewQueueOf(slugs ...string) eccsync.ReviewQueue {
	queue := eccsync.ReviewQueue{Total: len(slugs), Items: []eccsync.ReviewItem{}}
	for index, slug := range slugs {
		item := eccsync.ReviewItem{Slug: slug, Status: eccsync.ReviewStatusDerived}
		if index%2 == 1 {
			item.Status = eccsync.ReviewStatusStale
			queue.Stale++
		} else {
			queue.Pending++
		}
		queue.Items = append(queue.Items, item)
	}
	return queue
}

// 截断明细可以，改计数不行：定时任务靠 pending/stale 判断要不要提醒，
// 明细只是给人顺着读的。
func TestReviewSummaryTruncatesItemsWithoutChangingCounts(t *testing.T) {
	summary := reviewSummary(reviewQueueOf("a", "b", "c", "d"), 2)
	if summary["total"] != 4 || summary["pending"] != 2 || summary["stale"] != 2 {
		t.Fatalf("计数不该被截断改变：%+v", summary)
	}
	if summary["truncated"] != 2 {
		t.Fatalf("被截掉的条数不对：%+v", summary["truncated"])
	}
	items, ok := summary["items"].([]eccsync.ReviewItem)
	if !ok || len(items) != 2 {
		t.Fatalf("明细应留下前 2 条：%+v", summary["items"])
	}
	if items[0].Slug != "a" || items[1].Slug != "b" {
		t.Fatalf("明细顺序应保持队列顺序：%+v", items)
	}
}

func TestReviewSummaryWithoutLimitKeepsEveryItem(t *testing.T) {
	summary := reviewSummary(reviewQueueOf("a", "b", "c"), 0)
	items, ok := summary["items"].([]eccsync.ReviewItem)
	if !ok || len(items) != 3 {
		t.Fatalf("limit 为 0 时不该截断：%+v", summary["items"])
	}
	if summary["truncated"] != 0 {
		t.Fatalf("没有截断时不该报出条数：%+v", summary["truncated"])
	}
}

// 被分发策略挡下的技能必须留在发布报告里。
//
// 定时任务没人看着：少发了几条，唯一能被事后发现的地方就是这份报告。
// 只报一个条数不够——「被分类、模块还是它自己被排除」决定了这件事该找谁改。
func TestPublishReportKeepsExcludedItemsOnDisk(t *testing.T) {
	repoRoot := t.TempDir()
	value := eccsync.PublishResult{
		Repository: "MrBaoquan/himind-extensions-ecc",
		Channel:    "beta",
		DryRun:     true,
		Pending:    1,
		Excluded:   8,
		ExcludedItems: []eccsync.ExcludedItem{{
			Slug:       "video-post",
			ID:         "com.mrbaoquan.ecc.skill.video-post",
			Version:    "2.2.3",
			Module:     "media-generation",
			Categories: []string{"video-post"},
			Rule:       eccsync.DispatchRuleCategory,
			Keyword:    "video-post",
			Reason:     "团队不做视频生成",
		}},
		Items: []eccsync.PublishItem{{Extension: "blueprint", Status: "dry-run"}},
	}

	artifact, err := writeReport(repoRoot, "publish", publishReport(repoRoot, value))
	if err != nil {
		t.Fatalf("发布报告没落盘：%v", err)
	}
	uri, ok := artifact["uri"].(string)
	if !ok || !strings.HasPrefix(uri, "file:///") {
		t.Fatalf("报告信封应给出本地文件 URI：%+v", artifact)
	}
	data, err := os.ReadFile(filepath.FromSlash(strings.TrimPrefix(uri, "file:///")))
	if err != nil {
		t.Fatalf("按报告信封找不到文件：%v", err)
	}
	var written map[string]any
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatalf("报告不是合法 JSON：%v", err)
	}
	if written["kind"] != "publish" {
		t.Fatalf("报告应标明这是发布报告：%+v", written["kind"])
	}
	publishValue, ok := written["publish"].(map[string]any)
	if !ok {
		t.Fatalf("报告应带上发布结论：%+v", written["publish"])
	}
	if publishValue["excluded"] != float64(8) {
		t.Fatalf("跳过条数应落进报告：%+v", publishValue["excluded"])
	}
	items, ok := publishValue["excluded_items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("跳过明细应落进报告：%+v", publishValue["excluded_items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("跳过明细的形状不对：%+v", items[0])
	}
	if item["rule"] != eccsync.DispatchRuleCategory || item["reason"] != "团队不做视频生成" {
		t.Fatalf("明细没记住被哪条规则挡下、为什么：%+v", item)
	}

	// 上游锚点读不到时（空仓库）报告照写，只是少一块锚点。
	if _, present := written["upstream"]; !present {
		t.Fatalf("锁文件缺失时也应写出上游锚点字段：%+v", written)
	}
}
