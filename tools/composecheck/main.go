// composecheck 是 Docker 部署配置的契约测试。
//
// 每个用例对应一个**真实的部署缺陷**，不是「最佳实践清单」：
//   - config.json 权限：以 root 644 单文件挂进 uid 65532 的容器 → 设置页保存
//     "permission denied"
//   - 日志无限增长写满磁盘
//   - warp compose 挂了一个已被删除的宿主文件 → docker 创建同名目录，
//     privoxy 永远起不来，app 的 depends_on 永不满足（当时 43 项检查全过，
//     故障仍在 —— 所以才补了「宿主路径必须存在」这一组）
//   - 把 app 的 cap_drop:[ALL] 复制给需要 NET_ADMIN 的 warp 容器 → 起不来
//
// 用 `go run ./tools/composecheck` 跑，无外部依赖。
//
// 为什么用 Go 而不是原来的 Python 脚本：本仓库后端是 Go，而这个脚本是
// main 上**最后一个** Python 文件，还依赖第三方 PyYAML —— 贡献者为了跑一个
// 开发检查要装 Python 环境。用 Go 写可以直接 `go run`，且 go-yaml 已在
// 依赖树里（无新增模块）。
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/goccy/go-yaml"
)

var composeFiles = []string{
	"docker-compose.yml",
	"docker-compose.local.yml",
	"docker-compose.warp.yml",
}

var (
	checks   int
	failures []string
)

func check(ok bool, msg string) {
	checks++
	if !ok {
		failures = append(failures, msg)
	}
}

// load 读并解析一个 compose 文件，返回 services 段。
func load(path string) map[string]any {
	payload, err := os.ReadFile(path)
	if err != nil {
		failures = append(failures, fmt.Sprintf("%s: 读取失败: %v", path, err))
		checks++
		return nil
	}
	var doc map[string]any
	if err := yaml.Unmarshal(payload, &doc); err != nil {
		failures = append(failures, fmt.Sprintf("%s: YAML 解析失败: %v", path, err))
		checks++
		return nil
	}
	services, _ := doc["services"].(map[string]any)
	return services
}

type mount struct{ src, tgt, mode string }

// mountsOf 把 volumes 归一成 [(source, target, mode)]，兼容短语法与长语法。
func mountsOf(service map[string]any) []mount {
	if service == nil {
		return nil
	}
	raw, _ := service["volumes"].([]any)
	out := make([]mount, 0, len(raw))
	for _, entry := range raw {
		switch typed := entry.(type) {
		case string:
			parts := strings.Split(typed, ":")
			m := mount{src: parts[0]}
			if len(parts) > 1 {
				m.tgt = parts[1]
			}
			if len(parts) > 2 {
				m.mode = parts[2]
			}
			out = append(out, m)
		case map[string]any:
			out = append(out, mount{
				src: stringValue(typed["source"]),
				tgt: stringValue(typed["target"]),
			})
		}
	}
	return out
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	return fmt.Sprint(value)
}

func stringSlice(value any) []string {
	raw, _ := value.([]any)
	out := make([]string, 0, len(raw))
	for _, entry := range raw {
		out = append(out, stringValue(entry))
	}
	return out
}

