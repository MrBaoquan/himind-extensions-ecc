package eccsync

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// 校对闸门上的三种状态。
//
// 派生条目发不出去，不是因为规则写错了，而是因为「上游 frontmatter 的机械
// 推导」只够撑起一个占位符：名字会被压到 18 字上限，说明会从句号后面断掉。
// 这类文案摆在市场里只会让用户挑花眼，所以它们停在待校对队列里。
//
//   - derived：还没人读过，只发 skill 包、不发市场
//   - stale：人读过，但读完之后上游正文又变了，市场照常可用，只是要重校一遍
//   - reviewed：校对过，且与当前上游正文对得上
const (
	ReviewStatusDerived  = "derived"
	ReviewStatusStale    = "stale"
	ReviewStatusReviewed = "reviewed"
)

// ReviewItem 是校对队列里的一条技能。
//
// 上游事实与当前文案并排放，是因为校对要做的判断只有一个：
// 「这条技能实际是干什么的，中文该叫什么、该怎么用一句话说清」。
// 于是队列里必须有上游原文，否则人得自己回去翻源码。
type ReviewItem struct {
	Slug       string   `json:"slug"`
	SkillID    string   `json:"skill_id"`
	Version    string   `json:"version,omitempty"`
	Status     string   `json:"status"`
	Categories []string `json:"categories"`
	// UpstreamName / UpstreamDescription 是上游 frontmatter 原文，校对时的依据。
	UpstreamName        string `json:"upstream_name"`
	UpstreamDescription string `json:"upstream_description"`
	// CurrentName / CurrentDescription 是此刻 market 上会显示的文案。
	// 派生条目显示的是机械推导结果；stale 条目显示的是上次人工校对的结果。
	CurrentName        string `json:"current_name"`
	CurrentDescription string `json:"current_description"`
	Note               string `json:"note,omitempty"`
	// Files / BodyChars 让人一眼看出这条技能有多重：一个 Markdown 还是带几十个
	// references 的整包，校对投入的判断成本不一样。
	Files     int `json:"files"`
	BodyChars int `json:"body_chars"`
	// ReviewedDigest 只在 stale 条目上出现，用于说明「校对基线」与当前正文的差别。
	ReviewedDigest string `json:"reviewed_digest,omitempty"`
	CurrentDigest  string `json:"current_digest,omitempty"`
}

// ReviewQueue 是一次待校对清单。
//
// 清单是算出来的，不落盘：落盘就会和 metadata.json 抢「谁是真相」，
// 而上游每天都在动，两份真相只会越走越远。
type ReviewQueue struct {
	GeneratedAt    string         `json:"generated_at"`
	Repository     string         `json:"repository"`
	UpstreamCommit string         `json:"upstream_commit"`
	Total          int            `json:"total"`
	Pending        int            `json:"pending"`
	Stale          int            `json:"stale"`
	ByCategory     map[string]int `json:"by_category"`
	Items          []ReviewItem   `json:"items"`
}

// BuildReviewQueue 算出此刻还欠着人工校对的技能。
//
// 口径是「锁文件里已经生成、但市场文案还没落到 reviewed」：被隔离的技能压根
// 没进过仓库，它们该出现在 quarantine.json 里，不该混进校对队列。
func BuildReviewQueue(repoRoot, sourceRoot string) (ReviewQueue, error) {
	policy, err := LoadPolicy(filepath.Join(repoRoot, PolicyFile))
	if err != nil {
		return ReviewQueue{}, err
	}
	SetSkillIDPrefix(policy.SkillIDPrefix)
	if strings.TrimSpace(sourceRoot) == "" {
		sourceRoot, err = CachedSource(repoRoot)
		if err != nil {
			return ReviewQueue{}, err
		}
	}
	source, err := Discover(sourceRoot, policy)
	if err != nil {
		return ReviewQueue{}, err
	}
	metadata, err := LoadMetadata(filepath.Join(repoRoot, filepath.FromSlash(MetadataFile)))
	if err != nil {
		return ReviewQueue{}, err
	}
	lock, err := LoadLock(filepath.Join(repoRoot, LockFile))
	if err != nil {
		return ReviewQueue{}, err
	}

	queue := ReviewQueue{
		GeneratedAt:    time.Now().UTC().Format(time.RFC3339),
		Repository:     policy.Upstream.Repository,
		UpstreamCommit: lock.Upstream.Commit,
		ByCategory:     map[string]int{},
		Items:          []ReviewItem{},
	}
	for _, skill := range source.Skills {
		locked, ok := lock.Skills[skill.Slug]
		if !ok {
			continue
		}
		current := metadata.Skills[skill.Slug]
		if current.Source != "reviewed" {
			categories, _, categoryErr := categoriesOf(skill, policy)
			if categoryErr != nil {
				continue
			}
			derived, derivedOK := DeriveEntry(skill, categories)
			if !derivedOK {
				continue
			}
			current = derived
		}
		item := ReviewItem{
			Slug:                skill.Slug,
			SkillID:             SkillID(skill.Slug),
			Version:             locked.Version,
			Categories:          append([]string(nil), current.Categories...),
			UpstreamName:        strings.TrimSpace(skill.Frontmatter["name"]),
			UpstreamDescription: normalizeSpace(skill.Frontmatter["description"]),
			CurrentName:         current.Name,
			CurrentDescription:  current.Description,
			Note:                current.Note,
			Files:               len(skill.Files),
			BodyChars:           utf8.RuneCountInString(documentOf(skill)),
		}
		switch {
		case current.Source != "reviewed":
			item.Status = ReviewStatusDerived
		case locked.ReviewedDigest != "" && locked.ReviewedDigest != skill.Digest:
			// 校对过的文案还在市场里跑，只是上游正文又动了：标成待重校，
			// 但不撤下——下架对用户是倒退，对校对没有帮助。
			item.Status = ReviewStatusStale
			item.ReviewedDigest = locked.ReviewedDigest
			item.CurrentDigest = skill.Digest
		default:
			continue
		}
		queue.Items = append(queue.Items, item)
	}
	sort.Slice(queue.Items, func(i, j int) bool { return queue.Items[i].Slug < queue.Items[j].Slug })
	queue.Total = len(lock.Skills)
	for _, item := range queue.Items {
		if item.Status == ReviewStatusStale {
			queue.Stale++
		} else {
			queue.Pending++
		}
		for _, category := range item.Categories {
			queue.ByCategory[category]++
		}
	}
	return queue, nil
}

