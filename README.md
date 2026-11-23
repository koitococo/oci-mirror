# OCI Registry Mirror

这是一个部署在可信内网的通用 Registry Mirror。客户端到 Mirror 的下游认证和仓库授权由后端负责；Mirror 到上游 Registry 的回源认证默认匿名，可单独配置拉取凭据。

## 后端与信任边界

同一个引用（例如 `mirror-poc.test:15443/registry.k8s.io/pause:3.10`）可以先从公网 Pull，再由正常 Push 写入 Zot 并覆盖该 Tag。删除 Zot 的覆盖后，若允许回退，入口重新使用 Mirror 缓存；默认关闭 Manifest TTL 时，返回已固定的公网版本。Mirror 缓存不写入 Zot，也不会让 Push 误以为 Zot 已有某个 Blob。

`-backend` 指定受操作者信任的私有 HTTP/HTTPS Registry origin（可使用 IP 或 DNS 名称；在可信容器网络以外应使用 HTTPS），`-cache-dir` 必填。`-listen` 接受 IP 字面量和端口，默认 `127.0.0.1:15000`；在独立的 Docker 网络中可显式绑定 `0.0.0.0:15000`，由 ingress 访问而不发布宿主机端口。后端可使用符合下述契约的 Zot 或 Distribution。客户端连接受信任的 HTTPS 入口；Mirror 原样转发正常的 `/v2/` 探测与具体资源请求的 `Authorization`，并透传后端认证挑战。Mirror 不验证下游凭据、不读取后端 JWT 证书或 issuer，也不会把 `/v2/` 成功当作具体仓库的拉取授权。客户端的 `Authorization` 不会转发给上游。

后端是本地存储和写入的权威：须先执行具体资源的鉴权再查找，不得把拒绝或故障伪装成类型化缺失。使用本地优先语义时，不能启用后端自身的代理缓存（如 Distribution proxy 或 Zot Sync）。Mirror 不用缓存代替后端上传检查，也不重写 `Location`；Distribution 应配置 `http.relativeurls: true` 或将 `http.host` 指向客户端可达的公开入口。

Distribution 2.8.3 与 3.1.2 均已实测公网回退。Mirror 兼容 v2 的对象和 v3 的字符串缺失详情，但只在详情与请求资源精确匹配时回退。

## 容器构建与 Compose 示例

在 Linux Docker Engine 上，从仓库根目录执行：

```bash
docker buildx bake --load
docker compose up -d --no-build
curl -fsS --retry 10 --retry-all-errors http://127.0.0.1:15000/v2/
```

Bake 通过 [Dockerfile](Dockerfile) 构建镜像，默认标签为 `oci-mirror:local`。可在构建和启动前用 `OCI_MIRROR_IMAGE` 覆盖标签，也可以直接用 `docker compose up -d --build`。

[Compose 示例](compose.yaml) 在独立的 Docker 网络里启动 Mirror 和 Distribution 3.1.2，按服务名直连后端。Mirror 和 Registry 分别只发布到宿主机的 `127.0.0.1:15000` 和 `127.0.0.1:15001`，各用独立的命名卷；`docker compose down` 保留数据。该示例使用无认证的本机 HTTP，不应直接开放到其他机器；需要对外提供服务时，使用 HTTPS 入口和有认证的后端。

可用 Skopeo 验证真实回源（`--src-tls-verify=false` 仅用于这个回环 HTTP 示例）：

```bash
skopeo --override-os linux --override-arch amd64 copy --src-tls-verify=false \
  docker://127.0.0.1:15000/registry.k8s.io/pause:3.10.1 \
  "dir:$(mktemp -d)"
```

## 本机启动

下例在可信 Linux amd64 主机上新建临时环境。需要 rootful Docker（使用 host 网络）、Go 1.25+、`curl`、`jq`、`openssl`、`htpasswd` 和 Skopeo。预先为 `mirror-poc.test` 配置解析到 `127.0.0.1`，准备对此名称有效的 TLS 证书及私钥，并让宿主机 Docker daemon 信任其 CA（例如该 Registry 的 `/etc/docker/certs.d/mirror-poc.test:15443/ca.crt`）。设置 `POC_TLS_CERT`、`POC_TLS_KEY`、`POC_TLS_CA` 为对应文件的绝对路径；不要关闭 TLS 校验。TLS 证书与下面生成的 JWT 签名证书是两套不同材料。确认 `15443`、`15000`、`15001`、`15002` 空闲，并可匿名访问 `registry.k8s.io` 的 HTTPS 上游。在仓库根目录进入专用 `bash` 子 shell，执行：

