# 0.1.0 验证记录

> 这是 0.1.x 的历史记录；当前 0.2.0 已改为内置 Go SSH。最新行为与验证见 [0.2.0 验证](v2-validation.md)。

验证日期：2026-09-13。使用隔离配置和随机生成的测试密钥，没有连接用户的实际服务器。

## 环境

- macOS arm64：Go 1.24.2，系统 OpenSSH 10.3p1。
- Linux amd64：Docker 中的 Go 1.24.13，运行单元测试与 race 检查。
- SSH 服务端：一次性 Debian trixie / OpenSSH 测试容器，端口仅发布到 127.0.0.1。
- Linux arm64：在测试容器内执行交叉编译后的真实 CLI，连接测试 SSH 服务端。

## 结果

| 检查 | 结果 |
| --- | --- |
| 11 项 Go 单元测试 | macOS / Linux 均通过，启用 race 检查 |
| `go vet` 与格式检查 | 通过 |
| 16 项真实 SSH 集成测试 | 全部通过，最后一次完整运行 10.644 秒 |
| Skill frontmatter / 内容结构验证 | skill-creator 的 quick_validate.py 通过 |
| 源码安装与发布包安装 | 通过；使用临时前缀，不安装到用户全局目录 |
| 含空格路径、重复安装、Skill 复制 | 通过 |
| macOS / Linux × arm64 / amd64 发布包 | 构建成功，四份 SHA-256 校验通过 |
| 发布包内容检查 | 仅二进制、README、安装器和 Skill；不包含 macOS AppleDouble 文件 |
| 收尾清理 | 测试 SSH 进程无残留，测试 SSH 容器已删除 |

## 已验证行为

1. stdout、stderr 原样分离，远端退出码 17 / 125 保留，255 明确报告歧义。
2. stdin 默认 EOF；管道、普通文件、中文、NUL 和二进制内容按字节传输。
3. 20 MB 输出可完整排空，显示上限和省略字节数正确；无限制输出可重定向保存。
4. 8 个并发冷调用只建立一条真实 SSH 连接，远端命令没有被连接锁串行化。
5. 顺序调用复用同一个控制进程；空闲后 socket 自动消失。
6. 活动命令运行超过空闲期限仍可完成。
7. 超时请求返回 124 及结果未知说明，不影响同连接上的兄弟请求。
8. SIGTERM 回收本次前台 SSH 子进程。
9. 非交互执行不启动配置中的 LocalCommand、LocalForward 或其他 RemoteCommand。
10. doctor 只查询本地控制 socket，不新增远端认证连接。
11. 原生 SSH PTY 可连续执行命令、保留目录、等待输入再回答并正常退出。
12. 主机密钥不匹配时拒绝执行且不弹交互提示。
13. ProxyJump 使用原生配置完成跳板连接。
14. 下游输出管道提前关闭时，CLI 回收前台 SSH 并报告输出写入失败。
15. 大输出下采样 CLI RSS，不随输出总量线性增长。
16. Linux 交叉编译二进制可真实执行命令、复用连接并通过 doctor 查询控制 socket。

单元测试另覆盖 Include、候选清单不完整提示、循环引用、引号处理、不执行 Match exec、参数原样传递、输出排空、可取消的锁、运行目录权限、身份/TTL 隔离以及 FIFO 不阻塞。

## 性能观察

- 最后一次完整运行中，8 个并发调用完成耗时 0.591 秒；每条远端命令包含 `sleep 0.4`，服务端日志只记录一次成功认证。
- 排空 20 MB 输出时，本地 CLI 的采样峰值 RSS 为 5.5 MiB。该值仅包含 CLI，不包括 SSH 控制进程、宿主 Agent 或 Docker。
- 这是本机回环环境中的一次观察，不是公网延迟、所有设备性能或长期无泄漏的保证。

## 复现

```sh
make check
make integration
make build
make dist
python3 tests/install.py
```

安装测试会使用并删除自己的临时目录；集成测试会删除自己的 SSH 容器和密钥。`sshm-test-server:local` 镜像保留在本机供再次测试使用。

## 明确边界

- 未实测 Windows、BSD 或网络设备服务端，不能将 Linux 结果视为这些环境的验收。
- 本地 Windows 客户端暂不提供；darwin/amd64 与 linux/amd64 发布包完成构建，真实端到端客户端测试使用 macOS arm64 和 Linux arm64。
- 未构建终端屏幕仿真器，交互能力来自 Agent 宿主与原生 SSH。
- 未承诺强杀 CLI（SIGKILL）后 macOS 前台子进程自动回收，或本地取消后远端整个进程树已停止。
- 主机密钥检查沿用 OpenSSH 配置；工具没有增加独立权限边界，也不会把文本错误分类当成可靠的远端执行证明。