// groupInitData：挂了 config.json 的 compose 必须有 init-data 处理它的权限。
//
// 真实缺陷：config.json 以 root:root 644 单文件挂进容器，容器跑 uid 65532，
// 设置页保存 → "open /app/config.json: permission denied"。
// 三个 compose 都挂 config.json，但只有主 compose 的 init-data 处理了它。
func groupInitData() {
	for _, path := range composeFiles {
		services := load(path)
		payload, _ := os.ReadFile(path)
		if !strings.Contains(string(payload), "config.json") {
			continue
		}
		init, ok := services["init-data"].(map[string]any)
		check(ok, fmt.Sprintf("%s: 挂载了 config.json 却没有 init-data 服务", path))
		if !ok {
			continue
		}
		text := strings.Join(stringSlice(init["command"]), " ")
		if len(stringSlice(init["command"])) == 0 {
			text = stringValue(init["command"])
		}
		check(strings.Contains(text, "config.json"),
			fmt.Sprintf("%s: init-data 的 command 没有处理 config.json（设置页保存会 permission denied）", path))
		check(strings.Contains(text, "65532"),
			fmt.Sprintf("%s: init-data 没有把 config.json chown 给 65532", path))

		// init-data 必须真的挂到 config.json，否则 chown 的是别的东西
		var targets []string
		for _, m := range mountsOf(init) {
			targets = append(targets, m.tgt)
		}
		found := false
		for _, t := range targets {
			if strings.Contains(t, "config.json") {
				found = true
			}
		}
		check(found, fmt.Sprintf("%s: init-data 没有挂载 config.json（chown 无法生效），当前挂载 %v", path, targets))
	}
}

// groupAppHardening：app 必须以非 root 运行且禁止提权。
func groupAppHardening() {
	for _, path := range composeFiles {
		services := load(path)
		app, ok := services["app"].(map[string]any)
		check(ok, fmt.Sprintf("%s: 没有 app 服务", path))
		if !ok {
			continue
		}
		user := stringValue(app["user"])
		check(strings.HasPrefix(user, "65532"),
			fmt.Sprintf("%s: app 未显式以 65532 运行（当前 user=%q），镜像默认已是 nonroot，但 compose 显式声明才能防回归", path, user))

		opts := stringSlice(app["security_opt"])
		hasNoNewPrivs := false
		for _, o := range opts {
			if strings.Contains(o, "no-new-privileges") {
				hasNoNewPrivs = true
			}
		}
		check(hasNoNewPrivs, fmt.Sprintf("%s: app 缺 security_opt: no-new-privileges", path))

		hasAll := false
		for _, c := range stringSlice(app["cap_drop"]) {
			if strings.EqualFold(c, "ALL") {
				hasAll = true
			}
		}
		check(hasAll, fmt.Sprintf("%s: app 缺 cap_drop: [ALL]（进程不需要任何 capability）", path))
	}
}

// groupLogging：容器日志必须限大小，否则单机磁盘会被写满。
func groupLogging() {
	for _, path := range composeFiles {
		for name, svc := range load(path) {
			if name == "init-data" || name == "init-config" {
				continue
			}
			service, _ := svc.(map[string]any)
			logging, _ := service["logging"].(map[string]any)
			options, _ := logging["options"].(map[string]any)
			_, has := options["max-size"]
			check(has, fmt.Sprintf("%s: 服务 %s 没有限制日志大小（长时间运行会写满磁盘）", path, name))
		}
	}
}

// groupDataVolume：data 卷必须可写（这是唯一需要持久化的目录）。
func groupDataVolume() {
	for _, path := range composeFiles {
		app, ok := load(path)["app"].(map[string]any)
		if !ok {
			continue
		}
		var dataMounts []mount
		for _, m := range mountsOf(app) {
			if strings.HasSuffix(strings.TrimRight(m.tgt, "/"), "/app/data") {
				dataMounts = append(dataMounts, m)
			}
		}
		check(len(dataMounts) > 0, fmt.Sprintf("%s: app 没有挂载 /app/data", path))
		for _, m := range dataMounts {
			readOnly := false
			for _, part := range strings.Split(m.mode, ",") {
				if part == "ro" {
					readOnly = true
				}
			}
			check(!readOnly, fmt.Sprintf("%s: /app/data 被挂成只读（%s:%s:%s），无法写入", path, m.src, m.tgt, m.mode))
		}
	}
}

