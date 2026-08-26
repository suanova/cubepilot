# Harbor 镜像迁移实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 cubepilot 的 5 个公网基础镜像迁到 `harbor.isuanova.com/library`,构建产物镜像迁到 `harbor.isuanova.com/cubestack`。

**Architecture:** 基础镜像保持原 tag、仅重定向仓库地址(镜像 ghcr/docker.io → harbor library);构建产物统一 tag 为 `harbor.isuanova.com/cubestack/cubepilot-<name>:<tag>`。改 4 个 Dockerfile、CI pre-pull、Makefile(加 push 目标)、setup.sh(显式 push)、values.yaml、config.go 默认值。

**Tech Stack:** Docker buildx(镜像)、Make、bash、Helm、Go、GitHub Actions。

## Global Constraints

- 基础镜像目标地址固定为 `harbor.isuanova.com/library/<name>:<tag>`(5 个:golang:1.26-bookworm、node:24-bookworm-slim、node:22-alpine、nginx:1.27-alpine、openclaw:2026.6.33),tag 与当前公网引用完全一致。
- 构建产物目标地址固定为 `harbor.isuanova.com/cubestack/cubepilot-{openclaw,operator,api,web}:<tag>`。
- `IMAGE_TAG` 默认 `local`(设计决策),发布用显式版本号,不用 `latest`。
- `scripts/setup.sh` 默认**不 push**,`CUBEPILOT_PUSH=1` 或 `--push` 显式触发。
- Harbor 写权限:zhujian 在 `library` 是 maintainer(可 push);在 `cubestack` **无成员身份** —— 推 cubepilot 镜像前需管理员把 zhujian 加入 cubestack(developer/maintainer)。
- 依赖设计文档:`docs/superpowers/specs/2026-08-26-harbor-image-migration-design.md`。

---

### Task 1: 镜像 5 个基础镜像到 harbor library + 写可复用脚本

**Files:**
- Create: `scripts/mirror-base-images.sh`
- (无代码文件改动)

**Interfaces:**
- Produces: harbor 上 5 个 `library/<name>:<tag>` 工件(供 Task 2 的 Dockerfile FROM 解析、Task 8 的 `make images` 拉取)。

- [ ] **Step 1: 确认 docker login 与 library 写权限**

```bash
docker login harbor.isuanova.com   # 已登录;zhujian=library maintainer
```

- [ ] **Step 2: 写 `scripts/mirror-base-images.sh`**

```bash
#!/usr/bin/env bash
# Mirror CubePilot base images from public registries into
# harbor.isuanova.com/library. Base images keep their exact public tag and
# only change registry, so Dockerfiles / CI references resolve unchanged.
#
# Requires: docker login harbor.isuanova.com (maintainer or above on library).
# Reference: docs/superpowers/specs/2026-08-26-harbor-image-migration-design.md
set -euo pipefail

REG="harbor.isuanova.com/library"

mirror() { # $1=source ref  $2=target ref
  echo "[mirror] $1 -> $2"
  docker buildx imagetools create -t "$2" "$1"
}

mirror docker.io/library/golang:1.26-bookworm  "$REG/golang:1.26-bookworm"
mirror docker.io/library/node:24-bookworm-slim "$REG/node:24-bookworm-slim"
mirror docker.io/library/node:22-alpine        "$REG/node:22-alpine"
mirror docker.io/library/nginx:1.27-alpine     "$REG/nginx:1.27-alpine"
mirror ghcr.io/openclaw/openclaw:2026.6.33     "$REG/openclaw:2026.6.33"

echo "mirror done: $REG"
```

- [ ] **Step 3: 运行脚本(真实验证 push 权限与源可用性)**

Run: `bash scripts/mirror-base-images.sh`
Expected: 5 行 `[mirror] <src> -> <dst>`,最后 `mirror done: harbor.isuanova.com/library`。任一 push 失败即退出非 0。

- [ ] **Step 4: 用 Harbor API 验证 5 个工件存在**

