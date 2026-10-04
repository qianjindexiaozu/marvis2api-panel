# 部署说明

[返回 README](../README.md)

## 准备文件

`scripts/prepare.sh` 使用 Bash 和 Python 3，将 `access_key` 与 `qimei36` 写入 `.prepare/marvis.json`。没有内置钥匙，不导出账号 token，不修改官方客户端。

### macOS 自动提取

```bash
bash scripts/prepare.sh
```

签名钥匙按以下顺序查找：

1. `~/Library/Application Support/com.tencent.mac.marvis/OfflinePack/main/current/assets/*.js`
2. `/Applications/Marvis.app/Contents/Resources/offline-pack/main/assets/*.js`

脚本查找 SDK 的 `marvis_client` 属性，不依赖带内容哈希的文件名。若同一来源存在不同钥匙，直接报错，不猜测。

设备号来自 `~/Library/Application Support/com.tencent.mac.marvis/device-info.cache`。需要官方客户端已经完成注册；随机 UUID 或扫码登录的 guid 不能代替它。

### Linux 导入

先通过安全渠道将准备文件传入一个私有目录：

```bash
bash scripts/prepare.sh --import /private/path/marvis.json
```

已有客户端文件时，也可直接提取：

```bash
bash scripts/prepare.sh --client-dir /private/client-data
```

该目录需包含 `OfflinePack/main/current/assets/` 和 `device-info.cache`。可以用 `--app-dir` 指定备用 App 目录，用 `--output` 指定输出文件。Linux 本身不会自动安装 macOS App 或注册设备。

### 权限与更新

默认目录为 `0700`，文件为 `0644`。目录权限阻止其他普通宿主机用户访问；只读单文件挂载允许容器内 UID `10001` 读取。root 和 Docker 管理员仍可访问这些数据，这不是加密存储。

自定义输出目录必须归当前用户所有，并禁止其他用户访问。不要把准备文件单独复制到共享目录、提交 Git 或上传 issue；建议自定义文件也放在仓库之外。

生成过程采用临时文件和原子替换。提取失败不会覆盖已有文件。重新生成后需重建容器实例：

```bash
bash scripts/prepare.sh
docker compose up -d --force-recreate
```

程序只在启动时读取 prepare 数据，不能热更新。更新失败时不会退回内置钥匙。

## Docker Compose

```bash
docker compose up -d --build
docker compose ps
docker compose logs --tail 50
```

默认只映射 `127.0.0.1:18620`，以非 root 用户运行。容器只挂载 prepare 文件与项目数据卷，不挂载 App、Cookie 或客户端目录。

面板为 <http://127.0.0.1:18620/panel/>。默认密码 `marvis`，首次登录后立即修改，并为 API 调用单独生成密钥。

`.env` 可从 `.env.example` 复制。`MV2A_PORT` 改宿主机端口，`MV2A_PREPARE_FILE` 改准备文件路径，`MV2A_IMAGE` 改镜像名。容器内 `MV2A_PREPARE_FILE` 固定为 `/run/secrets/marvis.json`。

### 使用发布镜像

首次 GitHub Actions 发布成功，且 GHCR 包设为公开之后，才可以拉取：

```bash
export MV2A_IMAGE=ghcr.io/qianjindexiaozu/marvis2api-panel:latest
docker compose pull
docker compose up -d --no-build
```

生产部署建议使用具体版本，例如 `:0.1.0`，不要假定示例版本已经发布。启动前仍需先运行 prepare 脚本。

### 远程访问

优先保持端口只绑定本机，通过 SSH 转发访问：

```bash
ssh -L 18620:127.0.0.1:18620 <user>@<server>
```

需要对外开放时，先修改默认密码、配置 HTTPS 反向代理和访问控制，再按需设置 `MV2A_BIND`。上游 token 和 API Key 不应通过公网明文 HTTP 传输。

### 停止、删除与备份

```bash
docker compose down
```

上面的命令保留数据卷和 prepare 文件。`docker compose down --volumes` 会删除项目数据卷里的账号、密码、API Key、配置、状态和用量，无法恢复，除非你有备份。

prepare 文件不在数据卷里，需要独立备份。数据卷备份含完整账号凭证和 API Key，应限制访问并加密保存；不要上传公共网盘或 GitHub。

## 本机程序

需要 Go 1.22+，推荐当前稳定版。先准备数据，再构建运行：

```bash
go build -o marvis2api ./cmd/server
MV2A_LISTEN=127.0.0.1:18620 ./marvis2api -config config.json
```

原生运行默认从当前工作目录读取 `.prepare/marvis.json`；可以用 `MV2A_PREPARE_FILE` 指定路径。账号等数据默认写到本机 `data/`，不是 Docker 数据卷。

Mac 上若已安装官方 Host / Agent 组件，程序可尝试为账号启动隔离内核。需要 Xcode Command Line Tools，使用独立目录、端口和 `DYLD_INSERT_LIBRARIES`；不会修改官方二进制签名。此路径依赖 macOS arm64 官方组件，不适用于 Linux 容器，也不要将隔离目录指向正在运行的官方 App 数据。

`scripts/marvis-token.sh` 是另一个仅 macOS 的手工诊断工具，会访问钥匙串并把账号 token 输出到终端。它不是 prepare 流程的一部分；不要把输出贴到 issue、日志或聊天记录。

## 常见错误

| 情况 | 检查方式 |
|---|---|
| prepare 文件不存在 | 先运行 `prepare.sh`；检查 `MV2A_PREPARE_FILE` 路径 |
| prepare 文件格式无效 | 重新提取或导入；不能用随机 UUID 代替已注册设备号 |
| 输出目录权限错误 | 使用归当前用户所有的私有目录；不要 `chmod 777` |
| 容器读取失败 | 保持目录 `0700`、文件 `0644`，使用单文件只读挂载；不要挂整个私有目录给不同 UID |
| 未添加账号，`/healthz` 返回 `503` | 属业务未就绪状态，先在面板添加账号；Docker 面板探活仍可正常 |
| 对话返回 `4100404` | 核对客户端已注册的设备号；文件格式有效不代表上游一定接受迁移后的设备信息 |
| 模型无效 | 用 `/v1/models` 返回的 id，不要使用 `marvis` |
| 更新客户端后失效 | 上游可能改钥匙或协议；重新准备文件并重建容器，必要时适配代码 |
