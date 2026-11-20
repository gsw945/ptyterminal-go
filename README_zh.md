# ptyterminal-go

.NET 跨平台伪终端库 [PtyTerminal](https://github.com/gsw945/PtyTerminal) 的 Go 实现：跨平台伪终端 (PTY) 库与使用示例。

[English](./README.md) | **简体中文**

## 项目结构

```
ptyterminal-go/
├── ptygo/                  # 核心库（Go module: github.com/gsw945/ptyterminal-go/ptygo）
│   ├── ptygo.go            # 公共 API：Options / Connection / Start()
│   ├── unix.go             # Unix 后端（Linux/macOS/BSD，基于 github.com/creack/pty）
│   ├── conpty_windows.go   # Windows ConPTY 后端（P/Invoke 实现，参考 microsoft/hcsshim）
│   ├── winpty_windows.go   # Windows winpty 后端（DLL 动态加载，参考 iamacarpet/go-winpty）
│   └── deps/winpty/        # winpty DLL 三件套（与 C# PtyTerminal 同源）
├── examples/
│   ├── pty-cli/            # 控制台示例：raw 模式字节透传 + 尺寸同步
│   ├── pty-web/            # Web 示例：内置 http + WebSocket + xterm.js
│   ├── pty-web-signalr/    # Web 示例：内置 http + SignalR + xterm.js
│   └── pty-session/        # Web 示例：pty 会话管理（列表/创建/销毁/断线恢复）
└── go.mod
```

## 快速开始

```bash
# 控制台终端
go run ./examples/pty-cli

# Web 终端（WebSocket，默认 http://localhost:8877，路径 /terminal）
go run ./examples/pty-web

# Web 终端（SignalR，默认 http://localhost:8878，hub 路径 /terminalHub）
go run ./examples/pty-web-signalr

# Web 终端（pty 会话管理，默认 http://localhost:8879，路径 /terminal）
go run ./examples/pty-session
```

## 构建可执行文件（bin/）

仓库根目录的 `bin/` 是构建产物目录（已被 `.gitignore` 忽略），由 `go build -o bin/...` 生成。构建全部示例：

```bash
# 构建全部示例到 bin/（Windows 上自动生成 .exe）
mkdir -p bin
go build -o bin/pty-cli ./examples/pty-cli
go build -o bin/pty-web ./examples/pty-web
go build -o bin/pty-web-signalr ./examples/pty-web-signalr
go build -o bin/pty-session ./examples/pty-session
```

- 单个示例：`go build -o bin/pty-web ./examples/pty-web`
- 运行构建产物：`./bin/pty-web -addr :8877`（参数与 `go run` 相同，如 `-winpty`）
- **Windows + winpty 模式**（`-winpty` / `ForceWinPty: true`）需要把 winpty DLL 复制到可执行文件旁：

```bash
# 仅 Windows：复制 winpty DLL 三件套到 bin/deps/winpty/（可执行文件按此路径自动查找）
mkdir -p bin/deps/winpty
cp ptygo/deps/winpty/* bin/deps/winpty/
```

> 说明：`bin/` 由上述命令生成，不属于源码，请勿手动提交；winpty DLL 的完整查找顺序见下文「Windows 后端选择」。

也可以直接使用仓库提供的构建脚本：`build.cmd`（Windows）或 `build.sh`（Unix/Git Bash）。

## 核心 API

```go
import "github.com/gsw945/ptyterminal-go/ptygo"

conn, err := ptygo.Start(ptygo.Options{
    Rows: 30, Cols: 80,
    Cwd:  "/home/user",
    App:  "/bin/bash",
    Args: []string{"--login"},
    Env:  []string{"FOO=bar"},      // 空值表示删除该环境变量
    ForceWinPty: false,             // Windows 上强制 winpty（默认 ConPTY）
})
if err != nil { /* ... */ }
defer conn.Close()

go io.Copy(os.Stdout, conn)   // pty 输出
go io.Copy(conn, os.Stdin)    // pty 输入
<-conn.Done()                 // 等待进程退出
fmt.Println(conn.ExitCode())
```

`Connection` 接口：

| 方法 | 说明 |
|---|---|
| `Read/Write` | pty 输出读取 / 输入写入（`io.Reader`/`io.Writer`） |
| `Close()` | 释放全部资源（幂等，会终止未退出的子进程） |
| `Pid()` / `ExitCode()` | 进程 ID / 退出码（未退出时为 0） |
| `Resize(cols, rows)` | 调整终端尺寸 |
| `Kill()` | 立即终止子进程 |
| `Wait()` / `Done()` | 阻塞等待退出 / 退出通知 channel |

## Windows 后端选择（与 PtyTerminal 规则一致）

| 条件 | 后端 |
|---|---|
| Windows 10 1809+ / Server 2019+（默认） | **ConPTY**（系统 API，无额外依赖） |
| 旧系统（无 CreatePseudoConsole） | winpty（自动回退） |
| `ForceWinPty: true`（或 CLI `--winpty`） | winpty |

**ConPTY 下的 Ctrl+C**：创建伪控制台会让**宿主进程**进入忽略 Ctrl+C 的状态，
导致写入输入管道的 `0x03` 不会变成子进程的 `CTRL_C_EVENT`。库现在在
`CreatePseudoConsole` 之后立即调用 `SetConsoleCtrlHandler(NULL, FALSE)` 恢复默认
处理（与 node-pty / VS Code 相同），ConPTY 下 Ctrl+C 已能中断运行中的程序。
ConPTY 与 winpty 两个后端都有回归测试。

winpty DLL（`winpty.dll`/`winpty-agent.exe`/`winpty-debugserver.exe`）位于 `ptygo/deps/winpty/`，与 C# PtyTerminal 同源。运行时按以下顺序查找：

1. 环境变量 `PTYGO_WINPTY_DLL`（指定 winpty.dll 的完整路径）
2. 可执行文件同目录的 `deps/winpty/winpty.dll`
3. 可执行文件同目录的 `winpty.dll`
4. 当前工作目录的 `deps/winpty/winpty.dll`

## 示例说明

### pty-cli

- 把宿主编解码切换为 raw 模式（Windows `SetConsoleMode` / Unix `termios`），按键以原始字节直接转发给 pty——方向键/功能键/Ctrl/Alt 组合/IME 输入均正常工作
- 控制台窗口尺寸变化自动同步（500ms 轮询）
- stdin 重定向（管道/文件）时自动跳过 raw 模式，字节原样透传
- 退出方式：在 pty 的 shell 中正常退出（如 `exit`）；Ctrl+C 会作为按键转发给 pty 内程序

### pty-web

- 内置 `net/http` + [github.com/coder/websocket](https://github.com/coder/websocket)，前端 xterm.js（通过 `go:embed` 打进二进制，无外部文件依赖）
- 线协议与 C# PtyTerminal 的 web 示例一致：
  - 客户端 → 服务端，文本帧：作为输入字节转发给 pty
  - 客户端 → 服务端，二进制帧：JSON 指令，如 `{"action":"resize","data":{"cols":120,"rows":40}}`
  - 服务端 → 客户端，二进制帧：pty 原始输出
- 参数：`-addr`（默认 `:8877`）、`-winpty`（强制 winpty 后端）

### pty-web-signalr

- 基于 [github.com/philippseith/signalr](https://github.com/philippseith/signalr) 的 SignalR Hub 示例（前端 xterm.js + `@microsoft/signalr`，协议与 C# PtyWeb.SignalR 对齐）：
  - 客户端 → 服务端：`input(string)`（小驼峰映射自 Go 方法 `Input`）、`resize(cols, rows)`
  - 服务端 → 客户端：`Output(byte[])`（JSON 协议下为 base64）、`Closed(exitCode)`
- 注意：philippseith/signalr 的 hub 方法**不能带 `context.Context` 参数**（与 ASP.NET Core 约定不同，context 通过 `h.Context()` 获取）
- 每个连接一个 hub 实例；hub 方法可能在 `OnConnected`（pty spawn）完成前被调用——`Input` 与 `Resize` 都会等待 pty 就绪而非丢弃调用（Resize 另有延迟重试，规避 ConPTY 在 spawn 后立即 resize 被忽略的竞态）
- 已知兼容性处理：winpty 后端存在"agent 未就绪时输入丢失"的 quirk，示例对首次输入延迟 2 秒（仅 `--winpty` 时生效；ConPTY 后端有库内就绪门控，不受影响）
- 参数：`-addr`（默认 `:8878`）、`-winpty`

### pty-session

基于 coder/websocket 的 pty 会话管理示例（协议与 C# PtyTerminal 的 examples/PtySession 对齐）：

- **会话独立于浏览器连接**：关闭页面后 pty 在服务端继续运行；重新打开页面可从列表选择会话并恢复 xterm 交互（每个会话缓存最近 64KB 输出，attach 时回放，解决"开头输出看不到"的 JoinGroup 时序问题）
- **销毁**：客户端显式 destroy，或 pty 进程退出（如输入 exit）时自动销毁
- 协议：二进制帧为 JSON 命令（list/create/attach/detach/destroy/resize），文本帧为 pty 输入，二进制帧为输出（含回放）
- 参数：`-addr`（默认 `:8879`）、`-winpty`

## 修改指南

| 想改什么 | 改哪里 |
|---|---|
| 默认 shell（cmd/bash） | `examples/pty-cli/main.go`、`examples/pty-web/main.go`、`examples/pty-web-signalr/main.go`、`examples/pty-session/session.go` 的 `defaultShell()` |
| Web 监听端口 | 启动参数 `-addr=:8888` |
| 前端页面（配色/字体/标题） | `examples/pty-web/index.html`、`examples/pty-web-signalr/index.html`（改后重新 `go build`） |
| 传给子进程的环境变量 | `Options.Env`（空值=删除变量） |
| 初始终端尺寸 | `Options.Rows/Cols` |
| 库引入后的 DLL 部署 | 把 `deps/winpty` 放到 exe 旁或设置 `PTYGO_WINPTY_DLL` |

## 已知限制

- **winpty 后端的输入就绪 quirk**：在长驻服务进程（pty-web-signalr / pty-session）中，spawn 后过早写入的输入可能整体丢失（直接进程与 ConPTY 后端无此问题）。示例已通过首次输入延迟与会话通道就绪等待缓解；自动化场景如仍遇输入丢失，请使用 ConPTY 后端或 C# PtyTerminal（生产验证稳定）。

- 本仓库在"无控制台的自动化环境"（如 CI、管道重定向）下实测：**winpty 后端稳定，ConPTY 后端存在与 Go 运行时相关的初始化竞态**（cmd.exe 偶发 `0xC0000142` 或输出 banner 后立即退出；交互式真实终端场景正常）。ConPTY 实现已修复环境块双 NUL、COORD 按值传递、环境排序三个问题，并内置就绪等待与自动重试；如需在自动化场景获得稳定表现，请使用 winpty 后端（`ForceWinPty: true` / `--winpty`）
- 请勿在 fork 后（`forkpty` 创建的 Unix 子进程内）调用 Go runtime 的并发特性

## 参考

- [PtyTerminal (C#)](https://github.com/gsw945/PtyTerminal)：本项目的 API 与行为对齐来源
- [microsoft/hcsshim internal/conpty](https://github.com/microsoft/hcsshim/blob/main/internal/conpty/conpty.go)：ConPTY 参考实现
- [github.com/creack/pty](https://github.com/creack/pty)：Unix 后端
- [github.com/coder/websocket](https://github.com/coder/websocket)：WebSocket 库
