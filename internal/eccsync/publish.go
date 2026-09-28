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
	"strings"
	"time"

	"github.com/MrBaoquan/himind-extensions/tooling/catalog"
	"github.com/MrBaoquan/himind-extensions/tooling/distribution"
	"github.com/MrBaoquan/himind-extensions/tooling/skillproject"
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

// PublishItem 是单个技能的发布结果。
type PublishItem struct {
	Skill      string `json:"skill"`
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
	Repository string        `json:"repository"`
	Channel    string        `json:"channel"`
	DryRun     bool          `json:"dry_run"`
	Pending    int           `json:"pending"`
	Held       []string      `json:"held_for_metadata_review,omitempty"`
	Published  int           `json:"published"`
	Failed     int           `json:"failed"`
	Items      []PublishItem `json:"items"`
}

// Publish 把「本地已生成、索引里还没有」的版本发到 GitHub Release，
// 再按同一份发布清单增补市场索引。
//
// 顺序不能反：索引里的 download_url 必须指向已经存在的 Release 资产，
// 先写索引就等于把「装不上」的东西挂到市场上。
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
	result := PublishResult{Repository: repository, Channel: channel, DryRun: options.DryRun}

	index, err := catalog.Load(filepath.Join(repoRoot, filepath.FromSlash(CatalogFile)))
	if err != nil {
		return result, err
	}
	pending := pendingSkills(lock, index)
	if !options.AllowDerived {
		metadata, err := LoadMetadata(filepath.Join(repoRoot, filepath.FromSlash(MetadataFile)))
		if err != nil {
			return result, err
		}
		pending, result.Held = splitByMetadataSource(pending, metadata)
	}
	result.Pending = len(pending)
	if options.Limit > 0 && len(pending) > options.Limit {
		pending = pending[:options.Limit]
	}
	if len(pending) == 0 {
		return result, nil
	}

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

	for _, slug := range pending {
		item := PublishItem{Skill: slug, ID: SkillID(slug), Version: lock.Skills[slug].Version}
		tag, tagErr := distribution.ReleaseTag(distribution.KindSkill, item.ID, item.Version)
		if tagErr != nil {
			return result, tagErr
		}
		item.Tag = tag
		artifactName, nameErr := distribution.ArtifactName(distribution.KindSkill, item.ID, item.Version)
		if nameErr != nil {
			return result, nameErr
		}
		item.Artifact = artifactName

		skillDir := filepath.Join(repoRoot, "skills", slug)
		artifactPath := filepath.Join(distDir, artifactName)
		manifestPath := filepath.Join(distDir, distribution.ManifestName(item.ID, item.Version))

		if err := os.MkdirAll(distDir, 0o755); err != nil {
			return result, err
		}
		if err := skillproject.Package(skillDir, artifactPath); err != nil {
			item.Status = "failed"
			item.Detail = "打包失败：" + err.Error()
			result.Failed++
			result.Items = append(result.Items, item)
			continue
		}
		if options.DryRun {
			item.Status = "dry-run"
			result.Items = append(result.Items, item)
			continue
		}
		release, err := buildRelease(repoRoot, skillDir, artifactPath, manifestPath, tag, item, repository, channel, keyID, signer)
		if err != nil {
			item.Status = "failed"
			item.Detail = "签发失败：" + err.Error()
			result.Failed++
			result.Items = append(result.Items, item)
			continue
		}
		url, err := createRelease(repository, tag, item, artifactPath, manifestPath, release)
		if err != nil {
			item.Status = "failed"
			item.Detail = "创建 Release 失败：" + err.Error()
			result.Failed++
			result.Items = append(result.Items, item)
			continue
		}
		item.ReleaseURL = url
		if err := upsertCatalog(repoRoot, skillDir, manifestPath, artifactPath, release, repository); err != nil {
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

// DefaultPrivateKeyPath 与 DefaultSigningKeyID 是 Agent 生产密钥的默认位置。
const (
	DefaultPrivateKeyPath = `C:\Users\Administrator\AppData\Local\HiMind\signing\private\himind-production-2026.key.pem`
	DefaultSigningKeyID   = "himind-production-2026"
)

// pendingSkills 找出「本地锁文件里的版本还没进索引」的技能。
func pendingSkills(lock Lock, index *catalog.Catalog) []string {
	published := map[string]string{}
	for _, entry := range index.Skills {
		id := catalog.IDOf(distribution.KindSkill, entry)
		version, _ := entry["version"].(string)
		published[id] = version
	}
	result := []string{}
	for slug, item := range lock.Skills {
		if published[SkillID(slug)] == item.Version {
			continue
		}
		result = append(result, slug)
	}
	sortStrings(result)
	return result
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// splitByMetadataSource 把「元数据还没人工确认」的技能挑出来。
//
// 判定只看 manifests/metadata.json 里的 source 字段：reviewed 是有人读过
// 的名字与说明，derived 是机器从上游 frontmatter 压出来的临时产物。
// 释放闸门只有一个动作——把那条记录改成 reviewed。
func splitByMetadataSource(pending []string, metadata Metadata) (ready []string, held []string) {
	for _, slug := range pending {
		entry, ok := metadata.Skills[slug]
		if ok && entry.Source == "reviewed" {
			ready = append(ready, slug)
			continue
		}
		held = append(held, slug)
	}
	return ready, held
}

// buildRelease 打包 + 签名 + 写发布清单，返回清单内容。
func buildRelease(repoRoot, skillDir, artifactPath, manifestPath, tag string, item PublishItem, repository, channel, keyID string, signer *rsa.PrivateKey) (distribution.ReleaseManifest, error) {
	data, err := os.ReadFile(artifactPath)
	if err != nil {
		return distribution.ReleaseManifest{}, err
	}
	digest := sha256.Sum256(data)
	manifest, err := catalog.ReadManifest(skillDir, distribution.KindSkill)
	if err != nil {
		return distribution.ReleaseManifest{}, err
	}
	signature, err := signArtifact(artifactPath, item.Artifact, data, keyID, signer)
	if err != nil {
		return distribution.ReleaseManifest{}, err
	}
	release := distribution.ReleaseManifest{
		SchemaVersion:   distribution.ReleaseManifestSchema,
		Repository:      repository,
		Tag:             tag,
		Kind:            distribution.KindSkill,
		ID:              item.ID,
		Version:         item.Version,
		Channel:         channel,
		SourceCommit:    repoRevision(repoRoot),
		MinAgentVersion: manifest.MinAgentVersion,
		Artifact: distribution.ReleaseArtifact{
			Name:      item.Artifact,
			SizeBytes: int64(len(data)),
			SHA256:    hex.EncodeToString(digest[:]),
		},
		Signature: &signature,
	}
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

// createRelease 用 gh 创建 Release 并上传制品与发布清单。
// 凭据走 gh 自己的登录态，不经由 AI 上下文传递。
func createRelease(repository, tag string, item PublishItem, artifactPath, manifestPath string, release distribution.ReleaseManifest) (string, error) {
	notes := fmt.Sprintf("ECC 技能同步：%s %s\n\n上游提交：%s\n制品摘要：%s",
		item.ID, item.Version, release.SourceCommit, release.Artifact.SHA256)
	args := []string{
		"release", "create", tag,
		artifactPath, manifestPath,
		"--repo", repository,
		"--title", fmt.Sprintf("%s %s", item.ID, item.Version),
		"--notes", notes,
	}
	command := exec.Command("gh", args...)
	configureHiddenCommand(command)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

// upsertCatalog 把这次发布写进市场索引。
func upsertCatalog(repoRoot, skillDir, manifestPath, artifactPath string, release distribution.ReleaseManifest, repository string) error {
	path := filepath.Join(repoRoot, filepath.FromSlash(CatalogFile))
	index, err := catalog.Load(path)
	if err != nil {
		return err
	}
	manifest, err := catalog.ReadManifest(skillDir, distribution.KindSkill)
	if err != nil {
		return err
	}
	entry, err := catalog.Entry(distribution.KindSkill, manifest, catalog.ReleaseFacts{
		Release:     release,
		PublishedAt: time.Now().UTC().Format(time.RFC3339),
		Repository:  repository,
	})
	if err != nil {
		return err
	}
	if err := catalog.ValidateEntry(distribution.KindSkill, entry, release); err != nil {
		return err
	}
	if err := index.Upsert(distribution.KindSkill, entry); err != nil {
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
