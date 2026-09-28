package eccsync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 目录与文件名约定，和 himind-repo-check 保持一致。
const (
	ExtensionsFile = "extensions.json"
	LockFile       = "upstream.lock.json"
	PolicyFile     = "upstream-policy.json"
	MetadataFile   = "manifests/metadata.json"
	ModulesFile    = "manifests/modules.json"
	QuarantineFile = "manifests/quarantine.json"
	CatalogFile    = ".himind/catalog.json"
)

// GeneratedHead 标记生成物，人改会被下一次同步覆盖。
const GeneratedHead = "<!-- 由 himind-ecc-sync 生成，请勿手改：改动请落在 manifests/metadata.json。 -->\n"

// supportedClients 与仓库既有技能保持一致。
var supportedClients = []string{"agent-skills"}

// defaultTargets 是本仓库所有扩展的分发落点。
var defaultTargets = []string{"workbench", "github"}

// GenerateInput 是一次生成的输入。
type GenerateInput struct {
	// RepoRoot 是本仓库根目录。
	RepoRoot string
	// SourceRoot 是解包好的上游源码树。
	SourceRoot string
	Policy     Policy
	PolicyPath string
	// Upstream 是本次对齐的上游提交事实。
	Upstream LockUpstream
	Now      time.Time
	Tool     string
}

// GenerateResult 是一次生成的产出摘要。
type GenerateResult struct {
	Sequence    int               `json:"sequence"`
	Version     string            `json:"version"`
	Skills      []string          `json:"skills"`
	Changed     []string          `json:"changed"`
	Unchanged   []string          `json:"unchanged"`
	Quarantined []QuarantineEntry `json:"quarantined"`
	Derived     int               `json:"derived_metadata"`
	Reviewed    int               `json:"reviewed_metadata"`
	Categories  map[string]int    `json:"categories"`
}

// plannedSkill 是一个已经通过全部前置检查、准备落盘的技能。
type plannedSkill struct {
	skill  SourceSkill
	entry  MetadataEntry
	files  map[string][]byte
	digest string
	meta   string
	tier   string
}

