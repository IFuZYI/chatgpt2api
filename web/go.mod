// 这个文件的存在不是为了编译任何 Go 代码 —— web/ 是纯前端目录。
//
// 它是**模块边界标记**：Go 的 `./...` 无法排除 node_modules，而依赖树里
// 可能有 Go 源码（实测：flatted 包内含 golang/ 目录）。没有这个 go.mod 时，
// 从仓库根跑 `go list ./...` 会把它当成我们模块的一部分：
//
//   有 node_modules → 11 个包；没有 → 10 个包
//
// 于是 `go test ./...` / `go vet ./...` 的结果取决于「有没有跑过 npm install」，
// 而且会编译与测试一段我们既没有写过、也不控制的第三方代码（它随 npm 依赖
// 版本变化）。
//
// 放一个最小 go.mod 后，`./...` 到此为止，行为与 node_modules 是否存在无关。
// 对 Next.js / npm / Docker 构建均无影响（它们不读 go.mod）。
module github.com/IFuZYI/chatgpt2api/web

go 1.27
