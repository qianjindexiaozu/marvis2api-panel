# 公开与发布检查

目标仓库：<https://github.com/qianjindexiaozu/marvis2api-panel>

[返回 README](../README.md)

## 首次公开前

- [ ] 确认代码、文档和第三方材料的许可；MIT 不授予官方客户端或上游服务的使用权。
- [ ] `.prepare/`、账号数据、`.env`、本地配置、日志、备份和官方客户端文件没有进入暂存区。
- [ ] 审查完整 Git 历史、旧分支、旧 release、镜像及附件。删除当前文件不等于删除历史凭据。
- [ ] 确认没有内置上游钥匙，没有在 CI 或构建参数中注入真实数据。
- [ ] 使用假值跑测试；真实账号功能另行验证，不把真实凭证用作测试夹具。
- [ ] 阅读 [SECURITY.md](../SECURITY.md)，启用 GitHub 私有漏洞报告。

如果是没有 Git 历史的本地目录，可以先初始化，再审查暂存内容：

```bash
git init -b main
git add .
git status --short
git diff --cached --stat
```

提交前逐项检查暂存内容；可以使用 `gitleaks git --redact` 扫描已有提交历史。该命令需要另行安装 Gitleaks，且不能代替人工检查未知格式的数据文件。

确认后再提交并配置远端：

```bash
git commit -m "Initial open-source release"
git remote add origin https://github.com/qianjindexiaozu/marvis2api-panel.git
git push -u origin main
```

若远端已有提交，先克隆远端并合并整理后的源码，不要用强制推送直接覆盖。不要把已泄漏钥匙的旧历史原样搬入公开仓库。是否需要撤销或更新凭据，应按凭据类型与上游能力处理。

## 本地检查

```bash
go test -race ./...
go vet ./...
for script in scripts/*.sh; do bash -n "$script"; done
sh -n docker/entrypoint.sh
docker build -t marvis2api-panel:local .
```

必须使用 GitHub 实际运行结果确认 CI 可用。本地测试通过不等于已经验证 GitHub Actions、GHCR 权限、发布可见性或所有真实账号请求。

## CI 行为

`.github/workflows/ci.yml` 统一管理：

| 事件 | 行为 |
|---|---|
| Pull request | 扫描 Git 历史中的秘密，格式 / vet / race 测试，四个平台的交叉编译和双架构 Docker 构建；不推送镜像 |
| 推送 `main` | 检查通过后推送 GHCR 的 `latest` 与提交 SHA 标签 |
| 推送 `v*` 标签 | 检查通过后推送版本镜像，并创建包含二进制、MIT 许可证和 SHA-256 校验文件的 GitHub Release |
| 手工触发 | 检查与构建，上传二进制 artifact；普通分支不推送镜像或创建 Release |

原生构建目标为 Linux / macOS 的 `amd64`、`arm64`；暂不发布 Windows `.exe`。Docker 目标为 Linux `amd64` / `arm64`。

工作流默认只有仓库读取权限，只有镜像任务申请 `packages: write`，Release 任务申请 `contents: write`。标准部署不需要给 CI 添加真实钥匙、账号 token 或 prepare 文件。

## GHCR

镜像地址为：

```text
ghcr.io/qianjindexiaozu/marvis2api-panel
```

首次发布后检查包与仓库的关联、访问权限和可见性。GitHub 仓库公开不保证已有 GHCR 包自动公开；需要在包设置中确认。

## 版本发布

选择实际版本号，例如：

```bash
git tag v0.1.0
git push origin v0.1.0
```

这会触发真实发布操作，不要为了测试随意推送版本标签。示例版本号不代表已经发布。

带预发布后缀的标签（例如 `v0.1.0-alpha.1`）会自动标记为 GitHub 预发布。首次实验版本采用预发布标签；完整真实账号验证完成前，不宣称生产稳定。

发布后检查：

- [ ] GitHub Actions 全部成功，Release 附件包含四个二进制、`LICENSE` 与 `checksums.txt`。
- [ ] GHCR 版本镜像能被匿名拉取，具有 Linux 两种架构。
- [ ] 新镜像中没有 prepare 文件、账号数据或真实签名值；无 prepare 文件时应拒绝启动。
- [ ] 从干净数据卷部署并测试实际账号登录、配额、token 刷新与流式聊天。
- [ ] 文档、镜像标签、Release 版本一致；用户知道首次准备仍依赖官方客户端数据。
