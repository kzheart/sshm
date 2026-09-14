# 本机 MCP → CLI + Skill 安装记录

> 这是 0.1.x 的历史记录；当前 0.2.0 已改为内置 Go SSH。最新行为与验证见 [0.2.0 验证](v2-validation.md)。

2026-09-14，按用户要求将 Cursor、Codex、Claude Code 的 SSH Manager MCP 替换为 sshm 0.1.1，并将 SSH 配置集中到 `~/.ssh`。

## 已安装的文件

| 路径 | 用途 |
| --- | --- |
| `~/.local/bin/sshm` | 独立 CLI 二进制，当前 PATH 可直接调用 |
| `~/.ssh/sshm.conf` | 11 个迁移的 SSH 连接条目，包含密钥与 ProxyJump 配置 |
| `~/.ssh/config` | 开头 Include 上述文件，原有 7 个明确别名行为保持不变 |
| `~/.ssh/sshm.toml` | 原 SSH Manager TOML 原样迁入，权限 600，作为认证辅助程序的凭据来源 |
| `~/.local/libexec/sshm/askpass` | 可选 Python 3.11+ 认证辅助程序，不持有后台进程 |
| `~/.agents/skills/sshm` | Codex 与 Cursor 共用的 Skill，包含本机认证与原默认目录说明 |
| `~/.claude/skills/sshm` | 指向共享 Skill 的符号链接 |

CLI 的 `-F` 仍接受标准 SSH config。连接字段迁移后以 `sshm.conf` 为准；原 TOML 的地址、账号和密钥路径用于匹配认证提示，需要在变更这些字段时同步维护。默认工作目录作为 Skill 参考信息保留，由 Agent 按任务显式切换，CLI 不隐式注入命令。

## 已卸载的接入

- 移除 `~/.codex/config.toml`、`~/.cursor/mcp.json`、`~/.claude.json` 中的 `ssh-manager` 注册。
- 验证三个配置在语义上仅少了该 MCP，其他内容保持不变。
- 卸载 `/opt/homebrew` 前缀下的全局 npm 包 `mcp-ssh-manager`。
- 对命令行严格匹配旧 SSH MCP 入口的 8 个进程发送 SIGTERM，确认无剩余匹配进程。
- 旧 MCP 源码仓库保留，未删除用户代码；不再由上述配置启动。

修改前的配置备份保存在 `~/.local/state/sshm/migration-20260914-oq5yuuk2`，目录权限 700，文件权限 600。原 `~/.codex/ssh-config.toml` 已移走；备份是历史副本，不参与认证。

## 验证

- 11 个迁移别名通过 OpenSSH 有效配置解析核对，包含端口、账号、密钥路径和跳板关系；未连接其他服务器。
- `macmini` 与 `tencent` 使用已安装 CLI 和正式认证辅助程序登录成功，二进制 stdin 往返一致，原生 SSH 使用同一辅助程序也成功。
- TOML 移到 `~/.ssh` 后再次验证 `macmini` 登录成功。
- Mac mini 新 IP 的 ED25519 公钥与既有可信记录一致，核对后将该新地址记录追加到 `known_hosts`，保留原记录。
- 六组独立认证辅助程序测试通过：跳板与目标分别取密码、交互密码/私钥口令、未知目标/账号/私钥/验证码拒绝、主机确认拒绝、凭据冲突拒绝、配置权限及格式检查。
- Skill 格式验证通过。通过 Codex app-server `skills/list` 实际发现恰好一个启用的用户级 `sshm` Skill，无模型调用。
- Cursor 和 Claude Code 按本机发现路径安装；未通过它们的模型会话执行额外任务。已有客户端会话可能缓存旧 MCP，重新加载窗口或重开会话后使用新接入。

## 使用

```sh
sshm hosts
sshm exec macmini --askpass "$HOME/.local/libexec/sshm/askpass" --command 'uname -s'
```

Skill 已写明认证辅助程序路径，Agent 使用时无需用户重复说明。CLI 的普通 `exec` 默认保持 BatchMode；密码认证仍需显式指定 `--askpass`。

本机安装路径依据：[Codex Skill 发现](https://learn.chatgpt.com/docs/build-skills)、[Cursor Skill 目录](https://cursor.com/docs/skills)、[Claude Code 个人 Skill 与符号链接](https://code.claude.com/docs/en/skills)。
