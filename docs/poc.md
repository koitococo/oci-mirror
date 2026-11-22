# Zot 覆盖 PoC：架构与手工验收

## 实现边界

```text
客户端 → HTTPS 入口 → Mirror → 私有直连回环上的专用 Zot
           └── 签发端       └── 已授权的可信本地 miss → Mirror 缓存或匿名 HTTPS 上游
```

### 下游认证与可信本地缺失

启用回退必须设置 `-zot-config` 和 `-jwt-issuer`；后者等于 docker-auth 的 `token.issuer`。将同一份不可变 JSON 和同一份 JWT 签名公钥证书只读提供给 Zot 和 Mirror；JSON 中 `http.auth.bearer.cert` 必须是两端均可读的同一绝对路径。Zot 只允许传统 `http.auth.bearer`，不得启用其他认证（包括 OIDC）、`http.accessControl`、集群或远端同步（`extensions.sync`）。不热更新配置；修改后协调重启两端。Mirror 从 `http.auth.bearer.service` 取预期 `aud`、从 `realm` 取挑战端点，并在启动时确认直连 Zot `/v2/` 返回匹配的未限定 scope 的 Bearer challenge。这不是运行时配置或版本证明：信任依赖于操作者确实部署同一配置、保持 Mirror→Zot 的私有直连回环（无中间代理），并使用约定的 Zot v2.1.15 行为。升级 Zot 要重新验证真实缺失和错误响应语义。

