package eccsync

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/MrBaoquan/himind-extensions/tooling/catalog"
	"github.com/MrBaoquan/himind-extensions/tooling/distribution"
	"github.com/MrBaoquan/himind-extensions/tooling/pluginpack"
	"github.com/MrBaoquan/himind-extensions/tooling/releaseplan"
	"github.com/MrBaoquan/himind-extensions/tooling/skillproject"
	"github.com/MrBaoquan/himind-extensions/tooling/workflowproject"
)

// PublishOptions 控制一次发布。
type PublishOptions struct {
	// DryRun 只算出要发什么，不碰远端。
	DryRun bool
	// Limit 限制本次最多发布几个技能，0 表示不限制。
	Limit int
	// Repository 覆盖仓库写法，用于本地演练。
	Repository string
	// Channel 覆盖渠道，缺省读 extensions.json。
	Channel string
	// PrivateKeyPath 是签名私钥，缺省用 Agent 生产密钥。
	PrivateKeyPath string
	// KeyID 是签名密钥标识。
	KeyID string
	// DistDir 是制品与发布清单的暂存目录。
	DistDir string
	// AllowDerived 允许把「元数据由上游机械推导」的技能也发到市场。
	//
	// 默认不放开：派生名称会被压到 18 字硬上限，描述会被截断，摆进市场
	// 只会让用户挑花眼。派生条目照常生成、照常过门禁，只是等 metadata
	// 里那条记录从 derived 变成 reviewed 之后才发。
	AllowDerived bool
}

// PublishItem 是单个扩展的发布结果。
type PublishItem struct {
	Extension  string `json:"extension"`
	ID         string `json:"id"`
	Version    string `json:"version"`
	Tag        string `json:"tag"`
	Artifact   string `json:"artifact"`
	ReleaseURL string `json:"release_url,omitempty"`
	Status     string `json:"status"`
	Detail     string `json:"detail,omitempty"`
}

// PublishResult 是一次发布的汇总。
type PublishResult struct {
	Repository string   `json:"repository"`
	Channel    string   `json:"channel"`
	DryRun     bool     `json:"dry_run"`
	Pending    int      `json:"pending"`
	Held       []string `json:"held_for_metadata_review,omitempty"`
	// Excluded 是本次按分发策略跳过的技能条数。
	//
	// 它和 Held 是两个不同的闸门：Held 是「文案还没校对」，改了 metadata 就能发；
	// Excluded 是「这一类的分发策略说不要」，只有改 dispatch-policy.json 才会变。
	// 跳过必须报数，否则定时任务会安安静静地少发一批，谁也不知道为什么。
	Excluded      int            `json:"excluded"`
	ExcludedItems []ExcludedItem `json:"excluded_items,omitempty"`
	Published     int            `json:"published"`
	Failed        int            `json:"failed"`
	// CleanedDrafts 是本次顺手清掉的残留 Draft Release id，正常情况下为空。
	CleanedDrafts []int64       `json:"cleaned_drafts,omitempty"`
	Items         []PublishItem `json:"items"`
}

// ExcludedItem 是一条被分发策略挡下的上游技能。
//
// 记的不是「少了一条」而是「少了哪一条、被哪条规则挡的、为什么」：策略是
// 按类生效的，一条技能消失时必须能立刻回溯到是分类、模块还是它自己被排除。
type ExcludedItem struct {
	Slug       string   `json:"slug"`
	ID         string   `json:"id"`
	Version    string   `json:"version"`
	Module     string   `json:"module,omitempty"`
	Categories []string `json:"categories,omitempty"`
	Rule       string   `json:"rule"`
	Keyword    string   `json:"keyword"`
	Reason     string   `json:"reason"`
}

// publishTarget 是一次待发布的扩展：类型、稳定 ID、版本与源码目录。
type publishTarget struct {
	kind    string
	id      string
	version string
	dir     string
	// slug 只有技能才有：它是锁文件的键，也是技能目录名。
	slug string
}

// label 是发布汇报里给这条记录用的名字：技能用 slug，插件与工作流用 ID。
func (t publishTarget) label() string {
	if t.slug != "" {
		return t.slug
	}
	return t.id
}