// Generate 把上游技能树写成本仓库的扩展源码。
//
// 整个过程是纯函数式的：同样的上游提交 + 同样的策略与元数据，
// 跑多少次都得到同样的字节，因此「没有变化」可以被精确判定成零动作。
func Generate(input GenerateInput) (GenerateResult, error) {
	result := GenerateResult{Categories: map[string]int{}}
	source, err := Discover(input.SourceRoot, input.Policy)
	if err != nil {
		return result, err
	}
	metadata, err := LoadMetadata(filepath.Join(input.RepoRoot, filepath.FromSlash(MetadataFile)))
	if err != nil {
		return result, err
	}
	lock, err := LoadLock(filepath.Join(input.RepoRoot, LockFile))
	if err != nil {
		return result, err
	}
	major, minor := versionCore(input.Upstream.Version)
	if major == 0 && minor == 0 {
		return result, fmt.Errorf("无法从上游版本 %q 推出制品版本", input.Upstream.Version)
	}

	plans := []plannedSkill{}
	quarantine := []QuarantineEntry{}
	moduleUsage := map[string]string{}

	for _, skill := range source.Skills {
		if reason, blocked := input.Policy.ExcludedSkills[skill.Slug]; blocked {
			quarantine = append(quarantine, QuarantineEntry{Slug: skill.Slug, Reason: "策略排除：" + reason})
			continue
		}
		if err := checkIdentity(skill.Slug, input.Policy); err != nil {
			quarantine = append(quarantine, QuarantineEntry{Slug: skill.Slug, Reason: err.Error()})
			continue
		}
		if len(skill.Blocked) > 0 {
			quarantine = append(quarantine, QuarantineEntry{
				Slug:   skill.Slug,
				Reason: "技能包不允许携带可执行脚本，需人工决定排除或改造成插件",
				Files:  skill.Blocked,
			})
			continue
		}
		if len(skill.Oversize) > 0 {
			quarantine = append(quarantine, QuarantineEntry{
				Slug:   skill.Slug,
				Reason: "存在超出单文件体积上限的内容",
				Files:  skill.Oversize,
			})
			continue
		}
		if len(skill.Files) > input.Policy.FilePolicy.MaxFilesPerSkill {
			quarantine = append(quarantine, QuarantineEntry{
				Slug:   skill.Slug,
				Reason: fmt.Sprintf("文件数 %d 超过上限 %d", len(skill.Files), input.Policy.FilePolicy.MaxFilesPerSkill),
			})
			continue
		}
		categories, moduleID, err := categoriesOf(skill, input.Policy)
		if err != nil {
			quarantine = append(quarantine, QuarantineEntry{Slug: skill.Slug, Reason: err.Error()})
			continue
		}
		moduleUsage[skill.Slug] = moduleID

		entry, err := resolveEntry(skill, categories, metadata.Skills[skill.Slug])
		if err != nil {
			quarantine = append(quarantine, QuarantineEntry{Slug: skill.Slug, Reason: err.Error()})
			continue
		}
		if entry.Source == "reviewed" {
			result.Reviewed++
		} else {
			result.Derived++
		}
		metadata.Skills[skill.Slug] = entry

		files, rewritten, err := materialize(skill, entry, input.Policy)
		if err != nil {
			return result, err
		}
		plans = append(plans, plannedSkill{
			skill:  skill,
			entry:  entry,
			files:  files,
			digest: digestGenerated(files),
			meta:   digestMetadata(entry),
			tier:   tierOf(rewritten),
		})
	}

	sort.Slice(plans, func(i, j int) bool { return plans[i].skill.Slug < plans[j].skill.Slug })

	// 先判断有没有真变化，再决定要不要推进同步序号。
	//
	// 变化判断逐技能做：上游改了一个技能，只有那个技能的版本要动。
	// 如果整批一起涨版本，市场里几百个技能会同时冒出「可更新」，
	// 用户要为此点几百次——那是噪音，不是功能。
	planChanged := map[string]bool{}
	changed := false
	for _, plan := range plans {
		previous, ok := lock.Skills[plan.skill.Slug]
		// 老锁文件没有 metadata_digest 字段，空值只当作「还没记过」：
		// 回填即可，不能拿它去判变化，否则第一次跑会把几百个技能一起涨版本。
		metadataChanged := previous.MetadataDigest != "" && previous.MetadataDigest != plan.meta
		planChanged[plan.skill.Slug] = !ok ||
			previous.Version == "" ||
			previous.SourceDigest != plan.skill.Digest ||
			previous.SyncedDigest != plan.digest ||
			metadataChanged
		if planChanged[plan.skill.Slug] {
			changed = true
		}
	}
	if len(lock.Skills) != len(plans) {
		changed = true
	}
	sequence := lock.Sync.Sequence
	if changed || sequence == 0 {
		sequence++
	}
	version := fmt.Sprintf("%d.%d.%d", major, minor, sequence)
	result.Sequence = sequence
	result.Version = version

	// 内容一个字都没变时，锁文件也必须一个字节都不变。
	//
	// sync.generated_at 是这份清单里唯一的时钟字段：照抄「上一次真正产出内容
	// 的时间」，而不是「这一次跑起来的时间」。否则上游一动不动，每天照样
	// 产生一条改了时间戳的 diff，「上游没变就零动作」当场失效。
	nextSync := LockSync{
		Sequence:    sequence,
		GeneratedAt: input.Now.UTC().Format(time.RFC3339),
		Tool:        input.Tool,
	}
	if !changed && lock.Sync.GeneratedAt != "" {
		nextSync.GeneratedAt = lock.Sync.GeneratedAt
		nextSync.Tool = lock.Sync.Tool
	}
	nextLock := Lock{
		SchemaVersion: LockVersion,
		Upstream:      input.Upstream,
		Sync:          nextSync,
		Skills:        map[string]LockSkill{},
	}

	entries := make([]extensionEntry, 0, len(plans))
	for _, plan := range plans {
		entries = append(entries, extensionEntry{
			Type: "skill",
			ID:   SkillID(plan.skill.Slug),
			Path: "skills/" + plan.skill.Slug,
		})
	}
	extensions := newExtensions(entries)
	kept := map[string]bool{}
	for _, plan := range plans {
		kept[plan.skill.Slug] = true
		targetDirectory := filepath.Join(input.RepoRoot, "skills", plan.skill.Slug)
		if err := writeSkillDirectory(targetDirectory, plan.files); err != nil {
			return result, err
		}
		previous := lock.Skills[plan.skill.Slug]
		skillVersion := version
		if !planChanged[plan.skill.Slug] && previous.Version != "" {
			skillVersion = previous.Version
		}
		manifest, err := skillManifest(plan, skillVersion, input.Policy.MinAgentVersionOrDefault(), input.Upstream)
		if err != nil {
			return result, err
		}
		if err := os.WriteFile(filepath.Join(targetDirectory, "skill.json"), manifest, 0o644); err != nil {
			return result, err
		}
		nextLock.Skills[plan.skill.Slug] = LockSkill{
			Version:        skillVersion,
			SourceDigest:   plan.skill.Digest,
			SyncedDigest:   plan.digest,
			MetadataDigest: plan.meta,
			SourceCommit:   input.Upstream.Commit,
			SourcePath:     plan.skill.SourcePath,
			Tier:           plan.tier,
			Rewritten:      plan.tier != "A",
		}
		result.Skills = append(result.Skills, plan.skill.Slug)
		if planChanged[plan.skill.Slug] {
			result.Changed = append(result.Changed, plan.skill.Slug)
		} else {
			result.Unchanged = append(result.Unchanged, plan.skill.Slug)
		}
		for _, category := range plan.entry.Categories {
			result.Categories[category]++
		}
	}

	if err := pruneSkills(input.RepoRoot, kept, lock); err != nil {
		return result, err
	}
	if err := writeJSON(filepath.Join(input.RepoRoot, ExtensionsFile), extensions); err != nil {
		return result, err
	}
	if err := SaveLock(filepath.Join(input.RepoRoot, LockFile), nextLock); err != nil {
		return result, err
	}
	if err := SaveMetadata(filepath.Join(input.RepoRoot, filepath.FromSlash(MetadataFile)), metadata); err != nil {
		return result, err
	}
	sort.Slice(quarantine, func(i, j int) bool { return quarantine[i].Slug < quarantine[j].Slug })
	result.Quarantined = quarantine
	quarantinePath := filepath.Join(input.RepoRoot, filepath.FromSlash(QuarantineFile))
	quarantineDocument := Quarantine{
		SchemaVersion: QuarantineVersion,
		GeneratedAt:   input.Now.UTC().Format(time.RFC3339),
		UpstreamSHA:   input.Upstream.Commit,
		Entries:       quarantine,
	}
	// 与锁文件同理：清单内容没变就沿用上一次的生成时间，不留无意义的 diff。
	if previous, err := LoadQuarantine(quarantinePath); err == nil {
		if previous.GeneratedAt != "" && sameQuarantine(previous, quarantineDocument) {
			quarantineDocument.GeneratedAt = previous.GeneratedAt
		}
	}
	if err := writeJSON(quarantinePath, quarantineDocument); err != nil {
		return result, err
	}
	if err := writeJSON(filepath.Join(input.RepoRoot, filepath.FromSlash(ModulesFile)), moduleDocument{
		SchemaVersion: 1,
		Source:        input.Upstream.Repository + "/manifests/install-modules.json",
		Categories:    input.Policy.ModuleCategories,
		Skills:        moduleUsage,
	}); err != nil {
		return result, err
	}
	if err := ensureCatalog(input.RepoRoot); err != nil {
		return result, err
	}
	return result, nil
}

