package eccsync

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/MrBaoquan/himind-extensions/tooling/catalog"
	"github.com/MrBaoquan/himind-extensions/tooling/distribution"
)

// ReleaseRetention 是发布回收策略：每类扩展在自己的发货地里保留最近几个版本。
//
// 为什么要有回收这回事：Release 一旦发出去就是永久的，而同步链是每周跑一次的。
// 插件与工作流每次改代码都涨一个补丁号，一年下来光自己的两件扩展就能堆出几十条
// Release；市场索引里真正需要的只有「当前版本」和「上一版」，更早的版本既没有人
// 回装，又会把仓库首页铺满，让「这次发了什么」彻底看不见。
type ReleaseRetention struct {
	SchemaVersion int `json:"schema_version"`
	// UpdatedAt 是策略最后一次改动的时间，只用于追溯，不参与判定。
	UpdatedAt string `json:"updated_at,omitempty"`
	// KeepVersions 按扩展类型给出保留版本数；缺省用 DefaultKeepVersions。
	KeepVersions map[string]int `json:"keep_versions"`
}

// RetentionVersion 是回收策略文件的结构版本。
const RetentionVersion = 1

// BatchRetentionKey 是批次 Release 在保留策略里的计数键。
//
// 批次不是第四种扩展类型，它是技能的容器；但回收是按 Release 条数算的，批次
// 必须有自己的计数，否则「技能留三版」会被理解成「每一件的三角版本各留一条」，
// 而批次里根本不存在「某一件的某一版」这种可以单独回收的粒度。
const BatchRetentionKey = "batch"

// DefaultKeepVersions 是缺省保留窗口。
//
// 插件与工作流留两版：够覆盖「发新版时发现回归、退回上一版」这条真实路径。
// 技能留三版：技能是按批次发的，批次之间跨度一周，三批约等于近一个月的可回装
// 窗口；早期逐件发出去的技能 Release 也按这个数收。
var DefaultKeepVersions = map[string]int{
	distribution.KindPlugin:   2,
	distribution.KindWorkflow: 2,
	distribution.KindSkill:    3,
	BatchRetentionKey:         3,
}

// LoadReleaseRetention 读回收策略；文件不存在时返回缺省策略，便于首次落地。
//
// 缺省值直接来自 DefaultKeepVersions，而不是写死一份 JSON：策略文件是给人改的，
// 缺省值是给代码用的，两者同源才不会出现「没写文件」与「写了空文件」两种行为。
func LoadReleaseRetention(path string) (ReleaseRetention, error) {
	value := ReleaseRetention{SchemaVersion: RetentionVersion, KeepVersions: map[string]int{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return value, nil
		}
		return ReleaseRetention{}, err
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return ReleaseRetention{}, fmt.Errorf("%s: %w", path, err)
	}
	if value.SchemaVersion != RetentionVersion {
		return ReleaseRetention{}, fmt.Errorf("%s: schema_version 必须是 %d", path, RetentionVersion)
	}
	for kind, count := range value.KeepVersions {
		if count < 1 {
			return ReleaseRetention{}, fmt.Errorf("%s: keep_versions.%s 至少是 1，回收不能把当前版本一起删掉", path, kind)
		}
	}
	return value, nil
}

// SaveReleaseRetention 落盘回收策略，写法与其它策略文件一致。
func SaveReleaseRetention(path string, value ReleaseRetention) error {
	value.SchemaVersion = RetentionVersion
	if value.KeepVersions == nil {
		value.KeepVersions = map[string]int{}
	}
	value.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return writeJSON(path, value)
}

// KeepFor 返回某类扩展的保留版本数，没写就用缺省值。
func (r ReleaseRetention) KeepFor(kind string) int {
	if count, ok := r.KeepVersions[kind]; ok && count > 0 {
		return count
	}
	if count, ok := DefaultKeepVersions[kind]; ok {
		return count
	}
	return 1
}

// KeepMap 摊平成「类型 → 保留数」，带给纯函数用。
func (r ReleaseRetention) KeepMap() map[string]int {
	out := map[string]int{}
	for kind := range DefaultKeepVersions {
		out[kind] = r.KeepFor(kind)
	}
	return out
}

// SupersededRelease 是一条被更新版本取代、可以回收的历史 Release。
type SupersededRelease struct {
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	Version   string `json:"version"`
	Tag       string `json:"tag"`
	ReleaseID int64  `json:"release_id"`
}

// supersedeScope 是一次回收判定的范围：哪些 tag 归本仓库管，哪些被钉住不能删。
//
// 装成一个结构而不是继续加参数：这几份名单都是 map，位置传错一个编译器不会
// 吭声，而回收是真的删远端数据。
type supersedeScope struct {
	// Owners 是本仓库名下的单件扩展归属键（形如 skill/<id>）。
	Owners map[string]bool
	// Batches 是本仓库发过的批次 tag。批次不属于任何单个扩展，只能按 tag 认。
	Batches map[string]bool
	// Protected 是被依赖 pin 钉住的 tag：删掉它，已发布的工作流就指向一个空处。
	Protected map[string]bool
	// Keep 是各类扩展与批次的保留数量。
	Keep map[string]int
}