// Publish 把「本地已经有、索引里还没有」的扩展发到 GitHub Release，
// 再按同一份发布清单增补市场索引。
//
// 两个顺序都不能反：索引里的 download_url 必须指向已经存在的 Release 资产，
// 先写索引就等于把「装不上」的东西挂到市场上；插件必须先进索引，工作流的
// 依赖 pin 才解析得出来——否则发出去的工作流会指向一个不存在的插件版本。
func Publish(repoRoot string, options PublishOptions) (PublishResult, error) {
	policy, err := LoadPolicy(filepath.Join(repoRoot, PolicyFile))
	if err != nil {
		return PublishResult{}, err
	}
	SetSkillIDPrefix(policy.SkillIDPrefix)
	lock, err := LoadLock(filepath.Join(repoRoot, LockFile))
	if err != nil {
		return PublishResult{}, err
	}
	extensions, err := readExtensions(repoRoot)
	if err != nil {
		return PublishResult{}, err
	}
	repository := strings.TrimSpace(options.Repository)
	if repository == "" {
		repository = extensions.DistributionID
	}
	channel := strings.TrimSpace(options.Channel)
	if channel == "" {
		channel = extensions.Channel
	}
	distDir := strings.TrimSpace(options.DistDir)
	if distDir == "" {
		distDir = filepath.Join(repoRoot, "dist")
	}
	// Items 显式初始化为空切片，避免没有可发布项时编出 null（report schema 要求 array）。
	result := PublishResult{Repository: repository, Channel: channel, DryRun: options.DryRun, Items: []PublishItem{}}

	index, err := catalog.Load(filepath.Join(repoRoot, filepath.FromSlash(CatalogFile)))
	if err != nil {
		return result, err
	}
	// 分发策略在挑「待发目标」时生效：判定必须发生在目标成型之前，
	// 否则限流截断、依赖解析、制品打包各自都要再判一次，早晚判得不一样。
	dispatchPolicy, err := LoadDispatchPolicy(filepath.Join(repoRoot, DispatchPolicyFile))
	if err != nil {
		return result, err
	}
	modules, err := LoadModuleMap(filepath.Join(repoRoot, filepath.FromSlash(ModulesFile)))
	if err != nil {
		return result, err
	}
	metadata, err := LoadMetadata(filepath.Join(repoRoot, filepath.FromSlash(MetadataFile)))
	if err != nil {
		return result, err
	}
	context := DispatchContext{Policy: dispatchPolicy, Modules: modules, Fallback: policy.ModuleCategories}
	targets, excluded, err := pendingTargets(repoRoot, lock, extensions, index, context, metadata)
	if err != nil {
		return result, err
	}
	result.Excluded = len(excluded)
	if len(excluded) > 0 {
		result.ExcludedItems = excluded
	}
	if !options.AllowDerived {
		targets, result.Held = splitByMetadataSource(targets, metadata)
	}
	result.Pending = len(targets)
	if options.Limit > 0 && len(targets) > options.Limit {
		targets = targets[:options.Limit]
	}
	if len(targets) == 0 {
		return result, nil
	}
	// 有东西要发，顺便把历史遗留的半成品清掉：残留 Draft 会在重跑时和正式
	// Release 撞成两条同 tag 记录，越积越难查。
	cleaned, err := sweepStaleDrafts(repository, extensions)
	if err != nil {
		return result, err
	}
	result.CleanedDrafts = cleaned

	var signer *rsa.PrivateKey
	keyID := strings.TrimSpace(options.KeyID)
	if keyID == "" {
		keyID = DefaultSigningKeyID
	}
	if !options.DryRun {
		keyPath := strings.TrimSpace(options.PrivateKeyPath)
		if keyPath == "" {
			keyPath = DefaultPrivateKeyPath
		}
		signer, err = loadPrivateKey(keyPath)
		if err != nil {
			return result, err
		}
	}

	if err := os.MkdirAll(distDir, 0o755); err != nil {
		return result, err
	}
	catalogPath := filepath.Join(repoRoot, filepath.FromSlash(CatalogFile))
	// 干跑不碰远端，也就不会真把插件写进索引；但工作流的依赖 pin 必须解析得
	// 出来，否则干跑会报一个真跑不会发生的错（插件先发，工作流随后就解析得到）。
	// 这里把本次待发的条目记进一份临时索引，只用于后续条目的依赖解析。
	planCatalogPath := catalogPath
	if options.DryRun {
		planCatalogPath = filepath.Join(distDir, "dry-run-catalog.json")
		if err := index.Save(planCatalogPath); err != nil {
			return result, err
		}
	}
	for _, target := range targets {
		item := PublishItem{Extension: target.label(), ID: target.id, Version: target.version}
		plan, planErr := releaseplan.Build(target.kind, target.dir, repository, channel, planCatalogPath)
		if planErr != nil {
			item.Status = "failed"
			item.Detail = "解析发布计划失败：" + planErr.Error()
			result.Failed++
			result.Items = append(result.Items, item)
			continue
		}
		plan.SourceCommit = repoRevision(repoRoot)
		item.Tag = plan.Tag
		item.Artifact = plan.ArtifactName
		artifactPath := filepath.Join(distDir, plan.ArtifactName)
		manifestPath := filepath.Join(distDir, plan.ManifestName)

		if err := packageExtension(target, artifactPath); err != nil {
			item.Status = "failed"
			item.Detail = "打包失败：" + err.Error()
			result.Failed++
			result.Items = append(result.Items, item)
			continue
		}
		if options.DryRun {
			if err := recordPlanned(index, target, plan, artifactPath, repository, planCatalogPath); err != nil {
				item.Status = "failed"
				item.Detail = "推演索引失败：" + err.Error()
				result.Failed++
				result.Items = append(result.Items, item)
				continue
			}
			item.Status = "dry-run"
			result.Items = append(result.Items, item)
			continue
		}
		release, err := buildRelease(plan, artifactPath, manifestPath, keyID, signer)
		if err != nil {
			item.Status = "failed"
			item.Detail = "签发失败：" + err.Error()
			result.Failed++
			result.Items = append(result.Items, item)
			continue
		}
		url, err := ensureRelease(repository, plan.Tag, item, artifactPath, manifestPath, release)
		if err != nil {
			item.Status = "failed"
			item.Detail = "发布 Release 失败：" + err.Error()
			result.Failed++
			result.Items = append(result.Items, item)
			continue
		}
		item.ReleaseURL = url
		if err := upsertCatalog(repoRoot, target.kind, target.dir, release, repository); err != nil {
			item.Status = "failed"
			item.Detail = "索引增补失败：" + err.Error()
			result.Failed++
			result.Items = append(result.Items, item)
			continue
		}
		item.Status = "published"
		result.Published++
		result.Items = append(result.Items, item)
	}
	return result, nil
}