// Filter 按状态与分类收窄队列，方便一次只推一批。
func (q ReviewQueue) Filter(status, category string) ReviewQueue {
	filtered := q
	filtered.Items = []ReviewItem{}
	filtered.Pending = 0
	filtered.Stale = 0
	filtered.ByCategory = map[string]int{}
	for _, item := range q.Items {
		if status != "" && item.Status != status {
			continue
		}
		if category != "" && !containsString(item.Categories, category) {
			continue
		}
		if item.Status == ReviewStatusStale {
			filtered.Stale++
		} else {
			filtered.Pending++
		}
		for _, name := range item.Categories {
			filtered.ByCategory[name]++
		}
		filtered.Items = append(filtered.Items, item)
	}
	return filtered
}

// Markdown 把队列排成一份可以直接读的清单。
//
// 排序按「分类 → slug」而不是按字母：校对是成批做的，同一分类里的技能
// 说的是同一件事，连着读比跳着读快得多，也不容易两批写出互相打架的文案。
func (q ReviewQueue) Markdown() string {
	byCategory := map[string][]ReviewItem{}
	autoCategory := map[string]string{}
	for _, item := range q.Items {
		category := "未分类"
		for _, name := range item.Categories {
			if _, ok := validCategories[name]; ok {
				category = name
				break
			}
		}
		byCategory[category] = append(byCategory[category], item)
		for _, name := range item.Categories {
			if name != category {
				autoCategory[item.Slug] = name
			}
		}
	}
	categories := make([]string, 0, len(byCategory))
	for category := range byCategory {
		categories = append(categories, category)
	}
	sort.Strings(categories)

	var builder strings.Builder
	fmt.Fprintf(&builder, "# ECC 待校对技能：%d 条（待校对 %d，待重校 %d）\n\n", len(q.Items), q.Pending, q.Stale)
	for _, category := range categories {
		items := byCategory[category]
		sort.Slice(items, func(i, j int) bool { return items[i].Slug < items[j].Slug })
		fmt.Fprintf(&builder, "## %s（%d）\n\n", category, len(items))
		for _, item := range items {
			fmt.Fprintf(&builder, "- `%s` [%s]", item.Slug, item.Status)
			if extra := autoCategory[item.Slug]; extra != "" {
				fmt.Fprintf(&builder, " 备用分类 %s", extra)
			}
			builder.WriteString("\n")
			fmt.Fprintf(&builder, "  - 上游名称：%s\n", firstLine(item.UpstreamName))
			fmt.Fprintf(&builder, "  - 上游说明：%s\n", firstLine(item.UpstreamDescription))
			fmt.Fprintf(&builder, "  - 当前文案：%s —— %s\n", firstLine(item.CurrentName), firstLine(item.CurrentDescription))
			if item.Note != "" {
				fmt.Fprintf(&builder, "  - 推导留痕：%s\n", firstLine(item.Note))
			}
		}
		builder.WriteString("\n")
	}
	return builder.String()
}

