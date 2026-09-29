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
	CleanedDrafts []int64 `json:"cleaned_drafts,omitempty"`
	// Batches 是本次发出去的批次 Release。
	//
	// 技能整批发之后，「发了几条 Release」与「发了几件技能」是两个数：前者决定
	// 仓库首页读起来是什么样子，后者才是市场里真正新增的东西。两个数都报出来，
	// 一次同步才不至于看起来像发了 268 条。
	Batches []PublishBatch `json:"batches,omitempty"`
	Items   []PublishItem  `json:"items"`
	// Pruned 是发布收尾时顺手回收的历史版本。
	//
	// 回收必须排在发布之后：新版本先落地，旧版本才有人接手。顺序反过来，
	// 一旦中间断掉就是「旧的删了、新的没发出去」。
	Pruned []SupersededRelease `json:"pruned,omitempty"`
	// PruneError 记录回收这一步失败的原因。
	//
	// 发布本身已经成功，回收只是收尾，不该把一个已经发生的发布判成失败——
	// 那会让定时任务在下一轮把同一批东西再发一次。失败照实报出来，下一轮再收。
	PruneError string `json:"prune_error,omitempty"`
}

// PublishBatch 是本次发出去的一条批次 Release。
type PublishBatch struct {
	Tag     string `json:"tag"`
	Version string `json:"version"`
	Members int    `json:"members"`
	URL     string `json:"url,omitempty"`
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
	// 真发布前先确认这份提交已经在远端。
	//
	// Release 的 tag 会被 --target 钉在 SourceCommit 上，而 SourceCommit 就是
	// 本地 HEAD：HEAD 没推到远端，tag 就只能落在一个远端认得的旧提交上，
	// 制品资产照样是对的，只有「checkout tag 拿到的源码」跟制品对不上号，
	// 全程没有任何一步会报错（1.1.1~1.1.5 就是这么错的）。
	// 干跑不写 tag，也就不拦，免得离线推演被迫先连一次网。
	revision := repoRevision(repoRoot)
	if !options.DryRun {
		if err := ensureRevisionOnRemote(repository, revision); err != nil {
			return result, err
		}
	}
	// 有东西要发，顺便把历史遗留的半成品清掉：残留 Draft 会在重跑时和正式
	// Release 撞成两条同 tag 记录，越积越难查。
	cleaned, err := sweepStaleDrafts(repository, extensions, managedBatchTags(index))
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
	// 三段按固定次序走：插件 → 技能批次 → 工作流。
	//
	// 次序是硬约束：工作流的依赖 pin 要从索引里解析出插件版本，插件必须先落
	// 索引；技能整批发一条 Release，排在两者中间，工作流的 pin 才不会指到一条
	// 还不存在的批次 Release 上。
	run := publishRun{
		repoRoot:    repoRoot,
		repository:  repository,
		channel:     channel,
		distDir:     distDir,
		planCatalog: planCatalogPath,
		revision:    revision,
		keyID:       keyID,
		signer:      signer,
		dryRun:      options.DryRun,
		index:       index,
	}
	plugins, skills, workflows := splitTargets(targets)
	for _, target := range plugins {
		result.take(run.single(target))
	}
	if len(skills) > 0 {
		items, batch := run.batch(lock, skills)
		for _, item := range items {
			result.take(item)
		}
		if batch != nil {
			result.Batches = append(result.Batches, *batch)
		}
	}
	for _, target := range workflows {
		result.take(run.single(target))
	}
	// 收尾回收历史版本：发布成功之后，本仓库名下超出保留窗口的旧版本就不再需要了。
	// 干跑不碰远端，也就不做这一步（要看回收计划用 `prune -dry-run`）。
	if !options.DryRun {
		pruned, pruneErr := PruneReleases(repoRoot, PruneOptions{Repository: repository})
		switch {
		case pruneErr != nil:
			result.PruneError = pruneErr.Error()
		default:
			result.Pruned = pruned.Pruned
		}
	}
	return result, nil
}

// take 把一条发布结果记进汇总。
//
// 计数由状态反推，而不是各条分支自己累加：分支多了之后，漏记一处就是
// 「报了几条、实际几条」对不上，而且只在失败路径上对不上，最难发现。
func (r *PublishResult) take(item PublishItem) {
	switch item.Status {
	case "published":
		r.Published++
	case "failed":
		r.Failed++
	}
	r.Items = append(r.Items, item)
}

