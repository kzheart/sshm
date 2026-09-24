# sshm

面向 Agent 的独立 SSH CLI + Skill。内置 `golang.org/x/crypto/ssh` 和 SFTP，文本输出，一份 TOML 配置。运行时不需要 OpenSSH、Node、Python 或 MCP。

```sh
sshm hosts --details
sshm exec macmini --command 'uname -s'
sshm shell macmini
sshm copy macmini --upload ./file.bin --remote /tmp/file.bin
```

## 安装

### 让 AI 一键安装

把下面这段话发给 Claude Code、Codex 等 Agent：

```text
请帮我安装 sshm CLI 和它的 Agent Skill，仓库是 https://github.com/kzheart/sshm 。
1. 用 `uname -s` 和 `uname -m` 判断平台：Darwin→darwin，Linux→linux；arm64/aarch64→arm64，x86_64→amd64。
2. 从 https://api.github.com/repos/kzheart/sshm/releases/latest 读取最新版本号 tag（形如 v0.4.0，文件名里的 VERSION 去掉前缀 v）。
3. 在临时目录下载 https://github.com/kzheart/sshm/releases/download/<tag>/sshm_<VERSION>_<os>_<arch>.tar.gz 和同一 Release 的 SHA256SUMS-<VERSION>，用 shasum -a 256 或 sha256sum 校验通过后再解压。
4. 进入解压目录运行 `./install.sh --prefix "$HOME/.local" --skill-dir <你自己加载用户级 Skill 的目录>`。Claude Code 用 ~/.claude/skills，Codex 用 ~/.agents/skills；其他 Agent 按其文档选择。
5. 运行 `~/.local/bin/sshm --help` 确认可用；若 ~/.local/bin 不在 PATH 中，告诉我需要加到哪个 shell 配置文件，不要擅自修改。
6. 如果 ~/.ssh/sshm.toml 不存在，只告诉我按 README 的“配置”一节创建并 chmod 600，不要替我编造服务器信息。
```

### 手动安装

