# OCI Mirror + Zot 覆盖 PoC

同一个引用（例如 `127.0.0.1:15000/registry.k8s.io/pause:3.10`）可以先从公网 Pull，再由正常 Push 写入 Zot 并覆盖该 Tag。删除 Zot 的覆盖后，入口重新返回 Mirror 固定的公网版本。Mirror 缓存不写入 Zot，也不会让 Push 误以为 Zot 已有某个 Blob。

**仅限可信本机。** Mirror 和 Zot 的宿主机入口都绑定回环地址，使用明文 HTTP、允许匿名写入。不要公开监听、通过其他代理发布这两个端口，或把示例接到现有 Zot。示例 Zot 不配置认证、访问控制或远端同步；在这个约定下，指定仓库的 Zot `404` 才可作为公网回退依据。

## 本机启动

需要 Linux amd64、本地 Docker、Go 1.25 或更高版本、`curl` 7.71 或更高版本。完整 smoke 另需 `skopeo`、`jq` 及常用 GNU 工具。确认本机 `15000` 和 `15001` 两个端口未被占用，并可通过 HTTPS 匿名访问 `registry.k8s.io` 及其重定向的公网后端。在仓库根目录先运行 `bash` 进入一个专用子 shell，再执行：

```bash
set -euo pipefail
scratch=$(mktemp -d /tmp/oci-mirror-poc.XXXXXXXX)
zot_id=
mirror_pid=
cleanup() {
  local status=$?
  if [ "$status" -ne 0 ]; then
    if [ -f "$scratch/mirror.log" ]; then tail -n 30 "$scratch/mirror.log" >&2; fi
    if [ -n "$zot_id" ]; then docker logs --tail 30 "$zot_id" >&2 || true; fi
  fi
  if [ -n "$mirror_pid" ]; then
    kill -TERM "$mirror_pid" 2>/dev/null || true
    wait "$mirror_pid" || true
  fi
  if [ -n "$zot_id" ]; then
    docker stop "$zot_id" >/dev/null || printf '请检查这次的 Zot 容器：%s\n' "$zot_id" >&2
  fi
  rm -rf -- "$scratch"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

repo=registry.k8s.io/pause
tag=3.10
mirror=http://127.0.0.1:15000
zot=http://127.0.0.1:15001
export NO_PROXY="127.0.0.1${NO_PROXY:+,$NO_PROXY}"
export no_proxy="127.0.0.1${no_proxy:+,$no_proxy}"

mkdir "$scratch/cache"
cp examples/zot-poc.json "$scratch/zot-poc.json"
go build -o "$scratch/oci-mirror" ./cmd/oci-mirror
zot_id=$(docker run --detach --rm --name "${scratch##*/}" \
  -p 127.0.0.1:15001:5000 \
  --volume "$scratch/zot-poc.json:/etc/zot/config.json:ro,Z" \
  ghcr.io/project-zot/zot-linux-amd64:v2.1.15)
curl -fsS --max-time 2 --retry 15 --retry-all-errors --retry-delay 1 "$zot/v2/" >/dev/null

"$scratch/oci-mirror" -listen 127.0.0.1:15000 -poc-zot "$zot" \
  -poc-enable-fallback -poc-public-repo "$repo" -cache-dir "$scratch/cache" >"$scratch/mirror.log" 2>&1 &
mirror_pid=$!
curl -fsS --max-time 2 --retry 15 --retry-all-errors --retry-delay 1 "$mirror/v2/" >/dev/null
kill -0 "$mirror_pid"
```

该固定版本的 [Zot 发布配置](https://github.com/project-zot/zot/blob/v2.1.15/build/stacker.yaml) 默认执行 `serve /etc/zot/config.json`，镜像名见 [发布工作流](https://github.com/project-zot/zot/blob/v2.1.15/.github/workflows/publish.yaml)。上面的挂载让配置文件只读；`:Z` 仅重新标记临时副本，便于在启用 SELinux 的主机上使用。Zot 的 `/var/lib/registry` 留在容器可写层，不与 Mirror 缓存或现有数据卷共用。

本机能访问 `r.ltkk.run` 时，也可把 `repo`、`tag` 改为 `r.ltkk.run/library/alpine`、`latest`，并在 Mirror 启动命令加 `-poc-private-dns-host r.ltkk.run`。该域名解析到私网，未列入 allowlist 时不会回源。此替代源是单平台镜像，不适用下文针对 `pause` 多架构清单的验收步骤。

## 动态上游

启用 `-poc-enable-fallback` 后，省略 `-poc-public-repo` 即可对任意符合出站策略的显式仓库回源，例如 `docker.io/library/alpine` 或 `registry.k8s.io/pause`。可重复配置 `-poc-allow-registry`、`-poc-deny-registry` 缩小范围，deny 优先。只想开放几个仓库时，重复 `-poc-public-repo`；`alpine`、`library/alpine` 等短名必须逐个用该选项批准。不开 `-poc-enable-fallback` 时仅转发 Zot。

规范化只用于回源：Docker Hub 的 API 是 `registry-1.docker.io`，官方镜像补 `library/`；Zot 路径、JWT scope、覆盖 Tag 和缓存仍按客户端原仓库名隔离。上游须支持匿名 HTTPS/443，不能用路径指定端口、IP 或上游凭据。

## 验收与清理

保持在同一个子 shell，按 [PoC 架构与 smoke 步骤](docs/poc.md) 顺序操作。该流程先读 Manifest、按需缓存公网层，随后从本地目录向同名引用 Push，最后验证覆盖、旧 digest 和删除后回退。预期结果不等于实测报告。

规格完工状态与上线门槛见 [spec 0002 Checklist](docs/spec-0002-checklist.md)。

结束时在子 shell 执行 `exit`。清理会停止这次的 Mirror 和 Zot、删除临时缓存和文件；`--rm` 会随容器退出移除 Zot 容器及其可写层。保留 Docker 镜像，不删除其他镜像、容器或卷。