```bash
set -euo pipefail
umask 077
scratch=$(mktemp -d /tmp/oci-mirror-poc.XXXXXXXX)
zot_id= edge_id= issuer_id= mirror_pid=
cleanup() {
  local status=$? id
  if [ "$status" -ne 0 ] && [ -f "$scratch/mirror.log" ]; then
    tail -n 30 "$scratch/mirror.log" >&2
  fi
  if [ -n "$mirror_pid" ]; then
    kill -TERM "$mirror_pid" 2>/dev/null || true
    wait "$mirror_pid" || true
  fi
  for id in "$edge_id" "$zot_id" "$issuer_id"; do
    if [ -n "$id" ]; then docker stop "$id" >/dev/null || true; fi
  done
  rm -rf -- "$scratch"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

repo=registry.k8s.io/pause
tag=3.10
authority=mirror-poc.test:15443
mirror="https://$authority"
zot=http://127.0.0.1:15001
issuer=oci-mirror-poc
export NO_PROXY="127.0.0.1,mirror-poc.test${NO_PROXY:+,$NO_PROXY}"
export no_proxy="$NO_PROXY"
mkdir "$scratch/cache" "$scratch/trust" "$scratch/docker"
cp "$POC_TLS_CERT" "$scratch/edge.crt"
cp "$POC_TLS_KEY" "$scratch/edge.key"
cp "$POC_TLS_CA" "$scratch/trust/ca.crt"
openssl req -new -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=$issuer" \
  -keyout "$scratch/signing.key" -out "$scratch/signing.crt"

for account in writer reader; do
  openssl rand -hex 24 >"$scratch/$account.password"
  printf 'machine mirror-poc.test login %s password %s\n' "$account" \
    "$(cat "$scratch/$account.password")" >"$scratch/$account.netrc"
done
writer_hash=$(htpasswd -niB writer <"$scratch/writer.password")
reader_hash=$(htpasswd -niB reader <"$scratch/reader.password")
cat >"$scratch/auth.yml" <<EOF
server:
  addr: 127.0.0.1:15002
token:
  issuer: "$issuer"
  expiration: 900
  certificate: /config/signing.crt
  key: /config/signing.key
users:
  writer: {password: '${writer_hash#*:}'}
  reader: {password: '${reader_hash#*:}'}
acl:
  - match: {account: "writer", name: "$repo"}
    actions: ["pull", "push"]
  - match: {account: '/.+/', type: repository}
    actions: ["pull"]
EOF
jq --arg realm "$mirror/auth" --arg service "$authority" --arg cert "$scratch/signing.crt" \
  '.http = {address:"127.0.0.1", port:"15001", auth:{bearer:{$realm,$service,$cert}}}' \
  examples/zot-poc.json >"$scratch/zot.json"
chmod a-w "$scratch/zot.json" "$scratch/signing.crt"
cat >"$scratch/nginx.conf" <<'EOF'
events {}
http {
  access_log off;
  server {
    listen 127.0.0.1:15443 ssl;
    ssl_certificate /edge.crt;
    ssl_certificate_key /edge.key;
    client_max_body_size 0;
    proxy_http_version 1.1;
    proxy_request_buffering off;
    proxy_set_header Host $http_host;
    proxy_set_header X-Forwarded-Proto https;
    proxy_set_header Connection "";
    location /v2/ {
      proxy_buffering off;
      proxy_pass http://127.0.0.1:15000;
    }
    location = /auth { proxy_pass http://127.0.0.1:15002; }
  }
}
EOF

go build -o "$scratch/oci-mirror" ./cmd/oci-mirror
issuer_id=$(docker run --detach --rm --network host \
  -v "$scratch/auth.yml:/config/auth_config.yml:ro,Z" \
  -v "$scratch/signing.key:/config/signing.key:ro,Z" \
  -v "$scratch/signing.crt:/config/signing.crt:ro,z" \
  cesanta/docker_auth:1.14.0)
zot_id=$(docker run --detach --rm --network host \
  -v "$scratch/zot.json:/etc/zot/config.json:ro,Z" \
  -v "$scratch/signing.crt:$scratch/signing.crt:ro,z" \
  ghcr.io/project-zot/zot-linux-amd64:v2.1.15)
test "$(curl -sS --max-time 2 --retry 15 --retry-all-errors --retry-delay 1 \
  -o /dev/null -w '%{http_code}' "$zot/v2/")" = 401

"$scratch/oci-mirror" -listen 127.0.0.1:15000 -backend "$zot" \
  -cache-dir "$scratch/cache" \
  >"$scratch/mirror.log" 2>&1 &
mirror_pid=$!
edge_id=$(docker run --detach --rm --network host \
  -v "$scratch/nginx.conf:/etc/nginx/nginx.conf:ro,Z" \
  -v "$scratch/edge.crt:/edge.crt:ro,Z" -v "$scratch/edge.key:/edge.key:ro,Z" nginx:1.28)
test "$(curl -sS --cacert "$scratch/trust/ca.crt" --max-time 2 \
  --retry 15 --retry-all-errors --retry-delay 1 \
  -o /dev/null -w '%{http_code}' "$mirror/v2/")" = 401
kill -0 "$mirror_pid"
```

