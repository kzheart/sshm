# sshm

给 Agent 使用的精简 SSH CLI + Skill。三个命令，文本输出，复用系统 OpenSSH；没有自己的后台服务、SSH 协议实现、服务器数据库或运行时依赖。

```sh
sshm hosts
sshm exec my-server --command '远端命令'
sshm doctor
```

## 安装

本地客户端支持 macOS 和 Linux（arm64 / amd64）。需要系统 OpenSSH；CLI 本体不需要 Go、Node、npm 或 Python。可选的 SSH Manager TOML 认证辅助程序使用 Python 3.11+。远端可以是提供所需 SSH 能力的任意系统，但实际验证范围见下文。

从源码构建需要 Go 1.24+：

```sh
make build
./bin/sshm --help

# 安装二进制；Skill 安装目录按所用 Agent 的发现机制选择。
./install.sh --prefix "$HOME/.local" --skill-dir /path/to/agent/skills
```

安装器只复制本地文件，不联网、不修改 SSH 配置、不修改 PATH 或 Agent 配置。二进制安装到 `PREFIX/bin/sshm`；Skill 安装到指定目录下的 `sshm/SKILL.md`。也可以直接使用项目内的二进制绝对路径，并让 Agent 阅读 [Skill](skills/sshm/SKILL.md)。

```sh
# 生成四个平台的独立发布包和 SHA-256 校验文件，不发布到任何仓库。
make dist
```

发布包包含 `sshm`、`install.sh`、本说明和 Skill。解压对应架构的包后可直接运行，或执行其中的安装器。升级用新包重复安装；回退使用旧包。删除已安装的二进制和本 Skill 目录即可卸载，原有 SSH 配置不受影响。

从 SSH Manager 迁移时，可保留原 TOML 作为凭据来源，使用源码中的 `scripts/ssh-manager-askpass.py`。该可选程序默认读取 `~/.ssh/sshm.toml`（可用 `SSHM_CREDENTIALS` 指定其他路径），要求文件属于当前用户且权限为 600；根据 OpenSSH 请求的账号、地址或私钥路径选择凭据，支持跳板和目标分别认证，拒绝未知主机确认、验证码和歧义条目。它不随基础发布包安装。连接字段仍需放入标准 SSH config；本机迁移布局和验证记录见源码中的 `docs/migration.md`。

## 命令

### `hosts`：发现目标

```sh
sshm hosts
sshm hosts prod
sshm hosts -F ./ssh-config
```

每行一个明确的候选主机别名，不显示密钥和密码。默认扫描 `~/.ssh/config` 与 `/etc/ssh/ssh_config`；指定 `-F` 时只扫描该配置及其 Include，与 OpenSSH 的配置入口一致。

支持 Host 多别名、去重、大小写不敏感筛选、Include 通配符与递归。相对 Include 路径按 OpenSSH 规则相对于 `~/.ssh` 或 `/etc/ssh`，**不是当前配置文件目录**。

这是语法层面的候选发现，不是配置求值或网络探测：

- 忽略 Host 通配符和否定模式，不展开 DNS、云主机或 SSH known_hosts。
- 条件 Include 中的明确声明也可能列出；能否用于当前请求由 OpenSSH 判断。
- 不执行 `Match exec`，不调用 shell；发现不等于认证成功或机器在线。
- 无法静态展开的 Include token 会给出提示，并以退出码 1 表达清单可能不完整。Include 循环等解析错误返回 125。
- 实际连接完全由 OpenSSH 解释配置，本工具的发现解析器不参与认证或连接参数生成。

### `exec`：非交互执行

```sh
sshm exec my-server --command '远端命令'
sshm exec my-server -F ./ssh-config --command '远端命令' --timeout 30s

# 已知远端提供 POSIX sh 时，由调用者显式选择解释器。
sshm exec my-server --command 'sh -s' --stdin ./script.sh
printf 'some input\n' | sshm exec my-server --command 'cat' --stdin -

# 不截断输出，以普通 shell 重定向保存完整结果。
sshm exec my-server --command '远端命令' --max-output 0 > result.txt
```

`--command` 是一个完整的远端命令字符串，不是 argv 数组。CLI 不通过本地 shell 启动 SSH，不修改命令内容，不添加 `sh -c`、`cd`、`sudo`、`timeout` 或结束标记。远端 SSH 服务如何解释命令取决于它的 shell/设备环境。调用 CLI 的本地 shell 引号仍由调用者负责。

