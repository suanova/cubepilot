# 镜像切换到 harbor.isuanova.com 设计

日期: 2026-08-26
状态: 待评审

## Goal

当前 repo 的容器镜像引用全部走公网仓库。目标:

1. **需要 pull 的镜像**(Dockerfile 基础镜像 / CI pre-pull)一律改用 `harbor.isuanova.com/library`。
2. **构建产生的镜像**(cubepilot-openclaw / operator / api / web)push 到 `harbor.isuanova.com/cubestack`。

## 审计结果(现状)

### 从公网 pull 的 5 个基础镜像

| 公网镜像 | 使用位置 | harbor library 现状 |
|---|---|---|
| `docker.io/library/golang:1.26-bookworm` | openclaw/operator/api 三个 Dockerfile 的 build 阶段;CI pre-pull | 有 `golang:1.26.1-alpine`、`latest`,无 bookworm |
| `docker.io/library/node:24-bookworm-slim` | operator/api 运行时基座(提供 `node` 用户,配合 `fsGroup:1000`);CI pre-pull | 无 |
| `docker.io/library/node:22-alpine` | web build 阶段;CI pre-pull | 无 |
| `docker.io/library/nginx:1.27-alpine` | web 运行时基座;CI pre-pull | 无 |
| `ghcr.io/openclaw/openclaw:2026.6.33` | openclaw-image.Dockerfile 运行时基座;CI pre-pull | 全 harbor 无 |

### 本地构建、待 push 到 cubestack 的 4 个镜像

`cubepilot-openclaw` / `cubepilot-operator` / `cubepilot-api` / `cubepilot-web`
当前只在 `scripts/setup.sh` 与 `Makefile` 中打 `:local` tag 并 `kind load`,无任何 push。

### 必选 vs 可替换(已与用户确认)

- **golang build 阶段**: 可替换(有 `1.26.1-alpine`),但决定**镜像 `1.26-bookworm` 原 tag**,保持零行为变更。
- **node:24-bookworm-slim**(operator/api 运行时): 不可直接替换 —— `api.yaml` 依赖 `fsGroup:1000` + `node` 用户;换成 debian 需要改用户/fsGroup,风险大于收益。**镜像**。
- **node:22-alpine / nginx:1.27-alpine / openclaw**: 必须镜像(前端构建、静态服务、agent 运行时均无可替代的 library 镜像)。

## 设计

### A. 补齐基础镜像到 `harbor.isuanova.com/library`

保持**原 tag 完全不变**,仅重定向仓库地址(镜像公网 pull → 重 tag → push)。推荐用 buildx 保留多架构 manifest list:

```bash
docker login harbor.isuanova.com   # 账号需对 library 有 Developer/ProjectAdmin 写权限

for img in golang:1.26-bookworm node:24-bookworm-slim node:22-alpine nginx:1.27-alpine; do
  docker buildx imagetools create -t harbor.isuanova.com/library/$img docker.io/library/$img
done
docker buildx imagetools create -t harbor.isuanova.com/library/openclaw:2026.6.33 \
  ghcr.io/openclaw/openclaw:2026.6.33
```

> 若 buildx 不可用,退化为 `docker pull` + `docker tag` + `docker push`(仅当前平台架构)。

镜像后结果:

| 公网源 | → library 目标 |
|---|---|
| `golang:1.26-bookworm` | `harbor.isuanova.com/library/golang:1.26-bookworm` |
| `node:24-bookworm-slim` | `harbor.isuanova.com/library/node:24-bookworm-slim` |
| `node:22-alpine` | `harbor.isuanova.com/library/node:22-alpine` |
| `nginx:1.27-alpine` | `harbor.isuanova.com/library/nginx:1.27-alpine` |
| `openclaw:2026.6.33` | `harbor.isuanova.com/library/openclaw:2026.6.33` |

### B. Dockerfile / CI 引用切换(纯文件改动)

统一把基础镜像改为 `harbor.isuanova.com/library/<name>:<tag>`:

- `deploy/openclaw-image.Dockerfile`:
  - `FROM golang:1.26-bookworm AS supervisor-build` → `FROM harbor.isuanova.com/library/golang:1.26-bookworm AS supervisor-build`
  - `FROM ghcr.io/openclaw/openclaw:${OPENCLAW_IMAGE_TAG}` → `FROM harbor.isuanova.com/library/openclaw:${OPENCLAW_IMAGE_TAG}`
  - 同步更新头部注释(原注释提到 ghcr.io 与 tag 校验)。
