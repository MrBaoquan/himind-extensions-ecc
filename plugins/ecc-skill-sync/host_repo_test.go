package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHost 造一台「本机」：一个 agent home，里面有哪些本地市场源、本插件从哪装的。
//
// 三个环境变量一起收窄是必须的：漏掉任何一个，推断都会顺手读这台真机器上的
// profile，测试就变成「碰巧在这台机器上通过」。
type fakeHost struct {
	home string
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	root := t.TempDir()
	t.Setenv("LOCALAPPDATA", root)
	t.Setenv("HIMIND_AGENT_PROFILE", "")
	t.Setenv("HIMIND_PLUGIN_DATA_ROOT", "")
	t.Setenv(repoRootEnv, "")
	home := filepath.Join(root, "HiMindAgent")
	t.Setenv("HIMIND_AGENT_HOME", home)
	return &fakeHost{home: home}
}

// addLocalSource 注册一个本地市场源，与 Agent 写进 extension-sources.json 的形状一致。
func (host *fakeHost) addLocalSource(t *testing.T, id, name, repository, catalogPath string) {
	t.Helper()
	path := filepath.Join(host.home, "data", "extension-sources.json")
	file := extensionSourcesFile{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &file); err != nil {
			t.Fatalf("既有源清单不是合法 JSON：%v", err)
		}
	}
	file.Sources = append(file.Sources, extensionSource{
		ID:             id,
		Name:           name,
		Kind:           "local",
		Repository:     repository,
		CatalogPath:    catalogPath,
		Enabled:        true,
		DistributionID: "mrbaoquan/" + filepath.Base(repository),
	})
	// 真实文件里还有 github 源，混一条进来确保推断只认 local。
	file.Sources = append(file.Sources, extensionSource{
		ID:         "github-not-local",
		Name:       "远程源",
		Kind:       "github",
		Repository: "MrBaoquan/himind-extensions",
	})
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		t.Fatalf("源清单序列化失败：%v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建 data 目录失败：%v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("写源清单失败：%v", err)
	}
}

// installPluginFrom 写下「本插件是从这个源装的」这条安装记录。
func (host *fakeHost) installPluginFrom(t *testing.T, sourceID string) {
	t.Helper()
	dir := filepath.Join(host.home, "plugins", ownPluginID, "current")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建插件目录失败：%v", err)
	}
	record := map[string]any{"source": "local:" + sourceID, "management": "user_managed"}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("安装记录序列化失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "policy.json"), data, 0o644); err != nil {
		t.Fatalf("写安装记录失败：%v", err)
	}
}

// makeRepo 造一个仓库目录：可选的同步策略文件 + 一份仓库清单。
func makeRepo(t *testing.T, path string, withPolicy bool, catalogIDs ...string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("建仓库目录失败：%v", err)
	}
	if withPolicy {
		if err := os.WriteFile(filepath.Join(path, "upstream-policy.json"), []byte("{}"), 0o644); err != nil {
			t.Fatalf("写策略文件失败：%v", err)
		}
	}
	extensions := make([]map[string]any, 0, len(catalogIDs))
	for _, id := range catalogIDs {
		extensions = append(extensions, map[string]any{"type": "plugin", "id": id, "path": "plugins/" + id})
	}
	catalog := map[string]any{"schema_version": 1, "extensions": extensions}
	data, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		t.Fatalf("清单序列化失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "extensions.json"), data, 0o644); err != nil {
		t.Fatalf("写清单失败：%v", err)
	}
	return path
}

// 面板第一次打开时问的那一句话，答案必须是「本插件是从哪个源装的」，而不是
// 宿主给的当前工作区——那两件事在同一台机器上通常指向不同的目录。
func TestRepoHintPrefersTheSourceThisPluginCameFrom(t *testing.T) {
	host := newFakeHost(t)
	eccRepo := makeRepo(t, filepath.Join(t.TempDir(), "himind-extensions-ecc"), true, ownPluginID)
	otherRepo := makeRepo(t, filepath.Join(t.TempDir(), "himind-ext-projects"), false, "com.other.plugin")
	host.addLocalSource(t, "local-ecc", "HiMind ECC 扩展", eccRepo, "extensions.json")
	host.addLocalSource(t, "local-other", "HiMind 扩展项目", otherRepo, "extensions.json")
	host.installPluginFrom(t, "local-ecc")

	candidates := inferRepoCandidates()
	if len(candidates) != 2 {
		t.Fatalf("两个本地源应各提名一次（github 源不该参与）：%+v", candidates)
	}
	top := candidates[0]
	if top.Path != eccRepo {
		t.Fatalf("装进来的那个源应排在最前：%+v", candidates)
	}
	if !top.HasPolicy || !top.CatalogHit || !top.InstallHit {
		t.Fatalf("三条依据都应命中：%+v", top)
	}
	if candidates[1].HasPolicy {
		t.Fatalf("没有 upstream-policy.json 的目录不该被当成可用候选：%+v", candidates[1])
	}
	if usable := usableCandidates(candidates); len(usable) != 1 || usable[0].Path != eccRepo {
		t.Fatalf("可用候选只应有 ECC 仓库：%+v", usable)
	}

	// 无参调用同样落在这个仓库上：定时任务里没人填路径时靠的就是它。
	resolved, rpcError := resolveRepo(input{})
	if rpcError != nil {
		t.Fatalf("唯一候选时应能推断出仓库：%+v", rpcError)
	}
	if resolved != eccRepo {
		t.Fatalf("推断结果不对：%s", resolved)
	}
}