type moduleDocument struct {
	SchemaVersion int               `json:"schema_version"`
	Source        string            `json:"source"`
	Categories    map[string]string `json:"module_categories"`
	Skills        map[string]string `json:"skill_modules"`
}

// sameQuarantine 判断两份隔离清单除了生成时间以外是否一致。
func sameQuarantine(left, right Quarantine) bool {
	if left.SchemaVersion != right.SchemaVersion || left.UpstreamSHA != right.UpstreamSHA {
		return false
	}
	if len(left.Entries) != len(right.Entries) {
		return false
	}
	for index := range left.Entries {
		before, after := left.Entries[index], right.Entries[index]
		if before.Slug != after.Slug || before.Reason != after.Reason || len(before.Files) != len(after.Files) {
			return false
		}
		for fileIndex := range before.Files {
			if before.Files[fileIndex] != after.Files[fileIndex] {
				return false
			}
		}
	}
	return true
}

type extensionEntry struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Path string `json:"path"`
}

type extensionsDocument struct {
	SchemaVersion              int              `json:"schema_version"`
	Repository                 string           `json:"repository"`
	DistributionID             string           `json:"distribution_id"`
	Channel                    string           `json:"channel"`
	CatalogID                  string           `json:"catalog_id"`
	DefaultDistributionTargets []string         `json:"default_distribution_targets"`
	DefaultBranch              string           `json:"default_branch"`
	Extensions                 []extensionEntry `json:"extensions"`
}