// planSuperseded 挑出超出保留窗口的历史 Release。
//
// 三条硬规则，任何一条不满足就不回收：
//   - 只碰本仓库名下的东西：tag 认不出归属、或者归属不在名单里，一律跳过。
//   - 每一组至少留一个：单件扩展按 ID 分组，批次合成一组，排序后前 keep 个在窗口里。
//   - 被索引当依赖钉住的 tag 不回收：工作流 pin 住的那个插件版本一旦删掉，
//     已发布的工作流就会指向一个取不到的制品。
//
// 返回顺序按「类型 → 归属 → 版本」排定：回收会真的删远端数据，删了哪几条
// 必须在日志里有确定的次序，否则事后对不上账。
func planSuperseded(records []releaseRecord, scope supersedeScope) []SupersededRelease {
	out := append(supersededReleases(records, scope), supersededBatches(records, scope)...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return catalog.CompareVersions(out[i].Version, out[j].Version) < 0
	})
	return out
}

// supersededReleases 挑出单件发布的、超出保留窗口的历史 Release。
//
// 单件发布有三个来源：插件与工作流一直是逐件发的，ECC 技能则是在批次发布之前
// 逐件发过一轮。两者在这里的处理完全一样——按扩展 ID 分组，各留 keep 个版本。
func supersededReleases(records []releaseRecord, scope supersedeScope) []SupersededRelease {
	type candidate struct {
		tag     string
		id      int64
		version string
	}
	byOwner := map[string][]candidate{}
	for _, record := range records {
		if record.Draft {
			continue
		}
		owner, ok := managedTag(record.Tag)
		if !ok || !scope.Owners[owner] {
			continue
		}
		_, _, version, err := distribution.ParseReleaseTag(record.Tag)
		if err != nil {
			continue
		}
		byOwner[owner] = append(byOwner[owner], candidate{tag: record.Tag, id: record.ID, version: version})
	}

	out := []SupersededRelease{}
	for owner, items := range byOwner {
		kind, id, found := strings.Cut(owner, "/")
		if !found {
			continue
		}
		limit := scope.Keep[kind]
		if limit < 1 {
			limit = 1
		}
		// 版本从大到小：最新的先占保留窗口，剩下的才轮到回收。
		sort.Slice(items, func(i, j int) bool {
			if cmp := catalog.CompareVersions(items[i].version, items[j].version); cmp != 0 {
				return cmp > 0
			}
			return items[i].tag > items[j].tag
		})
		for index, item := range items {
			if index < limit {
				continue
			}
			if scope.Protected[item.tag] {
				continue
			}
			out = append(out, SupersededRelease{
				Kind: kind, ID: id, Version: item.version, Tag: item.tag, ReleaseID: item.id,
			})
		}
	}
	return out
}

// supersededBatches 挑出超出保留窗口的批次 Release。
//
// 批次整体回收，不拆成员：批次 tag 的版本号就是同步序号，一批对应一次同步，
// 「这一批里有两件还要留」不是一个能表达的诉求——要留就该整批留在窗口里。
// 成员是否已被新批次接管，由新批次整批覆盖，所以按批保留天然是安全的。
func supersededBatches(records []releaseRecord, scope supersedeScope) []SupersededRelease {
	type candidate struct {
		tag     string
		id      int64
		version string
	}
	items := make([]candidate, 0, len(records))
	for _, record := range records {
		if record.Draft || !scope.Batches[record.Tag] {
			continue
		}
		version, err := distribution.ParseBatchTag(record.Tag)
		if err != nil {
			continue
		}
		items = append(items, candidate{tag: record.Tag, id: record.ID, version: version})
	}
	limit := scope.Keep[BatchRetentionKey]
	if limit < 1 {
		limit = DefaultKeepVersions[BatchRetentionKey]
	}
	sort.Slice(items, func(i, j int) bool {
		if cmp := catalog.CompareVersions(items[i].version, items[j].version); cmp != 0 {
			return cmp > 0
		}
		return items[i].tag > items[j].tag
	})
	out := []SupersededRelease{}
	for index, item := range items {
		if index < limit {
			continue
		}
		if scope.Protected[item.tag] {
			continue
		}
		out = append(out, SupersededRelease{
			Kind: BatchRetentionKey, ID: item.tag, Version: item.version,
			Tag: item.tag, ReleaseID: item.id,
		})
	}
	return out
}

// pinnedTags 收出索引里被依赖钉住的 Release tag。
//
// 依赖 pin 的 reference 就是发布时的 tag，删掉它等于让已发布的工作流指向一个
// 取不到的制品；这种版本哪怕落在保留窗口之外也不能回收。
func pinnedTags(index *catalog.Catalog) map[string]bool {
	protected := map[string]bool{}
	if index == nil {
		return protected
	}
	for _, entry := range index.AllEntries() {
		dependencies, ok := entry["dependencies"].([]interface{})
		if !ok {
			continue
		}
		for _, raw := range dependencies {
			dependency, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			source, ok := dependency["source"].(map[string]interface{})
			if !ok {
				continue
			}
			if reference, ok := source["reference"].(string); ok && strings.TrimSpace(reference) != "" {
				protected[strings.TrimSpace(reference)] = true
			}
		}
	}
	return protected
}

