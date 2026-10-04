# marvis2api-panel

[English](README.en.md) · [部署说明](docs/DEPLOYMENT.md) · [贡献指南](CONTRIBUTING.md)

[![CI](https://github.com/qianjindexiaozu/marvis2api-panel/actions/workflows/ci.yml/badge.svg)](https://github.com/qianjindexiaozu/marvis2api-panel/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

把腾讯 Marvis 账号转成 OpenAI 兼容 API 的自托管网关，带 Web 管理面板。支持多个微信 / QQ 账号、扫码登录、API Key 管理、配额查询、token 刷新和流式对话。

**不是腾讯官方产品，也未获得腾讯背书。** 协议来自对桌面客户端的观察，没有公开规范，上游更新可能导致失效或账号受限。请只使用你有权使用的账号与客户端，并遵守上游条款及当地法律。

## 使用条件

- Docker Engine / Docker Desktop，以及 Docker Compose v2 或更新版本。
- 宿主机有 Bash 和 Python 3；准备脚本只使用 Python 标准库。
- 首次准备数据需要官方客户端文件和已经注册的设备号。目前支持在 macOS 上自动读取；Linux 可以导入已有文件。

源码、二进制和镜像不内置上游签名钥匙，也不包含账号凭证。**这不是一个能在空白 Linux 环境里自动注册设备、零配置使用的项目。** 准备好数据后，Docker 运行不需要 App 本体。

## 快速开始

### macOS

先安装并运行官方 Marvis，完成设备注册：

```bash
git clone https://github.com/qianjindexiaozu/marvis2api-panel.git
cd marvis2api-panel
bash scripts/prepare.sh
docker compose up -d --build
```

### Linux

先克隆仓库，将你在 Mac 上准备的私有文件安全传到 Linux，再执行：

```bash
bash scripts/prepare.sh --import /private/path/marvis.json
docker compose up -d --build
```

两种方式都生成 `.prepare/marvis.json`，容器只读挂载它。脚本不打印钥匙或设备号，也不读取账号 token、Cookie 或钥匙串。详细的文件来源、权限与更新方法见[部署说明](docs/DEPLOYMENT.md)。

打开 <http://127.0.0.1:18620/panel/>，默认密码为 `marvis`：

1. 立即修改面板密码。
2. 在「账号」里扫码添加微信或 QQ，也可手工添加你自己的凭证。
3. 在「密钥」里生成 API Key。面板密码不能代替 API Key 调用 `/v1`。

默认仅将端口绑定到宿主机 `127.0.0.1`。不要直接把默认密码的面板开放到公网。`.env` 和宿主机 `config.json` 都不是必需的。

## 调用 API

用 API Key 查询模型，选择返回的模型 id；`marvis` 不是有效的上游模型 id：

```bash
curl http://127.0.0.1:18620/v1/models \
  -H 'Authorization: Bearer <API_KEY>'

curl -N http://127.0.0.1:18620/v1/chat/completions \
  -H 'Authorization: Bearer <API_KEY>' \
  -H 'Content-Type: application/json' \
  -d '{"model":"<MODEL_ID>","stream":true,"messages":[{"role":"user","content":"你好"}]}'
```

可选请求头：

| 请求头 | 作用 |
|---|---|
| `X-Marvis-Account` | 指定账号 id |
| `X-Marvis-Session` | 固定同一段对话的会话 id |

不指定账号时，新对话分配给剩余额度最多的使用中账号，同一段对话保持使用该账号。客户端若每次只发最新一条消息，应提供稳定的会话 id，否则可能被当成新对话。

`GET /healthz` 表示业务可用状态。未添加账号时返回 `503` 属正常情况；Docker 探活使用面板页面检查进程是否可用。

## 配置与数据

| 项目 | 保存位置 |
|---|---|
| 上游签名钥匙、设备号 | 宿主机 `.prepare/marvis.json`，容器内 `/run/secrets/marvis.json` |
| 运行配置 | 数据卷内 `/app/data/config.json`，首次从 `config.example.json` 复制 |
| 账号、面板密码、API Key、状态、用量 | Compose 项目数据卷 `marvis2api-data` |

准备文件和数据卷是两份独立数据，都不能提交或公开。配置文件的旧 `api_key` 字段不再作为网关凭证，启动时会删除。

可以复制 `.env.example` 为 `.env`，按需设置：

| 变量 | 默认值 / 用途 |
|---|---|
| `MV2A_BIND` | `127.0.0.1`，宿主机端口绑定地址 |
| `MV2A_PORT` | `18620`，宿主机端口 |
| `MV2A_PREPARE_FILE` | `./.prepare/marvis.json`，宿主机准备文件路径 |
| `MV2A_IMAGE` | `marvis2api-panel:local`，可替换为已发布的 GHCR 镜像 |

不要使用 `chmod 777` 处理准备文件或数据目录。密钥权限、远程访问、备份、发布镜像和常见错误见[部署说明](docs/DEPLOYMENT.md)。

## 支持范围

- Docker：Linux `amd64` / `arm64`，聊天走上游 HTTP，不运行官方内核。
- 原生程序：Linux / macOS `amd64` / `arm64`。本机编译要求 Go 1.22+，建议使用当前稳定版。
- macOS 官方内核：可选实验功能，需要官方 Host / Agent 组件和 Xcode Command Line Tools。官方组件为 macOS arm64，不能放进 Linux 容器运行。
- 暂不提供 Windows 原生程序或 PowerShell 初始化脚本。
- 不保证微信昵称可用；QQ 扫码有时可从登录页取得昵称。

## 开发与发布

```bash
go test -race ./...
go vet ./...
```

前端由 `internal/panel/index.html` 和 `app.js` 内嵌，没有单独的前端构建，也没有外部 Go 模块依赖。

- [贡献指南](CONTRIBUTING.md)：目录结构、测试、构建。
- [安全说明](SECURITY.md)：私有数据和漏洞报告。
- [发布检查](docs/PUBLISHING.md)：首次公开、GHCR 和版本发布。

## 许可证

[MIT](LICENSE)。许可证仅覆盖本仓库代码，不授予第三方客户端、账号、服务或签名材料的使用权。
