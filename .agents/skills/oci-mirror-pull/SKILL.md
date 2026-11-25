---
name: oci-mirror-pull
description: 使用已部署的 OCI Mirror 服务拉取上游镜像，支持 Docker Engine 与 Skopeo。Use when 目标主机无法直连上游 Registry，或需要从指定 OCI Mirror 获取镜像。
---

# 从现有 OCI Mirror 拉取镜像

## 1. 确认入口和镜像引用

- 使用用户指定或环境已配置的 Mirror HTTPS 入口。在实际拉取镜像的主机上确认入口可达；如果使用远程 Docker context，则由目标 daemon 连接 Mirror。入口未知时先查现有部署配置，仍不明确才询问。本技能只做客户端拉取，不部署服务、不建隧道、不改 daemon 的镜像源或 TLS 策略。
- 将原始引用写成 `<上游主机>/<仓库>:<tag>` 或 `<上游主机>/<仓库>@sha256:…`，再在前面加 `<Mirror主机[:端口]>/`。Docker Hub 短名例如 `alpine:3.22` 先展开成 `docker.io/library/alpine:3.22`。如果运营方为特定仓库指定了短路径，保留那个路径：不同拼写在 Mirror 上可能有不同的 ACL、覆盖镜像和缓存。
- 用 `curl -sS -D - -o /dev/null "https://$mirror/v2/"` 检查 Registry 入口。`200` 或带 `WWW-Authenticate` 的 `401` 均可能正常，不代表目标仓库已授权。保持 TLS 校验；私有 CA 用客户端已有的受信配置或指定 CA 目录。需要登录时，用该 Mirror 的下游凭据（`docker login "$mirror"` 或 `skopeo login "$mirror"`，分别使用各自的凭据存储），不要把上游账号密码交给 Mirror 客户端入口。

## 2. 按交付目标完整拉取

`mirror.example.com` 是文档占位域名，运行前替换为获准使用的 Mirror 入口；示例源镜像来自公开上游。

```bash
mirror=mirror.example.com
image=registry.k8s.io/pause:3.10.1
ref="$mirror/$image"
```

**目标是 Docker Engine：**

```bash
docker pull "$ref"
docker image inspect --format '{{.Id}} {{.Os}}/{{.Architecture}}' "$ref"
```

需要指定平台时加 `--platform linux/arm64`（按实际目标填写）。Docker 保存带 Mirror 前缀的引用；只有工作负载必须使用原名时，才将镜像另打目标 tag：`docker tag "$ref" "$dest"`。`dest` 必须是允许写入的带 tag 引用；源为 digest 时也不能把 digest 用作目标 tag。

**目标是目录，或没有 Docker daemon：**

```bash
out=$(mktemp -d)
skopeo copy --src-no-creds "docker://$ref" "dir:$out"
```

`--src-no-creds` 仅用于明确匿名的仓库；认证入口去掉该标志并使用已登录的凭据或显式 `--src-authfile`。Skopeo 默认取客户端平台，指定平台时把 `--override-os linux --override-arch arm64` 放在 `copy` 前。若需导入 Docker，可将 `dir:$out` 换成 `docker-daemon:$dest`，并确认 Skopeo 的 daemon 与 `docker image inspect "$dest"` 检查的是同一个。

## 3. 验收与排障

- 以完整 `docker pull` 或 `skopeo copy` 成功为准，报告实际使用的 Mirror 引用、平台与落地位置。`/v2/` 和 `inspect` 不能证明镜像层都已下载。
- 如需核对上游内容，首选已知上游 digest；同一 tag 可能被 Mirror 后端覆盖或命中缓存。查 Manifest 用 `skopeo inspect --raw "docker://$ref"`，只查元数据用 `--no-tags`。`tags/list` 只查后端，本地未登记的镜像即使可回源拉取，默认 `skopeo inspect` 仍可能在枚举 tag 时失败。
- `401/403` 查 Mirror 的登录、token 服务可达性和原仓库路径 ACL；`404` 核对完整引用及部署的回源许可；`502/503/507` 保存 `X-Request-ID` 给运维定位后端、回源或配额。Mirror 不可达时，报告是哪个主机或 daemon 失败，不把一次直连上游成功当作 Mirror 拉取成功。
