# repro_outputs — 历史运行快照

这里的文件是**某一时刻的真实运行输出**，不是文档，不要当成当前状态来读。

## 为什么保留

它们是重构期与审计期的证据：证明当时的构建、测试、竞态检测、内存测量、
端点冒烟确实跑过、结果是什么。删掉就等于丢掉「我们验证过」的凭据。

## 读的时候注意三件事

1. **包路径是旧的。** 文件里出现 `github.com/basketikun/chatgpt2api/...`，
   那是当时的模块路径。现在模块已改为 `github.com/IFuZYI/chatgpt2api`。
   这些输出是历史记录，**故意不回填** —— 改了就变成伪造的证据。
2. **数字会过时。** 测试数、覆盖率、内存占用都随代码演进变化。
   要看当前值请自己跑 `go test ./...`。
3. **时间戳是采集时刻**，见各文件开头。

## 文件说明

| 文件 | 内容 |
|:--|:--|
| `AUDIT_SUMMARY.md` | 全量运行审计汇总（构建 / vet / gofmt / 测试 / 竞态） |
| `FINAL_VERIFICATION.txt` | 最终验收记录 |
| `COMPARABILITY_REPORT.md` | 与 Python 版的行为对比报告 |
| `SCIENTIFIC_CHANGELOG.md` | 重构期的方法学变更记录 |
| `PATCHES.md` | 补丁说明 |
| `build_vet_fmt.txt` | 构建与静态检查原始输出 |
| `test_all.txt` / `test_race.txt` | 全量测试与 `-race` 原始输出 |
| `runtime_audit.txt` | 运行时审计 |
| `leak_check.txt` | 内存 / goroutine / fd 泄漏测量 |
| `smoke_evidence.txt` | 端点冒烟证据 |

## 与安全审计的关系

2026-10 那轮安全审计的完整产出不在这里，在
`~/security-audit-skill/chatgpt2api/run-1/`（仓库外）。
其中的凭据副本已脱敏。
