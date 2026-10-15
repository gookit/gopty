# gopty

![GitHub go.mod Go version](https://img.shields.io/github/go-mod/go-version/gookit/gopty?style=flat-square)
[![GitHub tag (latest SemVer)](https://img.shields.io/github/tag/gookit/gopty)](https://github.com/gookit/gopty)
[![Unit-Tests](https://github.com/gookit/gopty/actions/workflows/go.yml/badge.svg)](https://github.com/gookit/gopty/actions/workflows/go.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/gookit/gopty.svg)](https://pkg.go.dev/github.com/gookit/gopty)

> [English](./README.md) | [简体中文](./README.zh-CN.md)

平台无关的 Go pty（伪终端）后端，以及建立在它之上的交互式测试夹具。把子进程跑在 pty master 上，
读写它的字节流、调整终端尺寸并等待退出码——同一份代码在 Linux、macOS 和 Windows(ConPTY) 上通用。

## 两个包

| 包 | 用途 |
| --- | --- |
| `github.com/gookit/gopty` | 产品代码：`Spec` + `Start` → `Pty`（`Read` / `Write` / `Resize` / `Wait` / `Close`）、`IsAvailable()`、`IsHostBlocked(err)`、`IsBlockedExitCode(code)` |
| `github.com/gookit/gopty/ptytest` | 测试代码：在 `go test` 里驱动 pty 中的程序 —— `Start` / `StartSpec`、`Mark`、`Expect` / `Contains` / `Count`、`Text`(剥 ANSI) / `RawText`、`Send` / `SendLine` / `SendCtrlC`、`WaitExit`，并自动注册 `Close` 清理 |

## 安装

```bash
go get github.com/gookit/gopty
go get github.com/gookit/gopty/ptytest   # 测试夹具
```

要求 Go 1.23 及以上。

## 后端与依赖

| GOOS | 后端 | 依赖 |
| --- | --- | --- |
| `linux`、`macOS`、`*BSD` | [creack/pty](https://github.com/creack/pty)（master 文件就是数据流） | `github.com/creack/pty` |
| `windows`（Win10 1809+） | vendored 的 [ConPTY](https://github.com/UserExistsError/conpty) 副本，在 `./conpty`，MIT | `golang.org/x/sys` |

两个后端满足同一套 `Pty` 契约，所以跑 pty 子进程的程序在 unix worker 与 Windows 上编译和行为一致。

> Windows 侧刻意只保留 `golang.org/x/sys` 一个依赖：不像 `aymanbagabas/go-pty` 那样拖进一串间接依赖。

## 用法

### 在 pty 上跑一个子进程

```go
import "github.com/gookit/gopty"

p, err := gopty.Start(gopty.Spec{Command: "bash", Args: []string{"-i"}, Cols: 100, Rows: 30})
if err != nil {
	return err
}
defer p.Close()

_, _ = p.Write([]byte("echo hi\n"))
out, _ := io.ReadAll(p)          // stdout + stderr 合流
code, err := p.Wait(ctx)         // 阻塞到子进程退出（或 ctx 被取消）
```

`Spec` 字段：

| 字段 | 含义 |
| --- | --- |
| `Command`、`Args` | 可执行文件 + 参数（不含 argv[0]）。Windows 上会合并成一条安全引号化的命令行（`windows.EscapeArg`），因为 ConPTY 收的是命令行而不是 argv |
| `Env` | 完整环境变量；`nil` 继承父进程。Windows 上重名变量按 “后者生效”（`os/exec` 的规则）去重，因为 `CreateProcess` 取的是第一个 |
| `Dir` | 工作目录；`""` 继承 |
| `Cols`、`Rows` | 初始终端尺寸；`0` 用平台默认 |

`Pty` 契约：`Read` 是 stdout+stderr，`Write` 是 stdin，`Resize` 按字符单元设置窗口大小，
`Wait(ctx)` 返回退出码（真正的进程退出是 `(code, nil)`，只有 reap 本身失败才返回 error），
`Close` 释放 master 并在 unix 上杀掉子进程。**拆除顺序由调用方的状态机决定**，本包只提供原语。

### 在测试里驱动交互式程序

```go
import "github.com/gookit/gopty/ptytest"

func TestAskName(t *testing.T) {
	s := ptytest.Start(t, "sh", "-c", `printf "name? "; read n; echo got=$n`)
	mark := s.Mark()
	if !s.Expect(`name\?`, mark) {
		t.Fatalf("no prompt, output:\n%s", s.Text())
	}
	s.SendLine("tom")
	if !s.Expect(`got=tom`, mark) {
		t.Fatalf("no answer, output:\n%s", s.Text())
	}
}
```

它就是 “pty 冒烟脚本” 的 Go 版本（以前要靠 Python pty driver 或 Windows 控制台 harness 做的事），
所以同一份测试代码在每个平台都能跑。

几条经验规则：

- 只断言 `Mark` 之后产生的输出——更早的轮次会误打误撞满足检查；
- 先 `Expect` 等提示符再输入：提问前会清空待处理输入的程序会丢掉太早敲的键；
- `Enter` 是 `"\r"`，Ctrl-C 是 `0x03`（用 `SendCtrlC`），Ctrl-D 是 `0x04`；
- `Text()` 剥掉 ANSI 转义，`RawText()` 保留（纯颜色输出对 `Text` 不可见）；
- 没有可用 pty 后端、或宿主拒绝 pty 子进程时，`Start` 会带原因 `t.Skip`，并自行注册 `Close` 清理。

## 平台注意事项

### Windows（ConPTY）

- ConPTY 需要 Win10 1809+；`IsAvailable()` 会探测，缺能力时 `ptytest.Start` 直接跳过。
- **部分 Windows 进程上下文会拒绝 ConPTY 子进程**：在不允许 breakaway 的 job 对象里，
  `CreateProcess` 报 `ERROR_ACCESS_DENIED`，或子进程在加载阶段以 `STATUS_DLL_INIT_FAILED`
  (`0xC0000142`) 退出。用 `IsHostBlocked(err)` / `IsBlockedExitCode(code)` 识别并优雅降级；
  `ptytest` 会带原因跳过，而不是抛一个裸错误码。结果取决于环境——在同一个 agent job 里实测过两种
  情况（`cmd.exe` 正常、另一个 harness 被拒）——所以不要预设哪一种。
- 宿主确实拒绝 ConPTY、又必须测交互提示时，改用真实控制台驱动：`AllocConsole`、子进程 stdin 指向
  `CONIN$`，再用 `WriteConsoleInput` 发按键。
- `Close()` 幂等。vendored 的 `conpty.Close()` 会无条件关闭 4 个管道句柄 + 伪控制台，二次调用会
  关到 Windows 可能已复用的句柄值（实测会让测试进程静默挂掉），适配层用 `sync.Once` 护住了。
- 往输入写 `0x03` 合成的 Ctrl-C 不等于 `CTRL_C_EVENT`，子进程的控制台模式决定它怎么被读取。

### unix

- 父进程会一直持有自己的 slave fd 直到子进程被 reap，让 reaper 那次关闭成为最后一次关闭。在 darwin
  上，BSD tty 层会在 slave 最后一次关闭时把 master 尚未读走的输出刷掉——短命子进程在 reader 追上之前
  打印的内容会被整段丢掉。
- 子进程已经退出后，`Close()` 会先（有界，2s）等 reader 读到 EOF 再关 master，避免丢掉退出前刚写入
  的输出；子进程还活着（取消）时立即拆除。

## 开发与测试

```bash
go test ./...                  # 本机平台
go test -race ./...            # unix
GOOS=windows go build ./...    # 交叉编译检查（还有 GOOS=darwin、GOOS=linux）
go vet ./...
```

CI（[.github/workflows/go.yml](./.github/workflows/go.yml)）在 ubuntu + windows 上跑 Go
1.23 / 1.24 / 1.25 / stable，macOS 上跑 1.23 与 stable，并在 ubuntu/stable 这一条上跑 race 检测。
如果 runner 的会话不能承载 ConPTY 子进程，Windows 的 pty 用例会跳过并在日志里写明原因。

## 使用方

- inhere/gofer —— 交互式 agent 会话的 pty runner（`internal/runner/pty`）；
- gookit/gcli —— 提示与共享交互选项的端到端测试（`e2e_pty_test.go`）。

## License

MIT，见 [LICENSE](./LICENSE)。vendored 的 `./conpty` 包保留自己的副本
[conpty/LICENSE](./conpty/LICENSE)。