```bash
for t in golang:1.26-bookworm node:24-bookworm-slim node:22-alpine nginx:1.27-alpine openclaw:2026.6.33; do
  n="${t%%:*}"; tag="${t##*:}"
  code=$(curl -s -o /dev/null -w '%{http_code}' "https://harbor.isuanova.com/api/v2.0/projects/library/repositories/$n/artifacts/$tag")
  echo "$t -> $code"
done
```

Expected: 全部 `200`。

- [ ] **Step 5: Commit**

```bash
git add scripts/mirror-base-images.sh
git commit -s -m "chore: mirror base images into harbor.isuanova.com/library

Adds scripts/mirror-base-images.sh (buildx imagetools create) so base
images keep their exact public tags but resolve from the private harbor.

Assisted-by: Claude Code"
```

---

### Task 2: 4 个 Dockerfile 的 FROM 切换到 library

**Files:**
- Modify: `deploy/openclaw-image.Dockerfile:10,18`(含头注释 7)
- Modify: `deploy/operator-image.Dockerfile:4,12`
- Modify: `deploy/api-image.Dockerfile:4,12`
- Modify: `web/Dockerfile:4,5,12`

**Interfaces:**
- Consumes: Task 1 的 5 个 library 基础镜像。
- Produces: FROM 全部指向 `harbor.isuanova.com/library/…`,供 Task 8 的 `make images` 直接构建。

- [ ] **Step 1: `deploy/openclaw-image.Dockerfile`**

将 L7 `# wanted, verify it exists:  ghcr.io/openclaw/openclaw:2026.6.33` 改为:
```
# wanted, mirror it from ghcr.io/openclaw/openclaw into
# harbor.isuanova.com/library (scripts/mirror-base-images.sh).
```
将 L10 `FROM golang:1.26-bookworm AS supervisor-build` 改为 `FROM harbor.isuanova.com/library/golang:1.26-bookworm AS supervisor-build`。
将 L18 `FROM ghcr.io/openclaw/openclaw:${OPENCLAW_IMAGE_TAG}` 改为 `FROM harbor.isuanova.com/library/openclaw:${OPENCLAW_IMAGE_TAG}`。

- [ ] **Step 2: `deploy/operator-image.Dockerfile`**

L4 `FROM golang:1.26-bookworm AS build` → `FROM harbor.isuanova.com/library/golang:1.26-bookworm AS build`
L12 `FROM node:24-bookworm-slim` → `FROM harbor.isuanova.com/library/node:24-bookworm-slim`

- [ ] **Step 3: `deploy/api-image.Dockerfile`**

同 Step 2 两处替换(文件内容一致)。

- [ ] **Step 4: `web/Dockerfile`**

L4 `# Build:  docker build -t cubepilot-web:local -f web/Dockerfile web` → `# Build:  docker build -t harbor.isuanova.com/cubestack/cubepilot-web:<tag> -f web/Dockerfile web`
L5 `FROM node:22-alpine AS build` → `FROM harbor.isuanova.com/library/node:22-alpine AS build`
L12 `FROM nginx:1.27-alpine` → `FROM harbor.isuanova.com/library/nginx:1.27-alpine`

- [ ] **Step 5: 校验语法 + 基础镜像可解析**

Run:
```bash
docker build --check -f deploy/openclaw-image.Dockerfile . 2>&1 | tail -2
docker build --check -f deploy/operator-image.Dockerfile . 2>&1 | tail -2
docker build --check -f deploy/api-image.Dockerfile . 2>&1 | tail -2
docker build --check -f web/Dockerfile web 2>&1 | tail -2
```
Expected: 每条不报错(允许 BuildKit 提示信息)。再抽查一个 FROM 能解析:
`docker buildx imagetools inspect harbor.isuanova.com/library/openclaw:2026.6.33` 返回 manifest 信息。

- [ ] **Step 6: Commit**

```bash
git add deploy/openclaw-image.Dockerfile deploy/operator-image.Dockerfile deploy/api-image.Dockerfile web/Dockerfile
git commit -s -m "build: point Dockerfile base images at harbor.isuanova.com/library

All five base images keep their exact tags and now resolve from the private
harbor library instead of docker.io / ghcr.io.

Assisted-by: Claude Code"
```