| 选项 | 默认 | 含义 |
| --- | --- | --- |
| `--command` | 必填 | 远端命令字符串 |
| `-F` / `--config` | 系统默认 | OpenSSH 配置入口 |
| `--stdin` | EOF | 普通文件路径，或 `-` 继承 stdin |
| `--askpass` | 不启用 | 显式指定 OpenSSH 密码/密钥口令辅助程序；不占用远端 stdin |
| `--timeout` | `2m` | 配置解析、等待连接锁、连接和执行的本地总期限；`0` 无限 |
| `--max-output` | `65536` | 每个输出流显示的字节上限；`0` 不截断 |
| `--persist` | `1m` | 控制连接空闲期限；`0` 禁用本次复用，不会永久保持 |
| `--debug` | 关闭 | OpenSSH `-v` 诊断，写入 stderr |

输出保留原始 stdout 和 stderr，没有默认 JSON 或成功装饰。超过上限后继续排空输出，内存不随总输出增长；结束时在 stderr 报告省略的字节数。上限按字节计算，可能截在多字节字符中间；二进制完整传输请使用 `--max-output 0` 或专用文件传输工具。

普通执行默认 `BatchMode=yes`、无 PTY，禁用配置中的 `LocalCommand` 和端口转发，覆盖 `RemoteCommand`、`SessionType`、后台启动和 stdin 设置，以保证执行的是本次显式命令且等待其结果。连接超时设为 10 秒，总期限仍由 `--timeout` 限制。身份、密钥、跳板机、代理和主机密钥策略沿用 OpenSSH 配置。

0.1.1 增加显式的 `--askpass /path/to/helper`：设置 `BatchMode=no`、`NumberOfPasswordPrompts=1`，并通过 `SSH_ASKPASS_REQUIRE=force` 让 OpenSSH 调用该程序取得密码或密钥口令。辅助程序路径可以包含空格；CLI 不读取或保存返回的凭据，也不占用远端 stdin。辅助程序应把凭据写到自己的 stdout、拒绝不认识的提示和主机密钥确认，并避免输出敏感日志。辅助程序卡住时同样受总期限约束。需要人参与的 MFA 或终端问答仍使用原生 SSH。

为复用连接而进行的 `ssh -G` 配置求值**可能执行可信本地配置中的 `Match exec`**，实际 SSH 调用也可能再次求值。因此不应把非幂等操作放进 Match exec；本工具不把外部下载的配置当成可信输入。

### 退出状态

正常结束时原样返回远端退出码；不依据错误文本猜测成功。

| 状态 | 本工具的含义 |
| --- | --- |
| `2` | CLI 参数错误 |
| `124` + 本地诊断 | 本地等待超时 |
| `125` + 本地诊断 | 配置、启动、I/O 等本地错误 |
| `130` + 本地诊断 | 本地收到取消信号 |
| `255` | OpenSSH 错误或远端退出 255，不能仅凭此区分 |

远端也可以返回 2、124、125、130，不能只看数字将其判为本地错误。`sshm:` 诊断属于文本约定，并非可用于识别恶意远端输出的可信协议边界。

超时或取消后，CLI 会终止并回收本次前台 SSH 进程组，报告“远端执行结果未知”。**这不保证远端任务或整个远端进程树已经停止，也不自动重试。**共享控制连接上的其他调用不应被取消。

### `doctor`：本地诊断

```sh
sshm doctor
sshm doctor -F ./ssh-config
```

检查 OpenSSH 路径与版本、配置候选发现、本工具的运行目录和控制 socket。通过 `ssh -O check` 查询本地 socket，不建立远端连接，不执行配置中的 Match exec，也不打印配置中的凭据。发现失效 socket 时只报告；下一次对同一连接的执行会在持锁后回收该失效 socket。

## 交互与文件传输

不重复封装系统工具：

```sh
ssh -tt my-server
scp ./local-file my-server:/remote/path
sftp my-server
```

交互需要 Agent 的执行宿主能够分配 PTY、保持进程并后续读写。会话标识由宿主管理，sshm 不提供 session 服务。只有一次性执行能力的宿主无法通过增加一个参数获得多轮交互。

安装问答、REPL 和设备 CLI 按需使用原生终端；全屏程序还依赖宿主的终端屏幕能力。目录同步使用 rsync 时需确认两端环境和版本条件。远端断线后继续工作的任务使用目标已有的持久化机制，本工具不实现后台任务调度。

## 连接与资源生命周期

