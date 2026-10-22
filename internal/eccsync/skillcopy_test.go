package eccsync

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSplitSkillCopyCutsScenarioTail 用库里真实出现过的几种句式钉住切分口径：
// 切错一边，浏览目录的人就会把「什么时候用」当成「这是干什么的」。
func TestSplitSkillCopyCutsScenarioTail(t *testing.T) {
	cases := []struct {
		name     string
		desc     string
		intro    string
		scenario string
	}{
		{
			name:     "常见句式",
			desc:     "面向 agent 与 LLM 应用的全栈诊断。当自研 agent 表现不对劲时用。",
			intro:    "面向 agent 与 LLM 应用的全栈诊断。",
			scenario: "当自研 agent 表现不对劲时用。",
		},
		{
			name:     "简介本身多句",
			desc:     "第一句介绍。第二句补充。当要发布或提 PR 前用。",
			intro:    "第一句介绍。第二句补充。",
			scenario: "当要发布或提 PR 前用。",
		},
		{
			name:     "只在句式",
			desc:     "只在编写或修正 C++ 测试时用",
			intro:    "只在编写或修正 C++ 测试时用",
			scenario: "",
		},
		{
			name:     "先用它收尾",
			desc:     "组合与回撤数据。当流程要接触账户授权、组合数据或 API 密钥时，先用它",
			intro:    "组合与回撤数据。",
			scenario: "当流程要接触账户授权、组合数据或 API 密钥时，先用它",
		},
		{
			name:     "没有场景句",
			desc:     "给混在外部方群聊里的 agent 定发言规矩：逐渠道提示词、只在被点名时应答。",
			intro:    "给混在外部方群聊里的 agent 定发言规矩：逐渠道提示词、只在被点名时应答。",
			scenario: "",
		},
		{
			name: "句首是当但不是场景",
			// 末句以「当」开头却不是「用」收尾，宁可不切。
			desc:     "处理生产排程。当上游交期变动时排产要重算，这里给出重算顺序。",
			intro:    "处理生产排程。当上游交期变动时排产要重算，这里给出重算顺序。",
			scenario: "",
		},
		{
			name:     "空文案",
			desc:     "   ",
			intro:    "",
			scenario: "",
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			intro, scenario := SplitSkillCopy(item.desc)
			if intro != item.intro {
				t.Fatalf("简介应当是 %q，得到 %q", item.intro, intro)
			}
			if scenario != item.scenario {
				t.Fatalf("场景应当是 %q，得到 %q", item.scenario, scenario)
			}
			// 切出来的两段拼回去应当还是原文，免得切分顺手改了文案。
			if intro != "" && scenario != "" && intro+scenario != item.desc {
				t.Fatalf("切分改动了原文：%q + %q", intro, scenario)
			}
		})
	}
}

// TestSplitSkillCopyCoversShippedMetadata 拿仓库里真实的那份元数据过一遍：
// 句式一变，切分不能悄悄退化成「整段当简介」而没人发现。
func TestSplitSkillCopyCoversShippedMetadata(t *testing.T) {
	if testing.Short() {
		t.Skip("要读仓库里的 manifests/metadata.json")
	}
	metadataPath := filepath.Join("..", "..", filepath.FromSlash(MetadataFile))
	if _, err := os.Stat(metadataPath); err != nil {
		t.Skipf("本地没有 %s，跳过", metadataPath)
	}
	metadata, err := LoadMetadata(metadataPath)
	if err != nil {
		t.Fatalf("读元数据失败：%v", err)
	}
	if len(metadata.Skills) == 0 {
		t.Fatal("元数据里没有技能，这条断言等于没跑")
	}
	cut := 0
	for slug, entry := range metadata.Skills {
		intro, scenario := SplitSkillCopy(entry.Description)
		if intro == "" {
			t.Fatalf("%s 切出来没有简介", slug)
		}
		if scenario != "" {
			cut++
			if !looksLikeScenario(scenario) {
				t.Fatalf("%s 切出来的场景不像场景句：%q", slug, scenario)
			}
		}
	}
	// 上游卡片文案换句式时会掉到这里：不是失败，而是提醒切分口径要跟着走。
	if cut*10 < len(metadata.Skills)*9 {
		t.Fatalf("只有 %d/%d 条切出了场景句，切分口径可能已经跟不上上游文案", cut, len(metadata.Skills))
	}
}
