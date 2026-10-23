// Package health 提供容器健康检查探针。
//
// 存在理由：运行镜像是 distroless/static，里面没有 curl、wget、nc。
// Docker 的 HEALTHCHECK 无法用外部命令探活，只能用二进制自己的子命令：
//
//	docker healthcheck test: ["CMD", "/app/chatgpt2api", "--healthcheck"]
//
// 没有它的话，容器只能靠 restart 策略兜底 —— 进程还在但已经卡死
// （比如号池全限流、事件循环堵住）的情况不会被发现。
package health

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// Check 对 baseURL 的 /health 发一次 GET，2xx 视为健康。
//
// timeout 控制整个请求（含连接、读响应），与 Docker healthcheck 的
// timeout 配置对应，不能依赖调用方传进来的 ctx 一定有 deadline。
func Check(ctx context.Context, baseURL string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	url := trimTrailingSlash(baseURL) + "/health"
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build healthcheck request: %w", err)
	}

	client := &http.Client{Timeout: timeout}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("healthcheck request: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("healthcheck status %d", response.StatusCode)
	}
	return nil
}

// trimTrailingSlash 去掉末尾斜杠，避免拼出 //health。
func trimTrailingSlash(value string) string {
	for len(value) > 0 && value[len(value)-1] == '/' {
		value = value[:len(value)-1]
	}
	return value
}