// 安装记录读不到时仍要能推断：仓库清单里写着本插件，这条事实比「名字像 ecc」硬。
func TestRepoHintSurvivesAMissingInstallRecord(t *testing.T) {
	host := newFakeHost(t)
	namedLikeECC := makeRepo(t, filepath.Join(t.TempDir(), "ecc-old"), true)
	publisher := makeRepo(t, filepath.Join(t.TempDir(), "publisher"), true, ownWorkflowID)
	host.addLocalSource(t, "local-named", "ecc-old", namedLikeECC, "extensions.json")
	host.addLocalSource(t, "local-publisher", "发布源", publisher, "extensions.json")

	candidates := inferRepoCandidates()
	if candidates[0].Path != publisher {
		t.Fatalf("清单命中应压过名字里的 ecc：%+v", candidates)
	}
	if candidates[0].InstallHit {
		t.Fatalf("没有安装记录时不该报命中：%+v", candidates[0])
	}
}

// 两个目录都像同步仓库时不许挑一个：猜错的代价是把这一批发到另一个仓库去。
func TestResolveRepoRefusesToGuessBetweenTwoRepos(t *testing.T) {
	host := newFakeHost(t)
	first := makeRepo(t, filepath.Join(t.TempDir(), "ecc-a"), true, ownPluginID)
	second := makeRepo(t, filepath.Join(t.TempDir(), "ecc-b"), true, ownPluginID)
	host.addLocalSource(t, "local-a", "ECC A", first, "extensions.json")
	host.addLocalSource(t, "local-b", "ECC B", second, "extensions.json")

	_, rpcError := resolveRepo(input{})
	if rpcError == nil {
		t.Fatal("两个候选时应当报错，而不是挑一个")
	}
	message := rpcError.Message
	if !contains(message, first) || !contains(message, second) {
		t.Fatalf("报错应列出两个候选：%s", message)
	}

	hint := repoHintPayload(t, input{})
	auto, ok := hint["auto"].(map[string]any)
	if !ok || auto["ambiguous"] != true || auto["count"] != 2 {
		t.Fatalf("面板应看到「有多个候选」而不是一个答案：%+v", hint["auto"])
	}
}

// 一个候选都没有时，要的是「请填路径」，不是把当前工作区当成仓库。
func TestRepoHintWithoutAnySourceAsksForAPath(t *testing.T) {
	newFakeHost(t)

	if _, rpcError := resolveRepo(input{}); rpcError == nil {
		t.Fatal("没有线索时不该凭空给一个路径")
	}
	hint := repoHintPayload(t, input{})
	if _, present := hint["auto"]; present {
		t.Fatalf("没有候选时不该给自动结果：%+v", hint)
	}
	if _, present := hint["current"]; present {
		t.Fatalf("没传路径时不该有 current 段：%+v", hint)
	}
}

// 面板手里那条路径到底还能不能用，要在同一次往返里问清楚：分开问会出现
// 「先渲染错路径、再自我纠正」的空窗。
func TestRepoHintValidatesThePathItWasHanded(t *testing.T) {
	host := newFakeHost(t)
	repo := makeRepo(t, filepath.Join(t.TempDir(), "himind-extensions-ecc"), true, ownPluginID)
	host.addLocalSource(t, "local-ecc", "HiMind ECC 扩展", repo, "extensions.json")
	elsewhere := t.TempDir()

	stale := repoHintPayload(t, input{RepoRoot: elsewhere})
	current, ok := stale["current"].(map[string]any)
	if !ok || current["valid"] != false {
		t.Fatalf("别的目录应被判成不可用：%+v", stale["current"])
	}
	if current["path"] != elsewhere {
		t.Fatalf("回给面板的应是被问的那条路径：%+v", current)
	}
	if auto, ok := stale["auto"].(map[string]any); !ok || auto["path"] != repo {
		t.Fatalf("不可用时仍应给出正确的候选：%+v", stale["auto"])
	}

	fresh := repoHintPayload(t, input{RepoRoot: repo})
	current, ok = fresh["current"].(map[string]any)
	if !ok || current["valid"] != true {
		t.Fatalf("仓库根目录应被判成可用：%+v", fresh["current"])
	}
}

// 同一个仓库在多个 profile 里注册过，面板上不该并列出现两次。
func TestRepoHintDeduplicatesAcrossProfiles(t *testing.T) {
	host := newFakeHost(t)
	repo := makeRepo(t, filepath.Join(t.TempDir(), "himind-extensions-ecc"), true, ownPluginID)
	host.addLocalSource(t, "local-ecc", "HiMind ECC 扩展", repo, "extensions.json")

	// 另一个 profile 注册了同一个仓库：agentHomes 会把两个 profile 都扫一遍。
	other := filepath.Join(host.home, "profiles", "independent")
	if err := os.MkdirAll(filepath.Join(other, "data"), 0o755); err != nil {
		t.Fatalf("建第二个 profile 失败：%v", err)
	}
	data, err := os.ReadFile(filepath.Join(host.home, "data", "extension-sources.json"))
	if err != nil {
		t.Fatalf("读源清单失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(other, "data", "extension-sources.json"), data, 0o644); err != nil {
		t.Fatalf("写第二个 profile 的源清单失败：%v", err)
	}

	candidates := inferRepoCandidates()
	if len(candidates) != 1 || candidates[0].Path != repo {
		t.Fatalf("同一个仓库只该出现一次：%+v", candidates)
	}
}

func repoHintPayload(t *testing.T, in input) map[string]any {
	t.Helper()
	response, rpcError := repoHint(in)
	if rpcError != nil {
		t.Fatalf("repo_hint 不该报错：%+v", rpcError)
	}
	envelope, ok := response.(map[string]any)
	if !ok {
		t.Fatalf("应答形状不对：%+v", response)
	}
	hint, ok := envelope["repo_hint"].(map[string]any)
	if !ok {
		t.Fatalf("应答里没有 repo_hint：%+v", envelope)
	}
	return hint
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