// splitTargets 把待发目标按「插件 → 技能 → 工作流」切成三段。
//
// 三段各自走不同的发布路径：插件与工作流一件一条 Release，技能整批发一条。
// 组内保持传入次序（已按 ID 排定），报告因此与「这次打算发什么」一一对应。
func splitTargets(targets []publishTarget) (plugins, skills, workflows []publishTarget) {
	for _, target := range targets {
		switch target.kind {
		case distribution.KindPlugin:
			plugins = append(plugins, target)
		case distribution.KindWorkflow:
			workflows = append(workflows, target)
		default:
			skills = append(skills, target)
		}
	}
	return plugins, skills, workflows
}

// publishRun 是一次发布的运行上下文。
//
// 三个阶段用的是同一份路径、渠道与凭据，装进一个结构里传：每加一个阶段就要
// 把十来个参数再抄一遍，早晚会出现某一段用了另一个目录里的制品。
type publishRun struct {
	repoRoot    string
	repository  string
	channel     string
	distDir     string
	planCatalog string
	revision    string
	keyID       string
	signer      *rsa.PrivateKey
	dryRun      bool
	index       *catalog.Catalog
}

// single 发布一件「一件一条 Release」的扩展：插件与工作流走这条路。
//
// 任一步失败只把这一条判失败，后面的目标继续走：插件失败不该连带把工作流
// 一起停掉，报告里逐条写清谁卡在哪一步，比整批失败更好排查。
func (run publishRun) single(target publishTarget) PublishItem {
	item := PublishItem{Extension: target.label(), ID: target.id, Version: target.version}
	plan, err := releaseplan.Build(target.kind, target.dir, run.repository, run.channel, run.planCatalog)
	if err != nil {
		return item.failing("解析发布计划失败：" + err.Error())
	}
	plan.SourceCommit = run.revision
	item.Tag = plan.Tag
	item.Artifact = plan.ArtifactName
	artifactPath := filepath.Join(run.distDir, plan.ArtifactName)
	manifestPath := filepath.Join(run.distDir, plan.ManifestName)
	if err := packageExtension(target, artifactPath); err != nil {
		return item.failing("打包失败：" + err.Error())
	}
	if run.dryRun {
		if err := recordPlanned(run.index, target, plan, artifactPath, run.repository, run.planCatalog); err != nil {
			return item.failing("推演索引失败：" + err.Error())
		}
		item.Status = "dry-run"
		return item
	}
	release, err := buildRelease(plan, artifactPath, manifestPath, run.keyID, run.signer)
	if err != nil {
		return item.failing("签发失败：" + err.Error())
	}
	url, err := ensureRelease(run.repository, releaseSpec{
		tag:    plan.Tag,
		title:  fmt.Sprintf("%s %s", plan.ID, plan.Version),
		notes:  singleNotes(plan, release),
		target: release.SourceCommit,
		assets: []string{artifactPath, manifestPath},
	})
	if err != nil {
		return item.failing("发布 Release 失败：" + err.Error())
	}
	item.ReleaseURL = url
	if err := upsertCatalog(run.repoRoot, target.kind, target.dir, release, run.repository); err != nil {
		return item.failing("索引增补失败：" + err.Error())
	}
	item.Status = "published"
	return item
}

