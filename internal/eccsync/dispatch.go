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

// DispatchPolicyFile 是分发策略文件，与 upstream-policy.json 并排放在仓库根。
//
// 两份策略管的是两件事：upstream-policy 决定「哪些上游技能值得搬进仓库」，
// 分发策略决定「已经搬进来的技能里，哪些继续往外发」。合成一份的话，
// 「不想发」会被记成「不想收」——技能从仓库里消失，用户既看不见也回不来。
//
// 放在仓库里而不是插件私有目录，是因为它是内容事实：谁不发、为什么，
// 要能进版本控制、能被评审、能回滚，也要让定时任务读到同一份策略。
const DispatchPolicyFile = "dispatch-policy.json"

// DispatchPolicyVersion 是 dispatch-policy.json 的结构版本。
const DispatchPolicyVersion = 1

// 分发判定的三类规则。越具体的先判定：单条技能 > 上游模块 > HiMind 分类。
//
// 三档同时存在是有用的：按分类排掉一整片（例如「测试质量」里全是上游
// 内部流程）、按上游模块更精确地排掉其中一块（例如 framework-language 里
// 只排掉 Kotlin 系）、偶尔单条技能需要单独豁免。
const (
	DispatchRuleSkill    = "skill"
	DispatchRuleModule   = "module"
	DispatchRuleCategory = "category"
)

// DefaultDispatchReason 是没写原因时的兜底文案。
//
// 原因不是装饰：半年后有人问「这条技能怎么不更新了」，只有原因能回答。
const DefaultDispatchReason = "按分发策略排除"

// DispatchPolicy 决定哪些技能参与分发。
//
// 三个集合都是「被排除项 → 原因」。缺省语义是「全部参与分发」，
// 所以文件不存在、字段缺失都等于全部分发，不做反向白名单。
type DispatchPolicy struct {
	SchemaVersion int `json:"schema_version"`
	// UpdatedAt 记录最后一次人工改动策略的时间，供界面显示。
	UpdatedAt string `json:"updated_at,omitempty"`
	// ExcludedModules 按上游模块排除，键是 manifests/modules.json 里的模块名。
	ExcludedModules map[string]string `json:"excluded_modules"`
	// ExcludedCategories 按 HiMind 功能分类排除，键是 11 个合法分类之一。
	ExcludedCategories map[string]string `json:"excluded_categories"`
	// ExcludedSkills 按单条技能排除，键是上游 slug。
	ExcludedSkills map[string]string `json:"excluded_skills"`
}

// NewDispatchPolicy 返回一份「全部参与分发」的空策略。
func NewDispatchPolicy() DispatchPolicy {
	return DispatchPolicy{
		SchemaVersion:      DispatchPolicyVersion,
		ExcludedModules:    map[string]string{},
		ExcludedCategories: map[string]string{},
		ExcludedSkills:     map[string]string{},
	}
}

// LoadDispatchPolicy 读取分发策略。
//
// 文件不存在时返回空策略而不是报错：老仓库升级上来时还没有这份文件，
// 报错会让整条同步链在安装的第一天就停摆。文件存在但版本号不对则报错——
// 那种情况说明有人写了本工具读不懂的策略，静默忽略等于悄悄改了分发范围。
func LoadDispatchPolicy(path string) (DispatchPolicy, error) {
	value := NewDispatchPolicy()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return value, nil
		}
		return DispatchPolicy{}, err
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return DispatchPolicy{}, fmt.Errorf("%s: %w", path, err)
	}
	if value.SchemaVersion != DispatchPolicyVersion {
		return DispatchPolicy{}, fmt.Errorf("%s: schema_version 必须是 %d", path, DispatchPolicyVersion)
	}
	value.normalize()
	return value, nil
}

// SaveDispatchPolicy 写回分发策略。
//
// 所有文案都过一遍 trim，空原因补成兜底文案，键排序交给 json.Marshal——
// 同一份意图每次写出的字节都一样，策略改动在 diff 里只剩真的变化。
func SaveDispatchPolicy(path string, value DispatchPolicy) error {
	if value.SchemaVersion == 0 {
		value.SchemaVersion = DispatchPolicyVersion
	}
	value.normalize()
	return writeJSON(path, value)
}