// pendingTargets 找出「本地已经有、索引里还缺、分发策略也允许发」的扩展。
//
// 技能来自上游搬运锁，版本由上游内容推导；插件与工作流是本仓库自有的两件
// 扩展，版本写在各自的源码清单里，改代码就要手工涨版本。这里只负责把索引里
// 没有的版本挑出来，顺序固定为插件、技能、工作流。
//
// 分发策略只约束上游技能。插件与工作流是本仓库自己写的扩展：它们要停发，
// 改的是自己的分发落点，而不是「上游某类技能不发」这条策略；把两者混在一起，
// 哪天误排了一个分类，工具链自己就跟着停更了。
//
// 被策略挡下的技能走 excluded 返回，不静默消失——「少了几条」是这套流程里
// 最需要被看出来的事。
func pendingTargets(
	repoRoot string,
	lock Lock,
	extensions extensionsDocument,
	index *catalog.Catalog,
	context DispatchContext,
	metadata Metadata,
) ([]publishTarget, []ExcludedItem, error) {
	published := publishedVersions(index)
	targets := make([]publishTarget, 0, len(lock.Skills)+2)
	excluded := []ExcludedItem{}
	for _, entry := range extensions.Extensions {
		if entry.Type != distribution.KindPlugin && entry.Type != distribution.KindWorkflow {
			continue
		}
		dir := filepath.Join(repoRoot, filepath.FromSlash(entry.Path))
		manifest, err := catalog.ReadManifest(dir, entry.Type)
		if err != nil {
			return nil, nil, err
		}
		if published[entry.Type+"/"+manifest.ID][manifest.Version] {
			continue
		}
		targets = append(targets, publishTarget{
			kind: entry.Type, id: manifest.ID, version: manifest.Version, dir: dir,
		})
	}
	for slug, item := range lock.Skills {
		id := SkillID(slug)
		if published[distribution.KindSkill+"/"+id][item.Version] {
			continue
		}
		module := context.Modules.ModuleOf(slug)
		categories := context.CategoriesOf(metadata.Skills[slug], module)
		if decision := context.Decide(slug, module, categories); decision.Excluded {
			excluded = append(excluded, ExcludedItem{
				Slug:       slug,
				ID:         id,
				Version:    item.Version,
				Module:     module,
				Categories: categories,
				Rule:       decision.Rule,
				Keyword:    decision.Keyword,
				Reason:     decision.Reason,
			})
			continue
		}
		targets = append(targets, publishTarget{
			kind: distribution.KindSkill, id: id, version: item.Version,
			dir: filepath.Join(repoRoot, "skills", slug), slug: slug,
		})
	}
	sortTargets(targets)
	// 锁文件是 map，遍历顺序随机；跳过清单必须自己排序，否则同一份策略
	// 每次跑出来的报告顺序都不一样，运行记录之间没法比对。
	sort.Slice(excluded, func(i, j int) bool { return excluded[i].Slug < excluded[j].Slug })
	return targets, excluded, nil
}

