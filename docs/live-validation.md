# 0.1.1 真实服务器验证

> 这是 0.1.x 的历史记录；当前 0.2.0 已改为内置 Go SSH。最新行为与验证见 [0.2.0 验证](v2-validation.md)。

日期：2026-09-13。根据用户授权，使用 `~/.codex/ssh-config.toml` 中的 `macmini`、`tencent` 两个条目测试。

## 范围与最终结果

| 目标 | 实测远端 | 认证方式 | 最终场景结果 |
| --- | --- | --- | --- |
| macmini | macOS / Darwin | 原 TOML 中的密码 | 19 / 19 通过 |
| tencent | Linux | 原 TOML 中的密码 | 19 / 19 通过（两项大输出在调整测试期限后重测） |

38 是按“目标 + 场景”去重后的数量，不代表全部在首轮通过。首轮 36 项通过、2 项触发测试程序的 30 秒期限；后续对腾讯云执行了 5 项重测，包含认证、临时目录、两项大输出和清理，全部通过。

原始记录：[首轮](live-results.json)、[重测](live-retest-results.json)。记录不包含服务器地址、登录密码或口令。

## 两台机器都实际执行的场景

1. 密码认证与远端系统识别。
2. 创建唯一临时目录并写入本次测试的所有权标记。
3. 原始 stdout/stderr、退出码 0/1/2/17/124/125/130。
4. 中文、多行、单双引号、美元符号、反引号等命令内容。
5. 默认 stdin EOF、管道、含空格文件路径、NUL 与二进制内容。
6. 通过 stdin 发送脚本，并检查脚本退出状态。
7. 同一命令中保留目录和变量、不同 exec 之间不共享 shell 状态。
8. 1 MiB stdout + 8 KiB stderr 的排空和截断计数。
9. 完整 1 MiB 输出写入本地文件。
10. 4 个并发冷调用，共用一个本工具控制 socket。
11. 20 次连续调用，控制 socket 数量保持为 1。
12. 一个调用超时，同连接上的另一个命令继续完成。
13. 短空闲期限到期后，控制 socket 自动消失。
14. 延迟产生的文本输出完整返回。
15. 原生 scp 上传/下载，中文与空格文件名、32 KiB 二进制内容逐字节一致。
16. 原生 sftp 批处理上传/下载。
17. 原生 rsync dry-run 不写文件，正式同步和后续更新结果正确。
18. 原生 SSH PTY 连续输入、保留目录、等待输入后作答、正常退出。
19. 校验所有权标记后删除本次远端临时目录，确认不存在，并关闭本次控制连接。

文件写入仅发生在新建的 `/tmp/sshm-live-*` 目录。没有修改服务器登录设置、密钥、应用文件或服务状态。交互测试显式运行 `sh`，避免把测试指令写入用户交互登录 shell 的历史。

## 根据真实测试补上的能力

两台目标都使用密码登录，0.1.0 的默认 BatchMode 不能直接满足这个场景。0.1.1 新增 `exec --askpass /path/to/helper`，调用 OpenSSH 现有认证辅助机制：

- 用户显式启用；默认仍保持 BatchMode，不弹密码提示。
- OpenSSH 直接读取辅助程序的 stdout，CLI 不接收或保存凭据。
- 远端 stdin 独立，密码认证不消耗脚本/文件输入。
- 不同辅助程序路径不复用同一个认证连接标识。
- 辅助程序执行仍受 CLI 总期限和进程组取消约束。
- 测试适配器直接从原 TOML 按别名读取凭据；TOML 未被复制到项目、临时 OpenSSH 配置或命令参数中。

这没有增加 TOML 运行时解析器、服务器数据库或新的子命令。`-F` 的公开契约仍为 OpenSSH 配置文件。

## 腾讯云大输出的调查

初始测试在外层 Python 程序使用统一的 30 秒期限。该链路实际吞吐较低，1 MiB 输出在这段时间内未完成。

同目标、同密码认证方式、关闭复用的比较观察：

| 数据量 | 原生 ssh | sshm |
| --- | --- | --- |
| 64 KiB | 3.837 秒 | 6.386 秒 |
| 256 KiB | 12.256 秒 | 14.067 秒 |

双方都出现较慢传输，不能把这组结果归因为 sshm 输出缓冲卡死；网络波动和每次重新认证也包含在这些时间内，不是严格基准。

保持数据量不变，仅将大输出测试的 CLI 期限设为 90 秒、外层期限设为 100 秒，重测结果：

- 1 MiB stdout + 8 KiB stderr 排空/截断：49.123 秒，通过。
- 完整 1 MiB 保存到文件：47.848 秒，通过。

同时修正测试程序的超时处理：优先 SIGTERM，让 CLI 回收 SSH 子进程；不再直接使用 `subprocess.run(timeout=...)` 的立即 SIGKILL 路径。

## 其他回归检查

- 原有 16 项隔离真实 SSH 集成测试全部通过。
- 新增密码与 stdin 分离、辅助程序需显式启用/认证连接隔离、辅助程序卡住、加密私钥口令、Linux 密码辅助程序与二进制 stdin 五项测试，均通过。
- 11 项 Go 单元测试（含 race）、go vet、格式检查通过。
- Skill 验证通过；安装器和 0.1.1 四个平台发布包重新验证。
- 对项目进行选定服务器地址、密码和口令的字节匹配检查，无泄露匹配。
- 原 TOML 与原 `known_hosts` 内容校验保持不变；本次 SSH 测试进程无残留。

## 复现

```sh
make build
python3 tests/live.py \
  --toml "$HOME/.codex/ssh-config.toml" \
  --hosts macmini tencent \
  --report /tmp/sshm-live-results.json

# 仅重测大输出，仍会验证登录、创建和清理临时目录。
python3 tests/live.py \
  --toml "$HOME/.codex/ssh-config.toml" \
  --hosts tencent \
  --cases output-limits complete-output-to-file \
  --transfer-timeout 90

# 纯读取的原生 SSH / sshm 吞吐对比。
python3 tests/throughput_probe.py \
  --toml "$HOME/.codex/ssh-config.toml" --host tencent
```

仍未实测 Windows、BSD 或网络设备服务端。原生终端多轮读写已验证，不等于提供全屏终端仿真，也不保证本地取消后整个远端进程树已停止。
