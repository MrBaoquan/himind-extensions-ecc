package eccsync

import (
	"fmt"
	"math/rand"
	"strings"
	"time"
)

// runWithRetry 跑一个可能被网络抖动打断的动作。
//
// action 只需回答「这次失败能不能靠重发解决」，次数与退避由这里统一管。
// 同步链路上的两种失败（打 GitHub 接口、调 gh 发 Release）用的是同一套策略，
// 免得一处重试、另一处一次抖动就整条任务失败。
//
// 次数用尽后把最后一次错误交回上层，并标注重试过几次——否则最终报错看起来
// 像是第一次就没成，排查时会把「偶发抖动」误判成「配置错了」。
func runWithRetry[T any](attempts int, backoff time.Duration, action func() (T, bool, error)) (T, error) {
	var zero T
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			time.Sleep(retryDelay(backoff, attempt))
		}
		value, retryable, err := action()
		if err == nil {
			return value, nil
		}
		lastErr = err
		if !retryable {
			return zero, err
		}
		if attempt == attempts {
			return zero, fmt.Errorf("%w（已重试 %d 次）", lastErr, attempt-1)
		}
	}
	return zero, lastErr
}

// retryMaxBackoff 是单次重试等待的上限。
//
// 退避按 base 翻倍增长，但必须有顶：一个注定失败的调用不该把定时任务拖成
// 十几分钟才报错。有顶之后，5 次尝试的总等待仍在几十秒量级——本机实测到的
// 断流就是几十秒，等得起；真的断网也不是靠多等能解决的。
const retryMaxBackoff = 30 * time.Second

// retryDelay 算出第 attempt 次尝试之前的等待时间。
//
// 指数增长 + 上限 + 正向抖动。抖动只加不减：一次发布里会有多个 gh 调用同时
// 失败，全部在同一毫秒重发等于再压一次服务端；错开之后恢复期的第一个成功
// 请求就能把整条链带起来。只加不减也保证「退避时间不小于约定值」这个直觉成立。
func retryDelay(base time.Duration, attempt int) time.Duration {
	if base <= 0 || attempt <= 1 {
		return 0
	}
	shift := attempt - 2
	if shift > 16 {
		shift = 16
	}
	delay := base << shift
	if delay <= 0 || delay > retryMaxBackoff {
		delay = retryMaxBackoff
	}
	return delay + time.Duration(rand.Int63n(int64(delay/5)+1))
}

// transientMarkers 是网络层与服务端临时故障在报错文本里的常见片段。
//
// 本机出口到 GitHub 是偶发 TLS 握手超时、连接超时，go 与 gh 报的字符串不一样，
// 所以按片段匹配而不是按错误类型——两个工具把底层错误都压成了文本。
var transientMarkers = []string{
	"TLS handshake timeout",
	"connection reset",
	"connection attempt failed",
	"dial tcp",
	"no such host",
	"i/o timeout",
	"context deadline exceeded",
	"unexpected EOF",
	"temporary failure",
	"secondary rate limit",
	"HTTP 502",
	"HTTP 503",
	"HTTP 504",
	"502 Bad Gateway",
	"503 Service Unavailable",
	"504 Gateway Timeout",
}

// isTransient 判断一段报错文本是不是「重发可能就好了」。
//
// 4xx（除限流）与 tag 冲突这类确定性问题不算：重试只会白等，还会把真正的
// 报错拖到几分钟之后才浮出来。
func isTransient(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	lowered := strings.ToLower(text)
	for _, marker := range transientMarkers {
		if strings.Contains(lowered, strings.ToLower(marker)) {
			return true
		}
	}
	// 裸 EOF：响应被截断，重发有机会拿到完整结果。
	return strings.Contains(text, "EOF")
}
