package eccsync

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SourceSkill 是上游技能目录在内存里的投影。
type SourceSkill struct {
	Slug        string
	SourcePath  string
	Frontmatter map[string]string
	Files       []SourceFile
	// Modules 是这个技能所属的上游模块 id，按字典序排列。
	Modules []string
	// Blocked 列出命中「技能包不允许携带」规则的文件，非空即不可自动搬运。
	Blocked []string
	// Oversize 列出超出体积上限的文件。
	Oversize []string
	Digest   string
}

// SourceFile 是一个待搬运文件。
type SourceFile struct {
	// RelativePath 是相对技能目录的斜杠路径，也就是写进 skill.json contents 的名字。
	RelativePath string
	Content      []byte
}

// Source 是整棵上游技能树的扫描结果。
type Source struct {
	Root      string
	Version   string
	Skills    []SourceSkill
	ModuleMap map[string][]string
}

// Discover 扫描上游源码树。
//
// 只认 `<root>/skills/<slug>/SKILL.md` 这一处：上游把同一批技能在
// `.claude`、`.cursor`、`.codex` 等目录里复制了很多份，那些是安装产物，
// 不是真源。
func Discover(root string, policy Policy) (Source, error) {
	skillsRoot := filepath.Join(root, "skills")
	entries, err := os.ReadDir(skillsRoot)
	if err != nil {
		return Source{}, fmt.Errorf("上游缺少 skills 目录: %w", err)
	}
	moduleMap, err := readModuleMap(root, policy)
	if err != nil {
		return Source{}, err
	}
	result := Source{Root: root, ModuleMap: moduleMap, Version: readUpstreamVersion(root)}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		slug := entry.Name()
		directory := filepath.Join(skillsRoot, slug)
		if _, err := os.Stat(filepath.Join(directory, "SKILL.md")); err != nil {
			continue
		}
		skill, err := readSourceSkill(directory, slug, policy)
		if err != nil {
			return Source{}, err
		}
		skill.Modules = moduleMap[slug]
		result.Skills = append(result.Skills, skill)
	}
	sort.Slice(result.Skills, func(i, j int) bool { return result.Skills[i].Slug < result.Skills[j].Slug })
	return result, nil
}

func readSourceSkill(directory, slug string, policy Policy) (SourceSkill, error) {
	result := SourceSkill{Slug: slug, SourcePath: "skills/" + slug}
	err := filepath.WalkDir(directory, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if current != directory && strings.HasPrefix(entry.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(entry.Name(), ".") {
			return nil
		}
		relative, err := filepath.Rel(directory, current)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if blockedPath(relative) {
			result.Blocked = append(result.Blocked, relative)
			return nil
		}
		if info.Size() > policy.FilePolicy.MaxFileBytes {
			result.Oversize = append(result.Oversize, relative)
			return nil
		}
		content, err := os.ReadFile(current)
		if err != nil {
			return err
		}
		result.Files = append(result.Files, SourceFile{RelativePath: relative, Content: content})
		return nil
	})
	if err != nil {
		return SourceSkill{}, fmt.Errorf("%s: %w", directory, err)
	}
	sort.Slice(result.Files, func(i, j int) bool { return result.Files[i].RelativePath < result.Files[j].RelativePath })
	sort.Strings(result.Blocked)
	sort.Strings(result.Oversize)
	for _, file := range result.Files {
		if file.RelativePath == "SKILL.md" {
			result.Frontmatter = parseFrontmatter(string(file.Content))
		}
	}
	result.Digest = digestFiles(result.Files, result.Blocked)
	return result, nil
}