// normalize 补齐缺失集合、清理空白并给空原因兜底。
func (p *DispatchPolicy) normalize() {
	if p.ExcludedModules == nil {
		p.ExcludedModules = map[string]string{}
	}
	if p.ExcludedCategories == nil {
		p.ExcludedCategories = map[string]string{}
	}
	if p.ExcludedSkills == nil {
		p.ExcludedSkills = map[string]string{}
	}
	p.ExcludedModules = tidyReasons(p.ExcludedModules)
	p.ExcludedCategories = tidyReasons(p.ExcludedCategories)
	p.ExcludedSkills = tidyReasons(p.ExcludedSkills)
}

// tidyReasons 丢掉空键、修剪原因文本，并给空原因补上兜底文案。
func tidyReasons(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, reason := range source {
		trimmedKey := strings.TrimSpace(key)
		if trimmedKey == "" {
			continue
		}
		reason = strings.Join(strings.Fields(reason), " ")
		if reason == "" {
			reason = DefaultDispatchReason
		}
		result[trimmedKey] = reason
	}
	return result
}

// Empty 判断这份策略是不是「全部参与分发」。
func (p DispatchPolicy) Empty() bool {
	return len(p.ExcludedModules) == 0 && len(p.ExcludedCategories) == 0 && len(p.ExcludedSkills) == 0
}

// ExcludedCount 是三个集合的条目总数，只用于展示。
func (p DispatchPolicy) ExcludedCount() int {
	return len(p.ExcludedModules) + len(p.ExcludedCategories) + len(p.ExcludedSkills)
}

// DispatchSubject 是一次分发判定要看的技能事实。
type DispatchSubject struct {
	Slug       string
	Module     string
	Categories []string
}

// DispatchDecision 是一条技能的判定结论。
type DispatchDecision struct {
	Excluded bool
	// Rule 是命中的规则名（skill / module / category），未排除时为空。
	Rule string
	// Keyword 是命中的那个键，界面上要能指出「是哪一条把你排掉了」。
	Keyword string
	Reason  string
}

// Decide 按「单条技能 > 上游模块 > 功能分类」的顺序判定一条技能。
//
// 三个集合都是显式排除，所以判定结果与遍历顺序无关；这里固定优先级只是为了
// 让「原因」稳定——一条技能同时被分类和单条规则排掉时，永远显示更具体的那个原因。
func (p DispatchPolicy) Decide(subject DispatchSubject) DispatchDecision {
	if reason, ok := p.ExcludedSkills[strings.TrimSpace(subject.Slug)]; ok {
		return DispatchDecision{Excluded: true, Rule: DispatchRuleSkill, Keyword: subject.Slug, Reason: reason}
	}
	if module := strings.TrimSpace(subject.Module); module != "" {
		if reason, ok := p.ExcludedModules[module]; ok {
			return DispatchDecision{Excluded: true, Rule: DispatchRuleModule, Keyword: module, Reason: reason}
		}
	}
	for _, category := range subject.Categories {
		category = strings.TrimSpace(category)
		if category == "" {
			continue
		}
		if reason, ok := p.ExcludedCategories[category]; ok {
			return DispatchDecision{Excluded: true, Rule: DispatchRuleCategory, Keyword: category, Reason: reason}
		}
	}
	return DispatchDecision{}
}

// ModuleMap 是 manifests/modules.json 的可读形态：上游技能属于哪个模块，
// 模块又映射到哪个 HiMind 分类。分发策略靠它把「一条技能」落到「一类技能」。
type ModuleMap struct {
	Categories map[string]string
	Skills     map[string]string
}