支持 macOS / Linux，arm64 / amd64。从 [Releases](https://github.com/kzheart/sshm/releases) 下载对应平台的包，解压后运行：

```sh
./install.sh --prefix "$HOME/.local" --skill-dir "$HOME/.claude/skills"   # Claude Code
./install.sh --prefix "$HOME/.local" --skill-dir "$HOME/.agents/skills"   # Codex
```

安装器只复制本地二进制和 Skill，不联网，不改 SSH 配置或 PATH。升级时用新包重复安装即可。

### 从源码构建

需要 Go 1.26+（启用 Go 自动工具链时可以自动获取）：

```sh
make build
./install.sh --prefix "$HOME/.local" --skill-dir "$HOME/.claude/skills"
make dist   # 生成 macOS/Linux × arm64/amd64 四份发布包和 SHA-256 校验文件
```

## 配置

默认读取 `~/.ssh/sshm.toml`，也可通过 `-F /path/to/config.toml` 指定。配置必须属于当前用户、为普通文件且不允许组或其他用户读取（`chmod 600`）。以下只展示结构：

```toml
known_hosts = "~/.ssh/known_hosts" # 可省略，默认就是此路径

[ssh_servers.example]
host = "192.0.2.10"
port = 22
user = "admin"
key_path = "~/.ssh/id_ed25519"
# passphrase = "私钥口令"
# password = "登录密码"
description = "示例服务器"
# default_dir = "/srv"            # 仅提示，不自动切换目录

[ssh_servers.internal]
host = "192.0.2.20"
user = "admin"
key_path = "~/.ssh/id_ed25519"
proxy_jump = "example"             # 另一条配置的明确别名，可嵌套，最多 8 层
```

支持密码、私钥、加密私钥口令，以及只询问密码的 keyboard-interactive。验证码、多因素问答、SSH agent 和硬件安全密钥暂未实现。私钥文件也必须仅当前用户可访问。

配置只接受已定义字段，不会静默忽略拼写错误。相对 `key_path` 和 `known_hosts` 路径以 TOML 所在目录为基准。

`known_hosts` 是可信公钥记录，不是第二份服务器连接配置。首次连接自动接受并保存新主机公钥，无需输入 yes 或手动配置；文件不存在时自动创建。后续公钥发生变化或被撤销时，在发送认证凭据前拒绝连接并报告指纹。首次连接信任当时收到的公钥，不执行独立身份核验。兼容读取已有 OpenSSH known_hosts 格式，不依赖 OpenSSH 程序。不会执行 `~/.ssh/config`、ProxyCommand 或本地 shell 命令。

## 命令

| 命令 | 用途 |
| --- | --- |
| `hosts [筛选文本] [--details]` | 列服务器别名，详情可显示描述与默认目录提示，不连接远端或显示凭据 |
| `exec HOST --command STRING` | 原样执行远端命令，分开 stdout/stderr，保留远端退出码 |
| `shell HOST [--command STRING]` | 内置 SSH PTY 交互，保持该进程中的远端 shell 状态 |
| `copy HOST --upload LOCAL --remote PATH` | 内置 SFTP 单文件上传 |
| `copy HOST --download LOCAL --remote PATH` | 内置 SFTP 单文件下载 |
| `doctor` | 检查本地 TOML 与可信公钥文件，不连接服务器 |

各命令均支持 `-F` 指定 TOML，使用 `--help` 查看选项。

### exec

默认 stdin 为 EOF，不分配 PTY；`--stdin -` 继承输入，`--stdin FILE` 读取本地普通文件。命令原样发送，不添加 shell、sudo、cd、timeout 或结果标记，也不假定目标为 Linux。

```sh
sshm exec example --command '远端命令' --timeout 30s
sshm exec example --command 'sh -s' --stdin ./script.sh
sshm exec example --command '远端命令' --max-output 0 > result.bin
```

默认总期限 2 分钟，`--timeout 0` 不限。每一跳连接与握手另有 10 秒上限。每个远端输出流默认显示前 65536 字节，超出后持续排空而不缓存在内存；`--max-output 0` 完整流式输出。`--debug` 显示连接与命令阶段耗时，不打印凭据。

远端退出码原样保留。仅在出现 `sshm:` 本地诊断时，124/125/130 分别表示超时、本地或连接失败、取消；它们也可能是远端自己的退出码。SSH 错误使用 125，远端退出 255 可以准确保留。丢失执行请求确认或退出状态时不会自动重试；需要核实远端是否已执行。

### shell 与 copy

交互终端直接由本二进制提供。Agent 的执行宿主必须支持持续进程读写，通常应分配本地 PTY。CLI 负责 raw 模式恢复和终端窗口尺寸转发；无本地终端时用 `--cols` / `--rows` 设置初始尺寸。`shell` 默认不限时，可用 `--timeout`。宿主关闭 stdin 或输出接收方断开时会关闭连接，避免留下失去交互入口的进程；通过管道输入脚本并等待完整结果应使用 `exec --stdin`。全屏程序仍依赖宿主的终端仿真能力。

`copy` 要求远端支持 SFTP。单文件、流式传输，默认不覆盖。显式 `--overwrite` 会在开始写入时截断目标；失败可能留下部分文件。远端路径按字面传递，不展开 `~`、变量或通配符。目录同步、递归复制和后台任务调度不属于本工具。

## 生命周期与性能

`exec` 默认由同一个二进制按需启动本用户的短期连接服务；多个 Agent 共用它。服务不注册到系统启动项，不依赖外部程序。每个配置路径与主机别名最多一条可复用连接，每次命令使用独立 SSH 通道，不共享目录或变量。`shell` 和 `copy` 继续使用独立连接。

- `exec --fresh` 强制新连接，调用结束立即关闭；也可作为性能对照。`--debug` 显示 `reused=true/false`。
- 每条连接空闲 60 秒关闭；没有任务后服务空闲约 60 秒退出，并删除 socket。文件锁确保并发启动时只产生一个服务。
- 最多缓存 32 条连接，每条最多 8 个并发命令，超出排队且总期限继续计时；服务最多接收 64 个同时进行的请求。忙时可返回本地连接错误。
- 每次调用重新读取配置，检查私钥权限，并核对连接参数、私钥与 known_hosts 内容；变化后重新认证。服务器端撤销凭据不会自动终止已经认证的连接，需要立即重新认证时使用 `--fresh`。
- 取消或输出失败会停止复用该连接；已有其他命令可以完成，之后关闭底层连接。SSH 通道关闭可能要等远端确认，因此不能只发出关闭请求就把资源当作已回收。
- 每 10 秒探测连接存活，5 秒无响应则关闭失效连接。真实传输故障会影响该连接上的所有命令。断线后下一次调用重新连接，但结果未知的命令绝不自动重放。
- 本地控制 socket 位于 `/tmp/sshm-UID/control.sock`，目录 700、socket 600。可用 `SSHM_RUNTIME_DIR` 指定另一个专属目录用于隔离测试；不同目录是不同服务。内部协议不会改变 CLI 的纯文本与二进制流输出，也不把凭据或命令记录到磁盘。
- 服务无法启动时，在提交远端执行请求之前自动退回独立连接；请求提交之后失败不会退回重试。SIGKILL 导致调用端 socket 关闭时，服务也会取消该调用。

任何本地取消都不保证远端整个进程树已停止。确定的多步操作仍可合并到一次命令或脚本，减少通道往返。新版本增加一个短期 Go 进程及缓存连接的内存开销，换取重复调用延迟下降；并非每次首次连接都会更快。

真实延迟、并发、进程与空闲回收验证见 `docs/v3-validation.md`；独立连接基线见 `docs/v2-validation.md`。

## 验证与开发

```sh
make check
make integration               # Docker 内的隔离真实 sshd
python3 tests/reuse.py          # 复用/新连接对照、1000 次调用、故障及 60 秒退出
python3 tests/reuse.py --live macmini tencent --report docs/reuse-live-results.json
python3 tests/memory.py         # 1 MiB、64 MiB、256 MiB 内存与 CPU
python3 tests/live.py --hosts macmini tencent  # 仅在用户授权目标上执行
```

测试工具可以使用系统 ssh/ssh-keygen 创建夹具或作为性能基线，发布的 CLI 不调用它们。集成测试还会把 PATH 设为空，并测试 Linux 二进制。真实测试仅在随机、带所有权标记的 `/tmp/sshm-v2-*` 目录内传输文件，结束后清理，原配置与 known_hosts 内容应保持不变。

实现依据：[Go SSH](https://pkg.go.dev/golang.org/x/crypto/ssh)、[knownhosts](https://pkg.go.dev/golang.org/x/crypto/ssh/knownhosts)、[SFTP](https://github.com/pkg/sftp)、[终端处理](https://pkg.go.dev/golang.org/x/term)。