func newExtensions(skills []extensionEntry) extensionsDocument {
	document := extensionsDocument{
		SchemaVersion:              1,
		Repository:                 "https://github.com/MrBaoquan/himind-extensions-ecc.git",
		DistributionID:             "mrbaoquan/himind-extensions-ecc",
		Channel:                    "beta",
		CatalogID:                  "public",
		DefaultDistributionTargets: append([]string(nil), defaultTargets...),
		DefaultBranch:              "main",
	}
	document.Extensions = append(document.Extensions,
		extensionEntry{Type: "plugin", ID: pluginID, Path: "plugins/ecc-skill-sync"},
		extensionEntry{Type: "workflow", ID: workflowID, Path: "workflows/ecc-skill-sync"},
	)
	document.Extensions = append(document.Extensions, skills...)
	return document
}

// skillSlugPattern 是落盘目录名与稳定 ID 末段允许的写法。
var skillSlugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// resolveEntry 决定一个技能这次用哪条元数据。
//
// 只有 source=reviewed 的记录被当成「人读过的」原样采用；source=derived
// 的条目每次重新推导——否则第一次跑出来的机器译文会被文件本身洗成
// 「已确认」，人就再也不会回头看了。
func resolveEntry(skill SourceSkill, categories []string, existing MetadataEntry) (MetadataEntry, error) {
	if existing.Source == "reviewed" {
		if strings.TrimSpace(existing.Name) == "" || strings.TrimSpace(existing.Description) == "" {
			return MetadataEntry{}, fmt.Errorf("manifests/metadata.json 里这条 reviewed 记录缺 name 或 description")
		}
		if len(existing.Categories) == 0 {
			existing.Categories = categories
		}
		for _, category := range existing.Categories {
			if _, ok := validCategories[category]; !ok {
				return MetadataEntry{}, fmt.Errorf("分类 %q 不在 HiMind 允许的 11 个功能分类里", category)
			}
		}
		if length := len([]rune(existing.Name)); length > 18 {
			return MetadataEntry{}, fmt.Errorf("显示名称 %d 字超过 18 上限", length)
		}
		if length := len([]rune(existing.Description)); length > 120 {
			return MetadataEntry{}, fmt.Errorf("用途说明 %d 字超过 120 上限", length)
		}
		return existing, nil
	}
	derived, ok := DeriveEntry(skill, categories)
	if !ok {
		return MetadataEntry{}, fmt.Errorf("上游元数据不足以推导显示名与说明，需要在 manifests/metadata.json 里补一条")
	}
	return derived, nil
}