// LoadModuleMap 读取模块归属表；文件不存在时返回空表。
//
// 空表不会让分发停摆，只会退化成「只按分类与单条技能判定」——
// 模块级规则命中不了，但工具照常跑得下去。这一点很重要：模块表是 generate
// 的产物，克隆下来还没跑过同步的仓库里根本没有它。
func LoadModuleMap(path string) (ModuleMap, error) {
	value := ModuleMap{Categories: map[string]string{}, Skills: map[string]string{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return value, nil
		}
		return ModuleMap{}, err
	}
	var document moduleDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return ModuleMap{}, fmt.Errorf("%s: %w", path, err)
	}
	if document.Categories != nil {
		value.Categories = document.Categories
	}
	if document.Skills != nil {
		value.Skills = document.Skills
	}
	return value, nil
}

// ModuleOf 返回技能所属的上游模块，没有记录时返回空串。
func (m ModuleMap) ModuleOf(slug string) string {
	return strings.TrimSpace(m.Skills[strings.TrimSpace(slug)])
}

// DispatchContext 是一次分发判定需要的全部依据。
//
// 把「策略 + 模块表 + 分类兜底」收在一起，是因为发布、界面、报告三处都要
// 判同一件事。分开传参的写法看着更直白，代价是三处各写一遍取值顺序，
// 早晚会有一处顺序不一样，于是界面说「会发」而发布任务跳过。
type DispatchContext struct {
	Policy  DispatchPolicy
	Modules ModuleMap
	// Fallback 是模块 → 分类的兜底表，来自 upstream-policy.json。
	// modules.json 是生成物，刚克隆的仓库里还没有它，那时模块级分类只能靠这份兜底。
	Fallback map[string]string
}

// CategoriesOf 取一条技能的分类：人工校对过的元数据优先，其次按上游模块推导。
func (c DispatchContext) CategoriesOf(entry MetadataEntry, module string) []string {
	if len(entry.Categories) > 0 {
		return append([]string(nil), entry.Categories...)
	}
	if module == "" {
		return []string{}
	}
	if category := c.Modules.Categories[module]; category != "" {
		return []string{category}
	}
	if category := c.Fallback[module]; category != "" {
		return []string{category}
	}
	return []string{}
}

// Decide 按策略判定一条技能，省掉调用方自己拼 DispatchSubject。
func (c DispatchContext) Decide(slug, module string, categories []string) DispatchDecision {
	return c.Policy.Decide(DispatchSubject{Slug: slug, Module: module, Categories: categories})
}

// 技能此刻的分发状态。
//
//   - distributed：当前版本已经在市场索引里
//   - pending：策略允许、文案也校对过，只是还没发出去
//   - held：策略允许，但市场文案还是机械推导的，过不了校对闸门
//   - excluded：被分发策略排除，后续同步不会再发它的新版本
const (
	DistributionDistributed = "distributed"
	DistributionPending     = "pending"
	DistributionHeld        = "held"
	DistributionExcluded    = "excluded"
)

// DistributionItem 是一条技能此刻的分发状态。
type DistributionItem struct {
	Slug       string   `json:"slug"`
	SkillID    string   `json:"skill_id"`
	Name       string   `json:"name,omitempty"`
	Version    string   `json:"version"`
	Module     string   `json:"module,omitempty"`
	Categories []string `json:"categories"`
	SourcePath string   `json:"source_path,omitempty"`
	Tier       string   `json:"tier,omitempty"`
	State      string   `json:"state"`
	// Detail 对 excluded 是排除原因，对 held 是为什么还上不了市场。
	Detail string `json:"detail,omitempty"`
	// Rule 与 Keyword 只在 excluded 时有值，指出是哪一类规则把这条排掉的。
	Rule    string `json:"rule,omitempty"`
	Keyword string `json:"keyword,omitempty"`
	// PublishedVersions 是这条技能历史上发出去的版本数。
	// 被排除之后它不会归零——已经发出去的版本不追回，只是不再更新。
	PublishedVersions int `json:"published_versions"`
}

// DistributionTotals 是总览计数，永远按全量算，不受界面筛选影响。
type DistributionTotals struct {
	Skills      int `json:"skills"`
	Distributed int `json:"distributed"`
	Pending     int `json:"pending"`
	Held        int `json:"held"`
	Excluded    int `json:"excluded"`
	// Frozen 是「已经被排除、但历史上发过版本」的条数。
	// 这批技能在市场里还在，只是停更了，和「从没发过」要分开看。
	Frozen      int `json:"frozen"`
	Quarantined int `json:"quarantined"`
	Published   int `json:"published_versions"`
}