// batch 把一件件技能发成一条批次 Release，并逐件增补市场索引。
//
// 技能占本仓库扩展的绝大多数（ECC 技能库是 268 件），逐件发一条 Release 会让
// 仓库首页退化成版本流水账，回收也只能落在发布后面。整批发之后，一次同步在
// GitHub 上就是一条记录，成员身份仍然写在各自的制品名与发布清单里。
//
// 打包与签发仍然逐件做：批次只是容器，不是「拿来一张大表糊过去」——每件技能
// 的摘要、签名与发布清单必须各自独立、各自可校验。
func (run publishRun) batch(lock Lock, skills []publishTarget) ([]PublishItem, *PublishBatch) {
	items := make([]PublishItem, len(skills))
	ready := make([]readyTarget, 0, len(skills))
	for position, target := range skills {
		items[position] = PublishItem{Extension: target.label(), ID: target.id, Version: target.version}
		plan, err := releaseplan.Build(target.kind, target.dir, run.repository, run.channel, run.planCatalog)
		if err != nil {
			items[position] = items[position].failing("解析发布计划失败：" + err.Error())
			continue
		}
		plan.SourceCommit = run.revision
		artifactPath := filepath.Join(run.distDir, plan.ArtifactName)
		manifestPath := filepath.Join(run.distDir, plan.ManifestName)
		if err := packageExtension(target, artifactPath); err != nil {
			items[position] = items[position].failing("打包失败：" + err.Error())
			continue
		}
		ready = append(ready, readyTarget{
			position: position, target: target,
			artifactPath: artifactPath, manifestPath: manifestPath, plan: plan,
		})
	}
	if len(ready) == 0 {
		return items, nil
	}
	version, err := batchVersion(lock)
	if err != nil {
		return failRemaining(items, ready, "批次版本推导失败："+err.Error()), nil
	}
	tag, err := distribution.BatchTag(version)
	if err != nil {
		return failRemaining(items, ready, "批次 tag 不合法："+err.Error()), nil
	}

	// 成员清单与成员发布清单照旧逐件生成，只有「挂在哪条 Release 上」变成批次
	// tag：索引里的下载地址随之指向批次资产，安装器按同一条 tag 取清单。
	signed := make([]readyTarget, 0, len(ready))
	members := make([]distribution.BatchMember, 0, len(ready))
	for _, member := range ready {
		plan := member.plan
		plan.Tag = tag
		item := items[member.position]
		item.Tag = tag
		item.Artifact = plan.ArtifactName
		release, err := run.memberRelease(plan, member)
		if err != nil {
			items[member.position] = item.failing("签发失败：" + err.Error())
			continue
		}
		member.release = release
		signed = append(signed, member)
		members = append(members, distribution.BatchMember{
			Kind: member.target.kind, ID: member.target.id, Version: member.target.version,
			Manifest: plan.ManifestName, Artifact: release.Artifact,
		})
		items[member.position] = item
	}
	if len(signed) == 0 {
		return items, nil
	}

	summary := PublishBatch{Tag: tag, Version: version, Members: len(signed)}
	manifest := batchManifest(tag, version, run.repository, run.channel, run.revision,
		batchUpstream(lock), members, time.Now())
	if err := manifest.Validate(); err != nil {
		return failRemaining(items, signed, "批次清单不自洽："+err.Error()), nil
	}
	manifestPath := filepath.Join(run.distDir, manifest.ManifestName())
	if err := distribution.WriteBatchManifest(manifestPath, manifest); err != nil {
		return failRemaining(items, signed, "写批次清单失败："+err.Error()), nil
	}
	if run.dryRun {
		for _, member := range signed {
			plan := member.plan
			plan.Tag = tag
			if err := recordPlanned(run.index, member.target, plan, member.artifactPath, run.repository, run.planCatalog); err != nil {
				items[member.position] = items[member.position].failing("推演索引失败：" + err.Error())
				continue
			}
			items[member.position].Status = "dry-run"
		}
		return items, &summary
	}

	assets := make([]string, 0, len(signed)*2+1)
	for _, member := range signed {
		assets = append(assets, member.artifactPath, member.manifestPath)
	}
	assets = append(assets, manifestPath)
	url, err := ensureRelease(run.repository, releaseSpec{
		tag: tag,
		// 标题以上游版本打头：Release 列表是用户翻「这批对应上游哪一版」的地方，
		// 批次号写在 tag 里，标题再重复一遍只会挤掉真正有信息量的部分。
		title:  fmt.Sprintf("ECC %s 技能批次（%s，%d 件）", lock.Upstream.Version, batchSyncLabel(lock), len(signed)),
		notes:  batchNotes(version, lock, signed),
		target: run.revision,
		assets: assets,
	})
	if err != nil {
		return failRemaining(items, signed, "发布批次 Release 失败："+err.Error()), nil
	}
	summary.URL = url
	for _, member := range signed {
		if err := upsertCatalog(run.repoRoot, member.target.kind, member.target.dir, member.release, run.repository); err != nil {
			items[member.position] = items[member.position].failing("索引增补失败：" + err.Error())
			continue
		}
		items[member.position].ReleaseURL = url
		items[member.position].Status = "published"
	}
	return items, &summary
}

