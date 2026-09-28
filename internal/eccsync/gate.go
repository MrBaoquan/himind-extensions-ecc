package eccsync

import (
	"bytes"
	"os/exec"
	"strings"
	"time"
)

// GateStep 是一条门禁的执行结果。
type GateStep struct {
	Name     string `json:"name"`
	Command  string `json:"command"`
	OK       bool   `json:"ok"`
	ExitCode int    `json:"exit_code"`
	Seconds  int    `json:"seconds"`
	// OutputTail 只留尾部：门禁失败时人要看的是最后几行报错。
	OutputTail string `json:"output_tail,omitempty"`
}

// GateResult 是一次门禁的结论。
type GateResult struct {
	OK    bool       `json:"ok"`
	Steps []GateStep `json:"steps"`
}

// Gate 跑完整门禁：本仓库能编译、单测通过、扩展仓库契约成立。
//
// 第三条复用 himind-extensions 里的 himind-repo-check，而不是在本仓库重写一套
// 近似规则：契约只应该有一个实现，否则「本地过了、装不上」会反复出现。
func Gate(repoRoot string) (GateResult, error) {
	steps := []struct {
		name string
		args []string
	}{
		{name: "构建", args: []string{"build", "./..."}},
		{name: "单元测试", args: []string{"test", "./..."}},
		{name: "扩展仓库契约", args: []string{"run", "github.com/MrBaoquan/himind-extensions/tools/cmd/himind-repo-check"}},
	}
	result := GateResult{OK: true}
	for _, step := range steps {
		started := time.Now()
		command := exec.Command("go", step.args...)
		command.Dir = repoRoot
		configureHiddenCommand(command)
		command.Env = append(command.Environ(), "GOFLAGS=-mod=mod")
		var output bytes.Buffer
		command.Stdout = &output
		command.Stderr = &output
		err := command.Run()
		entry := GateStep{
			Name:       step.name,
			Command:    "go " + strings.Join(step.args, " "),
			OK:         err == nil,
			Seconds:    int(time.Since(started).Seconds()),
			OutputTail: tail(output.String(), 4000),
		}
		if exit, ok := err.(*exec.ExitError); ok {
			entry.ExitCode = exit.ExitCode()
		} else if err != nil {
			entry.ExitCode = -1
			entry.OutputTail = tail(err.Error()+"\n"+output.String(), 4000)
		}
		if !entry.OK {
			result.OK = false
		}
		result.Steps = append(result.Steps, entry)
	}
	return result, nil
}

func tail(value string, limit int) string {
	trimmed := strings.TrimRight(value, "\n")
	if len(trimmed) <= limit {
		return trimmed
	}
	return "…\n" + trimmed[len(trimmed)-limit:]
}