// DistributionGroup 是一个分类或模块的分发概览。
type DistributionGroup struct {
	Key      string `json:"key"`
	Category string `json:"category,omitempty"`
	Total    int    `json:"total"`
	Excluded int    `json:"excluded"`
	// Reason 只在整组被策略排除时有值。
	Reason string `json:"reason,omitempty"`
	// Rule 是命中这个键的规则名（category / module）。
	Rule string `json:"rule,omitempty"`
}

// DistributionReport 是发布管理界面一次要读的全部事实。
type DistributionReport struct {
	GeneratedAt string `json:"generated_at"`
	RepoRoot    string `json:"repo_root"`
	Repository  string `json:"repository"`
	// UpstreamCommit 与 Sequence 让人确认这份状态对应哪一次同步。
	UpstreamCommit string `json:"upstream_commit"`
	CommitDate     string `json:"commit_date,omitempty"`
	Sequence       int    `json:"sequence"`
	PolicyPath     string `json:"policy_path"`
	PolicyFile     string `json:"policy_file"`
	// Policy 原样回给界面：界面不需要自己拼状态，改完再整份写回。
	Policy     DispatchPolicy      `json:"policy"`
	Totals     DistributionTotals  `json:"totals"`
	Categories []DistributionGroup `json:"categories"`
	Modules    []DistributionGroup `json:"modules"`
	Items      []DistributionItem  `json:"items"`
	// ModuleCategories 是模块 → 分类的映射，界面按它把模块挂到分类下。
	ModuleCategories map[string]string `json:"module_categories"`
}

// DistributionOptions 收窄返回的技能明细，方便界面按分类逐个看。
type DistributionOptions struct {
	State    string
	Category string
	Module   string
	Keyword  string
}