---

### Task 3: CI pre-pull 切换到 library

**Files:**
- Modify: `.github/workflows/e2e.yaml:50-56`

**Interfaces:**
- Consumes: Task 1 的 library 基础镜像。
- Produces: CI pre-pull 从 harbor 匿名拉取(public 项目)。

- [ ] **Step 1: 替换 pre-pull 块的 5 条命令**

`run:` 块内:
```yaml
          docker pull golang:1.26-bookworm
          docker pull node:24-bookworm-slim
          docker pull ghcr.io/openclaw/openclaw:2026.6.33
          docker pull node:22-alpine
          docker pull nginx:1.27-alpine
```
改为:
```yaml
          docker pull harbor.isuanova.com/library/golang:1.26-bookworm
          docker pull harbor.isuanova.com/library/node:24-bookworm-slim
          docker pull harbor.isuanova.com/library/openclaw:2026.6.33
          docker pull harbor.isuanova.com/library/node:22-alpine
          docker pull harbor.isuanova.com/library/nginx:1.27-alpine
```

- [ ] **Step 2: 校验**

Run: `grep -nE "docker pull" .github/workflows/e2e.yaml`
Expected: 5 行全部以 `harbor.isuanova.com/library/` 开头,无 `golang:1.26-bookworm` 等裸引用。

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/e2e.yaml
git commit -s -m "ci: pre-pull base images from harbor.isuanova.com/library

Assisted-by: Claude Code"
```

---

### Task 4: Makefile —— registry 寻址 + push 目标

**Files:**
- Modify: `Makefile:5-15,17-21,28,45-50`

**Interfaces:**
- Produces: `make images` 产出 `$(IMAGE_REGISTRY)/cubepilot-<name>:$(IMAGE_TAG)`;`make push` 推送 4 个镜像;`IMAGE_REGISTRY` 可覆盖。

- [ ] **Step 1: 更新头注释与变量**

L5-15 注释:将 `#   make images     build the four local images` 改为 `#   make images     build the four images (registry-addressed)`,并在其后加 `#   make push       push the four images to \$(IMAGE_REGISTRY)`。
L15 `# Overridable: IMAGE_TAG (default local), NAMESPACE, HELM_RELEASE.` 改为:
```
# Overridable: IMAGE_REGISTRY (default harbor.isuanova.com/cubestack),
#              IMAGE_TAG (default local), NAMESPACE, HELM_RELEASE.
```
L17-18 变量块,在 `IMAGE_TAG ?= local` 后加一行:
```makefile
IMAGE_REGISTRY ?= harbor.isuanova.com/cubestack
```
L28 `.PHONY: build test web images lint deploy undeploy clean` 改为:
```makefile
.PHONY: build test web images push lint deploy undeploy clean
```

- [ ] **Step 2: 替换 `images` 目标并新增 `push` 目标**

```makefile
## Build the four images (agent / operator / api / web) as
## $(IMAGE_REGISTRY)/cubepilot-<name>:$(IMAGE_TAG).
images:
	$(DOCKER) build -t $(IMAGE_REGISTRY)/cubepilot-openclaw:$(IMAGE_TAG) -f deploy/openclaw-image.Dockerfile .
	$(DOCKER) build -t $(IMAGE_REGISTRY)/cubepilot-operator:$(IMAGE_TAG) -f deploy/operator-image.Dockerfile .
	$(DOCKER) build -t $(IMAGE_REGISTRY)/cubepilot-api:$(IMAGE_TAG)      -f deploy/api-image.Dockerfile      .
	$(DOCKER) build -t $(IMAGE_REGISTRY)/cubepilot-web:$(IMAGE_TAG)      -f web/Dockerfile                  web

## Push the four images to $(IMAGE_REGISTRY).
push:
	$(DOCKER) push $(IMAGE_REGISTRY)/cubepilot-openclaw:$(IMAGE_TAG)
	$(DOCKER) push $(IMAGE_REGISTRY)/cubepilot-operator:$(IMAGE_TAG)
	$(DOCKER) push $(IMAGE_REGISTRY)/cubepilot-api:$(IMAGE_TAG)
	$(DOCKER) push $(IMAGE_REGISTRY)/cubepilot-web:$(IMAGE_TAG)
```

