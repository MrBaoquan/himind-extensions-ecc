package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/MrBaoquan/himind-extensions-ecc/internal/eccsync"
	"github.com/MrBaoquan/himind-extensions/sdk/jsonrpc"
)

// 面板上的仓库根目录不该靠人抄。
//
// 之前填的是宿主给的 workspace_root，那是「这次开发任务开在哪个目录」，
// 与 ECC 仓库在哪毫无关系，于是第一次打开面板必然填错。真实答案是一份已经
// 存在的事实：本插件是从本机注册的哪个市场源装进来的，那个源的 repository
// 就是仓库根目录。
//
// 推断只负责「提名」，能不能用仍然由 upstream-policy.json 说了算。猜错一个
// 目录的代价是继续报错，而不是把生成物写进别人的仓库。
//
// 只读事实来源，缺任何一个都只是少一条线索：
//   - <agent_home>/data/extension-sources.json：本机注册的市场源
//   - <agent_home>/plugins/<plugin_id>/current/policy.json：本插件的安装来源
//   - <repo> 里的仓库清单（extensions.json / .himind/catalog.json）：这个仓库确实发布本插件
const (
	ownPluginID   = "com.mrbaoquan.ecc-skill-sync"
	ownWorkflowID = "com.mrbaoquan.workflow.ecc-skill-sync"

	// repoRootEnv 是留给脚本与调试用的显式覆盖，优先级高于一切推断。
	repoRootEnv = "HIMIND_ECC_REPO_ROOT"
)

// 线索权重。差值拉得开，是为了让「确定的事实」永远压过「看起来像」：
// 安装来源与清单命中说的是同一个仓库，名字里带 ecc 只是巧合。
const (
	scoreInstallSource = 400 // 本插件就是从这个源装的
	scoreCatalogLists  = 200 // 这个仓库的清单里列着本插件或本工作流
	scoreHasPolicy     = 50  // 目录里有 upstream-policy.json
	scoreLooksLikeECC  = 10  // 名字或分发标识里带 ecc
	scoreEnabled       = 2   // 源处于启用状态
)

// repoCandidate 是一条「这个目录可能是 ECC 仓库」的提名及其依据。
//
// HasPolicy 是唯一决定能不能用的字段，其余字段是给人看的：路径填错时，
// 面板要能解释为什么选它，而不是给一个无法反驳的答案。
type repoCandidate struct {
	Path       string `json:"path"`
	SourceID   string `json:"source_id"`
	SourceName string `json:"source_name"`
	AgentHome  string `json:"agent_home"`
	CatalogHit bool   `json:"catalog_hit"`
	InstallHit bool   `json:"install_hit"`
	HasPolicy  bool   `json:"has_policy"`
	Enabled    bool   `json:"enabled"`
	Score      int    `json:"score"`
}

// extensionSourcesFile 只解出推断用得上的字段。
//
// Agent 那份文件里还有 acquisitions 之类的段落，插件不去碰它们：多读一处，
// 就多一处随宿主升级而失效的依赖。
type extensionSourcesFile struct {
	Sources []extensionSource `json:"sources"`
}

type extensionSource struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Kind           string `json:"kind"`
	Repository     string `json:"repository"`
	CatalogPath    string `json:"catalog_path"`
	Enabled        bool   `json:"enabled"`
	DistributionID string `json:"distribution_id"`
}

