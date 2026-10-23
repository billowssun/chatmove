# chatsync

把 Claude Code 和 Codex 的本地聊天记录增量同步到 WebDAV（坚果云、Nextcloud、群晖、Alist 等），也可以一键导出成 zip。单个 exe，无依赖。

## 桌面版：对话搬家

`对话搬家.exe` 是图形界面版，只做本地导入和导出，不用 WebDAV：

1. 打开后选 Claude 或 ChatGPT。
2. 选导出或导入。
3. 选择保存位置，或者要导入的 .zip，剩下的自动完成。

导入是合并：新文件直接加入；已有的文件只在压缩包里那份更长时才替换，旧文件先备份到 `chatmove-backup/`；10 分钟内还在写入的文件不碰。导入 Codex 前需要先退出 Codex，导入后会自动 reindex。

构建命令：

```
go build -tags desktop,production -ldflags "-s -w -H windowsgui" -o 对话搬家.exe ./cmd/chatmove
go build -ldflags "-s -w" -o chatsync.exe ./cmd/chatsync
```

核心逻辑在 `core/`。用本机真实记录跑一遍导出、导入的往返测试（只读取源目录）：

```
CHATMOVE_REAL_TEST=1 go test ./core -run TestRealRoundTrip -v
```

产品网页在 `website/index.html`，朋友圈文案在 `朋友圈文案.md`。

## 快速开始（命令行 + WebDAV）

```
chatsync init                 # 在 exe 旁边生成 chatsync.json
# 编辑 chatsync.json：webdav_url / username / password（坚果云用"应用密码"）
chatsync sync --dry-run       # 先看会做什么
chatsync sync                 # 双向同步
```

其他命令：

| 命令 | 作用 |
|---|---|
| `chatsync push` / `pull` | 只上传 / 只下载 |
| `chatsync export [x.zip]` | 打包全部记录为 zip |
| `chatsync codex-reindex` | 手动让 Codex 收录拉下来的会话并恢复标题（平时 pull 后会自动执行） |
| `chatsync schedule --every 30` | 注册 Windows 计划任务，每 30 分钟同步一次，日志写到 `chatsync.log` |
| `chatsync schedule --remove` | 删除计划任务 |

密码也可以不写进文件，改用环境变量 `CHATSYNC_PASSWORD`。

## 同步什么

| 来源 | 本地 | 远端 |
|---|---|---|
| Claude Code | `~/.claude/projects/**` | `<remote_dir>/files/claude/projects/**` |
| Codex | `~/.codex/sessions/**`、`~/.codex/archived_sessions/**` | `<remote_dir>/files/codex/...` |
| Codex 会话标题索引 | `~/.codex/session_index.jsonl` | 按 id 逐行合并 |

**不会同步**：`auth.json`、`secrets/`、各种 sqlite、配置文件。

## 规则

- **增量**：远端 `manifest.json` 记录每个文件的 sha256，本地 `chatsync.state.json` 记录上次同步状态，只传有变化的文件。
- **不删除**：本地删掉的会话不会从远端删除，也不会删别的机器上的。
- **正在使用的文件**：本地文件最近 `active_minutes` 分钟内被修改过，就不会被远端版本覆盖，留到下次。
- **冲突**：两边都改了同一个会话时，保留较大的一份（会话文件只追加），另一份存到 `conflicts/<时间>/`。
- **Codex 归档/压缩**：同一个 rollout 在本地已经以 `.jsonl`、`.jsonl.zst` 或归档形式存在时，不会再下载另一个变体。

## Codex reindex

Codex 的会话列表和标题存在它自己的 `state_5.sqlite` 里，chatsync 不直接改这个文件。拉下 Codex 会话后，chatsync 会启动一次 `codex app-server`，让 Codex 自己完成两件事：

1. 列一遍全部会话（包括已归档的）。这会让 Codex 扫描 rollout，把数据库里还没有的会话补进去。
2. 按 `session_index.jsonl` 里的名字，对标题不一致的会话调用 `thread/name/set`，恢复改过的标题。

需要注意：

- **Codex 开着时跳过。** 只要有 `codex.exe` 在运行（桌面版、CLI、VS Code 插件都算），就跳过这一步，并在 `chatsync.state.json` 里留下标记，下次同步时再试。
- **手动执行：** `chatsync codex-reindex`。
- **`--force`：** 只在那个正在运行的 Codex 用的是另一个 `CODEX_HOME` 时才用。
- **关闭：** 在配置里写 `"codex_reindex": false`。
- **找不到 codex 时：** 如果 `codex` 不在 PATH 里，用 `"codex_command"` 指定完整路径。
- **Guardian 审查记录不会出现在列表里。** 它们是 Codex 内部的子代理会话，Codex 本身也不显示，但文件照常同步。

## 路径映射

Claude Code 按项目的绝对路径给文件夹命名。两台电脑上同一个项目路径不同时，在 `path_map` 里写明：

```json
"path_map": [{ "remote": "D:\\projects\\my-app", "local": "E:\\work\\my-app" }]
```

`remote` 是推送那台机器上的路径，`local` 是本机路径。两个方向都会自动换算。

## 已知限制

- 置顶、分组、项目归属只存在 Codex 的数据库里，不会同步。
- 计划任务运行时会短暂弹出控制台窗口。
- 两台机器同时同步时，manifest 会先重新读取再合并写入，但没有加锁。