// memberRelease 生成一件成员技能的发布清单，并把清单落盘。
//
// 干跑不签名也不落盘：签名要私钥，而干跑的全部意义就是「不碰需要凭据的东西」。
// 清单本身照旧生成，后面的依赖 pin 与摘要比对走的是同一份事实。
func (run publishRun) memberRelease(plan releaseplan.Plan, member readyTarget) (distribution.ReleaseManifest, error) {
	data, err := os.ReadFile(member.artifactPath)
	if err != nil {
		return distribution.ReleaseManifest{}, err
	}
	digest := sha256.Sum256(data)
	artifact := distribution.ReleaseArtifact{
		Name:      plan.ArtifactName,
		SizeBytes: int64(len(data)),
		SHA256:    hex.EncodeToString(digest[:]),
	}
	if run.dryRun {
		return plan.Manifest(artifact, nil), nil
	}
	signature, err := signArtifact(member.artifactPath, plan.ArtifactName, data, run.keyID, run.signer)
	if err != nil {
		return distribution.ReleaseManifest{}, err
	}
	release := plan.Manifest(artifact, &signature)
	if err := release.Validate(); err != nil {
		return distribution.ReleaseManifest{}, err
	}
	if err := distribution.WriteReleaseManifest(member.manifestPath, release); err != nil {
		return distribution.ReleaseManifest{}, err
	}
	return release, nil
}

// failing 把一条汇报标成失败。
func (item PublishItem) failing(detail string) PublishItem {
	item.Status = "failed"
	item.Detail = detail
	return item
}

