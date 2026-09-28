// Package eccsync 把上游 ECC 技能库迁移成 HiMind 扩展仓库。
//
// 这一层只做「确定性搬运」：读上游源码树 + 人工审阅过的元数据表，
// 产出可校验的 skill 目录、扩展清单与市场索引。任何需要判断的地方
// （改名、改写说明、剥离脚本）都不在这里猜，而是落进待审清单等人确认。
package eccsync

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PolicyVersion 是 upstream-policy.json 的结构版本。
const PolicyVersion = 1

// UpstreamRef 指向上游仓库与包标识。
type UpstreamRef struct {
	Repository string `json:"repository"`
	Package    string `json:"package"`
	License    string `json:"license"`
	Holder     string `json:"holder"`
}

// FilePolicy 约束技能包内允许出现的文件。
//
// HiMind 的技能包不允许携带可执行脚本（skillproject 会直接拒绝），
// 所以上游一旦出现 shell/python 辅助脚本，这个技能就不能按「逐字搬运」
// 处理，必须停下来让人决定排除还是改造成插件。
type FilePolicy struct {
	MaxFilesPerSkill int      `json:"max_files_per_skill"`
	MaxFileBytes     int64    `json:"max_file_bytes"`
	TextExtensions   []string `json:"text_extensions"`
}

// Rewrite 是一条机械替换规则，只作用于文本内容与相对路径。
type Rewrite struct {
	From string `json:"from"`
	To   string `json:"to"`
	Note string `json:"note,omitempty"`
}

// Policy 是 upstream-policy.json。
type Policy struct {
	SchemaVersion    int               `json:"schema_version"`
	Upstream         UpstreamRef       `json:"upstream"`
	ModuleCategories map[string]string `json:"module_categories"`
	ExcludedSkills   map[string]string `json:"excluded_skills"`
	FilePolicy       FilePolicy        `json:"file_policy"`
	Rewrites         []Rewrite         `json:"rewrites"`
	SkillIDPrefix    string            `json:"skill_id_prefix"`
	// MinAgentVersion 写进每个技能的 skill.json，约束安装门槛。
	MinAgentVersion string `json:"min_agent_version,omitempty"`
}

// MinAgentVersionOrDefault 返回清单里要写的安装门槛。
func (p Policy) MinAgentVersionOrDefault() string {
	if value := strings.TrimSpace(p.MinAgentVersion); value != "" {
		return value
	}
	return defaultMinAgentVersion
}

// defaultMinAgentVersion 与仓库既有技能保持一致：这批技能只含 Markdown 与清单，
// 没有额外能力依赖，跟着现有基线走即可。
const defaultMinAgentVersion = "0.3.37"

// LoadPolicy 读取策略文件。
func LoadPolicy(path string) (Policy, error) {
	var value Policy
	if err := readJSON(path, &value); err != nil {
		return Policy{}, err
	}
	if value.SchemaVersion != PolicyVersion {
		return Policy{}, fmt.Errorf("%s: schema_version 必须是 %d", path, PolicyVersion)
	}
	if strings.TrimSpace(value.Upstream.Repository) == "" {
		return Policy{}, fmt.Errorf("%s: upstream.repository 不能为空", path)
	}
	if strings.TrimSpace(value.SkillIDPrefix) == "" {
		return Policy{}, fmt.Errorf("%s: skill_id_prefix 不能为空", path)
	}
	if value.FilePolicy.MaxFilesPerSkill <= 0 || value.FilePolicy.MaxFileBytes <= 0 {
		return Policy{}, fmt.Errorf("%s: file_policy 必须给出 max_files_per_skill 与 max_file_bytes", path)
	}
	return value, nil
}

// MetadataVersion 是 manifests/metadata.json 的结构版本。
const MetadataVersion = 1

// MetadataEntry 是一个技能的对外元数据。它是人工审阅层：
// 生成器只读取，不臆造。
type MetadataEntry struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Categories  []string `json:"categories"`
	// Source 记录这条元数据从哪来：reviewed 表示人工确认过，derived 表示由上游 frontmatter 机械推导。
	Source string `json:"source"`
	// Note 记录推导时的妥协点，供后续人工复核。
	Note string `json:"note,omitempty"`
}

// Metadata 是 manifests/metadata.json。
type Metadata struct {
	SchemaVersion int                      `json:"schema_version"`
	ReviewedAt    string                   `json:"reviewed_at,omitempty"`
	Skills        map[string]MetadataEntry `json:"skills"`
}

// LoadMetadata 读取元数据表；文件不存在时返回空表，便于首次生成。
func LoadMetadata(path string) (Metadata, error) {
	value := Metadata{SchemaVersion: MetadataVersion, Skills: map[string]MetadataEntry{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return value, nil
		}
		return Metadata{}, err
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return Metadata{}, fmt.Errorf("%s: %w", path, err)
	}
	if value.SchemaVersion != MetadataVersion {
		return Metadata{}, fmt.Errorf("%s: schema_version 必须是 %d", path, MetadataVersion)
	}
	if value.Skills == nil {
		value.Skills = map[string]MetadataEntry{}
	}
	return value, nil
}

