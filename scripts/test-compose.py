#!/usr/bin/env python3
"""Docker 部署配置的契约测试。

每个用例对应一个真实的部署缺陷 —— 不是「最佳实践清单」。
用 `python3 scripts/test-compose.py` 跑，无依赖（不装 pytest）。
"""

import sys
import yaml

COMPOSE_FILES = [
    "docker-compose.yml",
    "docker-compose.local.yml",
    "docker-compose.warp.yml",
]

failures = []
checks = 0


def check(ok, msg):
    global checks
    checks += 1
    if not ok:
        failures.append(msg)


def load(path):
    with open(path, encoding="utf-8") as fh:
        return yaml.safe_load(fh)


def mounts_of(service):
    """把 volumes 归一成 [(source, target, mode)] —— 兼容短语法与长语法。"""
    out = []
    for vol in service.get("volumes") or []:
        if isinstance(vol, str):
            parts = vol.split(":")
            src = parts[0]
            tgt = parts[1] if len(parts) > 1 else ""
            mode = parts[2] if len(parts) > 2 else ""
            out.append((src, tgt, mode))
        elif isinstance(vol, dict):
            out.append((vol.get("source", ""), vol.get("target", ""), ""))
    return out


# ── 1. 每个 compose 的 init-data 都必须 chown config.json ────────────
#
# 真实缺陷：config.json 以 root:root 644 单文件挂进容器，容器跑 uid 65532，
# 设置页保存 → "open /app/config.json: permission denied"。
# 三个 compose 都挂 config.json，但只有主 compose 的 init-data 处理了它。
for path in COMPOSE_FILES:
    cfg = load(path)
    services = cfg.get("services") or {}

    if "config.json" not in open(path, encoding="utf-8").read():
        continue

    init = services.get("init-data")
    check(init is not None, f"{path}: 挂载了 config.json 却没有 init-data 服务")
    if init is None:
        continue

    command = init.get("command") or []
    text = " ".join(command) if isinstance(command, list) else str(command)
    check(
        "config.json" in text,
        f"{path}: init-data 的 command 没有处理 config.json（设置页保存会 permission denied）",
    )
    check(
        "65532" in text,
        f"{path}: init-data 没有把 config.json chown 给 65532",
    )

    # init-data 必须真的挂到 config.json，否则 chown 的是别的东西
    targets = [t for _, t, _ in mounts_of(init)]
    check(
        any("config.json" in t for t in targets),
        f"{path}: init-data 没有挂载 config.json（chown 无法生效），当前挂载 {targets}",
    )

# ── 2. app 服务必须以非 root 运行，且禁止提权 ────────────────────────
for path in COMPOSE_FILES:
    cfg = load(path)
    app = (cfg.get("services") or {}).get("app")
    check(app is not None, f"{path}: 没有 app 服务")
    if app is None:
        continue

    user = str(app.get("user", ""))
    check(
        user.startswith("65532"),
        f"{path}: app 未显式以 65532 运行（当前 user={user!r}），"
        "镜像默认已是 nonroot，但 compose 显式声明才能防回归",
    )

    opts = app.get("security_opt") or []
    check(
        any("no-new-privileges" in str(o) for o in opts),
        f"{path}: app 缺 security_opt: no-new-privileges",
    )

    caps = app.get("cap_drop") or []
    check(
        any(str(c).upper() == "ALL" for c in caps),
        f"{path}: app 缺 cap_drop: [ALL]（进程不需要任何 capability）",
    )

# ── 3. 容器日志必须限大小，否则单机磁盘会被写满 ──────────────────────
for path in COMPOSE_FILES:
    cfg = load(path)
    for name, svc in (cfg.get("services") or {}).items():
        if name in ("init-data", "init-config"):
            continue
        logging = svc.get("logging") or {}
        options = logging.get("options") or {}
        check(
            "max-size" in options,
            f"{path}: 服务 {name} 没有限制日志大小（长时间运行会写满磁盘）",
        )

# ── 4. data 卷必须可写（这是唯一需要持久化的目录）────────────────────
for path in COMPOSE_FILES:
    cfg = load(path)
    app = (cfg.get("services") or {}).get("app")
    if app is None:
        continue
    mounts = mounts_of(app)
    data_mounts = [(s, t, m) for s, t, m in mounts if t.rstrip("/").endswith("/app/data")]
    check(bool(data_mounts), f"{path}: app 没有挂载 /app/data")
    for src, tgt, mode in data_mounts:
        check(
            "ro" not in mode.split(","),
            f"{path}: /app/data 被挂成只读（{src}:{tgt}:{mode}），无法写入",
        )

# ── 5. .dockerignore 必须挡住秘密与无关大文件 ────────────────────────
ignore = open(".dockerignore", encoding="utf-8").read().splitlines()
patterns = {line.strip() for line in ignore if line.strip() and not line.startswith("#")}

for needed, why in [
    ("data", "运行时数据目录（含 accounts.json 凭据）"),
    ("config.json", "运行配置（含代理密码）"),
    (".learnings", "本地 agent 日志"),
    ("*_test.go", "测试文件（1 MB，且含测试夹具常量）"),
]:
    check(needed in patterns, f".dockerignore 缺 {needed} —— {why} 会被打进构建上下文")

# ── 6. Dockerfile 最终层只能带二进制与版本号 ─────────────────────────
dockerfile = open("Dockerfile", encoding="utf-8").read()
runtime_part = dockerfile.split("AS runtime", 1)[-1]
copies = [line for line in runtime_part.splitlines() if line.strip().startswith("COPY")]
for line in copies:
    check(
        "/out/chatgpt2api" in line or "VERSION" in line,
        f"Dockerfile 最终层 COPY 了非预期内容: {line.strip()}",
    )
check(
    "USER nonroot" in runtime_part,
    "Dockerfile 最终层没有切到 nonroot",
)

print(f"\n{checks} 项检查，{len(failures)} 项失败")
for f in failures:
    print(f"  ✗ {f}")
sys.exit(1 if failures else 0)