// publishedVersions 把索引收成「(类型, ID) → 已发过的全部版本」。
//
// 索引是版本历史，同一个扩展会同时留着 1.0.0 与 1.0.1 两条记录。所以判「这个
// 版本发过没有」只能看集合成员，不能拿一个版本的字符串去比：后者会被后来的
// 记录覆盖成历史里最旧的那条，把刚发过的版本又判成待发，于是每次同步都去撞
// 同名 Release，撞一次失败一次。
func publishedVersions(index *catalog.Catalog) map[string]map[string]bool {
	published := map[string]map[string]bool{}
	for _, kind := range distribution.Kinds() {
		for _, entry := range index.Entries(kind) {
			version, _ := entry["version"].(string)
			if version == "" {
				continue
			}
			key := kind + "/" + catalog.IDOf(kind, entry)
			if published[key] == nil {
				published[key] = map[string]bool{}
			}
			published[key][version] = true
		}
	}
	return published
}

// sortTargets 固定发布顺序：插件、技能、工作流，同类按 ID。
// 工作流依赖插件，插件必须先进索引，工作流的依赖 pin 才解析得出来。
func sortTargets(targets []publishTarget) {
	rank := map[string]int{
		distribution.KindPlugin:   0,
		distribution.KindSkill:    1,
		distribution.KindWorkflow: 2,
	}
	sort.SliceStable(targets, func(i, j int) bool {
		if rank[targets[i].kind] != rank[targets[j].kind] {
			return rank[targets[i].kind] < rank[targets[j].kind]
		}
		return targets[i].id < targets[j].id
	})
}

// packageExtension 按类型打制品：扩展名与校验规则各自归各自的工具包，
// 这里只做分发，避免发布脚本自己拼 zip。
func packageExtension(target publishTarget, output string) error {
	switch target.kind {
	case distribution.KindPlugin:
		return pluginpack.Package(target.dir, output)
	case distribution.KindWorkflow:
		return workflowproject.Package(target.dir, output)
	default:
		return skillproject.Package(target.dir, output)
	}
}

// recordPlanned 把一条「干跑将要发布」的条目写进临时索引。
//
// 只给干跑用：后面条目的依赖 pin 要能从索引里查到这条插件的版本与摘要，
// 干跑才算推演到了真跑的结果。真跑走 upsertCatalog，索引写的是正式文件。
func recordPlanned(index *catalog.Catalog, target publishTarget, plan releaseplan.Plan, artifactPath, repository, indexPath string) error {
	data, err := os.ReadFile(artifactPath)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(data)
	release := plan.Manifest(distribution.ReleaseArtifact{
		Name:      plan.ArtifactName,
		SizeBytes: int64(len(data)),
		SHA256:    hex.EncodeToString(digest[:]),
	}, nil)
	manifest, err := catalog.ReadManifest(target.dir, target.kind)
	if err != nil {
		return err
	}
	entry, err := catalog.Entry(target.kind, manifest, catalog.ReleaseFacts{
		Release:     release,
		PublishedAt: time.Now().UTC().Format(time.RFC3339),
		Repository:  repository,
	})
	if err != nil {
		return err
	}
	if err := index.Upsert(target.kind, entry); err != nil {
		return err
	}
	return index.Save(indexPath)
}