func firstLine(value string) string {
	trimmed := strings.Join(strings.Fields(value), " ")
	const limit = 300
	if utf8.RuneCountInString(trimmed) <= limit {
		return trimmed
	}
	return string([]rune(trimmed)[:limit]) + "…"
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// ReviewDecision 是一条人工校对结论。
//
// 只有名字与说明是可填的：分类由上游模块推导而来，改它属于改搬运策略，
// 该落在 upstream-policy.json，而不是让校对顺手把技能挪个货架。
type ReviewDecision struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ReviewApplyResult 是一次落库的结果。
type ReviewApplyResult struct {
	Applied     []string `json:"applied"`
	Reviewed    int      `json:"reviewed_total"`
	Pending     int      `json:"pending_remaining"`
	ReviewedAt  string   `json:"reviewed_at"`
	DecisionsOf int      `json:"decisions"`
}

// ApplyReviewDecisions 把人工校对结论落进 manifests/metadata.json。
//
// 全批要么全落、要么一条不落：校对是一条条读出来的，如果 300 条里第 200 条
// 名字超长就中途停下，人根本不知道自己读到哪儿了。所以先整体校验，再整体写。
//
// 上游正文的校对基线不在这里记：它是 generate 按「元数据摘要变了」推出的，
// 这样「人改了文案」和「上游改了正文」两件事只有一个判据，不会互相覆盖。
func ApplyReviewDecisions(repoRoot string, decisions map[string]ReviewDecision, now time.Time) (ReviewApplyResult, error) {
	result := ReviewApplyResult{Applied: []string{}}
	if len(decisions) == 0 {
		return result, fmt.Errorf("decisions 是空的：没有要落库的校对结论")
	}
	policy, err := LoadPolicy(filepath.Join(repoRoot, PolicyFile))
	if err != nil {
		return result, err
	}
	SetSkillIDPrefix(policy.SkillIDPrefix)
	lock, err := LoadLock(filepath.Join(repoRoot, LockFile))
	if err != nil {
		return result, err
	}
	metadataPath := filepath.Join(repoRoot, filepath.FromSlash(MetadataFile))
	metadata, err := LoadMetadata(metadataPath)
	if err != nil {
		return result, err
	}

	slugs := make([]string, 0, len(decisions))
	for slug := range decisions {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)

	problems := []string{}
	for _, slug := range slugs {
		decision := decisions[slug]
		if _, ok := lock.Skills[slug]; !ok {
			problems = append(problems, fmt.Sprintf("%s：不在同步锁里，无法校对（是不是 slug 写错了）", slug))
			continue
		}
		if err := validateReviewedText(decision.Name, decision.Description); err != nil {
			problems = append(problems, fmt.Sprintf("%s：%v", slug, err))
			continue
		}
		categories := metadata.Skills[slug].Categories
		for _, category := range categories {
			if _, ok := validCategories[category]; !ok {
				problems = append(problems, fmt.Sprintf("%s：分类 %q 不在 HiMind 允许的 11 个分类里", slug, category))
				break
			}
		}
	}
	if len(problems) > 0 {
		return result, fmt.Errorf("校对结论有 %d 处不合法，这次不落库：\n%s", len(problems), strings.Join(problems, "\n"))
	}

	for _, slug := range slugs {
		decision := decisions[slug]
		entry := metadata.Skills[slug]
		entry.Name = strings.TrimSpace(decision.Name)
		entry.Description = strings.TrimSpace(decision.Description)
		entry.Source = "reviewed"
		// note 记的是「机械推导时妥协在哪」，人工校对过之后它已经没有意义；
		// 留着会让下一个人以为这条还没读过。
		entry.Note = ""
		metadata.Skills[slug] = entry
		result.Applied = append(result.Applied, slug)
	}
	metadata.ReviewedAt = now.UTC().Format(time.RFC3339)
	if err := SaveMetadata(metadataPath, metadata); err != nil {
		return result, err
	}
	result.DecisionsOf = len(slugs)
	result.ReviewedAt = metadata.ReviewedAt
	result.Reviewed = 0
	result.Pending = 0
	for _, entry := range metadata.Skills {
		if entry.Source == "reviewed" {
			result.Reviewed++
		} else {
			result.Pending++
		}
	}
	return result, nil
}

// validateReviewedText 是校对文案的硬约束，与 resolveEntry 用的是同一组数字。
//
// 这里提前报错，是为了让「落库成功但生成失败」不可能发生：市场里的名称栏位
// 就 18 字宽，说明栏位就 120 字宽，超出去的部分会被界面吃掉。
func validateReviewedText(name, description string) error {
	name = strings.TrimSpace(name)
	description = strings.TrimSpace(description)
	if name == "" || description == "" {
		return fmt.Errorf("name 与 description 都不能为空")
	}
	if length := utf8.RuneCountInString(name); length > 18 {
		return fmt.Errorf("显示名称 %d 字超过 18 上限", length)
	}
	if length := utf8.RuneCountInString(description); length > 120 {
		return fmt.Errorf("用途说明 %d 字超过 120 上限", length)
	}
	return nil
}

// LoadDecisions 读取一份校对结论文件。
func LoadDecisions(path string) (map[string]ReviewDecision, error) {
	decisions := map[string]ReviewDecision{}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &decisions); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return decisions, nil
}