// repoHint 是面板打开时问的一句话：「仓库该是哪个目录？」
//
// 入参带上界面此刻手里的路径，是为了让「记住的路径还能不能用」也在同一次
// 往返里问清楚。分开问会让界面出现一段「先渲染错路径、再自我纠正」的空窗。
func repoHint(in input) (any, *jsonrpc.Error) {
	handed := strings.TrimSpace(in.RepoRoot)
	if handed == "" {
		handed = strings.TrimSpace(in.WorkspaceRoot)
	}
	candidates := inferRepoCandidates()
	hint := map[string]any{
		"policy_file": eccsync.PolicyFile,
		"agent_homes": agentHomes(),
		"candidates":  candidates,
	}
	if handed != "" {
		absolute, err := filepath.Abs(handed)
		if err != nil {
			absolute = handed
		}
		valid, reason := usableRepoRoot(absolute)
		current := map[string]any{"path": absolute, "valid": valid}
		if !valid {
			current["reason"] = reason
		}
		hint["current"] = current
	}
	usable := usableCandidates(candidates)
	switch {
	case len(usable) == 1:
		hint["auto"] = map[string]any{"path": usable[0].Path, "count": 1, "ambiguous": false}
	case len(usable) > 1:
		paths := make([]string, 0, len(usable))
		for _, candidate := range usable {
			paths = append(paths, candidate.Path)
		}
		hint["auto"] = map[string]any{
			"path":       usable[0].Path,
			"count":      len(usable),
			"ambiguous":  true,
			"candidates": paths,
		}
	}
	return map[string]any{"ok": true, "repo_hint": hint}, nil
}

// inferredRepoRoot 在没有显式入参时给出唯一可用的仓库根目录。
//
// 多个候选宁可报错也不挑一个：本机同时放着两个 ECC 仓库时，猜错一次就是
// 把这一批生成物发到另一个仓库去，事后要靠 diff 才发现。
func inferredRepoRoot() (string, []repoCandidate) {
	usable := usableCandidates(inferRepoCandidates())
	if len(usable) == 0 {
		return "", nil
	}
	return usable[0].Path, usable
}

// usableCandidates 收窄到「真的能用」的提名，并保持既有顺序。
func usableCandidates(candidates []repoCandidate) []repoCandidate {
	usable := make([]repoCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.HasPolicy {
			usable = append(usable, candidate)
		}
	}
	return usable
}

// usableRepoRoot 回答「这个目录能不能当仓库根目录用」，理由与原实现同源。
func usableRepoRoot(path string) (bool, string) {
	if strings.TrimSpace(path) == "" {
		return false, "没有填仓库根目录"
	}
	info, err := os.Stat(filepath.Join(path, eccsync.PolicyFile))
	if err != nil || info.IsDir() {
		return false, "不是 ECC 同步仓库（缺少 " + eccsync.PolicyFile + "）"
	}
	return true, ""
}

// inferRepoCandidates 把本机认得的本地市场源逐条提名出来。
func inferRepoCandidates() []repoCandidate {
	var candidates []repoCandidate
	seen := map[string]int{}
	for _, home := range agentHomes() {
		installSource := installedFromSource(home)
		for _, source := range localSources(home) {
			path := strings.TrimSpace(source.Repository)
			if path == "" {
				continue
			}
			absolute, err := filepath.Abs(path)
			if err != nil {
				continue
			}
			candidate := repoCandidate{
				Path:       absolute,
				SourceID:   source.ID,
				SourceName: source.Name,
				AgentHome:  home,
				Enabled:    source.Enabled,
				CatalogHit: catalogListsOwnExtension(absolute, source.CatalogPath),
				InstallHit: installSource != "" && installSource == source.ID,
			}
			if valid, _ := usableRepoRoot(absolute); valid {
				candidate.HasPolicy = true
				candidate.Score += scoreHasPolicy
			}
			if candidate.CatalogHit {
				candidate.Score += scoreCatalogLists
			}
			if candidate.InstallHit {
				candidate.Score += scoreInstallSource
			}
			if candidate.Enabled {
				candidate.Score += scoreEnabled
			}
			if looksLikeECC(source, absolute) {
				candidate.Score += scoreLooksLikeECC
			}
			if index, exists := seen[strings.ToLower(absolute)]; exists {
				// 同一个仓库可能在多个 profile 里都注册过：留分数更高的那条，
				// 否则同一个路径会在面板上并列出现两次。
				if candidate.Score > candidates[index].Score {
					candidates[index] = candidate
				}
				continue
			}
			seen[strings.ToLower(absolute)] = len(candidates)
			candidates = append(candidates, candidate)
		}
	}
	sort.SliceStable(candidates, func(left, right int) bool {
		if candidates[left].Score != candidates[right].Score {
			return candidates[left].Score > candidates[right].Score
		}
		return candidates[left].Path < candidates[right].Path
	})
	return candidates
}

