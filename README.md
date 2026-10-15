# pty

平台无关的 pty（伪终端）后端 + 交互式测试夹具。

从 `gofer/internal/pty` 抽出，供工作区内多个项目复用（gofer serve 的交互会话、各 CLI 的
交互 e2e 测试）。

## 两个包

| 包 | 作用 |
|---|---|
| `github.com/gookit/gopty` | 产品用：`Spec` + `Start`（unix=creack/pty，windows=内置 vendored ConPTY）+ `Pty`(Read/Write/Resize/Wait/Close) + `IsAvailable()` / `IsHostBlocked(err)` |
| `github.com/gookit/gopty/ptytest` | 测试用：在 pty 里跑一个命令并提供 expect/发送/断言/收尾（`Expect`/`Count`/`Text`(剥 ANSI)/`Send`/`SendLine`/`SendCtrlC`/`SendKeys`/`WaitExit`/`Cleanup`），不可用时自动 `t.Skip` |

## 依赖

`creack/pty`（MIT，unix）+ `golang.org/x/sys`（windows）。**不引入 hcsshim**：Windows 侧用
vendored 的 `./conpty`（UserExistsError/conpty 单文件版，MIT，见 `conpty/LICENSE`），这也是当初
排除 `aymanbagabas/go-pty`（会拖进 hcsshim 一串间接依赖）的原因。

## 用法

产品侧：

```go
import "github.com/gookit/gopty"

p, err := pty.Start(pty.Spec{Command: "bash", Cols: 100, Rows: 30})
defer p.Close()
p.Write([]byte("ls\n"))
code, err := p.Wait(ctx)
```

测试侧：

```go
import "github.com/gookit/gopty/ptytest"

func TestAskPrompt(t *testing.T) {
	s := ptytest.Start(t, "sh", "-c", `printf "name? "; read n; echo got=$n`)
	s.SendLine("tom")
	if !s.Expect(`got=tom`) {
		t.Fatalf("no answer, output:\n%s", s.Text())
	}
}
```

## Windows 注意事项（实测）

- ConPTY 需要 Win10 1809+；`IsAvailable()` 会探测。
- **部分 Windows 进程上下文会拒绝 ConPTY 子进程**（job 对象不允许 breakaway 时，`CreateProcess`
  带 `PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE` 报 `0xC0000142`/STATUS_DLL_INIT_FAILED）。
  实测差异：suga 的 `scripts/wincons` 在 gofer job 里报过这个错，而本库在 gofer job 里
  `cmd.exe` 子进程正常（exit 0）——所以它取决于环境，不要假设哪种。`IsHostBlocked(err)` /
  `IsBlockedExitCode(code)` 用于识别；`ptytest.Start` 会据此 `t.Skip` 并说明原因。
  真被拒时想测交互提示，用真实控制台驱动（见 gookit/gcli 的 `scripts/wincons`）。
- `Pty.Close()` 在 Windows 上幂等：`conpty.Close()` 会无条件关闭 4 个管道句柄 + 伪控制台，
  二次调用可能关到 Windows 已复用的句柄值（实测会让测试进程静默挂掉），适配层用 `sync.Once` 护住了。
- Ctrl-C 等合成按键在 ConPTY 下不等于 `CTRL_C_EVENT`（往输入写 `0x03` 即可，但控制台模式决定）。

## 测试

```bash
go test ./...                      # unix 本机 (5 个 pty 用例 + 5 个 ptytest 用例)
GOOS=windows go build ./...        # 交叉编译检查
# windows 本机: 2 个 ptytest 用例 + 1 个 pty 用例(ConPTY)；
# host 被 job 包裹且拒绝 ConPTY 时, 会跳过并说明原因
```

实测(2026-10-15): Windows 本机 go1.25.10 全绿; Linux 容器 golang:1.26 全绿。
