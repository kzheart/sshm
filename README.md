# sshm

面向 Agent 的独立 SSH CLI + Skill。内置 `golang.org/x/crypto/ssh` 和 SFTP，文本输出，一份 TOML 配置。运行时不需要 OpenSSH、Node、Python 或 MCP。

```sh
sshm hosts --details
sshm exec macmini --command 'uname -s'
sshm shell macmini
sshm copy macmini --upload ./file.bin --remote /tmp/file.bin
```

## 安装

本地支持 macOS / Linux，arm64 / amd64。发布包解压后运行 `./install.sh --prefix "$HOME/.local" --skill-dir "$HOME/.agents/skills"`。安装器只复制本地二进制和 Skill，不联网，不改 SSH 配置或 PATH。构建依赖会编译进二进制，使用者无需安装它们。

从源码构建需要 Go 1.26+（启用 Go 自动工具链时可以自动获取）：

```sh
make build
./install.sh --prefix "$HOME/.local" --skill-dir "$HOME/.agents/skills"
make dist
```

`make dist` 生成 macOS/Linux × arm64/amd64 四份独立发布包和 SHA-256 校验文件，不上传公共仓库。升级用新包重复安装；回退使用旧包。

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

配置只接受已定义字段，不会静默忽略拼写错误。旧 MCP 的 `readonly/restricted` 等 mode 不会被直接当作无限制配置执行；发现时拒绝加载。相对 `key_path` 和 `known_hosts` 路径以 TOML 所在目录为基准。

`known_hosts` 是可信公钥记录，不是第二份服务器连接配置。未知或变化的公钥会在发送认证凭据前被拒绝，并报告指纹；应经独立可信渠道核对并配置公钥。兼容读取已有 OpenSSH known_hosts 格式，不依赖 OpenSSH 程序。不会执行 `~/.ssh/config`、ProxyCommand 或本地 shell 命令。

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

每次 CLI 调用拥有自己的连接和跳板链，结束时关闭。没有守护进程、连接池、控制 socket、自动重试或跨进程复用。持续 shell 和一次文件传输在自身调用内保持连接；不同 exec 的目录与变量不共享。

这种结构减少了后台残留来源，但每次新调用仍需握手。确定的多步操作可合并到一次命令或脚本，避免重复握手。SIGINT/SIGTERM/SIGHUP、总期限和本地输出失败会关闭本次连接；SIGKILL 时操作系统回收进程的本地 socket。任何本地取消都不保证远端整个进程树已停止。

内存与吞吐、首次调用与旧版复用调用的差别，以及 1,000 次连接压力测试，见源码中的 `docs/v2-validation.md`。数据来自明确记录的测试环境，不代表所有网络或设备。

## 从 0.1.x 迁移

0.2.0 的 `-F` 改为 TOML。删除调用中的 `--askpass`、`--persist`；密码直接从 TOML 读取。交互改用 `sshm shell`，单文件传输改用 `sshm copy`。更新 Agent Skill，避免沿用旧参数。

本机升级会备份旧 CLI、Skill 和迁移文件后，移除旧版生成的 `sshm.conf` Include 与 Python askpass；保留原本的 SSH 配置和 known_hosts。0.1.x 验证与安装文档仅为历史记录。

## 验证与开发

```sh
make check
make integration               # Docker 内的隔离真实 sshd
python3 tests/performance.py    # 与 OpenSSH / 可选旧版 CLI 对照
python3 tests/memory.py         # 1 MiB、64 MiB、256 MiB 内存与 CPU
python3 tests/live.py --hosts macmini tencent  # 仅在用户授权目标上执行
```

测试工具可以使用系统 ssh/ssh-keygen 创建夹具或作为性能基线，发布的 CLI 不调用它们。集成测试还会把 PATH 设为空，并测试 Linux 二进制。真实测试仅在随机、带所有权标记的 `/tmp/sshm-v2-*` 目录内传输文件，结束后清理，原配置与 known_hosts 内容应保持不变。

实现依据：[Go SSH](https://pkg.go.dev/golang.org/x/crypto/ssh)、[knownhosts](https://pkg.go.dev/golang.org/x/crypto/ssh/knownhosts)、[SFTP](https://github.com/pkg/sftp)、[终端处理](https://pkg.go.dev/golang.org/x/term)。
