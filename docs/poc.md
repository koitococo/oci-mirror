# Zot 覆盖 PoC：架构与手工验收

## 实现边界

```text
客户端 → Mirror（127.0.0.1:15000）→ 专用 Zot（127.0.0.1:15001）
 　　　　　　　　　　　└── 获准的本地 miss → Mirror 缓存或匿名 HTTPS 上游
```

回源只用 HTTPS/443，始终校验证书。默认拒绝解析到非公网地址的 DNS 主机；需要访问 `r.ltkk.run`（当前 DNS IPv4 为 `172.16.56.2`）时，显式配置 `-poc-private-dns-host r.ltkk.run`。该选项可重复，只匹配每次请求实际使用的主机名，不匹配父域或通配符；Registry、Bearer realm 和重定向目标分别检查。仅放行 DNS 返回的 RFC 1918 / IPv6 ULA 地址，私网 IP 字面量、回环和链路本地地址仍拒绝。这是本机 PoC 对 0001 §7 的显式例外，不是生产出口策略。

Zot 是本地仓库和上传会话的唯一权威，不配置认证、访问控制或 `sync`，也不共享 Mirror 的缓存。该隔离部署让 Mirror 可以把 Zot 的 `404` 视为本地缺失；这不是生产中的授权规则。

| 规格 | PoC 的行为 |
| --- | --- |
| [0002](../.omp/specs/0002-mirror-zot-overlay.md) §2：Zot 优先 | Manifest `GET`/`HEAD` 与 Blob `GET`（含 `Range`）先用原方法访问 Zot；只有符合回退策略的仓库 `404` 才进入回退路径。命中 Mirror 缓存也不能跳过 Zot。Blob `HEAD` 始终只查 Zot；上传、会话、mount、PUT、DELETE 和发现接口仍走 Zot。 |
| 0002 §3：同名覆盖 | 新 Push 提交后，下一次 Tag 读取优先返回 Zot；按公网旧 digest 读取不转成 Tag 查找。删除本地 Tag 后可再次露出 Mirror 固定版本，不建立屏蔽记录。 |
| 0002 §1、§4、[0001](../.omp/specs/0001-lazy-mirror.md) §3、§6：回源 | 用 [Distribution reference](https://pkg.go.dev/github.com/distribution/reference#ParseNormalizedNamed) 规范化上游。Docker Hub 补 `library/` 并访问 `registry-1.docker.io`；显式仓库可动态回源，短名需逐个批准。逻辑 Registry 的 deny 优先于 allow；这些规则不代替 realm/CDN 的 TLS 与拨号地址校验。Mirror 用 go-containerregistry 完成匿名 Bearer 握手，不用客户端入站凭据回源。 |
| 0001 §4：Manifest | 按请求独立获取原始 Manifest，不拉取 config、层或 Index 的子 Manifest。支持 OCI Image Manifest / Image Index 与 Docker schema 2 Manifest / Manifest List。Tag 和 digest 缓存在进程内；缓存的 Tag 不随公网更新而刷新。 |
| 0002 §5、0001 §2.2：Blob | 一次冷读只按 digest 下载请求的 Blob；同一 Blob 的并发冷读可合并。完整内容经 SHA-256 校验、发布到磁盘后才提供给客户端。冷 `Range` 同样先取并校验整个 Blob，再从本地文件服务 `206` 或 `416`。 |

冷 Manifest 与 Blob 共用 4 个并发回源槽。冷获取的 Manifest 上限为 4 MiB、30 秒，Blob 上限为 2 GiB、10 分钟。重启后磁盘 Blob 仍可复用，但进程内固定的 Tag 及 Manifest 会丢失；固定 Tag 也不等于所有依赖都已离线可读。

规范化不改变 Zot 仓库、JWT scope 或覆盖 Tag。`alpine`、`library/alpine`、`docker.io/library/alpine` 即使同指一个上游，也各自持有缓存。Blob 目录以原仓库名和规范化来源共同派生，因此旧单仓库 PoC 的缓存文件不会被新布局复用。Tag 省略时由 OCI 客户端请求 `latest`；协议层仍要求有效 Tag 或 SHA-256 digest。

改动入口：[`main.go`](../cmd/oci-mirror/main.go) 保留 Zot 转发和回退判断；[`resolver.go`](../cmd/oci-mirror/resolver.go) 负责引用规范化和回退策略；[`upstream.go`](../cmd/oci-mirror/upstream.go) 封装匿名上游与出站策略；[`cache.go`](../cmd/oci-mirror/cache.go) 负责 Manifest 固定、Blob 下载和校验发布。

## Smoke：按阶段执行

先完成 [README 的本机启动](../README.md#本机启动)，在同一个 Bash 子 shell 中依次运行下列命令。每次从空 Zot 和空 Mirror 缓存开始。下述状态码、digest 和日志是**验收预期**，不是运行记录。只从上游读，唯一的 Push 和 Delete 目标是本次启动的本地 Mirror。

### 1. 固定公网 Manifest，验证冷 Blob HEAD

用 Skopeo 匿名直读上游的 Docker Manifest List，然后通过 Mirror 分别读取根清单与 Linux/amd64 子清单。验证根清单原始字节和子清单 digest，再从子清单取 config 和首个层的描述符。

```bash
ref="docker://127.0.0.1:15000/$repo"
api="/v2/$repo"
oci_manifest=application/vnd.oci.image.manifest.v1+json
docker_list=application/vnd.docker.distribution.manifest.list.v2+json
docker_manifest=application/vnd.docker.distribution.manifest.v2+json
sha256_file() {
  local hash
  hash=$(sha256sum "$1")
  printf 'sha256:%s\n' "${hash%% *}"
}
http_head_code() { curl -sSI -o /dev/null -w '%{http_code}' "$1"; }
zot_http() {
  docker logs "$zot_id" 2>&1 |
    jq -c 'select(.module == "http" and .message == "HTTP API") | {method, path, statusCode}'
}

skopeo inspect --raw --no-creds --tls-verify=true "docker://$repo:$tag" >"$scratch/public-root.json"
jq -e --arg mt "$docker_list" '.mediaType == $mt' "$scratch/public-root.json"
public_digest=$(sha256_file "$scratch/public-root.json")
child_digest=$(jq -er '
  [.manifests[] | select(.platform.os == "linux" and .platform.architecture == "amd64")] |
  select(length == 1) | .[0].digest
' "$scratch/public-root.json")

curl -fsSI -H "Accept: $docker_list" "$mirror$api/manifests/$tag" |
  tr -d '\r' | grep -Fxi "Docker-Content-Digest: $public_digest"
test -z "$(find "$scratch/cache" -type f -print -quit)"

skopeo inspect --raw --no-creds --tls-verify=false "$ref:$tag" >"$scratch/mirror-root.json"
cmp "$scratch/public-root.json" "$scratch/mirror-root.json"
test "$(grep -Fc 'mirror manifest published digest=' "$scratch/mirror.log")" -eq 1
test "$(curl -sS -o /dev/null -w '%{http_code}' -H "Accept: $oci_manifest" \
  "$mirror$api/manifests/$tag")" = 406
test -z "$(find "$scratch/cache" -type f -print -quit)"

skopeo inspect --raw --no-creds --tls-verify=false "$ref@$child_digest" >"$scratch/public-child.json"
test "$(sha256_file "$scratch/public-child.json")" = "$child_digest"
test "$(grep -Fc 'mirror manifest published digest=' "$scratch/mirror.log")" -eq 2
jq -e --arg mt "$docker_manifest" '.mediaType == $mt and (.layers | length > 0)' \
  "$scratch/public-child.json"
layer=$(jq -er '.layers[0].digest' "$scratch/public-child.json")
layer_size=$(jq -er '.layers[0].size | select(. >= 16)' "$scratch/public-child.json")
old_config=$(jq -er '.config.digest' "$scratch/public-child.json")
old_config_size=$(jq -er '.config.size | select(. >= 16)' "$scratch/public-child.json")
test -z "$(find "$scratch/cache" -type f -print -quit)"
test "$(http_head_code "$mirror$api/blobs/$layer")" = 404
test -z "$(find "$scratch/cache" -type f -print -quit)"
zot_http | jq -ce --arg path "$api/blobs/$layer" \
  'select(.method == "HEAD" and .path == $path and .statusCode == 404)'
```

预期：Manifest 内容逐字节相同，冷 HEAD 返回 Zot 的 `404`，Zot 日志记录该 HEAD，磁盘没有 Blob。缓存和 Zot 日志不是完整的上游网络记录；它们不能单独排除一个不写缓存的上游请求。

### 2. 按需缓存层，确认热 Blob HEAD 仍只看 Zot

先用冷 Range 拉首个层的 16 字节，再读取完整 Blob 做 SHA-256 和内容核对；接着用 Skopeo 拉完整镜像到本地目录，作为 Push 源。完成后同一层的前端 HEAD 仍须返回 `404`。

```bash
check_range() {
  local url=$1 size=$2 expected=$3
  test "$(curl -fsS --range 0-15 -D "$scratch/range.headers" \
    -o "$scratch/range.body" -w '%{http_code}' "$url")" = 206
  tr -d '\r' <"$scratch/range.headers" | grep -Fxi "Content-Range: bytes 0-15/$size"
  head -c 16 "$expected" | cmp - "$scratch/range.body"
}

# 先发 Range；等它完成后，才发完整 GET。
test "$(curl -fsS --range 0-15 -o "$scratch/cold.range" \
  -w '%{http_code}' "$mirror$api/blobs/$layer")" = 206
curl -fsS "$mirror$api/blobs/$layer" -o "$scratch/layer.blob"
test "$(sha256_file "$scratch/layer.blob")" = "$layer"
head -c 16 "$scratch/layer.blob" | cmp - "$scratch/cold.range"
test "$(find "$scratch/cache" -type f | wc -l)" -eq 1
test "$(grep -Fc "mirror blob published digest=$layer " "$scratch/mirror.log")" -eq 1
check_range "$mirror$api/blobs/$layer" "$layer_size" "$scratch/layer.blob"
test "$(curl -sS --range "$layer_size-" -o /dev/null -w '%{http_code}' \
  "$mirror$api/blobs/$layer")" = 416

skopeo --override-os linux --override-arch amd64 copy --preserve-digests \
  --src-no-creds --src-tls-verify=false "$ref:$tag" "dir:$scratch/public-image"
cmp "$scratch/public-child.json" "$scratch/public-image/manifest.json"
test "$(http_head_code "$mirror$api/blobs/$layer")" = 404
zot_http | jq -ce --arg path "$api/blobs/$layer" \
  'select(.method == "HEAD" and .path == $path and .statusCode == 404)'
```

预期：第一条冷 Range 只导致一个 Blob 发布，Range 和完整 GET 内容一致；即使完整镜像已从 Mirror 拉过，Zot 仍缺该层，故热 HEAD 仍是 `404`，不会让接下来的 Push 跳过上传。

### 3. 复用层，从本地目录 Push 同名覆盖

给镜像 config 加一个 label，并用新 config 的 SHA-256 和大小更新子清单。这样公网的旧 config 仍只在 Mirror，可用于下一阶段的 Range 验收。`dir:` 目录用不含 `sha256:` 前缀的 digest 作 Blob 文件名。Push 时用 `--format oci` 写入同名 Tag，保留原层。

```bash
cp -a "$scratch/public-image" "$scratch/overlay"
jq '.config.Labels["org.example.oci-mirror.poc"] = "zot-overlay"' \
  "$scratch/public-image/${old_config#sha256:}" >"$scratch/overlay-config.json"
new_config=$(sha256_file "$scratch/overlay-config.json")
test "$new_config" != "$old_config"
config_size=$(wc -c <"$scratch/overlay-config.json")
mv "$scratch/overlay-config.json" "$scratch/overlay/${new_config#sha256:}"
jq --arg digest "$new_config" --argjson size "$config_size" \
  '.config.digest = $digest | .config.size = $size' \
  "$scratch/public-child.json" >"$scratch/overlay/manifest.json"

skopeo copy --format oci --digestfile "$scratch/pushed.digest" \
  --dest-no-creds --dest-tls-verify=false "dir:$scratch/overlay" "$ref:$tag"
overlay_digest=$(cat "$scratch/pushed.digest")
test "$overlay_digest" != "$public_digest"
test "$overlay_digest" != "$child_digest"
# Push 完成后，第一次从 Tag Pull 就必须得到 Zot 覆盖版。
skopeo copy --preserve-digests --src-no-creds --src-tls-verify=false \
  "$ref:$tag" "dir:$scratch/zot-image"
test "$(sha256_file "$scratch/zot-image/manifest.json")" = "$overlay_digest"
jq -e --arg mt "$oci_manifest" --arg layer "$layer" --arg config "$old_config" \
  '.mediaType == $mt and .layers[0].digest == $layer and .config.digest != $config' \
  "$scratch/zot-image/manifest.json"
test "$(http_head_code "$mirror$api/blobs/$layer")" = 200

zot_http | jq -c --arg prefix "$api/" 'select(.path | startswith($prefix))'
```

对照 Zot 日志：该层的 HEAD 从 `404` 变为 `200`，其间有 Blob 上传会话完成（上传 `PUT` 的查询中可找到该层 digest）以及同名 Tag 的 Manifest `PUT` 返回 `201`。镜像已缓存但仍成功上传共享层，才满足 [0002 §6 第 2 项](../.omp/specs/0002-mirror-zot-overlay.md#6-验收) 的覆盖场景。

### 4. 按旧 digest Pull，删除覆盖后再 Pull Tag

覆盖存在时，按原根 digest 读取公网列表，再按原子清单 digest 拉完整旧镜像。Range 使用旧 config（不是已上传到 Zot 的共享层），确保此时确实读取 Mirror 的数据。最后删除本次测试写入的本地 Tag，检查 Zot 已缺失、但前端仍能拉取原公网列表与镜像。

```bash
skopeo inspect --raw --no-creds --tls-verify=false "$ref@$public_digest" >"$scratch/old-root.json"
cmp "$scratch/public-root.json" "$scratch/old-root.json"
skopeo copy --preserve-digests --src-no-creds --src-tls-verify=false \
  "$ref@$child_digest" "dir:$scratch/old-image"
cmp "$scratch/public-child.json" "$scratch/old-image/manifest.json"
test "$(http_head_code "$mirror$api/blobs/$old_config")" = 404
check_range "$mirror$api/blobs/$old_config" "$old_config_size" \
  "$scratch/public-image/${old_config#sha256:}"

test "$(curl -fsS -X DELETE -o /dev/null -w '%{http_code}' "$mirror$api/manifests/$tag")" = 202
test "$(http_head_code "$zot$api/manifests/$tag")" = 404
skopeo inspect --raw --no-creds --tls-verify=false "$ref:$tag" >"$scratch/fallback-root.json"
cmp "$scratch/public-root.json" "$scratch/fallback-root.json"
skopeo --override-os linux --override-arch amd64 copy --preserve-digests \
  --src-no-creds --src-tls-verify=false "$ref:$tag" "dir:$scratch/fallback-image"
cmp "$scratch/public-child.json" "$scratch/fallback-image/manifest.json"
printf '公网/回退根 digest: %s\n旧子清单 digest: %s\nZot 覆盖 digest: %s\n' \
  "$public_digest" "$child_digest" "$overlay_digest"
```

### 5. Zot 不可达时不读热缓存

如需检查失败路径，在清理前停止**本次** Zot。尽管 Tag 已在 Mirror 缓存，入口仍应返回 `502`：

```bash
docker stop "$zot_id" >/dev/null
zot_id=
test "$(curl -sS -o /dev/null -w '%{http_code}' "$mirror$api/manifests/$tag")" = 502
```

不要在 Push 之后用共享层的 Range 探测 Mirror 的 `416`：Zot 已有这个层，响应由 Zot 决定。Zot v2.1.15 实测对该层的越界 Range 返回 `500`，Mirror 按规格透传，不回退。

检查所需日志后在子 shell 执行 `exit`，由 [README 的清理流程](../README.md#验收与清理) 停止服务并移除临时文件。要重跑，请重新创建空环境。

## 本轮实测

Zot v2.1.15、Skopeo v1.22.0 上，以上五个阶段全部通过。公网根清单为 `sha256:ee6521f290b2168b6e0935a181d4cff9be1ac3f505666ef0e3c98fae8199917a`，同名 Zot 覆盖为 `sha256:63881a70fa9f26ae2feb4987058dd5a0b1fb93f823b5d3d1e96a4103724aadec`；删除后字节级恢复公网根清单。

私网 DNS 例外单独验收：未配置 allowlist 或只列 `ltkk.run` 时 `r.ltkk.run` 的冷读返回 `502`；列精确主机 `r.ltkk.run` 后通过 Skopeo 完整匿名拉取 `library/alpine:latest`，Manifest 原始字节和层 SHA-256 均与上游一致。直接配置 `172.16.56.2` 为仓库主机或 allowlist 项都被拒绝。

动态回源在隔离 JWT fixture 下实测完整拉取 `registry.k8s.io/pause:3.10`、`r.ltkk.run/library/alpine:latest` 和省略 Tag 的 `docker.io/library/alpine`，并校验落盘层摘要。两个短名默认不回退，逐个批准后可拉取；三个 Docker Hub 拼写和 `r.ltkk.run` 上碰巧相同的 Blob 分别保存在四个独立缓存作用域。显式名的 JWT 不能授权短名（`401`）；Registry 同时在 allowlist 和 denylist 时不回源（`404`）。

### 复用留存的 JWT fixture

若本机已建好此独立 fixture，连接配置在 `.git/oci-mirror-jwt-fixture/env`，与 README 的临时匿名示例无关。Zot、docker-auth、Nginx 和 Mirror 没有发布宿主机端口。客户端使用 Docker 网络、容器内 hosts 和独立 CA：

```bash
source .git/oci-mirror-jwt-fixture/env
docker run --rm --network "$JWT_NETWORK" --add-host "$JWT_HOST:$JWT_IP" \
  --user "$(id -u):$(id -g)" \
  -v "$JWT_CA_DIR:/trust:ro,z" -v "$JWT_READER_AUTHFILE:/auth.json:ro,z" \
  skopeo:latest inspect --raw --authfile /auth.json --cert-dir /trust \
  "docker://$JWT_AUTHORITY/registry.k8s.io/pause:3.10"
```

## 延后项与下一阶段顺序

1. **补齐生产边界，再考虑对外开放。** 接入统一登录和仓库级授权，与可信后端约定真实缺失；验证错误 scope、过期 token、权限隐藏和后端故障不回退，入站身份不进入公网请求。PoC 的匿名 Zot 不是可迁移的生产配置。
2. **持久化与容量。** 补齐跨重启固定 Tag/Manifest、缓存容量和 GC；此 PoC 只保留经校验的磁盘 Blob，没有缓存淘汰策略。
   多上游目前共用连接池和全局冷回源槽，尚无每 Registry 的独立速率限制或有界 Bearer token 上下文复用。
3. **传输优化。** 补齐 0002 §5.1 的完整冷 GET 提前出流、末尾字节保留与响应中止语义，再实现 §5.2 的上游 Range 续传，并以大 Blob、故障注入、读者断开和慢读者场景验证。PoC 不提前发前缀、不续传、不分片。
4. **上线验收。** 分别用 Docker Engine、containerd、nerdctl 和 Podman 验证冷/热拉取、多架构、覆盖与上传兼容性。此处的 Skopeo smoke 只拉取一个平台，不能替代客户端矩阵；若某个客户端依赖 Blob HEAD `200` 才能 Pull，不能靠伪造成功响应通过验收。