// blockedPath 复刻 skillproject 的拒绝规则。
//
// 上游的 shell / python 辅助脚本进了技能包会被 Agent 侧校验直接拒收，
// 所以这里提前拦下并点名，而不是生成一个装不上的包。
func blockedPath(relative string) bool {
	lower := strings.ToLower(relative)
	for _, suffix := range []string{".ps1", ".sh", ".bat", ".cmd", ".exe", ".dll", ".pyc", ".so", ".dylib"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	for _, segment := range []string{"/scripts/", "/bin/"} {
		if strings.Contains(lower, segment) {
			return true
		}
	}
	return strings.HasPrefix(lower, "scripts/") || strings.HasPrefix(lower, "bin/")
}

// digestFiles 对技能目录内容做稳定摘要，用于「有没有真变化」的判断。
func digestFiles(files []SourceFile, blocked []string) string {
	hash := sha256.New()
	for _, file := range files {
		fmt.Fprintf(hash, "%s\x00%d\x00", file.RelativePath, len(file.Content))
		hash.Write(file.Content)
		hash.Write([]byte{0})
	}
	for _, name := range blocked {
		fmt.Fprintf(hash, "blocked\x00%s\x00", name)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// ParseFrontmatter 解析 SKILL.md 顶部的 YAML frontmatter。
// 只支持上游实际使用的单行标量：`key: value`，值可用单双引号包裹。
func ParseFrontmatter(document string) map[string]string {
	return parseFrontmatter(document)
}

func parseFrontmatter(document string) map[string]string {
	document = strings.TrimPrefix(document, "\ufeff")
	fields := map[string]string{}
	if !strings.HasPrefix(document, "---") {
		return fields
	}
	rest := document[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return fields
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		colon := strings.Index(line, ":")
		if colon <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:colon])
		value := strings.TrimSpace(line[colon+1:])
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		fields[key] = value
	}
	return fields
}

// readModuleMap 把上游 install-modules.json 里的「模块 → 技能」关系读成技能视角的索引。
//
// 只取 paths 里指向 skills/ 的模块；上游同一模块还挂着 rules、agents 之类的
// 非技能路径，那些不是我们要搬的东西。
func readModuleMap(root string, policy Policy) (map[string][]string, error) {
	var document struct {
		Modules []struct {
			ID    string   `json:"id"`
			Kind  string   `json:"kind"`
			Paths []string `json:"paths"`
		} `json:"modules"`
	}
	if err := readJSON(filepath.Join(root, "manifests", "install-modules.json"), &document); err != nil {
		return nil, fmt.Errorf("上游缺少 manifests/install-modules.json: %w", err)
	}
	result := map[string][]string{}
	for _, module := range document.Modules {
		if _, ok := policy.ModuleCategories[module.ID]; !ok {
			continue
		}
		for _, item := range module.Paths {
			clean := path.Clean(strings.TrimSuffix(filepath.ToSlash(item), "/"))
			if !strings.HasPrefix(clean, "skills/") {
				continue
			}
			remainder := strings.TrimPrefix(clean, "skills/")
			if remainder == "" {
				continue
			}
			slug := remainder
			if slash := strings.Index(remainder, "/"); slash >= 0 {
				slug = remainder[:slash]
			}
			result[slug] = append(result[slug], module.ID)
		}
	}
	for slug := range result {
		sort.Strings(result[slug])
	}
	return result, nil
}

// readUpstreamVersion 读上游 package.json 的 version，用作制品版本的第一二段。
func readUpstreamVersion(root string) string {
	var document struct {
		Version string `json:"version"`
	}
	if err := readJSON(filepath.Join(root, "package.json"), &document); err != nil {
		return ""
	}
	return strings.TrimSpace(document.Version)
}

// DeriveEntry 从上游 frontmatter 机械推导一条元数据。
//
// 推导不追求好读，只追求「不用模型也能重复得到同一结果」：
// 名称取 frontmatter 的 name 做标题化，说明取用途句（自动截断到硬上限）。
// 推导过程中做过妥协的（截断、名称超长）会在 Note 里留痕。
func DeriveEntry(skill SourceSkill, categories []string) (MetadataEntry, bool) {
	entry := MetadataEntry{Source: "derived", Categories: categories}
	name := titleCase(strings.TrimSpace(skill.Frontmatter["name"]))
	if name == "" {
		name = titleCase(skill.Slug)
	}
	notes := []string{}
	if utf8.RuneCountInString(name) > 18 {
		short := shortenName(name, 18)
		if short == "" {
			return entry, false
		}
		notes = append(notes, "显示名称由上游客名缩写而来")
		name = short
	}
	entry.Name = name

	description := normalizeSpace(skill.Frontmatter["description"])
	if description == "" {
		// 没有 frontmatter 说明时退回正文第一段，但仍然标记待复核。
		description = firstParagraph(documentOf(skill))
		notes = append(notes, "上游未提供 frontmatter description，取自正文首段")
	}
	if description == "" {
		return entry, false
	}
	trimmed, truncated := clampDescription(description, 120)
	entry.Description = trimmed
	if truncated {
		notes = append(notes, "说明超过 120 字，已按句/词边界截断")
	}
	if len(notes) > 0 {
		entry.Note = strings.Join(notes, "；")
	}
	return entry, true
}

func documentOf(skill SourceSkill) string {
	for _, file := range skill.Files {
		if file.RelativePath == "SKILL.md" {
			return string(file.Content)
		}
	}
	return ""
}

func normalizeSpace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// clampDescription 把说明压进硬上限：优先保留完整句子，其次按词边界截断。
func clampDescription(value string, limit int) (string, bool) {
	if utf8.RuneCountInString(value) <= limit {
		return value, false
	}
	runes := []rune(value)
	// 先试句子边界：截到 limit 之前最后一个句末标记。
	for index := limit; index > limit/2; index-- {
		switch runes[index-1] {
		case '.', '!', '?', '。', '！', '？':
			candidate := strings.TrimSpace(string(runes[:index]))
			if utf8.RuneCountInString(candidate) <= limit {
				return candidate, true
			}
		}
	}
	// 再退到词边界。
	cut := limit - 1
	for cut > limit/2 && !unicode.IsSpace(runes[cut]) {
		cut--
	}
	return strings.TrimSpace(string(runes[:cut])) + "…", true
}

// shortenName 把超长名称缩到硬上限。
//
// 英语名词短语的语义中心在末尾（Architecture Decision Records 说的是 Records），
// 所以先丢开头的限定词，再退到从尾部按词边界截断。反过来先砍尾巴会得到
// 一堆只剩「Architecture」「Management」的空壳名字。
func shortenName(value string, limit int) string {
	value = strings.TrimSpace(value)
	if index := strings.IndexAny(value, "("); index > 0 {
		value = strings.TrimSpace(value[:index])
	}
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	words := strings.Fields(value)
	for start := 1; start < len(words); start++ {
		candidate := strings.Join(words[start:], " ")
		if utf8.RuneCountInString(candidate) <= limit {
			return candidate
		}
	}
	for end := len(words) - 1; end > 1; end-- {
		candidate := strings.Join(words[:end], " ")
		if utf8.RuneCountInString(candidate) <= limit {
			return candidate
		}
	}
	return ""
}

// titleCase 把 hyphen / underscore / 空格分隔的标识符变成标题形式。
func titleCase(value string) string {
	words := strings.FieldsFunc(value, func(r rune) bool {
		return r == '-' || r == '_' || r == ' '
	})
	for index, word := range words {
		runes := []rune(strings.ToLower(word))
		if len(runes) > 0 {
			runes[0] = unicode.ToUpper(runes[0])
		}
		words[index] = string(runes)
	}
	return strings.Join(words, " ")
}

// firstParagraph 取正文第一段可用文本，跳过标题与 frontmatter。
func firstParagraph(document string) string {
	body := strings.TrimPrefix(document, "\ufeff")
	if strings.HasPrefix(body, "---") {
		if end := strings.Index(body[3:], "\n---"); end >= 0 {
			body = body[3+end+4:]
		}
	}
	for _, block := range strings.Split(body, "\n\n") {
		trimmed := strings.TrimSpace(block)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "|") || strings.HasPrefix(trimmed, "```") {
			continue
		}
		return normalizeSpace(trimmed)
	}
	return ""
}