// DefaultPrivateKeyPath 与 DefaultSigningKeyID 是 Agent 生产密钥的默认位置。
const (
	DefaultPrivateKeyPath = `C:\Users\Administrator\AppData\Local\HiMind\signing\private\himind-production-2026.key.pem`
	DefaultSigningKeyID   = "himind-production-2026"
)

// splitByMetadataSource 把「元数据还没人工确认」的技能挑出来。
//
// 判定只看 manifests/metadata.json 里的 source 字段：reviewed 是有人读过
// 的名字与说明，derived 是机器从上游 frontmatter 压出来的临时产物。
// 释放闸门只有一个动作——把那条记录改成 reviewed。
//
// 插件与工作流没有「上游元数据」这回事，不受这条闸门约束。
func splitByMetadataSource(targets []publishTarget, metadata Metadata) (ready []publishTarget, held []string) {
	for _, target := range targets {
		if target.kind != distribution.KindSkill {
			ready = append(ready, target)
			continue
		}
		entry, ok := metadata.Skills[target.slug]
		if ok && entry.Source == "reviewed" {
			ready = append(ready, target)
			continue
		}
		held = append(held, target.slug)
	}
	return ready, held
}

// buildRelease 用发布计划的命名事实、制品字节与分离签名写出发布清单。
//
// 依赖 pin 来自 releaseplan：工作流依赖的插件必须精确到版本、摘要与 tag，
// Agent 才可能照着这份清单把插件一起装上。
func buildRelease(plan releaseplan.Plan, artifactPath, manifestPath, keyID string, signer *rsa.PrivateKey) (distribution.ReleaseManifest, error) {
	data, err := os.ReadFile(artifactPath)
	if err != nil {
		return distribution.ReleaseManifest{}, err
	}
	digest := sha256.Sum256(data)
	signature, err := signArtifact(artifactPath, plan.ArtifactName, data, keyID, signer)
	if err != nil {
		return distribution.ReleaseManifest{}, err
	}
	release := plan.Manifest(distribution.ReleaseArtifact{
		Name:      plan.ArtifactName,
		SizeBytes: int64(len(data)),
		SHA256:    hex.EncodeToString(digest[:]),
	}, &signature)
	if err := release.Validate(); err != nil {
		return distribution.ReleaseManifest{}, err
	}
	if err := distribution.WriteReleaseManifest(manifestPath, release); err != nil {
		return distribution.ReleaseManifest{}, err
	}
	return release, nil
}

// signArtifact 用 RSA-PSS/SHA-256 对制品字节签名。
// 签名与摘要写在同一次调用里，避免「制品和签名走散」。
func signArtifact(artifactPath, artifactName string, data []byte, keyID string, signer *rsa.PrivateKey) (distribution.ReleaseSignature, error) {
	digest := sha256.Sum256(data)
	signature, err := rsa.SignPSS(rand.Reader, signer, crypto.SHA256, digest[:], &rsa.PSSOptions{
		SaltLength: rsa.PSSSaltLengthEqualsHash,
		Hash:       crypto.SHA256,
	})
	if err != nil {
		return distribution.ReleaseSignature{}, err
	}
	return distribution.ReleaseSignature{
		FileName:           artifactName,
		FileSize:           int64(len(data)),
		SHA256:             hex.EncodeToString(digest[:]),
		Signature:          base64.StdEncoding.EncodeToString(signature),
		SignatureKeyID:     keyID,
		SignatureAlgorithm: "rsa-pss-sha256",
	}, nil
}

// loadPrivateKey 读 PEM 私钥，兼容 PKCS#1 与 PKCS#8 两种写法。
func loadPrivateKey(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取签名私钥失败: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("签名私钥不是 PEM 格式")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("解析签名私钥失败: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("签名私钥不是 RSA 密钥")
	}
	return key, nil
}

// ghAttempts 是一次 gh 调用的最多尝试次数，ghBackoff 是重试间隔。
// 与直接打 GitHub 接口共用同一套策略：定时任务无人值守，一次抖动不该让当天
// 的同步整条失败。
const ghAttempts = 5

var ghBackoff = 3 * time.Second

// runGH 跑一条 gh 命令，网络抖动时重发。
func runGH(args ...string) (string, error) {
	return runWithRetry(ghAttempts, ghBackoff, func() (string, bool, error) {
		command := exec.Command("gh", args...)
		configureHiddenCommand(command)
		output, err := command.CombinedOutput()
		text := strings.TrimSpace(string(output))
		if err != nil {
			return text, isTransient(text), fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, text)
		}
		return text, false, nil
	})
}