// checkIdentity 在生成之前挡住「ID 装不下」和「目录名不合规」两类技能。
//
// 这两类都不能靠截断糊过去：ID 一旦截短，扩展就是另一个扩展，老版本永远
// 收不到更新。所以宁可留在待审清单里点名，也不生成一个会漂移的 ID。
func checkIdentity(slug string, policy Policy) error {
	if !skillSlugPattern.MatchString(slug) {
		return fmt.Errorf("上游目录名 %q 不是小写连字符写法，需要在策略里显式指定别名", slug)
	}
	id := SkillID(slug)
	if length := len([]rune(id)); length > 64 {
		return fmt.Errorf("稳定 ID 长度 %d 超过 64 上限，需要在策略里显式指定更短的 slug", length)
	}
	return nil
}

// categoriesOf 由上游模块推出 HiMind 分类。
func categoriesOf(skill SourceSkill, policy Policy) ([]string, string, error) {
	if len(skill.Modules) == 0 {
		return nil, "", fmt.Errorf("不在任何已映射的上游模块里，无法定分类")
	}
	moduleID := skill.Modules[0]
	category := policy.ModuleCategories[moduleID]
	if strings.TrimSpace(category) == "" {
		return nil, moduleID, fmt.Errorf("上游模块 %s 没有映射到 HiMind 分类", moduleID)
	}
	if _, ok := validCategories[category]; !ok {
		return nil, moduleID, fmt.Errorf("上游模块 %s 映射到未知分类 %q", moduleID, category)
	}
	// 上游同一技能可能出现在多个模块里，分类取并集并去重排序。
	unique := map[string]bool{category: true}
	for _, module := range skill.Modules[1:] {
		extra := policy.ModuleCategories[module]
		if extra == "" {
			continue
		}
		if _, ok := validCategories[extra]; !ok {
			return nil, moduleID, fmt.Errorf("上游模块 %s 映射到未知分类 %q", module, extra)
		}
		if extra != category {
			unique[extra] = true
		}
	}
	categories := make([]string, 0, len(unique))
	for item := range unique {
		categories = append(categories, item)
	}
	sort.Strings(categories)
	return categories, moduleID, nil
}

// materialize 生成技能目录里的最终文件字节。
//
// 三件事会改写原文：frontmatter 的 description 必须压进 160 字硬上限、
// 落点路径按策略做机械替换、根目录加一行生成物提示。除此之外正文逐字保留。
func materialize(skill SourceSkill, entry MetadataEntry, policy Policy) (map[string][]byte, bool, error) {
	files := map[string][]byte{}
	rewritten := false
	for _, file := range skill.Files {
		relative := applyRewrites(file.RelativePath, policy.Rewrites)
		if _, exists := files[relative]; exists {
			return nil, false, fmt.Errorf("%s: 替换规则把两个文件映射到同一个路径 %s", skill.Slug, relative)
		}
		content := file.Content
		if isTextExtension(relative, policy.FilePolicy.TextExtensions) {
			updated, didRewrite := applyRewritesToContent(string(content), policy.Rewrites)
			content = []byte(updated)
			rewritten = rewritten || didRewrite
		}
		if relative == "SKILL.md" {
			clamped, didRewrite := clampTriggerDescription(string(content))
			content = []byte(clamped)
			rewritten = rewritten || didRewrite
		}
		files[relative] = content
	}
	if _, ok := files["SKILL.md"]; !ok {
		return nil, false, fmt.Errorf("%s: 缺少 SKILL.md", skill.Slug)
	}
	return files, rewritten, nil
}

// clampTriggerDescription 把 SKILL.md frontmatter 的 description 压进硬上限。
// 只动这一行：它是触发语义，Agent 侧按 160 字硬上限校验，超了整包被拒。
func clampTriggerDescription(document string) (string, bool) {
	fields := parseFrontmatter(document)
	current := fields["description"]
	if current == "" {
		return document, false
	}
	clamped, truncated := clampDescription(normalizeSpace(current), 160)
	if !truncated {
		return document, false
	}
	lines := strings.SplitAfter(document, "\n")
	for index, line := range lines {
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, "description:") {
			ending := line[len(trimmed):]
			lines[index] = "description: " + clamped + ending
			return strings.Join(lines, ""), true
		}
	}
	return document, false
}