// BuildDistribution 算出「哪些技能参与分发、各自卡在哪一步」。
//
// 只读：它不碰远端也不写仓库，所以界面可以随便刷新。判定口径与 Publish 完全
// 一致（同一份策略、同一份索引、同一份元数据表），否则界面会报「待发布 3 条」
// 而发布任务一条都不发。
func BuildDistribution(repoRoot string, options DistributionOptions) (DistributionReport, error) {
	policy, err := LoadPolicy(filepath.Join(repoRoot, PolicyFile))
	if err != nil {
		return DistributionReport{}, err
	}
	SetSkillIDPrefix(policy.SkillIDPrefix)
	dispatchPolicyPath := filepath.Join(repoRoot, DispatchPolicyFile)
	dispatchPolicy, err := LoadDispatchPolicy(dispatchPolicyPath)
	if err != nil {
		return DistributionReport{}, err
	}
	modules, err := LoadModuleMap(filepath.Join(repoRoot, filepath.FromSlash(ModulesFile)))
	if err != nil {
		return DistributionReport{}, err
	}
	lock, err := LoadLock(filepath.Join(repoRoot, LockFile))
	if err != nil {
		return DistributionReport{}, err
	}
	metadata, err := LoadMetadata(filepath.Join(repoRoot, filepath.FromSlash(MetadataFile)))
	if err != nil {
		return DistributionReport{}, err
	}
	index, err := catalog.Load(filepath.Join(repoRoot, filepath.FromSlash(CatalogFile)))
	if err != nil {
		return DistributionReport{}, err
	}
	quarantine, err := LoadQuarantine(filepath.Join(repoRoot, filepath.FromSlash(QuarantineFile)))
	if err != nil {
		return DistributionReport{}, err
	}
	published := publishedVersions(index)

	report := DistributionReport{
		GeneratedAt:      time.Now().UTC().Format(time.RFC3339),
		RepoRoot:         repoRoot,
		Repository:       policy.Upstream.Repository,
		UpstreamCommit:   lock.Upstream.Commit,
		CommitDate:       lock.Upstream.CommitDate,
		Sequence:         lock.Sync.Sequence,
		PolicyPath:       dispatchPolicyPath,
		PolicyFile:       DispatchPolicyFile,
		Policy:           dispatchPolicy,
		Categories:       []DistributionGroup{},
		Modules:          []DistributionGroup{},
		Items:            []DistributionItem{},
		ModuleCategories: map[string]string{},
	}
	for module, category := range modules.Categories {
		report.ModuleCategories[module] = category
	}
	context := DispatchContext{Policy: dispatchPolicy, Modules: modules, Fallback: policy.ModuleCategories}

	all := make([]DistributionItem, 0, len(lock.Skills))
	moduleTotals := map[string]*DistributionGroup{}
	categoryTotals := map[string]*DistributionGroup{}
	for slug, item := range lock.Skills {
		entry := metadata.Skills[slug]
		module := modules.ModuleOf(slug)
		categories := context.CategoriesOf(entry, module)
		id := SkillID(slug)
		versions := published[distribution.KindSkill+"/"+id]
		decision := context.Decide(slug, module, categories)

		state := DistributionPending
		detail := ""
		switch {
		case decision.Excluded:
			state = DistributionExcluded
			detail = decision.Reason
		case versions[item.Version]:
			state = DistributionDistributed
		case entry.Source != "reviewed":
			state = DistributionHeld
			detail = "市场文案还是机械推导的，校对后才发"
		}
		record := DistributionItem{
			Slug:              slug,
			SkillID:           id,
			Name:              entry.Name,
			Version:           item.Version,
			Module:            module,
			Categories:        categories,
			SourcePath:        item.SourcePath,
			Tier:              item.Tier,
			State:             state,
			Detail:            detail,
			Rule:              decision.Rule,
			Keyword:           decision.Keyword,
			PublishedVersions: len(versions),
		}
		all = append(all, record)
		report.Totals.Published += len(versions)
		report.Totals.Skills++
		switch state {
		case DistributionDistributed:
			report.Totals.Distributed++
		case DistributionPending:
			report.Totals.Pending++
		case DistributionHeld:
			report.Totals.Held++
		case DistributionExcluded:
			report.Totals.Excluded++
			if len(versions) > 0 {
				report.Totals.Frozen++
			}
		}
		if module != "" {
			group := moduleTotals[module]
			if group == nil {
				group = &DistributionGroup{Key: module}
				moduleTotals[module] = group
			}
			group.Total++
		}
		for _, category := range categories {
			group := categoryTotals[category]
			if group == nil {
				group = &DistributionGroup{Key: category}
				categoryTotals[category] = group
			}
			group.Total++
		}
	}
	report.Totals.Quarantined = len(quarantine.Entries)

	// 分组上的「已排除」按策略本身算，不按技能算：一个模块被排除时，
	// 它下面有多少条技能与「这一块要不要发」是两个问题，界面上要能分别回答。
	for key, group := range moduleTotals {
		group.Category = modules.Categories[key]
		if reason, ok := dispatchPolicy.ExcludedModules[key]; ok {
			group.Excluded = group.Total
			group.Reason = reason
			group.Rule = DispatchRuleModule
		} else if category := modules.Categories[key]; category != "" {
			if reason, ok := dispatchPolicy.ExcludedCategories[category]; ok {
				group.Excluded = group.Total
				group.Reason = reason
				group.Rule = DispatchRuleCategory
			}
		}
		report.Modules = append(report.Modules, *group)
	}
	for _, module := range modules.Categories {
		if _, ok := categoryTotals[module]; !ok {
			categoryTotals[module] = &DistributionGroup{Key: module}
		}
	}
	for key, group := range categoryTotals {
		if reason, ok := dispatchPolicy.ExcludedCategories[key]; ok {
			group.Excluded = group.Total
			group.Reason = reason
			group.Rule = DispatchRuleCategory
		}
		report.Categories = append(report.Categories, *group)
	}
	sort.Slice(report.Modules, func(i, j int) bool {
		if report.Modules[i].Category != report.Modules[j].Category {
			return report.Modules[i].Category < report.Modules[j].Category
		}
		return report.Modules[i].Key < report.Modules[j].Key
	})
	sort.Slice(report.Categories, func(i, j int) bool { return report.Categories[i].Key < report.Categories[j].Key })

	sort.Slice(all, func(i, j int) bool {
		left, right := all[i], all[j]
		if left.Module != right.Module {
			return left.Module < right.Module
		}
		return left.Slug < right.Slug
	})
	report.Items = filterDistribution(all, options)
	// 单条技能被排除时，分组计数也该反映出来，否则分类行看起来「只排了模块」。
	applySkillExclusions(report.Modules, report.Categories, all, dispatchPolicy)
	return report, nil
}

