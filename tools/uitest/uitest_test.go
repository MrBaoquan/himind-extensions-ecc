package uitest

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 面板发出去的能力载荷必须过得了宿主那道契约校验。
//
// 宿主在拉起插件进程之前就按 plugin.json 的 input_schema 拦人，不合法直接拒绝，
// 插件代码连这次调用都收不到——界面上只留下「读取分发状态失败」，而这条路径
// 只有在人点开面板时才走到。所以把它拉进 unit test：ecc-skill-sync-ui.test.mjs
// 用假 DOM 把界面真跑起来，把实际载荷按同一套规则验一遍。
//
// 没装 node 的机器跳过，不让纯 Go 的构建与门禁因此挂掉。
func TestEccSkillSyncUiPayloadsMatchManifest(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("本机没有 node，跳过面板载荷契约测试")
	}
	script, err := filepath.Abs("ecc-skill-sync-ui.test.mjs")
	if err != nil {
		t.Fatalf("定位测试脚本失败：%v", err)
	}
	command := exec.Command(node, script)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("界面载荷不符合 Manifest 契约：\n%s", strings.TrimSpace(string(output)))
	}
}