// groupHostPathsExist：所有 bind mount 的宿主源必须存在。
//
// 真实缺陷：warp compose 挂了 ./scripts/privoxy-warp.conf，而那个文件
// 随 Python 归档被删了。docker 会在该路径创建一个**目录**然后报
// "not a directory"，privoxy 永远起不来，app 的 depends_on 永不满足。
// 这条检查当时不存在，所以 43 项全过而故障仍在。
func groupHostPathsExist() {
	for _, path := range composeFiles {
		for name, svc := range load(path) {
			service, _ := svc.(map[string]any)
			for _, m := range mountsOf(service) {
				// 跳过命名卷（不以 . 或 / 开头）与容器内绝对路径
				if !strings.HasPrefix(m.src, ".") {
					continue
				}
				_, err := os.Stat(m.src)
				check(err == nil, fmt.Sprintf("%s: 服务 %s 挂载的宿主路径 %s 不存在（docker 会创建一个同名目录然后挂载失败）", path, name, m.src))
			}
		}
	}
}

// groupCapabilityConsistency：第三方服务的加固必须与实际需求一致。
//
// 真实缺陷：把 app 的 user/cap_drop/security_opt 复制到了 warp-proxy
// （caomingjun/warp），而它需要 root + NET_ADMIN —— 容器起不来。
// 判据：cap_drop: [ALL] 与 cap_add 同时存在 = 自相矛盾。
func groupCapabilityConsistency() {
	for _, path := range composeFiles {
		for name, svc := range load(path) {
			service, _ := svc.(map[string]any)
			dropped := stringSlice(service["cap_drop"])
			added := stringSlice(service["cap_add"])
			hasAll := false
			for _, c := range dropped {
				if strings.EqualFold(c, "ALL") {
					hasAll = true
				}
			}
			check(!(hasAll && len(added) > 0),
				fmt.Sprintf("%s: 服务 %s 同时 cap_drop:[ALL] 与 cap_add:%v —— 自相矛盾，容器会起不来", path, name, added))
		}
	}
}

// groupDockerignore：.dockerignore 必须挡住秘密与无关大文件。
func groupDockerignore() {
	payload, err := os.ReadFile(".dockerignore")
	if err != nil {
		check(false, fmt.Sprintf(".dockerignore 读取失败: %v", err))
		return
	}
	patterns := map[string]bool{}
	for _, line := range strings.Split(string(payload), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			patterns[trimmed] = true
		}
	}
	for _, need := range []struct{ pattern, why string }{
		{"data", "运行时数据目录（含 accounts.json 凭据）"},
		{"config.json", "运行配置（含代理密码）"},
		{".learnings", "本地 agent 日志"},
		{"*_test.go", "测试文件（1 MB，且含测试夹具常量）"},
	} {
		check(patterns[need.pattern], fmt.Sprintf(".dockerignore 缺 %s —— %s 会被打进构建上下文", need.pattern, need.why))
	}
}

// groupDockerfile：Dockerfile 最终层只能带二进制与版本号。
func groupDockerfile() {
	payload, err := os.ReadFile("Dockerfile")
	if err != nil {
		check(false, fmt.Sprintf("Dockerfile 读取失败: %v", err))
		return
	}
	content := string(payload)
	idx := strings.Index(content, "AS runtime")
	if idx < 0 {
		check(false, "Dockerfile 没有名为 runtime 的最终层")
		return
	}
	runtimePart := content[idx:]
	for _, line := range strings.Split(runtimePart, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "COPY") {
			continue
		}
		check(strings.Contains(trimmed, "/out/chatgpt2api") || strings.Contains(trimmed, "VERSION"),
			fmt.Sprintf("Dockerfile 最终层 COPY 了非预期内容: %s", trimmed))
	}
	check(strings.Contains(runtimePart, "USER nonroot"), "Dockerfile 最终层没有切到 nonroot")
}

func main() {
	groupInitData()
	groupAppHardening()
	groupLogging()
	groupDataVolume()
	groupHostPathsExist()
	groupCapabilityConsistency()
	groupDockerignore()
	groupDockerfile()

	fmt.Printf("\n%d 项检查，%d 项失败\n", checks, len(failures))
	for _, f := range failures {
		fmt.Printf("  ✗ %s\n", f)
	}
	if len(failures) > 0 {
		os.Exit(1)
	}
}