// applySkillExclusions 把「单条技能被排除」补进分组计数。
//
// 分组里的 Excluded 之前只算了策略命中整组的情况，这里补上零散的技能级
// 排除——否则界面上分类行写着「0 条被排除」，下面却挂着几条红色的技能。
func applySkillExclusions(modules, categories []DistributionGroup, items []DistributionItem, policy DispatchPolicy) {
	if len(policy.ExcludedSkills) == 0 {
		return
	}
	byModule := map[string]int{}
	byCategory := map[string]int{}
	for _, item := range items {
		if item.State != DistributionExcluded || item.Rule != DispatchRuleSkill {
			continue
		}
		byModule[item.Module]++
		for _, category := range item.Categories {
			byCategory[category]++
		}
	}
	for index := range modules {
		if modules[index].Rule == "" && byModule[modules[index].Key] > 0 {
			modules[index].Excluded = byModule[modules[index].Key]
		}
	}
	for index := range categories {
		if categories[index].Rule == "" && byCategory[categories[index].Key] > 0 {
			categories[index].Excluded = byCategory[categories[index].Key]
		}
	}
}

// filterDistribution 按界面给出的条件收窄明细。
func filterDistribution(items []DistributionItem, options DistributionOptions) []DistributionItem {
	keyword := strings.ToLower(strings.TrimSpace(options.Keyword))
	filtered := make([]DistributionItem, 0, len(items))
	for _, item := range items {
		if options.State != "" && item.State != options.State {
			continue
		}
		if options.Module != "" && item.Module != options.Module {
			continue
		}
		if options.Category != "" && !containsString(item.Categories, options.Category) {
			continue
		}
		if keyword != "" && !matchesKeyword(item, keyword) {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func matchesKeyword(item DistributionItem, keyword string) bool {
	for _, value := range []string{item.Slug, item.SkillID, item.Name, item.Module, item.SourcePath} {
		if strings.Contains(strings.ToLower(value), keyword) {
			return true
		}
	}
	return false
}

// ValidateDispatchTargets 检查一份待保存的策略里有没有写错的键。
//
// 写错一个模块名不会报错，只会静默地什么都不排除——「我明明排了它怎么还在发」
// 就是这么做出来的。所以在落盘前拿上游模块表、11 个合法分类和同步锁各校一遍。
func ValidateDispatchTargets(policy DispatchPolicy, modules ModuleMap, lock Lock) error {
	problems := []string{}
	for module := range policy.ExcludedModules {
		if _, ok := modules.Categories[module]; !ok {
			problems = append(problems, fmt.Sprintf("excluded_modules 里的 %q 不是已知的上游模块", module))
		}
	}
	for category := range policy.ExcludedCategories {
		if _, ok := validCategories[category]; !ok {
			problems = append(problems, fmt.Sprintf("excluded_categories 里的 %q 不在 HiMind 的 11 个分类里", category))
		}
	}
	for slug := range policy.ExcludedSkills {
		if _, ok := lock.Skills[slug]; !ok {
			problems = append(problems, fmt.Sprintf("excluded_skills 里的 %q 不在同步锁里（是不是 slug 写错了）", slug))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("分发策略有 %d 处写错，这次不落盘：\n%s", len(problems), strings.Join(problems, "\n"))
	}
	return nil
}
