package eccsync

import "strings"

// SplitSkillCopy 把市场文案切成「这条技能做什么」和「什么时候该用它」两段。
//
// 市场里一条技能的说明是一整段：「……（做什么）。当……时用。」浏览目录的人先要
// 判断这条跟自己有没有关系，真要用时再看触发条件，两件事挤在一行里只能靠眼睛找。
// 切开放在插件出口，是因为这里本来就掌管元数据；让界面去猜句子的边界，等于把
// 同一套规则抄到第二处。
//
// 认不出场景句就整段当简介：场景宁可空着，也不拿半句话冒充使用场景。
func SplitSkillCopy(description string) (intro, scenario string) {
	trimmed := strings.TrimSpace(description)
	if trimmed == "" {
		return "", ""
	}
	sentences := splitChineseSentences(trimmed)
	// 只有一句话时整段都是简介。单句既可能是简介也可能是场景，靠猜会猜错一半。
	if len(sentences) < 2 {
		return trimmed, ""
	}
	last := sentences[len(sentences)-1]
	if !looksLikeScenario(last) {
		return trimmed, ""
	}
	intro = strings.TrimSpace(strings.Join(sentences[:len(sentences)-1], ""))
	if intro == "" {
		return trimmed, ""
	}
	return intro, strings.TrimSpace(last)
}

// splitChineseSentences 按句号切句，并把句号补回每个片段，拼起来就是原句。
func splitChineseSentences(value string) []string {
	raw := strings.Split(value, "。")
	out := make([]string, 0, len(raw))
	for index, part := range raw {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// 结尾的句号在 Split 之后落成一个空片段，所以只有最后一片之外才补句号。
		if index < len(raw)-1 {
			part += "。"
		}
		out = append(out, part)
	}
	return out
}

// looksLikeScenario 认的是句首「当」或「只在」、句尾落在「用」附近的收尾。
//
// 库里 268 条文案里 253 条是这套句式，另有「当……前用」「……时，先用它」两个变体；
// 两头都卡住，认不出来的就当没有场景句，不往外扩。
func looksLikeScenario(sentence string) bool {
	trimmed := strings.TrimSuffix(strings.TrimSpace(sentence), "。")
	if trimmed == "" {
		return false
	}
	if !strings.HasPrefix(trimmed, "当") && !strings.HasPrefix(trimmed, "只在") {
		return false
	}
	runes := []rune(trimmed)
	tail := runes
	if len(runes) > 3 {
		tail = runes[len(runes)-3:]
	}
	return strings.Contains(string(tail), "用")
}