func applyRewrites(value string, rewrites []Rewrite) string {
	for _, rewrite := range rewrites {
		value = strings.ReplaceAll(value, rewrite.From, rewrite.To)
	}
	return value
}

func applyRewritesToContent(value string, rewrites []Rewrite) (string, bool) {
	updated := value
	for _, rewrite := range rewrites {
		updated = strings.ReplaceAll(updated, rewrite.From, rewrite.To)
	}
	return updated, updated != value
}

func isTextExtension(relative string, allowed []string) bool {
	extension := strings.ToLower(filepath.Ext(relative))
	for _, item := range allowed {
		if extension == strings.ToLower(item) {
			return true
		}
	}
	return false
}

// tierOf 判定这次搬运的自动化档位。
//
// A 档是「逐字搬运」：除了 frontmatter 触发说明的机械截断，正文没被动过，
// 因此变化可以无人值守地自动合并。B 档动过内容，必须有人看一眼。
func tierOf(rewritten bool) string {
	if !rewritten {
		return "A"
	}
	return "B"
}

// writeSkillDirectory 用「先写后清」的方式落盘，避免上一次同步留下的文件变成幽灵内容。
func writeSkillDirectory(directory string, files map[string][]byte) error {
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	existing, err := os.ReadDir(directory)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range existing {
		if err := os.RemoveAll(filepath.Join(directory, entry.Name())); err != nil {
			return err
		}
	}
	for relative, content := range files {
		target := filepath.Join(directory, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, content, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func digestGenerated(files map[string][]byte) string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	hash := sha256.New()
	for _, name := range names {
		fmt.Fprintf(hash, "%s\x00%d\x00", name, len(files[name]))
		hash.Write(files[name])
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// digestMetadata 把对外元数据摊成一行再取摘要，字段顺序固定，与 JSON 缩进无关。
func digestMetadata(entry MetadataEntry) string {
	hash := sha256.New()
	parts := append([]string{entry.Name, entry.Description}, entry.Categories...)
	parts = append(parts, entry.Source)
	for _, part := range parts {
		fmt.Fprintf(hash, "%s\x00", part)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// pruneSkills 删掉「上次生成过、这次不再生成」的技能目录。
// 只删锁文件里记录过的目录，人在 skills/ 下新建的东西不动。
func pruneSkills(repoRoot string, kept map[string]bool, lock Lock) error {
	for slug := range lock.Skills {
		if kept[slug] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(repoRoot, "skills", slug)); err != nil {
			return err
		}
	}
	return nil
}

var versionPattern = regexp.MustCompile(`^(\d+)\.(\d+)`)

// versionCore 取上游版本的前两段，第三段留给同步序号。
//
// 制品版本必须是纯 X.Y.Z：Agent 的版本比较只看前三段数字，
// 带 `+sha` 之类的后缀会让「同一个扩展的两个版本」永远相等，自动更新失效。
// 上游喜欢写 `v2.2.2`，前缀 v 要在进入正则前剥掉。
func versionCore(upstreamVersion string) (int, int) {
	trimmed := strings.TrimSpace(upstreamVersion)
	trimmed = strings.TrimPrefix(strings.TrimPrefix(trimmed, "v"), "V")
	match := versionPattern.FindStringSubmatch(trimmed)
	if match == nil {
		return 0, 0
	}
	major, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, 0
	}
	minor, err := strconv.Atoi(match[2])
	if err != nil {
		return 0, 0
	}
	return major, minor
}

// ensureCatalog 保证市场索引存在且结构完整。
// 索引是派生物，这里只在缺失时补一个空壳，条目由发布步骤写入。
func ensureCatalog(repoRoot string) error {
	path := filepath.Join(repoRoot, filepath.FromSlash(CatalogFile))
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	document := map[string]any{
		"schema_version":  1,
		"source_id":       "",
		"distribution_id": "mrbaoquan/himind-extensions-ecc",
		"channel":         "beta",
		"catalog_id":      "public",
		"generation":      "",
		"plugins":         []any{},
		"skills":          []any{},
		"workflows":       []any{},
		"feature_packs": []map[string]any{{
			"id":         "com.mrbaoquan.ecc.feature.sync",
			"name":       "ECC 技能同步",
			"plugin_ids": []string{pluginID},
			"skill_ids":  []string{},
		}},
	}
	return writeJSON(path, document)
}

// skillManifestJSON 是写进 skill.json 的结构。
// 字段顺序与仓库既有技能保持一致，便于人读 diff。
type skillManifestJSON struct {
	ID                  string         `json:"id"`
	Name                string         `json:"name"`
	Author              string         `json:"author"`
	Categories          []string       `json:"categories"`
	Version             string         `json:"version"`
	DistributionTargets []string       `json:"distribution_targets"`
	Scope               string         `json:"scope"`
	Description         string         `json:"description"`
	ReleaseNotes        string         `json:"release_notes"`
	MinAgentVersion     string         `json:"min_agent_version"`
	SupportedClients    []string       `json:"supported_clients"`
	Capabilities        []any          `json:"capabilities"`
	PluginDependencies  []any          `json:"plugin_dependencies"`
	RiskSummary         string         `json:"risk_summary"`
	Contents            []string       `json:"contents"`
	Upstream            upstreamOrigin `json:"upstream"`
}

type upstreamOrigin struct {
	Repository string `json:"repository"`
	Path       string `json:"path"`
	Commit     string `json:"commit"`
	License    string `json:"license"`
}

// skillManifest 生成 skill.json。
//
// contents 必须来自**落盘后**的文件名：改写规则可能把 `.claude/x` 变成
// `.himind/x`，如果这里写的是改写前的路径，包会声明一堆不存在的文件，
// skillproject 打包时会直接报「skill content is missing」。
//
// 这里刻意不写「同步时间」：生成物里一旦有随时间变化的字段，上游没动
// 也能制造出全仓库 diff，「本次无需搬运」就没法用文件字节证明。
// 溯源靠 upstream.commit，那是内容事实，不是执行事实。
func skillManifest(plan plannedSkill, version string, minAgentVersion string, upstream LockUpstream) ([]byte, error) {
	contents := make([]string, 0, len(plan.files))
	for name := range plan.files {
		contents = append(contents, name)
	}
	contents = append(contents, "skill.json")
	sort.Strings(contents)
	manifest := skillManifestJSON{
		ID:                  SkillID(plan.skill.Slug),
		Name:                plan.entry.Name,
		Author:              upstream.LicenseHolder,
		Categories:          plan.entry.Categories,
		Version:             version,
		DistributionTargets: append([]string(nil), defaultTargets...),
		Scope:               "organization",
		Description:         plan.entry.Description,
		ReleaseNotes:        releaseNotes(version, upstream),
		MinAgentVersion:     minAgentVersion,
		SupportedClients:    append([]string(nil), supportedClients...),
		Capabilities:        []any{},
		PluginDependencies:  []any{},
		RiskSummary:         "read_only",
		Contents:            contents,
		Upstream: upstreamOrigin{
			Repository: upstream.Repository,
			Path:       plan.skill.SourcePath,
			Commit:     upstream.Commit,
			License:    upstream.License,
		},
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func releaseNotes(version string, upstream LockUpstream) string {
	return fmt.Sprintf("%s：同步自上游 %s %s。", version, upstream.Package, shortSHA(upstream.Commit))
}

func shortSHA(value string) string {
	if len(value) > 7 {
		return value[:7]
	}
	return value
}

// SkillID 返回技能的稳定 ID。
func SkillID(slug string) string { return skillIDPrefix + slug }

// skillIDPrefix 是本仓库技能的 ID 前缀，由 policy 覆盖。
var skillIDPrefix = "com.mrbaoquan.ecc.skill."

// SetSkillIDPrefix 让生成器使用策略里的前缀。
func SetSkillIDPrefix(prefix string) {
	if strings.TrimSpace(prefix) != "" {
		skillIDPrefix = strings.TrimSpace(prefix)
	}
}
