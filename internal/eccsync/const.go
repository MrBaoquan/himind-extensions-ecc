package eccsync

// 本仓库自有扩展的稳定 ID。
const (
	// pluginID 是同步工具自身的插件 ID，Workflow 靠它拿到生成能力。
	pluginID = "com.mrbaoquan.ecc-skill-sync"
	// workflowID 是同步闭环工作流的 ID，定时计划指向它。
	workflowID = "com.mrbaoquan.workflow.ecc-skill-sync"
)

// PluginID 与 WorkflowID 供插件与 CLI 共享。
func PluginID() string   { return pluginID }
func WorkflowID() string { return workflowID }

// validCategories 是 HiMind 允许的 11 个功能分类。
//
// 分类不是自由文本：市场左侧的筛选就是按它分组的，写错一个字母
// 那个技能就会从所有分组里消失。
var validCategories = map[string]struct{}{
	"software-engineering":   {},
	"visual-design":          {},
	"video-post":             {},
	"3d-animation":           {},
	"content-production":     {},
	"audio-sound":            {},
	"data-automation":        {},
	"docs-knowledge":         {},
	"testing-quality":        {},
	"collaboration-delivery": {},
	"system-device":          {},
}