短命 CLI 调用 OpenSSH `ControlMaster=auto` / `ControlPersist`，没有自建守护进程。控制 socket 默认存放在 `/tmp/sshm-UID`，目录必须由当前用户拥有且不允许其他用户访问，也不能是符号链接。`SSHM_RUNTIME_DIR` 可指定更短的私有目录；路径长度受 Unix socket 限制。

连接标识包含当前目标的 OpenSSH 有效配置、`SSH_AUTH_SOCK`、认证辅助程序路径和空闲期限。并发首次连接通过文件锁协调；OpenSSH 发布控制 socket 后释放锁，因此远端命令可并行，不为每个 Agent 单独维护连接池。暖连接检查走本地控制 socket，不额外发送远端 `echo ping`。

空闲超时由 OpenSSH 处理，活动命令不因空闲期限而被中断。没有应用级定时保活或自动重试。后台 SSH master 是有期限的预期资源；锁文件是每个连接配置一个的小文件，不是活进程，运行期间不能随意删除锁文件。

连接复用继承已经认证的状态。密钥内容、known_hosts 或跳板别名内部配置改变时，不保证现有连接立即重新认证/改道；需要立即使用新状态时执行 `--persist 0`，或等待现有连接空闲退出。长期不再使用的运行目录可在确认无活动控制连接、无并发调用后手动移除。

SIGINT、SIGTERM、SIGHUP 会触发前台进程回收；任何程序都无法捕获 SIGKILL。macOS 上如果直接强杀 CLI，前台子进程不保证自动回收；远端失联检测也取决于 OpenSSH 的连接配置。第一版不引入进程监控守护服务来掩盖这个边界。

## 验证与开发

运行时仅使用 Go 标准库。测试需要 Go；真实 SSH 集成测试另需 Python 3 和 Docker。

```sh
make check
make integration
```

使用 SSH Manager TOML 测试真实服务器（显式选择目标，使用 Python 3.11+）：

```sh
python3 tests/live.py \
  --toml "$HOME/.ssh/sshm.toml" \
  --hosts macmini tencent \
  --report /tmp/sshm-live-results.json
```

这不是 `make integration` 的默认步骤。真实测试会登录指定服务器，在唯一的 `/tmp/sshm-live-*` 目录内创建、传输和同步测试文件，完成后校验标记并清理；应在用户允许测试这些目标的范围内运行。TOML 只由测试适配器读取并生成临时 OpenSSH 配置，CLI 的 `-F` 仍接受 OpenSSH 配置，不直接接受 TOML。密码由测试 askpass 辅助程序从原文件读取，不复制到临时配置或命令参数。认证使用现有 `known_hosts` 并严格校验，不自动信任新主机。

真实测试中的 1 MiB 输出默认允许 90 秒，可用 `--transfer-timeout` 调整；重测单项使用 `--cases output-limits complete-output-to-file`。网络较慢时不要将测试程序自己的短等待期限误判成远端命令失败。测试程序超时先发送 SIGTERM，让 CLI 回收子进程。

集成测试生成临时密钥，启动只发布到 `127.0.0.1` 的一次性 SSH 容器，使用独立配置和已固定的主机公钥，不读取开发者的服务器密钥、不连接真实服务器。测试结束后关闭自己的控制连接并删除容器和临时文件。测试镜像保留在本机供下次复用。镜像中的 `StrictModes no` 仅用于跨 macOS/Linux 的测试卷属主兼容，不用于客户端设置。

测试覆盖真实命令结果、stdin、20 MB 输出、并发冷启动连接复用、空闲退出、超时与并行任务隔离、前台进程回收，以及原生 SSH 的 PTY 问答。另有候选发现、引号、参数、锁和连接标识单元测试。

已验证 macOS 客户端到真实 macOS / Linux 服务器，含密码认证和原生终端交互；Linux 客户端在隔离服务器上验证密钥、密码、stdin 和复用。记录见 [初版隔离验证](docs/validation.md) 与 [真实服务器验证](docs/live-validation.md)。Windows、BSD 和网络设备的服务端兼容性依赖它们提供的标准 SSH 执行能力，尚未做设备实测；本地 Windows 客户端暂不提供。

源码的设计依据：[OpenSSH 客户端](https://man.openbsd.org/ssh)、[SSH 配置](https://man.openbsd.org/ssh_config)、[Go 进程管理](https://pkg.go.dev/os/exec)、[Agent Skills](https://agentskills.io/specification)。