// failRemaining 把整批成员一起判失败：批次是一条 Release，挂不上去就是全都没发出去。
//
// 一件件报同一句原因，是因为用户要能一眼看出「这批里哪几件没上去」；报一条
// 「批次失败」再把成员藏起来，市场里少了几件就只能靠人翻索引找。
func failRemaining(items []PublishItem, members []readyTarget, detail string) []PublishItem {
	for _, member := range members {
		items[member.position] = items[member.position].failing(detail)
	}
	return items
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

// managedTag 从 tag 里取出归属键，并说明这是不是本仓库管得着的名字。
//
// 单件 tag 形如 plugin/com.mrbaoquan.ecc-skill-sync@1.0.3，剥掉 @ 后面的版本就是
// 扩展的归属键；批次 tag 形如 batch/2.2.2.3，它不属于任何单个扩展，归属键就是它
// 自己。认不出来的（不是我们这三种类型、没有 @、@ 前是空的）一律不管。
func managedTag(tag string) (string, bool) {
	if distribution.IsBatchTag(tag) {
		return strings.TrimSpace(tag), true
	}
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
// 留在仓库里。清扫范围按扩展清单与索引里记过的批次 tag 精确圈定，不是「见到
// Draft 就删」——仓库里别人发的 Release 不在名单里，不会被碰。
func staleDraftIDs(records []releaseRecord, managed, batches map[string]bool) []int64 {
	drafts := map[int64]bool{}
	for _, record := range records {
		if !record.Draft {
			continue
		}
		owner, ok := managedTag(record.Tag)
		if !ok || (!managed[owner] && !batches[owner]) {
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

// managedBatchTags 列出索引里记过的批次 tag。
//
// 批次 tag 里没有扩展 ID，认它只能靠「本仓库自己发过」这件事，而发布过的批次
// 都会以 release_tag 写进索引。没进过索引的批次 tag 不动：那可能是别人在同一个
// 仓库里发的，删掉就是删别人的东西。
func managedBatchTags(index *catalog.Catalog) map[string]bool {
	tags := map[string]bool{}
	if index == nil {
		return tags
	}
	for _, entry := range index.AllEntries() {
		if tag := catalogText(entry, "release_tag"); distribution.IsBatchTag(tag) {
			tags[tag] = true
		}
	}
	return tags
}

// sweepStaleDrafts 清掉仓库里本仓库名下的残留 Draft。
//
// 只在这里删，是因为走到这一步说明本次确实有东西要发；上游没变时整条链在
// 前面就结束了，不会为了「顺便扫一下」去动网络。
func sweepStaleDrafts(repository string, extensions extensionsDocument, batches map[string]bool) ([]int64, error) {
	records, err := listReleases(repository)
	if err != nil {
		return nil, err
	}
	ids := staleDraftIDs(records, managedOwners(extensions), batches)
	if err := deleteDraftIDs(repository, ids); err != nil {
		return nil, err
	}
	return ids, nil
}

// releaseSpec 是一条 Release 的建法：挂在哪、叫什么、说明写什么、带哪些资产。
//
// 批次把「一件制品加一份清单」变成了「一批制品加一批清单」，建 Release 的那几步
// 因此只认资产清单，不再认那个固定搭配。资产按路径给，重名由调用方保证不出现。
type releaseSpec struct {
	tag    string
	title  string
	notes  string
	target string
	assets []string
}

// ensureRelease 让 tag 对应的 Release 真的带上全部资产，并返回地址。
//
// 凭据走 gh 自己的登录态，不经由 AI 上下文传递。
//
// 为什么要先查再建、建完再回读：
//
//   - Release 建立与资产上传是两次调用，中间断一次就会留下「Release 在、资产
//     只传了一半」的中间态。这时候重跑若直接 create，只会撞到
//     「a release with the same tag name already exists」，把本可自愈的中间态
//     变成要人工清理的故障。
//   - 索引里的 download_url 指向的正是这些资产，资产没传全就写索引，等于把
//     「装不上」挂到市场上。所以索引只在回读确认之后才更新。
//   - Draft 也要一起接管：Draft 不占 tag，「同名 Draft + 同名正式发布」这种脏状态
//     重跑时既不会报错也不会自愈，只会在仓库里越积越多。开始之前先把同 tag 上的
//     Draft 清掉，正式发布才是这个 tag 上唯一说得清的那一条。
func ensureRelease(repository string, spec releaseSpec) (string, error) {
	state, err := inspectRelease(repository, spec.tag)
	if err != nil {
		return "", err
	}
	if err := deleteDrafts(repository, state.Drafts); err != nil {
		return "", err
	}
	if !state.Exists {
		if _, err := runGH(releaseCreateArgs(repository, spec)...); err != nil {
			return "", err
		}
		// 刚建出来的 Release 上一个资产都没有，清单直接按「全都要传」算，
		// 省掉一次把五百多个资产名拉回来的列表请求。
		state = releaseAssetState{Exists: true, Assets: map[string]bool{}, Drafts: map[int64]bool{}}
	}
	if missing := missingAssets(state, spec.assets); len(missing) > 0 {
		if err := uploadAssets(repository, spec.tag, missing); err != nil {
			return "", err
		}
	}

	verified, err := inspectRelease(repository, spec.tag)
	if err != nil {
		return "", err
	}
	for _, path := range spec.assets {
		name := filepath.Base(path)
		if !verified.Assets[name] {
			return "", fmt.Errorf("Release %s 缺少资产 %s，索引暂不更新", spec.tag, name)
		}
	}
	return verified.URL, nil
}

// missingAssets 列出这次要挂、但 Release 上还没有的资产路径。
func missingAssets(state releaseAssetState, assets []string) []string {
	missing := make([]string, 0, len(assets))
	for _, path := range assets {
		name := filepath.Base(path)
		if !state.Assets[name] {
			missing = append(missing, path)
		}
	}
	return missing
}

// releaseCreateArgs 拼出建 Release 的 gh 参数。
//
// 建 Release 这一步不挂资产，资产全部走 upload 分批挂上去。
//
// 一条批次 Release 的资产是五百多个（每件技能一件制品、一份发布清单，另加一份
// 批次清单），全塞进命令行会撞上 Windows 32767 字符的上限——而那时制品已经
// 打好包、签名已经做完，白跑一轮。分成两次调用没有副作用：Release 建出来到
// 资产挂满之间，索引还没有指向它，市场里看不到这条半成品。
func releaseCreateArgs(repository string, spec releaseSpec) []string {
	args := []string{
		"release", "create", spec.tag,
		"--repo", repository,
		"--title", spec.title,
		"--notes", spec.notes,
	}
	// 必须显式指定 tag 落在哪个提交上。
	//
	// 不写 --target 时 gh 会按默认分支的当前 HEAD 建 tag，而这里的来源提交是
	// 本地 HEAD：两者一旦不同（分支还没推），tag 就指到别人的提交上去了。
	// 发布清单里的 source_commit 记的是本地 HEAD，两边必须钉成同一个。
	if commit := strings.TrimSpace(spec.target); commit != "" {
		args = append(args, "--target", commit)
	}
	return args
}

// releaseUploadChunk 是单次 gh release upload 最多带几个资产。
//
// 分批只为绕开命令行长度上限，不为省请求：判断「资产到齐没有」在回读那一步
// 统一做，传了第几批不作数。
const releaseUploadChunk = 40

// uploadAssets 把资产分批挂到 Release 上，已存在同名资产的直接覆盖。
//
// 用 --clobber 而不是「跳过已存在」：上一次可能挂在了一半，覆盖是唯一能保证
// 「这批资产和本地这一份字节一致」的做法，摘要校验随后会证实这一点。
func uploadAssets(repository, tag string, assets []string) error {
	for start := 0; start < len(assets); start += releaseUploadChunk {
		end := start + releaseUploadChunk
		if end > len(assets) {
			end = len(assets)
		}
		args := append([]string{"release", "upload", tag, "--repo", repository, "--clobber"}, assets[start:end]...)
		if _, err := runGH(args...); err != nil {
			return err
		}
	}
	return nil
}

// singleNotes 是单件 Release 的说明：来源提交与制品摘要。
//
// 只写这两个事实：说明是给人排查用的，版本号已经在标题里，上游是谁由发布清单
// 说得更清楚，重复一遍只会让说明变长。
func singleNotes(plan releaseplan.Plan, release distribution.ReleaseManifest) string {
	return fmt.Sprintf("%s %s\n\n来源提交：%s\n制品摘要：%s",
		plan.ID, plan.Version, release.SourceCommit, release.Artifact.SHA256)
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

// ensureRevisionOnRemote 确认 revision 已经在发布目标仓库里。
//
// 拦在这里而不是靠 gh 报错，是因为 gh 那一步真的会成功：它会把 tag 建到默认
// 分支的 HEAD 上，于是「这次发布」看起来一切正常，只有回头 checkout tag 才
// 发现源码是旧的。这种错没有第二步会兜住，只能在发布前把目标钉死。
//
// 问发布目标仓库本身，而不是本地 origin：本地远端引用可能过期，发布仓库也能
// 被 --repository 覆盖，谁被写进 tag 就该由谁说了算。
func ensureRevisionOnRemote(repository, revision string) error {
	revision = strings.TrimSpace(revision)
	if revision == "" {
		return errors.New("读不到当前提交，发布中止：tag 需要一个明确的提交目标")
	}
	_, err := runGH("api", fmt.Sprintf("repos/%s/commits/%s", repository, revision), "--jq", ".sha")
	if err == nil {
		return nil
	}
	if revisionMissing(err.Error()) {
		return fmt.Errorf("当前提交 %s 还没推到 %s，发布中止：先推送分支，再让 tag 指向它",
			shortRevision(revision), repository)
	}
	// 限流、网络抖动之类只是这次问不出来，不该当成「没推」硬停：能不能建
	// Release 由 gh 那一步断言，这里多拦一次只会误伤本来能发的场景。
	return nil
}

// revisionMissing 判断 gh 的报错是不是「远端不认识这个提交」。
//
// 只有这一句决定硬停：远端不认识提交，意味着 tag 将来只能落到别的提交上，
// 必须拦住；其余（限流、网络）只是问不出来，交给后面的步骤去报。
//
// 两种说法都要认。查提交接口对「没有这个 SHA」回的是 422 加一句
// `No commit found for SHA`（不是 404），别的入口才会是 404 Not Found——
// 只认 404 的话这道闸门等于没装，实测就是这么漏过去的。
func revisionMissing(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "no commit found for sha") ||
		strings.Contains(lower, "not found") ||
		strings.Contains(lower, "http 404")
}

// shortRevision 把提交号截成短写法，用于给人看的报错。
func shortRevision(revision string) string {
	if len(revision) > 12 {
		return revision[:12]
	}
	return revision
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