- [ ] **Step 3: 校验(dry-run)**

Run: `make -n images && make -n push`
Expected: `images` 显示 4 条 `-t harbor.isuanova.com/cubestack/cubepilot-<name>:local`;`push` 显示 4 条 `docker push harbor.isuanova.com/cubestack/cubepilot-<name>:local`。

- [ ] **Step 4: Commit**

```bash
git add Makefile
git commit -s -m "build: registry-address images and add make push target

make images now tags harbor.isuanova.com/cubestack/cubepilot-*:<tag>;
make push pushes them. IMAGE_REGISTRY is overridable.

Assisted-by: Claude Code"
```

---

### Task 5: setup.sh —— registry 引用 + 显式 push

**Files:**
- Modify: `scripts/setup.sh:20-37,61-67,104-112,153-157`

**Interfaces:**
- Consumes: Task 2/4 的镜像命名约定。
- Produces: `scripts/setup.sh [--push]` 构建/加载/部署 registry 寻址镜像;`CUBEPILOT_PUSH=1` 时构建后 push。

- [ ] **Step 1: 变量区新增 IMAGE_REPO / IMAGE_TAG / PUSH**

在 L22 `OPENCLAW_IMAGE_TAG=...` 后加:
```bash
# Built images are registry-addressed (harbor.isuanova.com/cubestack); set
# CUBEPILOT_PUSH=1 to push after building (dev machines may lack creds).
IMAGE_REPO="${CUBEPILOT_IMAGE_REPO:-harbor.isuanova.com/cubestack}"
IMAGE_TAG="${CUBEPILOT_IMAGE_TAG:-local}"
PUSH="${CUBEPILOT_PUSH:-0}"
```

- [ ] **Step 2: flag 循环与 --help 增加 --push**

在 L32 `--gateway-token)` 行附近、while 循环内加:
```bash
    --push)              PUSH=1; shift ;;
```
在 --help 的 Optional 块(L61-66 附近)加:
```
  CUBEPILOT_PUSH / --push
      Push the four built images to CUBEPILOT_IMAGE_REPO after building.
  CUBEPILOT_IMAGE_REPO / CUBEPILOT_IMAGE_TAG
      Image repository and tag for the built cubepilot images
      (default: harbor.isuanova.com/cubestack, local).
```

- [ ] **Step 3: build 段改为 registry 寻址并支持 push**

L104-108 整段替换为:
```bash
log "building images ($IMAGE_REPO, tag $IMAGE_TAG)"
docker build -t "$IMAGE_REPO/cubepilot-openclaw:$IMAGE_TAG" --build-arg OPENCLAW_IMAGE_TAG="$OPENCLAW_IMAGE_TAG" \
  -f "$REPO_DIR/deploy/openclaw-image.Dockerfile" "$REPO_DIR"
docker build -t "$IMAGE_REPO/cubepilot-operator:$IMAGE_TAG" -f "$REPO_DIR/deploy/operator-image.Dockerfile" "$REPO_DIR"
docker build -t "$IMAGE_REPO/cubepilot-api:$IMAGE_TAG"      -f "$REPO_DIR/deploy/api-image.Dockerfile"      "$REPO_DIR"
docker build -t "$IMAGE_REPO/cubepilot-web:$IMAGE_TAG"      -f "$REPO_DIR/web/Dockerfile"                  "$REPO_DIR/web"

if [ "$PUSH" = "1" ]; then
  log "pushing images to $IMAGE_REPO"
  docker push "$IMAGE_REPO/cubepilot-openclaw:$IMAGE_TAG"
  docker push "$IMAGE_REPO/cubepilot-operator:$IMAGE_TAG"
  docker push "$IMAGE_REPO/cubepilot-api:$IMAGE_TAG"
  docker push "$IMAGE_REPO/cubepilot-web:$IMAGE_TAG"
fi
```