// PruneOptions 控制一次历史版本回收。
type PruneOptions struct {
	// DryRun 只算出要回收哪几条，不删远端。
	DryRun bool
	// Repository 覆盖仓库写法，用于本地演练。
	Repository string
}

// PruneResult 是一次回收的汇总。
type PruneResult struct {
	Repository string              `json:"repository"`
	DryRun     bool                `json:"dry_run"`
	Keep       map[string]int      `json:"keep_versions"`
	Scanned    int                 `json:"scanned"`
	Pruned     []SupersededRelease `json:"pruned"`
	// DroppedEntries 是被一并摘掉的索引记录数：Release 删了、索引还留着，
	// 市场里就会挂出一条点不开的版本。
	DroppedEntries int      `json:"dropped_entries"`
	Failed         []string `json:"failed,omitempty"`
}

// PruneReleases 回收本仓库名下超出保留窗口的历史 Release，并摘掉索引里的对应记录。
//
// 顺序不能反：先删 Release 再摘索引，中间断掉最多留下一条指向空处的索引记录；
// 反过来先摘索引再删 Release，中间断掉就是「Release 删了、索引还在」的静默坏状态，
// 而这一条恰恰是用户点得到的那条。
func PruneReleases(repoRoot string, options PruneOptions) (PruneResult, error) {
	extensions, err := readExtensions(repoRoot)
	if err != nil {
		return PruneResult{}, err
	}
	repository := strings.TrimSpace(options.Repository)
	if repository == "" {
		repository = extensions.DistributionID
	}
	retention, err := LoadReleaseRetention(filepath.Join(repoRoot, ReleasePolicyFile))
	if err != nil {
		return PruneResult{}, err
	}
	keep := retention.KeepMap()
	result := PruneResult{
		Repository: repository, DryRun: options.DryRun, Keep: keep, Pruned: []SupersededRelease{},
	}
	index, err := catalog.Load(filepath.Join(repoRoot, filepath.FromSlash(CatalogFile)))
	if err != nil {
		return result, err
	}
	records, err := listReleases(repository)
	if err != nil {
		return result, err
	}
	result.Scanned = len(records)
	plan := planSuperseded(records, supersedeScope{
		Owners:    managedOwners(extensions),
		Batches:   managedBatchTags(index),
		Protected: pinnedTags(index),
		Keep:      keep,
	})
	if options.DryRun || len(plan) == 0 {
		result.Pruned = plan
		return result, nil
	}

	prunedTags := make([]string, 0, len(plan))
	for _, item := range plan {
		if _, err := runGH("release", "delete", item.Tag, "--repo", repository, "--yes", "--cleanup-tag"); err != nil {
			result.Failed = append(result.Failed, fmt.Sprintf("%s: %v", item.Tag, err))
			continue
		}
		prunedTags = append(prunedTags, item.Tag)
		result.Pruned = append(result.Pruned, item)
	}
	dropped, err := dropCatalogEntries(repoRoot, prunedTags)
	if err != nil {
		return result, err
	}
	result.DroppedEntries = dropped
	return result, nil
}

// dropCatalogEntries 摘掉已回收 tag 对应的索引记录。
//
// 只摘「tag 确实被回收了」的那些条目：索引还要靠这些记录回答「当前有哪些版本」，
// 按 id 批量删会把最新版本一起带走。
func dropCatalogEntries(repoRoot string, prunedTags []string) (int, error) {
	if len(prunedTags) == 0 {
		return 0, nil
	}
	pruned := make(map[string]bool, len(prunedTags))
	for _, tag := range prunedTags {
		pruned[tag] = true
	}
	path := filepath.Join(repoRoot, filepath.FromSlash(CatalogFile))
	index, err := catalog.Load(path)
	if err != nil {
		return 0, err
	}
	dropped := 0
	for _, kind := range []string{distribution.KindPlugin, distribution.KindSkill, distribution.KindWorkflow} {
		kept := make([]map[string]interface{}, 0, len(index.Entries(kind)))
		for _, entry := range index.Entries(kind) {
			if tag := catalogText(entry, "release_tag"); pruned[tag] {
				dropped++
				continue
			}
			kept = append(kept, entry)
		}
		index.SetEntries(kind, kept)
	}
	if dropped == 0 {
		return 0, nil
	}
	return dropped, index.Save(path)
}

// catalogText 取索引记录里的字符串字段；类型不对或没有就当空串。
//
// 判断只关心「这条记录挂在哪个 tag 下」，读不到就不是要被摘掉的那条，
// 没必要为一个诊断用途的读取把整次回收打断。
func catalogText(entry map[string]interface{}, field string) string {
	value, ok := entry[field].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}