// SaveMetadata 写回元数据表，键固定排序，保证重复执行字节一致。
func SaveMetadata(path string, value Metadata) error {
	if value.SchemaVersion == 0 {
		value.SchemaVersion = MetadataVersion
	}
	if value.Skills == nil {
		value.Skills = map[string]MetadataEntry{}
	}
	return writeJSON(path, value)
}

// LockVersion 是 upstream.lock.json 的结构版本。
const LockVersion = 1

// LockUpstream 记录本次同步对齐的上游版本。
type LockUpstream struct {
	Repository    string `json:"repository"`
	Package       string `json:"package"`
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	CommitDate    string `json:"commit_date,omitempty"`
	Channel       string `json:"channel,omitempty"`
	License       string `json:"license"`
	LicenseHolder string `json:"license_holder"`
}

// LockSync 记录同步序号与生成时间。序号进版本号的第三段，
// 因为 Agent 的版本比较只看前三段数字，后缀不会触发更新。
type LockSync struct {
	Sequence    int    `json:"sequence"`
	GeneratedAt string `json:"generated_at"`
	Tool        string `json:"tool"`
}

// LockSkill 记录单个技能的搬运事实。
type LockSkill struct {
	Version string `json:"version"`
	// SourceDigest 是上游技能目录内容的摘要，用来判断「有没有真变化」。
	SourceDigest string `json:"source_digest"`
	// SyncedDigest 是上次搬进本仓库后技能目录内容的摘要。
	SyncedDigest string `json:"synced_digest,omitempty"`
	// MetadataDigest 是对外元数据的摘要。
	//
	// 技能正文没动、但市场里显示的文案被人校对过时，只有这个摘要会变。
	// 少了它，改完文案不会涨版本，已发布的技能永远更新不上去。
	MetadataDigest string `json:"metadata_digest,omitempty"`
	// SourceCommit 记录这个技能的搬运基线来自哪次上游提交。
	SourceCommit string `json:"source_commit,omitempty"`
	// ReviewedDigest 是「人工校对这条市场文案时，上游正文长什么样」。
	//
	// 有了它才分得清两种变化：文案被校对过（MetadataDigest 变）与上游正文
	// 又动过（SourceDigest 变）。两者都不稀奇，稀奇的是第二种发生在第一种之后——
	// 那时文案还没过时，但正在过时的路上，需要重新读一遍，而正文本身没变时
	// 这条基线一动都不该动。
	ReviewedDigest string `json:"reviewed_digest,omitempty"`
	SourcePath     string `json:"source_path"`
	Tier           string `json:"tier"`
	Rewritten      bool   `json:"rewritten,omitempty"`
}

// Lock 是 upstream.lock.json。
type Lock struct {
	SchemaVersion int                  `json:"schema_version"`
	Upstream      LockUpstream         `json:"upstream"`
	Sync          LockSync             `json:"sync"`
	Skills        map[string]LockSkill `json:"skills"`
}

// LoadLock 读取锁文件；不存在时返回空锁。
func LoadLock(path string) (Lock, error) {
	value := Lock{SchemaVersion: LockVersion, Skills: map[string]LockSkill{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return value, nil
		}
		return Lock{}, err
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return Lock{}, fmt.Errorf("%s: %w", path, err)
	}
	if value.SchemaVersion != LockVersion {
		return Lock{}, fmt.Errorf("%s: schema_version 必须是 %d", path, LockVersion)
	}
	if value.Skills == nil {
		value.Skills = map[string]LockSkill{}
	}
	return value, nil
}

// SaveLock 写回锁文件。
func SaveLock(path string, value Lock) error {
	if value.SchemaVersion == 0 {
		value.SchemaVersion = LockVersion
	}
	if value.Skills == nil {
		value.Skills = map[string]LockSkill{}
	}
	return writeJSON(path, value)
}

// QuarantineEntry 是一个被拦下的上游技能。
//
// 被拦下不等于被删除：条目连同原因留在 manifests/quarantine.json 里，
// 每次同步重写，保证「谁没进来、为什么」始终可查。
type QuarantineEntry struct {
	Slug   string   `json:"slug"`
	Reason string   `json:"reason"`
	Files  []string `json:"files,omitempty"`
}

// Quarantine 是 manifests/quarantine.json。
type Quarantine struct {
	SchemaVersion int               `json:"schema_version"`
	GeneratedAt   string            `json:"generated_at"`
	UpstreamSHA   string            `json:"upstream_commit"`
	Entries       []QuarantineEntry `json:"entries"`
}

// QuarantineVersion 是 quarantine.json 的结构版本。
const QuarantineVersion = 1

// LoadQuarantine 读取隔离清单；文件不存在时返回空清单。
func LoadQuarantine(path string) (Quarantine, error) {
	value := Quarantine{SchemaVersion: QuarantineVersion, Entries: []QuarantineEntry{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return value, nil
		}
		return Quarantine{}, err
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return Quarantine{}, fmt.Errorf("%s: %w", path, err)
	}
	if value.Entries == nil {
		value.Entries = []QuarantineEntry{}
	}
	return value, nil
}

func readJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// writeJSON 以固定缩进写文件，末尾补换行，让同一份输入产出同一份字节。
func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