// agentHomes 列出本机上可能放着 profile 数据的目录，最可能的排在前面。
//
// 之所以要枚举而不是只认当前 profile：插件进程不一定拿得到 profile 名，
// 而「另一个 profile 里注册过这个源」同样是一条有效线索。
func agentHomes() []string {
	var homes []string
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			absolute = filepath.Clean(path)
		}
		for _, existing := range homes {
			if strings.EqualFold(existing, absolute) {
				return
			}
		}
		homes = append(homes, absolute)
	}
	add(os.Getenv("HIMIND_AGENT_HOME"))
	if dataRoot := strings.TrimSpace(os.Getenv("HIMIND_PLUGIN_DATA_ROOT")); dataRoot != "" {
		// 插件进程拿到的是 <agent_home>/plugin-data，上一层就是 profile 根。
		// 宿主改了布局，这个候选自然落空，不影响别的来源。
		add(filepath.Dir(filepath.Clean(dataRoot)))
	}
	base := filepath.Join(localAppDataBase(), "HiMindAgent")
	switch profile := strings.TrimSpace(os.Getenv("HIMIND_AGENT_PROFILE")); profile {
	case "", "production", "default":
		add(base)
	default:
		add(filepath.Join(base, "profiles", profile))
	}
	if entries, err := os.ReadDir(filepath.Join(base, "profiles")); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				add(filepath.Join(base, "profiles", entry.Name()))
			}
		}
	}
	add(base)
	return homes
}

func localAppDataBase() string {
	if value := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); value != "" {
		return value
	}
	return os.TempDir()
}

func localSources(home string) []extensionSource {
	data, err := os.ReadFile(filepath.Join(home, "data", "extension-sources.json"))
	if err != nil {
		return nil
	}
	var sources extensionSourcesFile
	if err := json.Unmarshal(data, &sources); err != nil {
		return nil
	}
	local := make([]extensionSource, 0, len(sources.Sources))
	for _, source := range sources.Sources {
		if strings.EqualFold(strings.TrimSpace(source.Kind), "local") {
			local = append(local, source)
		}
	}
	return local
}

// installedFromSource 读出本插件是从哪个源装的。
//
// 这是一条强线索但读的是 Agent 的安装记录，所以它只用来排序：读不到、
// 或者宿主换了字段名，最多是少加 400 分，候选照旧由清单与文件事实决定。
func installedFromSource(home string) string {
	path := filepath.Join(home, "plugins", ownPluginID, "current", "policy.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var record struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(record.Source), "local:")
}

// catalogListsOwnExtension 问的是「这份仓库清单里有没有本插件」。
//
// 只做字符串精确匹配、不认具体结构：本地源的清单是 extensions.json，
// GitHub 源的清单是 .himind/catalog.json，同一个 id 在两边的位置并不一样。
// 于是递归找出所有字符串字段，命中即算——这比写两套解析更不容易随清单改版失效。
func catalogListsOwnExtension(repoRoot, catalogPath string) bool {
	name := strings.TrimSpace(catalogPath)
	if name == "" {
		name = "extensions.json"
	}
	data, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(name)))
	if err != nil {
		return false
	}
	var catalog any
	if err := json.Unmarshal(data, &catalog); err != nil {
		return false
	}
	return jsonMentions(catalog, ownPluginID) || jsonMentions(catalog, ownWorkflowID)
}

func jsonMentions(value any, target string) bool {
	switch typed := value.(type) {
	case string:
		return typed == target
	case []any:
		for _, item := range typed {
			if jsonMentions(item, target) {
				return true
			}
		}
	case map[string]any:
		for _, item := range typed {
			if jsonMentions(item, target) {
				return true
			}
		}
	}
	return false
}

func looksLikeECC(source extensionSource, path string) bool {
	haystack := strings.ToLower(source.Name + " " + source.DistributionID + " " + filepath.Base(path))
	return strings.Contains(haystack, "ecc")
}
