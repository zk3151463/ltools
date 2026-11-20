# AGENTS.md — 给 OpenCode / AI Agent 的项目指引

LTools：Wails v3（alpha）+ Go 桌面插件工具箱。本仓库是 **fork 并定制** 版本：
原上游 20+ 个内置插件**已全部删除**，当前 `plugins/` 下**只有 `col1688/`**（1688 工厂/供应商采集）。
README.md 里列出的插件/功能（音乐播放器、lx-music-service、剪贴板等）均为上游残留描述，**不可信**，以实际目录为准。

## 构建顺序（关键，顺序错误必失败）

Windows 下必须按此顺序：

```
1. go mod download && go mod tidy
2. wails3 generate bindings -clean=true -ts   # 用 wails3 CLI（在 $HOME/go/bin，**不是** npx wails3，npm 上无此包）
3. cd frontend && npm install && npm run build  # 产出 frontend/dist，被 main.go 的 //go:embed all:frontend/dist 嵌入
4. $env:CGO_ENABLED=1; go build -buildvcs=false -gcflags=all="-l" -o bin/ltools.exe .
```

- `npm run build` 含 `tsc`，**bindings 未生成前必然失败**，属正常；先跑第 2 步。
- 改完 Go 服务方法后必须重跑 `wails3 generate bindings`，前端类型才能跟上。
- `frontend/bindings/` 是生成物，**不要手改**。
- `bin/ltools.exe` 与根目录 `ltools.exe` 均为构建产物（git 忽略）。

## 开发 / 测试

```
task dev                                  # wails3 dev，热重载，vite 端口 9245
go build . && go vet ./...               # 快速验证（go build ./... 在 build/ios 会报 main undeclared，属上游模板既有特性，忽略）
```

- `frontend` 下**没有** lint / typecheck 脚本，类型检查只在 `npm run build`（`tsc && vite build`）里发生。
- **不要** `taskkill /IM msedge.exe /F`：col1688 用独立 profile，但会误杀用户日常浏览器窗口。
- 跑 `task dev` / 后台构建被 kill 后，残留的 `wails3.exe` / `node.exe` / `esbuild.exe` 会锁住 go-build 缓存 → 报 "Access is denied"。需 `taskkill /F /IM wails3.exe` 等清残留，必要时 `go clean -cache`。

## Go 语言陷阱（真实踩过）

Go 正则引擎是 **RE2，不支持** `(?!)` `(?<!)` `(?=` 前瞻/后顾。
`plugins/col1688/parse.go` 曾在 `init()` 里 `MustCompile` 含 `(?![0-9])` 的正则 → 进程启动即 panic。写正则时避免前瞻。

## 插件架构（新增/修改插件必知）

- 插件实现 `internal/plugins` 的 `Plugin` 接口，`State` 字段**必须显式设为 `PluginStateInstalled`**，否则 Manager.Register 判定为禁用，前端 Home（仅展示 enabled）看不到该插件。
- 前端可见的服务方法：`app.RegisterService(application.NewService(...))` 注册后由 `wails3 generate bindings` 生成 TS 绑定。
- 前端事件需在 `main.go` 的 `init()` 里用**具体类型**注册，如 `application.RegisterEvent[col1688.LogEntry]("c1688:log")`（写 `map[string]any` 会报 "data does not match registered type"）。
- 插件持久化数据在 `%AppData%\ltools\`（`plugins.json`、`shortcuts.json`、各插件 dataDir）。

## col1688 插件：1688 浏览器采集（本仓库核心功能）

`plugins/col1688/`：go-rod 驱动真实 Chrome/Edge，采集 1688 工厂/供应商的 名称/电话/地址，可导出 CSV。

### 抗风控设计（改动前必读，1688 有阿里云盾 baxia 强风控）

- **新开冷浏览器无法 100% 不弹滑块**。当前设计是"预热养信任度 + 持久 profile + 首次人工拖一次滑块"，人工过掉后信任度保留。程序**不自动破解滑块**（也做不到），只负责检测 + 等待。
- **反检测注入必须是跨导航的**：`browser.go` 的 `connect()` 用 `Page.enable` + `EvalOnNewDocument`（CDP `Page.addScriptToEvaluateOnNewDocument`）注册 `stealthJS`，使**每个新文档（含 baxia 验证页）JS 运行前都先执行**。
  - ⚠️ 不要退回 `stealth.Page`（go-rod/stealth）——它只对当前页 Eval 一次，不跨导航，导致"人工拖滑块仍被拒绝(error:csnr0k)"。
- **旗标最小化**：`launch()` 用 `launcher.NewUserMode()`（真实浏览器引擎），只保留 `no-first-run` / `no-default-browser-check` / 调试端口 8023 / 独立 profile；已显式 `Delete("enable-automation")` 等"脚本味"旗标。**不要**加回 `disable-web-security` / `start-maximized`。
- **独立 profile**：`browserProfileDir` = `<dataDir>/chrome-profile`，登录态持久。`ResetProfile()` 会删除它（前端有「重置浏览器环境」按钮）——被 baxia 反复拒绝后的逃生通道。
- 不覆盖 UA（用真实浏览器 UA，避免"Edge 却报 Chrome"指纹矛盾）；只补 `AcceptLanguage=zh-CN` + locale `zh_CN` + 时区 `Asia/Shanghai`。

### 数据流

`service.go`（Wails 服务，暴露 Start/Stop/ExportCSV/OpenLogin/CloseLogin/ResetBrowserProfile 等）→ `browser.go`（go-rod 引擎）→ `parse.go`（离线解析，多策略：内嵌 `__INITIAL_DATA__` JSON + DOM 启发式 + 正则提电话/地址）。
`html` 模式（粘贴 HTML）离线可用、不依赖登录态，是稳定的降级路径。

## 版本管理

`Taskfile.yml` 顶层 `VERSION: "0.1.8"` 是**静态值**（原用 awk 读 `build/config.yml`，Windows 无 awk 导致 exit 127，连 `task dev` 都失败）。
改动版本号时需**手动同步** `Taskfile.yml` 的 `VERSION` 与 `build/config.yml` 的 `info.version`。

## 记忆 / 参考

- `.workbuddy/memory/*.md`：逐日项目操作日志（踩坑、根因、验证记录），改 col1688 / 构建问题前先看最近几天的。
- `CLAUDE.md`：上游模板的详尽架构说明（事件系统、快捷键、多窗口等），但其中"内置插件"章节与当前仓库不符（插件已删）。