// releaseAsset 是 Release 上挂的一个资产，这里只关心文件名。
type releaseAsset struct {
	Name string `json:"name"`
}

// releaseRecord 是 GitHub 上一条 Release 记录里我们关心的字段。
type releaseRecord struct {
	ID     int64          `json:"id"`
	Tag    string         `json:"tag_name"`
	Draft  bool           `json:"draft"`
	URL    string         `json:"html_url"`
	Assets []releaseAsset `json:"assets"`
}

// releaseAssetState 是某个 tag 的 Release 现状。
type releaseAssetState struct {
	Exists bool
	URL    string
	Assets map[string]bool
	// Drafts 是同 tag 上残留的半成品 Release 的 id。
	Drafts map[int64]bool
}

// listReleases 拉出仓库里的全部 Release，含 Draft。
//
// 走列表接口而不是 `gh release view <tag>`：`gh release create` 是「先建 Draft →
// 挂资产 → 再转正式」三步，中间断掉会留下一条同名 Draft，而 Draft 不占 tag ref，
// `gh release view <tag>` 根本查不到它。查不到的后果不是少一条记录，是重跑时会再建
// 一条同名正式 Release，把半成品永久留在仓库里（真实留下过一条 plugin 1.0.3 的
// Draft）。列表接口能看到 Draft，「接管半成品」才成立。
func listReleases(repository string) ([]releaseRecord, error) {
	// 重试交给 runGH：它按同一套标记判可重试，这里再包一层只会把最坏情况
	// 放大成 9 次请求。
	output, err := runGH("api", "--paginate", "--slurp",
		fmt.Sprintf("repos/%s/releases?per_page=100", repository))
	if err != nil {
		return nil, err
	}
	return parseReleases([]byte(output))
}

// parseReleases 把 `gh api --paginate --slurp` 的输出拍平成一个列表。
//
// --slurp 会把每一页包成一个数组再合成一个大数组，所以是 [][]releaseRecord；
// 分页边界不能丢，否则第 101 条之后的 Release 会被当成「还没发过」。
func parseReleases(output []byte) ([]releaseRecord, error) {
	var pages [][]releaseRecord
	if err := json.Unmarshal(output, &pages); err != nil {
		return nil, fmt.Errorf("解析 Release 列表失败: %w", err)
	}
	records := make([]releaseRecord, 0, len(pages))
	for _, page := range pages {
		records = append(records, page...)
	}
	return records, nil
}

// recordsForTag 挑出属于这个 tag 的记录。
func recordsForTag(records []releaseRecord, tag string) []releaseRecord {
	matched := make([]releaseRecord, 0, 1)
	for _, record := range records {
		if record.Tag == tag {
			matched = append(matched, record)
		}
	}
	return matched
}

// inspectRelease 查 tag 上有没有正式 Release、挂了哪些资产、有没有残留 Draft。
func inspectRelease(repository, tag string) (releaseAssetState, error) {
	records, err := listReleases(repository)
	if err != nil {
		return releaseAssetState{}, err
	}
	return releaseStateFromRecords(recordsForTag(records, tag)), nil
}

// releaseStateFromRecords 把同 tag 的记录收成「正式发布在不在、资产有哪些、
// 有哪些残留 Draft」。
//
// 正式发布的资产按并集算：历史上出现过「一条正式记录 + 一条残留 Draft」的脏状态，
// 资产表以正式那条为准，Draft 只标记待清理。
func releaseStateFromRecords(records []releaseRecord) releaseAssetState {
	state := releaseAssetState{Assets: map[string]bool{}, Drafts: map[int64]bool{}}
	for _, record := range records {
		if record.Draft {
			state.Drafts[record.ID] = true
			continue
		}
		state.Exists = true
		state.URL = record.URL
		for _, asset := range record.Assets {
			state.Assets[asset.Name] = true
		}
	}
	return state
}

// deleteDrafts 逐条清理同 tag 上残留的 Draft。
//
// 只删调用方挑出来的 id：tag 形如 plugin/<id>@<version>，是这条链自己发出来的，
// 不存在「删掉别人的东西」。顺序按 id 升序，让日志与排查有确定的次序。
func deleteDrafts(repository string, drafts map[int64]bool) error {
	if len(drafts) == 0 {
		return nil
	}
	return deleteDraftIDs(repository, sortedDraftIDs(drafts))
}