Mirror 验证 Registry Bearer JWT 的 RSA RS256 签名、`iss`、`aud`、`exp`、可选 `nbf` / `iat` 及非空 `sub`。Zot 仍负责仓库和操作权限及写入。回退还要求 token 对**下游原始仓库名**具有 `pull` scope。参考 [docker-auth ACL](https://github.com/cesanta/docker_auth/blob/1.14.0/examples/reference.yml)，在专用 writer、拒绝和私有仓库规则之后，对已认证账号与带 Registry 域名的仓库发布默认 `pull` scope，见 [README](../README.md#本机启动)。普通本地短名不继承这条默认授权；显式仓库的本地同名覆盖和上游共享同一仓库 ACL。

回退只信任与原请求相符的 Zot JSON `404`：`NAME_UNKNOWN` 必须匹配原仓库名，`MANIFEST_UNKNOWN` 匹配 Manifest 引用，`BLOB_UNKNOWN` 匹配 Blob digest；裸 `404`、不匹配的错误、鉴权挑战与后端故障均不触发回退。GET 原样先发往 Zot，绝不预检 HEAD；仅 Manifest GET 的 `NAME_UNKNOWN` 需要事后用同资源 HEAD 的 `404` 佐证，因为 Zot v2.1.15 的坏仓库布局在 GET 上也会返回此错误，但 HEAD 返回 `500`。初始 Manifest HEAD 的 `404` 没有响应体，只用后续一次直连 GET 验证有匹配的类型化 `404`。探测不一致则保留首次响应；探测不是原子快照，不能与并发 Push 串行化。Blob HEAD 从不回退。

### 匿名上游与出站策略

通过下游授权并确认可信缺失后，默认允许符合出站规则的显式 Registry；可选 allow 列表缩小范围，deny 始终优先；短名仍须逐个启用。回源只用匿名 HTTPS/443，以系统 CA 校验证书，并校验实际拨号地址。默认拒绝解析到非公网地址的主机。仅在受控私有部署且使用受管理 DNS 时，操作者才应按确切 FQDN 显式配置 `-poc-private-dns-host`（例如 `r.ltkk.run`）。可重复配置；不匹配父域或通配符，Registry、Bearer realm 和重定向目标分别检查。此例外只允许相应 DNS 返回的 RFC 1918 / IPv6 ULA 地址，不允许直接使用私网 IP 字面量，也不放行回环、链路本地或元数据目的地。是否加入名单是独立于公开 DNS 或证书签发的管理决策；有效证书不能代替出站授权。

| 规格 | PoC 的行为 |
| --- | --- |
| [0002](../.omp/specs/0002-mirror-zot-overlay.md) §2：Zot 优先 | Manifest `GET`/`HEAD` 与 Blob `GET`（含 `Range`）先用原方法访问 Zot；只有已授权、符合策略的可信本地缺失才回退。缓存不能跳过 Zot。Blob `HEAD`、上传、会话、mount、PUT、DELETE 和发现接口仍走 Zot。 |
| 0002 §3：同名覆盖 | 新 Push 提交后，下一次 Tag 读取优先返回 Zot；按公网旧 digest 读取不转成 Tag 查找。删除本地 Tag 后可再次露出 Mirror 固定版本，不建立屏蔽记录。 |
| 0002 §1、§4、[0001](../.omp/specs/0001-lazy-mirror.md) §3、§6：回源 | 用 [Distribution reference](https://pkg.go.dev/github.com/distribution/reference#ParseNormalizedNamed) 规范化上游。Docker Hub 补 `library/` 并访问 `registry-1.docker.io`；显式仓库可动态回源，短名需逐个批准。逻辑 Registry 的 deny 优先于 allow；这些规则不代替 realm/CDN 的 TLS 与拨号地址校验。Mirror 用 go-containerregistry 完成匿名 Bearer 握手，不用客户端入站凭据回源。 |
| 0001 §4：Manifest | 按请求独立获取原始 Manifest，不拉取 config、层或 Index 的子 Manifest。支持 OCI Image Manifest / Image Index 与 Docker schema 2 Manifest / Manifest List。Tag 和 digest 绑定持久化；已固定的 Tag 不随公网更新而刷新。 |
| 0002 §5、0001 §2.2：Blob | 一次冷读只下载目标 digest，并发读者共用同一个生产者。可信长度的完整冷 GET 可以读取正在写入的暂存前缀；末尾至少一字节在验证和发布后发送。冷 Range 等整块校验发布后才从正式缓存返回 `206` 或 `416`。 |

冷获取的 Manifest 上限为 4 MiB、30 秒，Blob 上限为 2 GiB、10 分钟。固定 Tag 不等于所有依赖都已离线可读。

规范化不改变 Zot 仓库、JWT scope 或覆盖 Tag。缓存同时按下游原仓库名和规范化上游来源隔离；`alpine`、`library/alpine`、`docker.io/library/alpine` 即使同指一个上游，也各自持有缓存。Tag 省略时由 OCI 客户端请求 `latest`；协议层仍要求有效 Tag 或 SHA-256 digest。

改动入口：[`main.go`](../cmd/oci-mirror/main.go) 保留 Zot 转发和回退判断；[`resolver.go`](../cmd/oci-mirror/resolver.go) 负责引用规范化和回退策略；[`upstream.go`](../cmd/oci-mirror/upstream.go) 封装匿名上游与出站策略；[`cache.go`](../cmd/oci-mirror/cache.go) 负责 Manifest 固定、Blob 下载和校验发布；[`blobstream.go`](../cmd/oci-mirror/blobstream.go) 负责并发读者的暂存出流和响应中止。

### 持久缓存与配额

启用回退时，每个 Mirror 进程使用一个专用的本地持久目录，不与其他进程共享。`<cache-dir>/mirror-v1/catalog.sqlite` 使用 SQLite WAL 与 `synchronous=FULL`，记录不可变的原始 Manifest 字节、上游来源与下游仓库、Tag 和 digest 绑定、Blob 描述符长度及已发布的 Blob 记录。`mirror-v1/staging/` 存放下载中的临时文件，`mirror-v1/blobs/` 存放已校验发布的 Blob。

重启恢复固定记录与已发布的 Blob，清理中断的暂存文件及孤立文件，并按调低后的 Blob 配额淘汰载荷，不修剪 Manifest 固定记录。已初始化的缓存丢失目录数据库时启动失败，不会静默创建新库并重新固定 Tag。旧布局在 `mirror-v1/` 旁的缓存文件不会被读取、迁移或删除。

以下参数均须为正数；字节参数传十进制整数：

| 参数 | 默认 | 计量对象 |
| --- | ---: | --- |
| `-cache-manifest-bytes` | 64 MiB | 固定的原始 Manifest 字节 |
| `-cache-manifest-entries` | 10000 | digest 记录与 Tag 绑定合计 |
| `-cache-blob-bytes` | 16 GiB | 已发布的 Blob 载荷 |
| `-cache-blob-entries` | 10000 | 已发布的 Blob 记录 |
| `-cache-staging-bytes` | 8 GiB | 全局 Blob 暂存预留 |
| `-cache-upstream-staging-bytes` | 4 GiB | 每个逻辑 Registry 的 Blob 暂存预留 |
| `-cache-fetches` | 4 | 全局并行冷回源请求 |
| `-cache-upstream-fetches` | 2 | 每个逻辑 Registry 的并行冷回源请求 |

例如，将下列参数加到已配置认证和回退的 Mirror 启动命令，前提是专用目录已由运维人员准备且运行用户可写：

```text
-cache-dir /var/lib/oci-mirror/cache -cache-blob-bytes 8589934592 -cache-blob-entries 5000
```

并发槽由冷 Manifest 和冷 Blob 请求共用；每上游配额按逻辑 Registry 计量，Docker Hub 别名共用配额，但不同 Registry 的小请求可在全局额度内并发。Blob 写入暂存前按已知预期长度预留空间，长度未知时按有上限的单对象额度预留。已知长度超出配额或剩余配额不足返回 `507`，冷回源槽用尽返回 `503`，目录中已有发布记录但文件缺失或长度不符返回 `500`。

Manifest 固定记录不淘汰；额度不足时新增绑定返回 `507`。Blob 淘汰采用近似 LRU，跳过正在读取的载荷，最近访问时间按一秒粒度更新。已发布载荷预算不含 SQLite 元数据及 WAL 开销，也不含暂存和旧布局文件；这些参数不是物理磁盘用量的硬上限。需要硬上限时另设文件系统配额，并为目录数据库与 WAL 留出空间。

### 持久缓存实测

在保留的 Zot 和 docker-auth 上，用专用缓存目录触发 `SIGKILL` 后重启，并移除 `r.ltkk.run` 的私网出站例外。相同 Tag、digest、原始 Manifest 字节和 Blob 仍命中，新的冷 Tag 因无法回源返回 `502`。SQLite 中保有原 Manifest 的 Blob 描述符；中断 staging 与无目录记录的 Blob 文件被清理，旧布局文件未触碰。第二个 Mirror 不能同时打开该目录。

缩小配额后，实测 Blob 按字节及条目上限分别淘汰，Manifest 固定记录不受影响；超出 Manifest 或 staging 配额返回 `507`。原已发布 Blob 被截断后返回 `500` 而不重新回源，另一个上游仓库不存在同一 digest 时仍返回 `404`。独立的压力场景也验证了全局/每上游的并发与暂存限制、活动读者不被回收、并发发布只保留同一份 Tag 绑定。

留存的 Mirror 已切换到新版持久缓存；本机 Docker Engine 通过 JWT/TLS 拉取 `registry.k8s.io/pause:3.10`，本机 Skopeo 完整拉取 `r.ltkk.run/library/alpine:latest` 并核对镜像层摘要。Zot、docker-auth 和旧版缓存目录均保留。

### 完整冷 Blob GET 提前出流

已授权且 Zot 真实缺失后，Mirror 按仓库、源和 digest 合并任务。单个后台生产者顺序读取上游、写一个暂存文件并增量计算 SHA-256；读取方通过各自的偏移读取已写入的内容，网络慢读和断连不控制生产者。暂存仍受原有的全局、每上游和单对象额度限制。

仅无 Range 或 HTTP 条件头、且总长可信的完整 GET 可提前出流。总长优先取已固定的 Manifest 描述符，并核对可用的上游 `Content-Length`；没有描述符时可用上游 `Content-Length`。长度未知或没有可提前发送的前缀时，先等待正式发布。提前出流带总 `Content-Length`，已发前缀未经整块验证；只有读到 HTTP 响应体结束、长度和 SHA-256 匹配且目录发布成功，才从正式缓存发送剩余字节。失败若发生在首字节之前，返回普通 Registry 错误；若已出流，则中止传输，不发送尾字节或追加 JSON。

冷 Range、条件请求只读正式发布的文件；热 GET 与 Blob HEAD 的分流不变。Nginx 入口应禁用 `/v2/` 的响应缓冲，或保留对 `X-Accel-Buffering: no` 的默认处理，避免缓存层或代理层吞掉提前出流的延迟收益。

**实测：** 用 1,073,754,169 字节的受控 Blob 验证客户端在上游未结束时已收到前缀；分别阻塞 EOF 和正式发布，最后一字节始终未发送。快慢读者和冷 Range 共用一次回源；所有客户端断开后，任务仍完成校验并发布。短读、超长、摘要错误和发布失败均中止已开始的 HTTP 响应，未留正式缓存，下一次请求可重试。未知总长等发布后才响应；冷、在途和已缓存三个状态的 Blob HEAD 均只得到 Zot 的结果。

保留的 JWT/TLS 环境已切到新版，仍使用同一个持久缓存目录。本机 Docker Engine 冷拉 `quay.io/libpod/alpine:latest`，本机 Skopeo 冷拉 `quay.io/libpod/busybox:latest` 并验证层摘要；Zot、docker-auth 和 Nginx 均保留。

### 上游 Range 续传

上游仍先收到一个不带 Range 的完整顺序 GET。只有响应体中断、已暂存至少一字节但尚未到总长、总长可信、内容编码为 identity（或未声明），且原响应有一个强 `ETag` 时，任务才会续传一次。Mirror 对原最终资源 URL（包括原查询参数）发送 `Range: bytes=<已暂存字节数>-` 和 `If-Range: <原 ETag>`；沿用同一个匿名上游客户端、暂存文件、摘要状态、配额和超时。续传不跟随新重定向，也不把客户端的 Range 或身份送往上游。

只接受同一强 `ETag` 的单区间 `206`，`Content-Range` 必须覆盖准确偏移至原总长的全部剩余部分，并核对可用的 `Content-Length`、实际字节数和 EOF。返回 `200`、验证器或编码变化、区间错误时不读取响应体、不拼接；符合头部条件的 `206` 若再次中断或长度不符，也不会发布。失败时，已出流的客户端会收到中断，下次请求从零开始。上游无需提供分段 checksum，只有全量 OCI SHA-256 和正式发布成功才解锁尾字节。没有跨进程续传或并行分片。

**实测：** 用真实 TLS 连接在写完前缀后主动断开，Mirror 从该字节偏移发送 `Range` / `If-Range`，保留原查询参数且未复制原请求的 Authorization 或 Cookie，续传后整块摘要一致。另用受控上游响应验证匹配 `206` 的尾字节在 EOF 和正式发布前扣留；`200`、错区间、弱或缺失验证器、重定向和实际体长错误均未发布，随后可重新从零下载。

新版留存入口通过本机 Docker Engine 拉取 `registry.k8s.io/pause:3.9`，以及本机 Skopeo 拉取 `docker.io/library/alpine:3.22` 并核对镜像层摘要。这两项是实际 Registry 的正常拉取回归，断线续传由上面的受控场景验收。

## Smoke：按阶段执行

先完成 [README 的本机启动](../README.md#本机启动)，在同一个 Bash 子 shell 中依次运行下列命令。每次从空 Zot 和空 Mirror 缓存开始。下述状态码、digest 和日志是**验收预期**，不是运行记录。只从上游读，唯一的 Push 和 Delete 目标是本次启动的本地 Mirror。

```bash
edge_curl() { curl --cacert "$scratch/trust/ca.crt" "$@"; }
issue_token() {
  local account=$1 scope=$2
  edge_curl -fsS --netrc-file "$scratch/$account.netrc" --get \
    --data-urlencode "service=$authority" --data-urlencode "scope=$scope" "$mirror/auth" |
    jq -er '(.token // .access_token) | select(type == "string" and length > 0)'
}
issue_headers() {
  local token
  token=$(issue_token reader "repository:$repo:pull")
  printf 'Authorization: Bearer %s\n' "$token" >"$scratch/reader.headers"
  token=$(issue_token writer "repository:$repo:pull,push")
  printf 'Authorization: Bearer %s\n' "$token" >"$scratch/writer.headers"
}
mirror_curl() { edge_curl -H "@$scratch/reader.headers" "$@"; }
for account in reader writer; do
  skopeo login --authfile "$scratch/$account.auth.json" --cert-dir "$scratch/trust" \
    --username "$account" --password-stdin "$authority" <"$scratch/$account.password"
done
printf '{"auths":{"%s":{}}}\n' "$authority" >"$scratch/docker/config.json"
docker --config "$scratch/docker" login --username reader --password-stdin \
  "$authority" <"$scratch/reader.password"
issue_headers

# 不带身份、或身份有效但没有原始仓库的 pull scope，都不是可回退的缺失。
test "$(edge_curl -sS -o /dev/null -w '%{http_code}' \
  "$mirror/v2/$repo/manifests/$tag")" = 401
wrong=$(issue_token reader 'repository:docker.io/library/busybox:pull')
printf 'Authorization: Bearer %s\n' "$wrong" >"$scratch/wrong.headers"
unset wrong
test "$(edge_curl -sS -H "@$scratch/wrong.headers" -o /dev/null -w '%{http_code}' \
  "$mirror/v2/$repo/manifests/$tag")" = 401
```

手工分阶段操作超过 token 的 `900` 秒有效期时，重新运行 `issue_headers`。

### 1. 固定公网 Manifest，验证冷 Blob HEAD

用 Skopeo 匿名直读上游的 Docker Manifest List，然后通过 Mirror 分别读取根清单与 Linux/amd64 子清单。验证根清单原始字节和子清单 digest，再从子清单取 config 和首个层的描述符。

```bash
ref="docker://$authority/$repo"
api="/v2/$repo"
oci_manifest=application/vnd.oci.image.manifest.v1+json
docker_list=application/vnd.docker.distribution.manifest.list.v2+json
docker_manifest=application/vnd.docker.distribution.manifest.v2+json
sha256_file() {
  local hash
  hash=$(sha256sum "$1")
  printf 'sha256:%s\n' "${hash%% *}"
}
http_head_code() { mirror_curl -sSI -o /dev/null -w '%{http_code}' "$1"; }
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

blob_dir="$scratch/cache/mirror-v1/blobs"

mirror_curl -fsSI -H "Accept: $docker_list" "$mirror$api/manifests/$tag" |
  tr -d '\r' | grep -Fxi "Docker-Content-Digest: $public_digest"
test -z "$(find "$blob_dir" -type f -print -quit)"

skopeo inspect --raw --authfile "$scratch/reader.auth.json" --cert-dir "$scratch/trust" \
  "$ref:$tag" >"$scratch/mirror-root.json"
cmp "$scratch/public-root.json" "$scratch/mirror-root.json"
test "$(grep -Fc 'mirror manifest published digest=' "$scratch/mirror.log")" -eq 1
test "$(mirror_curl -sS -o /dev/null -w '%{http_code}' -H "Accept: $oci_manifest" \
  "$mirror$api/manifests/$tag")" = 406
test -z "$(find "$blob_dir" -type f -print -quit)"

skopeo inspect --raw --authfile "$scratch/reader.auth.json" --cert-dir "$scratch/trust" \
  "$ref@$child_digest" >"$scratch/public-child.json"
test "$(sha256_file "$scratch/public-child.json")" = "$child_digest"
test "$(grep -Fc 'mirror manifest published digest=' "$scratch/mirror.log")" -eq 2
jq -e --arg mt "$docker_manifest" '.mediaType == $mt and (.layers | length > 0)' \
  "$scratch/public-child.json"
layer=$(jq -er '.layers[0].digest' "$scratch/public-child.json")
layer_size=$(jq -er '.layers[0].size | select(. >= 16)' "$scratch/public-child.json")
old_config=$(jq -er '.config.digest' "$scratch/public-child.json")
old_config_size=$(jq -er '.config.size | select(. >= 16)' "$scratch/public-child.json")
test -z "$(find "$blob_dir" -type f -print -quit)"
test "$(http_head_code "$mirror$api/blobs/$layer")" = 404
test -z "$(find "$blob_dir" -type f -print -quit)"
zot_http | jq -ce --arg path "$api/blobs/$layer" \
  'select(.method == "HEAD" and .path == $path and .statusCode == 404)'
```

预期：Manifest 内容逐字节相同，冷 HEAD 返回 Zot 的 `404`，Zot 日志记录该 HEAD，磁盘没有 Blob。缓存和 Zot 日志不是完整的上游网络记录；它们不能单独排除一个不写缓存的上游请求。

### 2. 按需缓存层，确认热 Blob HEAD 仍只看 Zot

先用冷 Range 拉首个层的 16 字节，再读取完整 Blob 做 SHA-256 和内容核对；接着用 Skopeo 拉完整镜像到本地目录，作为 Push 源。完成后同一层的前端 HEAD 仍须返回 `404`。

```bash
check_range() {
  local url=$1 size=$2 expected=$3
  test "$(mirror_curl -fsS --range 0-15 -D "$scratch/range.headers" \
    -o "$scratch/range.body" -w '%{http_code}' "$url")" = 206
  tr -d '\r' <"$scratch/range.headers" | grep -Fxi "Content-Range: bytes 0-15/$size"
  head -c 16 "$expected" | cmp - "$scratch/range.body"
}

# 先发 Range；等它完成后，才发完整 GET。
test "$(mirror_curl -fsS --range 0-15 -o "$scratch/cold.range" \
  -w '%{http_code}' "$mirror$api/blobs/$layer")" = 206
mirror_curl -fsS "$mirror$api/blobs/$layer" -o "$scratch/layer.blob"
test "$(sha256_file "$scratch/layer.blob")" = "$layer"
head -c 16 "$scratch/layer.blob" | cmp - "$scratch/cold.range"
test "$(find "$blob_dir" -type f | wc -l)" -eq 1
test "$(grep -Fc "mirror blob published digest=$layer " "$scratch/mirror.log")" -eq 1
check_range "$mirror$api/blobs/$layer" "$layer_size" "$scratch/layer.blob"
test "$(mirror_curl -sS --range "$layer_size-" -o /dev/null -w '%{http_code}' \
  "$mirror$api/blobs/$layer")" = 416

skopeo --override-os linux --override-arch amd64 copy --preserve-digests \
  --src-authfile "$scratch/reader.auth.json" --src-cert-dir "$scratch/trust" \
  "$ref:$tag" "dir:$scratch/public-image"
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
  --dest-authfile "$scratch/writer.auth.json" --dest-cert-dir "$scratch/trust" \
  "dir:$scratch/overlay" "$ref:$tag"
overlay_digest=$(cat "$scratch/pushed.digest")
test "$overlay_digest" != "$public_digest"
test "$overlay_digest" != "$child_digest"
# Push 完成后，第一次从 Tag Pull 就必须得到 Zot 覆盖版。
skopeo copy --preserve-digests \
  --src-authfile "$scratch/reader.auth.json" --src-cert-dir "$scratch/trust" \
  "$ref:$tag" "dir:$scratch/zot-image"
test "$(sha256_file "$scratch/zot-image/manifest.json")" = "$overlay_digest"
jq -e --arg mt "$oci_manifest" --arg layer "$layer" --arg config "$old_config" \
  '.mediaType == $mt and .layers[0].digest == $layer and .config.digest != $config' \
  "$scratch/zot-image/manifest.json"
test "$(http_head_code "$mirror$api/blobs/$layer")" = 200
image="$authority/$repo:$tag"
docker --config "$scratch/docker" pull --platform linux/amd64 "$image"
docker --config "$scratch/docker" image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' \
  "$image" | grep -Fx "$authority/$repo@$overlay_digest"

zot_http | jq -c --arg prefix "$api/" 'select(.path | startswith($prefix))'
```

对照 Zot 日志：该层的 HEAD 从 `404` 变为 `200`，其间有 Blob 上传会话完成（上传 `PUT` 的查询中可找到该层 digest）以及同名 Tag 的 Manifest `PUT` 返回 `201`。镜像已缓存但仍成功上传共享层，才满足 [0002 §6 第 2 项](../.omp/specs/0002-mirror-zot-overlay.md#6-验收) 的覆盖场景。

### 4. 按旧 digest Pull，删除覆盖后再 Pull Tag

覆盖存在时，按原根 digest 读取公网列表，再按原子清单 digest 拉完整旧镜像。Range 使用旧 config（不是已上传到 Zot 的共享层），确保此时确实读取 Mirror 的数据。最后删除本次测试写入的本地 Tag，检查 Zot 已缺失、但前端仍能拉取原公网列表与镜像。

```bash
skopeo inspect --raw --authfile "$scratch/reader.auth.json" --cert-dir "$scratch/trust" \
  "$ref@$public_digest" >"$scratch/old-root.json"
cmp "$scratch/public-root.json" "$scratch/old-root.json"
skopeo copy --preserve-digests \
  --src-authfile "$scratch/reader.auth.json" --src-cert-dir "$scratch/trust" \
  "$ref@$child_digest" "dir:$scratch/old-image"
cmp "$scratch/public-child.json" "$scratch/old-image/manifest.json"
test "$(http_head_code "$mirror$api/blobs/$old_config")" = 404
check_range "$mirror$api/blobs/$old_config" "$old_config_size" \
  "$scratch/public-image/${old_config#sha256:}"

test "$(edge_curl -fsS -H "@$scratch/writer.headers" -X DELETE -o /dev/null \
  -w '%{http_code}' "$mirror$api/manifests/$tag")" = 202
test "$(curl -sSI -H "@$scratch/reader.headers" -o /dev/null -w '%{http_code}' \
  "$zot$api/manifests/$tag")" = 404
skopeo inspect --raw --authfile "$scratch/reader.auth.json" --cert-dir "$scratch/trust" \
  "$ref:$tag" >"$scratch/fallback-root.json"
cmp "$scratch/public-root.json" "$scratch/fallback-root.json"
skopeo --override-os linux --override-arch amd64 copy --preserve-digests \
  --src-authfile "$scratch/reader.auth.json" --src-cert-dir "$scratch/trust" \
  "$ref:$tag" "dir:$scratch/fallback-image"
cmp "$scratch/public-child.json" "$scratch/fallback-image/manifest.json"
docker --config "$scratch/docker" pull --platform linux/amd64 "$image"
docker --config "$scratch/docker" image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' \
  "$image" | grep -Fx "$authority/$repo@$public_digest"
printf '公网/回退根 digest: %s\n旧子清单 digest: %s\nZot 覆盖 digest: %s\n' \
  "$public_digest" "$child_digest" "$overlay_digest"
```

### 5. Zot 不可达时不读热缓存

仅在 README 新建的临时环境（**不在留存 fixture**）检查失败路径：在清理前停止本次 Zot。尽管 Tag 已在 Mirror 缓存，入口仍应返回 `502`：

```bash
docker stop "$zot_id" >/dev/null
zot_id=
test "$(mirror_curl -sS -o /dev/null -w '%{http_code}' "$mirror$api/manifests/$tag")" = 502
```

不要在 Push 之后用共享层的 Range 探测 Mirror 的 `416`：Zot 已有这个层，响应由 Zot 决定。Zot v2.1.15 实测对该层的越界 Range 返回 `500`，Mirror 按规格透传，不回退。

检查所需日志后在子 shell 执行 `exit`，由 [README 的清理流程](../README.md#验收与清理) 停止服务并移除临时文件。要重跑，请重新创建空环境。

## 本轮 P0 实测

- 本机 Skopeo 1.22.0 一次登录完成 `r.ltkk.run/library/alpine:latest` 的冷拉、同名 OCI 覆盖、覆盖版回读、旧 digest 读取和删除后回退。错误 `aud` 的读写、错误 `iss`、过期、缺 `exp`、未来 `nbf`、错误签名、错误仓库 scope 和匿名热读均返回 `401`。
- 本机 Docker Engine 28.5.2 完成 `registry.k8s.io/pause:3.10` 的登录和 `linux/amd64` 冷拉。Zot 初次因未启用 Docker schema 2 兼容项返回 `415`；设置 `http.compat: ["docker2s2"]` 后，已验证 `p0-docker-smoke` 标签 Push 成功。宿主机的 hosts、证书和默认 Docker daemon 配置均未改动，测试使用了隔离挂载和临时 Docker daemon。
- 留存的 Zot 通过入口处理分块上传，`POST` / `PATCH` 返回 `202`；查询会话返回 `204` 和正确的 Range，Location 保持相对路径。带源和目标仓库 scope 的跨仓库 mount 由 Zot 返回 `201`。
- 默认签发 ACL 与默认 Registry allow 实测拉取未单列的 `quay.io/libpod/alpine:latest`；没有额外授权的本地短名仍返回 `401`。
- 受控上游日志显示：裸 `404`、`DENIED`、错误 detail、后端 HEAD/GET 不一致和 `429` 都未产生回源；有效缺失才回源。匿名 token realm 无入站凭据，Registry 只收到自己的上游 Bearer token，跨域 CDN 无 Authorization 或 Cookie。私网 Registry 和重定向 CDN 均须各自显式允许，deny 优先于 allow。

## 历史记录（切换前，不是上述认证流程的验收）

旧版匿名 PoC 曾在 Zot v2.1.15、Skopeo v1.22.0 上通过当时的五阶段流程。公网根清单为 `sha256:ee6521f290b2168b6e0935a181d4cff9be1ac3f505666ef0e3c98fae8199917a`，同名 Zot 覆盖为 `sha256:63881a70fa9f26ae2feb4987058dd5a0b1fb93f823b5d3d1e96a4103724aadec`；删除后字节级恢复公网根清单。旧匿名回退配置已不允许启动。

私网 DNS 例外单独验收：未配置 allowlist 或只列 `ltkk.run` 时 `r.ltkk.run` 的冷读返回 `502`；列精确主机 `r.ltkk.run` 后通过 Skopeo 完整匿名拉取 `library/alpine:latest`，Manifest 原始字节和层 SHA-256 均与上游一致。直接配置 `172.16.56.2` 为仓库主机或 allowlist 项都被拒绝。

动态回源在隔离 JWT fixture 下实测完整拉取 `registry.k8s.io/pause:3.10`、`r.ltkk.run/library/alpine:latest` 和省略 Tag 的 `docker.io/library/alpine`，并校验落盘层摘要。两个短名默认不回退，逐个批准后可拉取；三个 Docker Hub 拼写和 `r.ltkk.run` 上碰巧相同的 Blob 分别保存在四个独立缓存作用域。显式名的 JWT 不能授权短名（`401`）；Registry 同时在 allowlist 和 denylist 时不回源（`404`）。

### 复用留存的 JWT fixture

若本机已建好留存 fixture，连接配置在 `.git/oci-mirror-jwt-fixture/env`，与 README 的临时环境无关。不要运行上面的停止 Zot、删除覆盖或清理步骤；仅使用读取命令。宿主机和 Docker daemon 已能解析该域名并信任 fixture CA 时，直接使用宿主机上的 Skopeo 和 Docker：

```bash
source .git/oci-mirror-jwt-fixture/env
skopeo inspect --raw --authfile "$JWT_READER_AUTHFILE" --cert-dir "$JWT_CA_DIR" \
  "docker://$JWT_AUTHORITY/registry.k8s.io/pause:3.10"
(
  docker_config=$(mktemp -d)
  trap 'rm -rf -- "$docker_config"' EXIT
  printf '{"auths":{"%s":{}}}\n' "$JWT_AUTHORITY" >"$docker_config/config.json"
  docker --config "$docker_config" login --username "$(awk '$3 == "login" {print $4; exit}' "$JWT_READER_NETRC")" \
    --password-stdin "$JWT_AUTHORITY" <"$JWT_READER_PASSWORD_FILE"
  docker --config "$docker_config" pull --platform linux/amd64 "$JWT_AUTHORITY/registry.k8s.io/pause:3.10"
)
```

留存 fixture 没有发布宿主机端口。若宿主机未配置解析和信任，可用隔离 Docker 网络、容器内 hosts 和独立 CA 进行 Skopeo 读取；这不是宿主机 Docker 拉取的替代验收：

```bash
source .git/oci-mirror-jwt-fixture/env
docker run --rm --network "$JWT_NETWORK" --add-host "$JWT_HOST:$JWT_IP" \
  --user "$(id -u):$(id -g)" \
  -v "$JWT_CA_DIR:/trust:ro,z" -v "$JWT_READER_AUTHFILE:/auth.json:ro,z" \
  skopeo:latest inspect --raw --authfile /auth.json --cert-dir /trust \
  "docker://$JWT_AUTHORITY/registry.k8s.io/pause:3.10"
```

## 延后项与下一阶段顺序

1. **部署验证。** P0 的信任边界仅涵盖上面限定的专用 Zot 和传统 Registry Bearer JWT，不支持通用 OIDC、权限隐藏型后端、集群或远端同步。对外开放前按实际签发策略和目标版本复核错误 issuer/audience/scope、过期 token、权限隐藏和后端故障均不能触发回退；确认入站凭据不进入公网请求。
2. **多上游调度。** 现有配额限制全局与每个逻辑 Registry 的并发及暂存；尚无更细的速率策略或有界、可复用的 Bearer 上下文。
3. **续传兼容性。** 本实现只续传一次完整剩余区间，暂不支持依赖续传重定向、分段返回或多次续传的上游。普通下载仍不分片、不并行。
4. **上线验收。** 本文的验收只针对宿主机 Docker 与 Skopeo，不代表其它客户端或完整多架构矩阵。若客户端依赖 Blob HEAD `200` 才能 Pull，不能靠伪造成功响应通过验收。