- [ ] **Step 4: kind load 与 helm --set 用 registry 引用**

L111 `kind load docker-image ...` 改为:
```bash
kind load docker-image "$IMAGE_REPO/cubepilot-openclaw:$IMAGE_TAG" "$IMAGE_REPO/cubepilot-operator:$IMAGE_TAG" \
  "$IMAGE_REPO/cubepilot-api:$IMAGE_TAG" "$IMAGE_REPO/cubepilot-web:$IMAGE_TAG" --name "$KIND_CLUSTER"
```
L154-157 helm 段改为:
```bash
helm upgrade --install cubepilot "$REPO_DIR/deploy/charts/cubepilot" -n "$NAMESPACE" \
  --set agents.image="$IMAGE_REPO/cubepilot-openclaw:$IMAGE_TAG" \
  --set operator.image="$IMAGE_REPO/cubepilot-operator:$IMAGE_TAG" \
  --set api.image="$IMAGE_REPO/cubepilot-api:$IMAGE_TAG" \
  --set web.image="$IMAGE_REPO/cubepilot-web:$IMAGE_TAG"
```

- [ ] **Step 5: 校验**

Run:
```bash
bash -n scripts/setup.sh
scripts/setup.sh --help | grep -E "push|IMAGE_REPO"
```
Expected: `bash -n` 无输出(退出 0);`--help` 输出含 `--push` 与 `CUBEPILOT_IMAGE_REPO`。

- [ ] **Step 6: Commit**

```bash
git add scripts/setup.sh
git commit -s -m "feat(setup): registry-addressed images, opt-in push

scripts/setup.sh builds/tags/loads/deploys
harbor.isuanova.com/cubestack/cubepilot-*:<tag>; CUBEPILOT_PUSH=1 (or
--push) pushes them after building.

Assisted-by: Claude Code"
```

---

### Task 6: values.yaml + config.go 默认值

**Files:**
- Modify: `deploy/charts/cubepilot/values.yaml:1-2,12,33,45,57`
- Modify: `internal/config/config.go:77`

**Interfaces:**
- Produces: Helm 默认 `agents.image`/`operator.image`/`api.image`/`web.image` 与 Go 端 `CUBEPILOT_AGENT_IMAGE` 默认均为 `harbor.isuanova.com/cubestack/cubepilot-<name>:local`。

- [ ] **Step 1: values.yaml 头注释**

L1-2:
```
# CubePilot Helm values -- registry-addressed defaults
# (harbor.isuanova.com/cubestack); override the tag for real releases.
```

- [ ] **Step 2: 4 个 image 默认值**

L12 `  image: cubepilot-openclaw:local` → `  image: harbor.isuanova.com/cubestack/cubepilot-openclaw:local`
L33 `  image: cubepilot-operator:local` → `  image: harbor.isuanova.com/cubestack/cubepilot-operator:local`
L45 `  image: cubepilot-api:local` → `  image: harbor.isuanova.com/cubestack/cubepilot-api:local`
L57 `  image: cubepilot-web:local` → `  image: harbor.isuanova.com/cubestack/cubepilot-web:local`

- [ ] **Step 3: config.go 默认**

L77 `AgentImage:     getenv("CUBEPILOT_AGENT_IMAGE", "cubepilot-openclaw:local"),` →
```go
		AgentImage:     getenv("CUBEPILOT_AGENT_IMAGE", "harbor.isuanova.com/cubestack/cubepilot-openclaw:local"),
```

- [ ] **Step 4: 校验**

Run:
```bash
make lint
go build ./...
```
Expected: `make lint` 通过(helm lint + template 渲染无报错);`go build ./...` 退出 0。若 helm 未安装,改用 `helm lint deploy/charts/cubepilot && helm template cubepilot deploy/charts/cubepilot -n cubepilot >/dev/null`,再单独 `go build ./...`。

- [ ] **Step 5: Commit**