// deleteDraftIDs 按给定次序删掉这些 Release id。
func deleteDraftIDs(repository string, ids []int64) error {
	for _, id := range ids {
		if _, err := runGH("api", "-X", "DELETE", fmt.Sprintf("repos/%s/releases/%d", repository, id)); err != nil {
			return fmt.Errorf("清理残留 Draft Release %d 失败: %w", id, err)
		}
	}
	return nil
}

// sortedDraftIDs 把待清理的 Draft id 排成升序。
//
// map 的遍历顺序在 Go 里是随机的，直接用会让「这次清了哪几条」在日志里每次都不一样，
// 排查时对不上。顺序在这里定死，删的动作本身按这个次序走。
func sortedDraftIDs(drafts map[int64]bool) []int64 {
	ids := make([]int64, 0, len(drafts))
	for id := range drafts {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// managedTag 从 tag 里取出「类型 + 扩展 ID」，并说明这是不是本仓库管得着的名字。
//
// tag 形如 plugin/com.mrbaoquan.ecc-skill-sync@1.0.3，剥掉 @ 后面的版本就是扩展
// 的归属键。认不出来的（不是我们这三种类型、没有 @、@ 前是空的）一律不管。
func managedTag(tag string) (string, bool) {
	at := strings.LastIndex(tag, "@")
	if at <= 0 {
		return "", false
	}
	owner := tag[:at]
	kind, id, found := strings.Cut(owner, "/")
	if !found || id == "" {
		return "", false
	}
	switch kind {
	case distribution.KindPlugin, distribution.KindSkill, distribution.KindWorkflow:
		return owner, true
	default:
		return "", false
	}
}

// staleDraftIDs 找出仓库里属于本仓库名下、但已经没人认领的残留 Draft。
//
// 为什么要跨 tag 扫，而不是只管「这次要发的那个 tag」：残留 Draft 是上一次发布
// 卡在挂资产那一步留下的，正常情况下下次发同一个 tag 就顺手清掉了。但只要期间
// 版本号被手工涨过（这次就真发生过），旧 tag 永远不会再被发一次，半成品就永久
// 留在仓库里。清扫范围按 extensions.json 里的扩展 ID 精确圈定，不是「见到 Draft
// 就删」——仓库里别人发的 Release 不在名单里，不会被碰。
func staleDraftIDs(records []releaseRecord, managed map[string]bool) []int64 {
	drafts := map[int64]bool{}
	for _, record := range records {
		if !record.Draft {
			continue
		}
		owner, ok := managedTag(record.Tag)
		if !ok || !managed[owner] {
			continue
		}
		drafts[record.ID] = true
	}
	return sortedDraftIDs(drafts)
}

// managedOwners 列出本仓库名下的扩展归属键，用于圈定 Draft 清扫范围。
func managedOwners(extensions extensionsDocument) map[string]bool {
	owners := make(map[string]bool, len(extensions.Extensions))
	for _, entry := range extensions.Extensions {
		switch entry.Type {
		case distribution.KindPlugin, distribution.KindSkill, distribution.KindWorkflow:
			if entry.ID != "" {
				owners[entry.Type+"/"+entry.ID] = true
			}
		}
	}
	return owners
}

// sweepStaleDrafts 清掉仓库里本仓库名下的残留 Draft。
//
// 只在这里删，是因为走到这一步说明本次确实有东西要发；上游没变时整条链在
// 前面就结束了，不会为了「顺便扫一下」去动网络。
func sweepStaleDrafts(repository string, extensions extensionsDocument) ([]int64, error) {
	records, err := listReleases(repository)
	if err != nil {
		return nil, err
	}
	ids := staleDraftIDs(records, managedOwners(extensions))
	if err := deleteDraftIDs(repository, ids); err != nil {
		return nil, err
	}
	return ids, nil
}

// ensureRelease 让 tag 对应的 Release 真的带上制品与发布清单两个资产，并返回地址。
//
// 凭据走 gh 自己的登录态，不经由 AI 上下文传递。
//
// 为什么要先查再建、建完再回读：
//
//   - Release 建立与资产上传是两次调用，中间断一次就会留下「Release 在、资产
//     只传了一半」的中间态。这时候重跑若直接 create，只会撞到
//     「a release with the same tag name already exists」，把本可自愈的中间态
//     变成要人工清理的故障。
//   - 索引里的 download_url 指向的正是这两个资产，资产没传全就写索引，等于把
//     「装不上」挂到市场上。所以索引只在回读确认之后才更新。
//   - Draft 也要一起接管：Draft 不占 tag，「同名 Draft + 同名正式发布」这种脏状态
//     重跑时既不会报错也不会自愈，只会在仓库里越积越多。开始之前先把同 tag 上的
//     Draft 清掉，正式发布才是这个 tag 上唯一说得清的那一条。
func ensureRelease(repository, tag string, item PublishItem, artifactPath, manifestPath string, release distribution.ReleaseManifest) (string, error) {
	wanted := []string{filepath.Base(artifactPath), filepath.Base(manifestPath)}
	directory := filepath.Dir(artifactPath)

	state, err := inspectRelease(repository, tag)
	if err != nil {
		return "", err
	}
	if err := deleteDrafts(repository, state.Drafts); err != nil {
		return "", err
	}
	if !state.Exists {
		if _, err := runGH(releaseCreateArgs(repository, tag, item, artifactPath, manifestPath, release)...); err != nil {
			return "", err
		}
	} else if missing := missingAssets(state, wanted, directory); len(missing) > 0 {
		args := append([]string{"release", "upload", tag, "--repo", repository, "--clobber"}, missing...)
		if _, err := runGH(args...); err != nil {
			return "", err
		}
	}

	verified, err := inspectRelease(repository, tag)
	if err != nil {
		return "", err
	}
	for _, name := range wanted {
		if !verified.Assets[name] {
			return "", fmt.Errorf("Release %s 缺少资产 %s，索引暂不更新", tag, name)
		}
	}
	return verified.URL, nil
}

// missingAssets 列出这次要挂、但 Release 上还没有的资产路径。
func missingAssets(state releaseAssetState, wanted []string, directory string) []string {
	missing := make([]string, 0, len(wanted))
	for _, name := range wanted {
		if !state.Assets[name] {
			missing = append(missing, filepath.Join(directory, name))
		}
	}
	return missing
}

// releaseCreateArgs 拼出建 Release 的 gh 参数。
func releaseCreateArgs(repository, tag string, item PublishItem, artifactPath, manifestPath string, release distribution.ReleaseManifest) []string {
	notes := fmt.Sprintf("ECC 技能同步：%s %s\n\n上游提交：%s\n制品摘要：%s",
		item.ID, item.Version, release.SourceCommit, release.Artifact.SHA256)
	return []string{
		"release", "create", tag,
		artifactPath, manifestPath,
		"--repo", repository,
		"--title", fmt.Sprintf("%s %s", item.ID, item.Version),
		"--notes", notes,
	}
}

// upsertCatalog 把这次发布写进市场索引。
func upsertCatalog(repoRoot, kind, directory string, release distribution.ReleaseManifest, repository string) error {
	path := filepath.Join(repoRoot, filepath.FromSlash(CatalogFile))
	index, err := catalog.Load(path)
	if err != nil {
		return err
	}
	manifest, err := catalog.ReadManifest(directory, kind)
	if err != nil {
		return err
	}
	entry, err := catalog.Entry(kind, manifest, catalog.ReleaseFacts{
		Release:     release,
		PublishedAt: time.Now().UTC().Format(time.RFC3339),
		Repository:  repository,
	})
	if err != nil {
		return err
	}
	if err := catalog.ValidateEntry(kind, entry, release); err != nil {
		return err
	}
	if err := index.Upsert(kind, entry); err != nil {
		return err
	}
	return index.Save(path)
}

// repoRevision 读当前仓库 HEAD，作为制品溯源。
func repoRevision(repoRoot string) string {
	command := exec.Command("git", "rev-parse", "HEAD")
	command.Dir = repoRoot
	configureHiddenCommand(command)
	output, err := command.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// readExtensions 读仓库的扩展清单。
func readExtensions(repoRoot string) (extensionsDocument, error) {
	var document extensionsDocument
	data, err := os.ReadFile(filepath.Join(repoRoot, ExtensionsFile))
	if err != nil {
		return document, err
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return document, fmt.Errorf("%s: %w", ExtensionsFile, err)
	}
	return document, nil
}
