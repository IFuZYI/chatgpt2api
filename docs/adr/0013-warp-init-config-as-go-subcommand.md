# 13. WARP 的 init-config 用 Go 子命令，而不是恢复 Python 脚本

## 背景

`docker-compose.warp.yml` 里有一个一次性容器 `init-config`，职责是把
WARP / FlareSolverr 相关的 `proxy_runtime` 默认值合并进 `config.json`，
让用户不用手写那十几个嵌套字段。

2.0 全量重写为 Go 之后，这个容器一度仍指向 Python 实现：

```yaml
init-config:
  image: python:3.13-alpine
  volumes:
    - ./scripts:/app/scripts:ro
  command: ["python3", "/app/scripts/init_proxy_config.py"]
```

随后 Python 后端被归档到分支 `archive/python-backend-2.0`，`scripts/`
目录从 `main` 删除 —— 这个容器就再也起不来了。症状是 WARP 部署卡在第一步，
而报错信息指向「找不到文件」，与「为什么这里会有个 Python 脚本」毫无关联。

## 决策

把这段逻辑移植成主二进制的子命令：

```bash
/app/chatgpt2api --init-proxy-config
```

`init-config` 容器改用 app 自己的镜像（`build: context: .`）来跑它。

## 为什么不从归档分支恢复 Python 脚本

这是最省事的路，但代价是重新引入一个运行时依赖：

1. **与「单二进制」定位矛盾。** 整个 2.0 的核心卖点是「部署只有一个文件」。
   为了一个只跑一次的配置合并，把 `python:3.13-alpine` 拉进部署链路，
   用户就得为它付镜像体积、拉取时间，以及「Python 版本会不会变」的维护成本。
2. **多一个可能漂移的实现。** 配置的语义定义在 Go 的 `internal/config` 里
   （`DefaultProxyRuntime`、归一化逻辑）。Python 脚本有自己的一份
   `DEFAULT_PROXY_RUNTIME` 字面量 —— 两份定义一旦不同步，就会出现
   「init 写进去的字段主程序不认」这种极难排查的问题。
   移植到 Go 后两边共用同一个常量。
3. **测试面。** Go 版可以直接写表驱动测试（已覆盖 13 个用例 / 37 个子测试）。
   Python 脚本在归档后不再有 CI 覆盖。

## 为什么不干脆让主程序在启动时自己合并

看起来更简单：服务启动时发现 `proxy_runtime` 缺失就补上，不需要额外容器。

否决的理由是**职责错位**。`proxy_runtime` 的默认值依赖部署形态
（WARP 场景下代理是 `http://privoxy:8118`，裸机场景下根本没有这个地址）。
让主程序无条件补默认值，会把「WARP 部署的假设」写进所有部署形态。

更糟的是覆盖顺序问题：用户在 Web 面板改过 `proxy_runtime` 后重启，
主程序若认为「值不完整」而补齐，就会悄悄改掉用户的配置。
一次性容器把这件事限定在「部署时显式执行一次」，语义清楚得多。

## 代价

- 多了一个 `--init-proxy-config` 子命令要维护（约 200 行 + 测试）。
- `config` 包要能被 `cmd` 直接调用来写文件 —— 这本来就有（`writeFileAtomic`），
  没有引入新的依赖方向。
- 用户如果不用 compose、想手动初始化，需要知道这个 flag。已在
  README 的命令行参数表与 `.env.example` 的注释里写明。

## 相关

- 排障与手动用法：`docs/deployment.md` 的「WARP / FlareSolverr 代理」一节
- 环境变量清单：`.env.example`
- 同批修复的另一个部署阻断（`data/` 属主）：
  `docs/deployment.md` 的「容器起来了但写数据报 permission denied」
