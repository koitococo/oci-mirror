# OCI Mirror + Zot 覆盖 PoC

同一个引用（例如 `mirror-poc.test:15443/registry.k8s.io/pause:3.10`）可以先从公网 Pull，再由正常 Push 写入 Zot 并覆盖该 Tag。删除 Zot 的覆盖后，入口重新返回 Mirror 固定的公网版本。Mirror 缓存不写入 Zot，也不会让 Push 误以为 Zot 已有某个 Blob。

**回退必须认证。** 客户端只连接受信任的 HTTPS 入口；Mirror 与专用 Zot 之间走私有直连回环。Zot 是本地存储和写入的权威，Mirror 不会用缓存代替 Zot 的上传检查。不带 JWT 配置只能运行仅转发模式，不能启用回退。

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
  - match: {account: '/.+/', type: repository, name: '/^[a-z0-9-]+([.][a-z0-9-]+)+[/].+$/'}
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

"$scratch/oci-mirror" -listen 127.0.0.1:15000 -poc-zot "$zot" \
  -zot-config "$scratch/zot.json" -jwt-issuer "$issuer" \
  -poc-enable-fallback -poc-public-repo "$repo" -cache-dir "$scratch/cache" \
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

该固定版本的 [Zot 镜像](https://github.com/project-zot/zot/blob/v2.1.15/build/stacker.yaml) 默认执行 `serve /etc/zot/config.json`。Zot 与 Mirror 使用同一个不可变 JSON 和同一个绝对路径下的公开签名证书。示例把临时 JSON、证书只读挂入 Zot，宿主机 Mirror 读取只读临时文件；实际部署应在两端均只读挂载同一配置和证书。签名私钥仅供签发端使用。`examples/zot-poc.json` 单独使用是匿名 **proxy-only** 配置；上面先派生认证配置，绝不要用原文件直接开启回退。

这条默认 `pull` ACL 只覆盖带显式 Registry 域名的原始仓库名，不给普通本地短名自动授权。同名 Zot 覆盖与该仓库共享 `pull` 权限；需要保护的显式仓库必须在默认规则之前配置更具体的 ACL。经典 Docker Engine Push 如收到 Zot `415`，需在 Zot 的 `http` 中配置 `compat: ["docker2s2"]`；Mirror 不转换写入格式。

可将 `repo`、`tag` 改为 `r.ltkk.run/library/alpine`、`latest`，并额外传 `-poc-private-dns-host r.ltkk.run`。这只是启动示例的替代源；后面的多架构验收流程使用 `pause`。

### HTTP Basic 模式

也可将专用 Zot 配置改为仅使用 `http.auth.htpasswd.path`（绝对路径），将 htpasswd 文件只读挂载给 Zot，并从 Mirror 启动命令移除 `-jwt-issuer`。保留 `-zot-config`，客户端从 HTTPS 入口执行 Registry 登录；该模式不需要 docker-auth 或 `/auth` 路由。按需在 Zot `http.accessControl` 授予原仓库名的 `read` 权限；拒绝结果不会命中任何公网回源或 Mirror 缓存。不要混配 Basic、Bearer 或 OIDC。

## 动态上游

通过下游授权、确认是可信本地缺失后，启用 `-poc-enable-fallback` 且省略 `-poc-public-repo` 即默认允许符合出站策略的显式 Registry，例如 `docker.io/library/alpine` 或 `registry.k8s.io/pause`。`-poc-allow-registry`、`-poc-deny-registry` 都可重复：未设置 allow 列表时不额外限制 Registry；配置 deny 时 deny 始终优先。只开放几个仓库时，重复 `-poc-public-repo` 限定范围；`alpine`、`library/alpine` 等短名必须逐个用该选项批准。不开 `-poc-enable-fallback` 时仅转发 Zot。

规范化只用于确定回源位置：Docker Hub 的 API 是 `registry-1.docker.io`，官方镜像补 `library/`；Zot 路径、JWT scope 和覆盖 Tag 保持客户端原仓库名，缓存同时按原仓库名和规范化来源隔离。上游须支持匿名 HTTPS/443，不能用路径指定端口、IP 或上游凭据。

匿名上游的连接池和同仓库 pull token 可复用；速率与鉴权上下文数有界，见[出站参数](docs/poc.md#匿名上游与出站策略)。

## 持久缓存

启用回退时，将 `-cache-dir` 指向本机专用持久目录，只交给一个 Mirror 进程使用。重启会恢复固定的 Manifest 和 Tag；Blob 可按容量淘汰，固定 Tag 不保证依赖全部可离线读取。新布局不读取、不迁移也不删除旧缓存文件。[目录布局、配额参数和运维限制](docs/poc.md#持久缓存与配额)。

长度可信的完整冷 Blob GET 可以提前收到前缀；末尾至少一字节只会在校验和正式发布成功后发送。Range 和条件请求仍等正式缓存。前置代理须关闭 Registry 响应缓冲，或尊重 Mirror 的 `X-Accel-Buffering: no`；上面的 Nginx 示例已关闭 `/v2/` 的缓冲。

## 验收与清理

保持在同一个子 shell，按 [PoC 架构与 smoke 步骤](docs/poc.md) 顺序操作。该流程先读 Manifest、按需缓存公网层，随后从本地目录向同名引用 Push，最后验证覆盖、旧 digest 和删除后回退。预期结果不等于实测报告。

规格完工状态与上线门槛见 [spec 0002 Checklist](docs/spec-0002-checklist.md)。

结束时在子 shell 执行 `exit`。清理仅停止这次创建的 Mirror、Zot、docker-auth 和 Nginx，删除临时缓存、账号资料和本次 Zot 容器的可写层，不改宿主机解析/信任配置或留存的 JWT fixture。