```bash
git add deploy/charts/cubepilot/values.yaml internal/config/config.go
git commit -s -m "chore(config): default images to harbor.isuanova.com/cubestack

Helm values and CUBEPILOT_AGENT_IMAGE now default to the registry-addressed
cubepilot-*:local refs (dev overrides the tag for releases).

Assisted-by: Claude Code"
```

---

### Task 7: README 文档

**Files:**
- Modify: `README.md:70-81`

**Interfaces:**
- Consumes: Task 1 的 `scripts/mirror-base-images.sh`、Task 4 的 `make push`。

- [ ] **Step 1: Prerequisites 段加"镜像"说明**

在 `README.md` Prerequisites 块(L61-68)末尾追加:
```markdown
Images are registry-addressed:
- Base images resolve from `harbor.isuanova.com/library/...` (mirrored from the
  public registries by `scripts/mirror-base-images.sh`).
- Built images are `harbor.isuanova.com/cubestack/cubepilot-{openclaw,operator,api,web}:<tag>`:
  `make images` builds them, `make push` pushes them, and
  `CUBEPILOT_PUSH=1 scripts/setup.sh` pushes after building.
```

- [ ] **Step 2: 校验**

Run: `grep -nE "harbor.isuanova.com" README.md`
Expected: 输出 ≥3 行(基础镜像 + 构建镜像 + 脚本提及)。

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -s -m "docs: document harbor image sourcing and push

Assisted-by: Claude Code"
```

---

### Task 8: 全量验证 + 残留公网引用清扫

**Files:**
- 只读校验;若发现残留引用则修。

**Interfaces:**
- Consumes: Task 1-7 全部产出。

- [ ] **Step 1: 残留公网镜像引用扫描**

Run:
```bash
git grep -nE "golang:1\.26-bookworm|node:24-bookworm-slim|node:22-alpine|nginx:1\.27-alpine|ghcr\.io/openclaw|cubepilot-(openclaw|operator|api|web):local" -- ':!docs/superpowers/**'
```
Expected: 无输出(`docs/superpowers/**` 里设计/计划的引用除外)。

- [ ] **Step 2: 静态与测试套件**

Run:
```bash
go vet ./...
go test ./...
bash -n scripts/setup.sh scripts/mirror-base-images.sh
make lint
```
Expected: 全部退出 0。

- [ ] **Step 3: 真实构建冒烟(基于 harbor 基础镜像)**

Run:
```bash
make images   # 4 个镜像全部基于 library 基础镜像构建
```
Expected: 4 条 build 全部成功(会拉取 library 基础镜像)。若本机资源受限,退化为只构建最小的一个:`docker build -t harbor.isuanova.com/cubestack/cubepilot-operator:local -f deploy/operator-image.Dockerfile .`

- [ ] **Step 4: (可选,需 cubestack 授权)推送验证**

先确认 zhujian 已加入 `cubestack` 项目后再运行:
```bash
make push
```
Expected: 4 条 push 成功。当前 zhujian 在 cubestack 无成员身份 —— 此步会 403,属预期,需管理员授权后重跑。

- [ ] **Step 5: 汇总**

将验证结果(残留扫描、go/helm/构建、cubestack 授权状态)如实汇报给用户,不虚报。

---

## Self-Review

- **Spec 覆盖**:设计文档 A(镜像)→ Task 1;B(Dockerfile/CI)→ Task 2/3;C(Makefile/setup.sh/values/config.go)→ Task 4/5/6;D(验证)→ Task 8;文档 → Task 7。范围外项(apt/kubectl/npm/pnpm、node 22 vs 24 engines、openclaw 源码构建)在设计文档标注,不在计划内。
- **占位符扫描**:无 TBD/TODO;每个改文件步骤都有精确的 before→after 与校验命令。
- **类型/命名一致性**:镜像引用统一为 `harbor.isuanova.com/library/<name>:<tag>` 与 `harbor.isuanova.com/cubestack/cubepilot-<name>:<tag>`;Makefile `IMAGE_REGISTRY`、setup.sh `IMAGE_REPO`/`IMAGE_TAG`/`PUSH`、README 说明一致。Task 6 的 config.go 默认值引用与 Task 4/5 的 tag 命名一致。