- `deploy/operator-image.Dockerfile`:
  - `FROM golang:1.26-bookworm AS build` → library/golang:1.26-bookworm
  - `FROM node:24-bookworm-slim` → library/node:24-bookworm-slim
- `deploy/api-image.Dockerfile`: 同上两处。
- `web/Dockerfile`:
  - `FROM node:22-alpine AS build` → library/node:22-alpine
  - `FROM nginx:1.27-alpine` → library/nginx:1.27-alpine
  - 同步更新头部构建命令注释。
- `.github/workflows/e2e.yaml` Pre-pull 步骤: 5 条 `docker pull` 换成对应 harbor 地址(library 为 public 项目,CI 可匿名 pull)。

> 镜像 tag 的"待镜像清单"不再需要 —— A 完成后这些地址即真实存在。

### C. 构建产物 push 到 `harbor.isuanova.com/cubestack`

统一命名:`harbor.isuanova.com/cubestack/cubepilot-{openclaw,operator,api,web}:<IMAGE_TAG>`。

- **Makefile**:
  - 新增 `IMAGE_REGISTRY ?= harbor.isuanova.com/cubestack`(可覆盖)。
  - `images` 目标: 四个 build 的 `-t` 改为 `$(IMAGE_REGISTRY)/cubepilot-<name>:$(IMAGE_TAG)`。
  - 新增 `push` 目标: 对四个镜像执行 `docker push $(IMAGE_REGISTRY)/cubepilot-<name>:$(IMAGE_TAG)`。
  - `IMAGE_TAG` 默认仍为 `local`;发布时 `make images push IMAGE_TAG=v0.1.0`。
  - 更新头部用法注释。
- **scripts/setup.sh**:
  - build/kind load/helm `--set` 全部使用 `harbor.isuanova.com/cubestack/cubepilot-<name>:local`。
  - 新增 `CUBEPILOT_PUSH=1`(或 `--push`)显式触发 push,默认不 push(dev 机器不一定有 harbor 凭据)。更新 --help 与头注释。
- **deploy/charts/cubepilot/values.yaml**:
  - `agents.image` / `operator.image` / `api.image` / `web.image` 默认改为 `harbor.isuanova.com/cubestack/cubepilot-<name>:local`。
  - 更新顶部"本地 kind 镜像"注释为"registry 地址,发布时覆盖 tag"。
- **internal/config/config.go**:
  - `CUBEPILOT_AGENT_IMAGE` 默认值 `cubepilot-openclaw:local` → `harbor.isuanova.com/cubestack/cubepilot-openclaw:local`。

### D. 验证

1. Harbor API 确认 5 个基础镜像 + tag 存在(镜像后)。
2. `make lint`(helm lint + helm template)通过。
3. `go build ./...`、`go vet ./...` 通过。
4. `make images`(docker 可用时)能基于 harbor 基础镜像构建 4 个产物镜像。
5. `make web`(npm build)通过。
6. 文档同步: README(镜像来源/构建 push 说明)、AGENTS。

## 已确认决策

- 必须镜像的 4 个基础镜像(node:24-bookworm-slim、node:22-alpine、nginx:1.27-alpine、openclaw:2026.6.33)+ golang:1.26-bookworm: **用户提供 harbor 凭据,由本会话补齐**(选"我提供凭据,你补齐")。
- openclaw 镜像 → **library/openclaw**(作为基础镜像,遵循"需要 pull 的镜像在 library 找")。
- `scripts/setup.sh` **默认不 push**,`CUBEPILOT_PUSH=1` 显式触发。
- `IMAGE_TAG` 默认 **`local`**;发布用显式版本号(`v0.1.0` 等),不用 `latest`。

## 范围外 / 后续跟进

- `openclaw-image.Dockerfile` 内的 `apt-get`(debian 源)与 kubectl 二进制下载(`dl.k8s.io`)仍走公网 —— 是包/二进制而非容器镜像,不在本次范围。
- web build 阶段 `npm ci`、openclaw 构建的 `pnpm` 仍走 npm registry —— 非容器镜像,范围外。
- `web/package.json` 声明 `engines.node >= 24`,但 `web/Dockerfile` 用 `node:22-alpine`(不一致)。本次保持镜像 `node:22-alpine`(零行为变更);后续如需对齐 engines,可镜像 `node:24-alpine` 并切换 web build 阶段。
- openclaw 基础镜像目前为**镜像 ghcr 公开镜像**;后续可改为从 openclaw 源码仓库构建并 push 到 library(可复现性更可控)。
- Harbor 账号需对 `library` 与 `cubestack` 两个项目具备写权限。