该固定版本的 [Zot 镜像](https://github.com/project-zot/zot/blob/v2.1.15/build/stacker.yaml) 默认执行 `serve /etc/zot/config.json`。本例将派生的 JSON 和 JWT 签名公钥证书只读挂入 Zot；签名私钥仅供 Nginx `/auth` 转发到的签发端使用。Mirror 不读取这些文件。`examples/zot-poc.json` 本身未启用认证，不要直接替代本例派生的认证配置：允许匿名读取的本地后端也允许匿名请求在可信缺失后回退，并不会关闭回退。Zot v2.1.15 曾接受同一签发密钥签出的错误 audience token，且其传统 Bearer 验证不强制 issuer；生产使用 Zot Basic 或独立验证过签名、声明和资源 scope 的 JWT 后端，不要依靠旧版 Mirror 的 JWT 检查。

本例允许已认证账号拉取任意仓库（包括短名），仅为所示仓库的 writer 额外授予 Push。需要保护的仓库应在默认规则之前配置更具体的 ACL；同名后端覆盖和上游回退共享原始仓库的读取权限。经典 Docker Engine Push 如收到 Zot `415`，需在 Zot 的 `http` 中配置 `compat: ["docker2s2"]`；Mirror 不转换写入格式。

可将 `repo`、`tag` 改为 `r.ltkk.run/library/alpine`、`latest`，并额外传 `-private-dns-host r.ltkk.run`。这只是启动示例的替代源；后面的多架构验收流程使用 `pause`。

### HTTP Basic 模式

也可将专用 Zot 配置改为仅使用 `http.auth.htpasswd.path`（绝对路径），将 htpasswd 文件只读挂载给 Zot。Mirror 启动命令不变；客户端从 HTTPS 入口执行 Registry 登录。Basic 不需要 docker-auth 或 `/auth` 路由。按需在 Zot `http.accessControl` 授予原仓库名的 `read` 权限；后端拒绝不会触发公网回源或读取 Mirror 缓存。

## 动态上游

具体资源得到可信本地缺失后，回退始终启用。`-allow-registry`、`-deny-registry` 都可重复，按规范化后的逻辑 Registry 匹配：deny 优先；allow 为空时不额外限制 Registry（包括 Docker Hub），非空时只允许列出的 Registry。上游连接仍必须通过出站策略。

规范化只用于确定回源位置：`alpine`、`library/alpine` 自动回退到 Docker Hub，API 是 `registry-1.docker.io`，官方镜像补 `library/`。后端路径、鉴权 scope 和覆盖 Tag 保持客户端原仓库名，缓存同时按原仓库名和规范化来源隔离。短名的回源映射不授予后端读取权限。上游须使用 HTTPS/443，不能用路径指定端口、IP 或上游凭据。

上游连接池和同仓库 pull token 可复用；速率与鉴权上下文数有界，见[出站参数](docs/poc.md#上游认证与出站策略)。

### 可选上游认证

用仅有拉取权限的账号，在部署主机上通过 [Skopeo](https://github.com/podman-container-tools/skopeo/blob/main/docs/skopeo-login.1.md) 生成独立的凭据文件；下例会交互式询问用户名和密码，Docker Hub 可用访问令牌作密码：

```bash
umask 077
export UPSTREAM_AUTH_DIR="$(mktemp -d)"
skopeo login --authfile "$UPSTREAM_AUTH_DIR/auth.json" docker.io
```

将以下命令参数和挂载分别追加到现有 `services.mirror.command` 和 `services.mirror.volumes`（保留原条目）。在 Linux 上需要自动重载时，取消示例中监听开关的注释：

```yaml
services:
  mirror:
    command:
      - -upstream-authfile=/run/upstream-auth/auth.json
      # - -upstream-authfile-watch=true
    volumes:
      - ${UPSTREAM_AUTH_DIR}:/run/upstream-auth:ro
```

只读挂载专用父目录，不要单独挂载文件：原子替换会更换文件的 inode，单文件 bind mount 仍会指向旧文件。保持权限最小化，确保容器运行用户（默认 UID/GID `65532:65532`，按宿主机的用户映射换算）可遍历目录并读取文件；每次轮换也要保留所需权限。不要把凭据提交到仓库。也可使用 [Docker CLI 登录](https://docs.docker.com/reference/cli/docker/login/) 生成的 `config.json`，但凭据须内嵌在 `auths` 中（Base64 不是加密）；如果凭据由 Docker Desktop、`credsStore` 或 `credHelpers` 保存，请用上面的 Skopeo 命令生成独立文件。Mirror 不执行 helper、不搜索默认配置、不执行登录；解析时跳过无效条目。凭据匹配支持 Docker Hub 别名；更具体的仓库路径优先于上级路径或 Registry 条目。

轮换时，推荐先在同一目录完整写好新文件，再原子替换 `-upstream-authfile` 指向的文件，保持配置路径不变。默认不自动重载：对直接运行的 Mirror 执行 `kill -HUP <pid>`，或对 Compose 服务执行 `docker compose kill -s HUP mirror`。

`-upstream-authfile-watch` 默认关闭，启用时必须同时配置 `-upstream-authfile`。该开关仅支持 Linux；在非 Linux 上启用或无法建立监听时，Mirror 会启动失败。监听不轮询，只关注父目录中目标文件的同目录原子替换和关闭写入事件，启用后无需发送信号，仍可随时手动发送 `SIGHUP`。Compose 仍须挂载父目录；请勿替换或移动父目录本身，否则监听失效并记录告警，此时仍可手动发送 `SIGHUP`。

重载成功时记录 `upstream_authfile_reloaded`；之后开始的回源会用新凭据重新协商认证，已在途的回源可继续使用旧会话。Registry 限速状态和磁盘缓存都保留。

如果文件无法读取或顶层解析失败，Mirror 会保留最后一次有效的凭据快照，并记录不含文件路径或内容的 WARN。要清空凭据，请用有效的 `{"auths":{}}` 替换文件，再按上述方式重载；不要直接删除文件。

认证握手默认只信任 Registry 自身（Docker Hub 也信任 `auth.docker.io`）。确需独立令牌服务时，可用 `-upstream-auth-realm=registry.example.com=auth.example.com` 为该 Registry 增加一个精确主机名；仍适用 HTTPS/443、出站策略和 `-deny-registry`。启动时凭据文件不可用、回源所用的凭据条目不可用、上游拒绝凭据（按需先刷新 token），或握手目的地不受信任时，Mirror 告警并尝试匿名回源。匿名成功则正常返回，并按池内 pull scope 复用该选择，直至该上下文被淘汰、凭据成功重载或进程重启；匿名失败仍按原有状态处理。

上游账号由 Mirror 共享，不是客户端身份。仅在后端对原仓库名执行读取 ACL 并返回可信缺失后，才会查 Mirror 缓存或回源。已缓存的私有内容仍受该 ACL 控制；清空上游凭据不撤销客户端权限，也不会清除持久缓存。开放匿名读取的后端不应配置可读私有内容的上游账号。

## 持久缓存

将必填的 `-cache-dir` 指向本机专用持久目录，只交给一个 Mirror 进程使用。重启会恢复 Manifest 和 Tag 绑定。`-cache-manifest-ttl` 接受 Go duration（如 `30m`），默认 `0`，不刷新已缓存的 Tag；启用后，Zot 可信缺失且允许回退时，过期的 Tag 或 digest 请求必须重新回源，失败不返回旧清单。缓存了 Manifest 不保证依赖全部可离线读取。新布局不读取、不迁移也不删除旧缓存文件。

`-cache-blob-ttl` 默认 `0`（不保护）；设为 Go duration 可从 Blob 发布起暂时保护其不被配额淘汰，读命中不续期，重启不重置。到期只意味着可以被淘汰，不会主动删除。Mirror 在启动和发布新 Blob 时尽力回收到 `-cache-blob-soft-bytes` 和 `-cache-blob-soft-entries`（默认各等于对应硬限额），跳过正在读取和受 TTL 保护的 Blob。未达软水位仍可发布，只要不超过 `-cache-blob-bytes` 和 `-cache-blob-entries`；否则拒绝发布，未开始出流时返回 `507`，已出流时截断。[目录布局、配额参数和运维限制](docs/poc.md#持久缓存与配额)。

长度可信的完整冷 Blob GET 可以提前收到前缀；末尾至少一字节只会在校验和正式发布成功后发送。Range 和条件请求仍等正式缓存。前置代理须关闭 Registry 响应缓冲，或尊重 Mirror 的 `X-Accel-Buffering: no`；上面的 Nginx 示例已关闭 `/v2/` 的缓冲。

## 按日志排障

Mirror 为每个请求生成 `X-Request-ID`，在响应中返回并传给可信后端（含缺失确认请求），不会传给公网回源。先按该 ID 查 Mirror 日志；Compose 示例可用 `docker compose logs mirror`。

| 事件 | 如何解读 |
| --- | --- |
| `http_request` | 每个到达 Mirror 的 HTTP 请求完成时记录一条 INFO。包含 `method`、`url`（客户端请求的转义路径，不含查询串）和 `cache`。`hit` 表示首次查到已缓存对象，`miss` 表示首次未查到（包括等待同一次 Blob 下载），`bypass` 表示未查 Mirror 缓存（例如请求由后端直接处理），`error` 表示缓存查询失败。缓存状态不代表 HTTP 请求是否成功。 |
| `backend_response_failed` / `backend_transport_failed` | 后端返回 `5xx`，或 Mirror 未能完成后端 HTTP 请求。`source` 区分主请求和缺失确认；传输失败记录经过分类的 `reason`，不记录原始错误。 |
| `backend_fallback_skipped` | 后端 `404` 被原样返回。`reason` 区分 Registry 策略、无法识别的缺失响应和补探测没有确认；有探测时查看 `probe_method`、`probe_status` 或 `error_reason`。 |
| `manifest_request_failed` | `stage` 区分匿名鉴权初始化、刷新、限速等待、资源请求、正文读取和 Manifest 校验。`response_status` 是客户端收到的状态，与实际 `upstream_status` 分开；例如上游返回 `200` 但 digest 不匹配时，客户端仍收到 `502`。 |
| `blob_request_failed` / `blob_response_aborted` | Blob 响应未能完成。用 `request_id` 找到对应 `flight_id`；截断事件还记录 `stage`、`bytes_sent` 和 `bytes_expected`。`reason=producer_failed` 时到下载事件查根因。热 Blob 打开失败没有 `flight_id`，按 `request_id` 和 `stage` 区分目录查询、文件打开、校验和访问时间更新。 |
| `blob_download_failed` | 每个共享下载只记录一次失败根因。按 `flight_id` 查询 `stage`、`reason`、digest，以及已知的下载偏移和上游状态。配额拒绝在这条下载事件或 `manifest_request_failed` 中以 INFO 记录，`limit` 对应具体 CLI 参数（不含 `-`）；普通上游缺失和客户端取消不做默认告警。 |

为了能直接定位，日志保留有效的仓库名、Tag/digest、Registry/上游主机，以及可用的实际与预期字节数、摘要、媒体类型或 SQLite 错误码。只去掉不必要的敏感内容：不写认证头、Cookie、原始 URL/查询参数、签名 URL、验证器或响应体。普通后端 `401`/`403` 仍由后端处理，不逐条记错误。

## 验收与清理

保持在同一个子 shell，按 [PoC 架构与 smoke 步骤](docs/poc.md) 顺序操作。该流程先读 Manifest、按需缓存公网层，随后从本地目录向同名引用 Push，最后验证覆盖、旧 digest 和删除后回退。预期结果不等于实测报告。

结束时在子 shell 执行 `exit`。清理仅停止这次创建的 Mirror、Zot、docker-auth 和 Nginx，删除临时缓存、账号资料和本次 Zot 容器的可写层，不改宿主机解析/信任配置或留存的 JWT fixture。
