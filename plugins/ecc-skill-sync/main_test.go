package main

import (
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
