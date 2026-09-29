package music

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"lxsc/internal/admission"
)

// 连续调用同一个键，返回即重试；不能复用已结束的请求，也不能残留执行名额。
func TestRequestGroupCompletionAllowsImmediateRetry(t *testing.T) {
	for _, result := range []string{"成功", "取消", "超时", "异常"} {
		t.Run(result, func(t *testing.T) {
			group := requestGroup{gate: admission.New(1, 0, 0, 0)}
			for attempt := 0; attempt < 1000; attempt++ {
				want := json.RawMessage(fmt.Sprintf(`{"attempt":%d}`, attempt))
				data, err := group.do(context.Background(), "same-key", func() (json.RawMessage, error) {
					switch result {
					case "取消":
						return want, context.Canceled
					case "超时":
						return want, context.DeadlineExceeded
					case "异常":
						panic("测试异常")
					default:
						return want, nil
					}
				})
				switch result {
				case "成功":
					if err != nil {
						t.Fatalf("第 %d 次调用失败: %v", attempt, err)
					}
				case "取消":
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("取消错误不符: %v", err)
					}
				case "超时":
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("超时错误不符: %v", err)
					}
				case "异常":
					if err == nil || err.Error() != "在线目录请求异常: 测试异常" {
						t.Fatalf("异常未被正确转换: %v", err)
					}
				}
				if result != "异常" && string(data) != string(want) {
					t.Fatalf("立即重试复用了旧结果: got=%s want=%s", data, want)
				}
				if stats := group.gate.Stats(); stats.Active != 0 || stats.Queued != 0 {
					t.Fatalf("返回后仍占用请求名额: %+v", stats)
				}
				group.mu.Lock()
				remaining := len(group.m)
				group.mu.Unlock()
				if remaining != 0 {
					t.Fatalf("返回后仍有 %d 个已结束请求", remaining)
				}
			}
		})
	}
}
