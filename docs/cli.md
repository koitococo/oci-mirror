# OCI Mirror 命令行参数参考

第一次配置只需要 `-backend` 和 `-cache-dir`。直接在宿主机运行时，Mirror 默认只监听本机；在容器内通常还要加 `-listen=0.0.0.0:15000`，让同一容器网络里的入口能访问它。

下面的默认值指**程序不传该参数时**的值，不是某个部署的现有设置。容量参数用十进制字节数，例如 1 GiB 写成 `1073741824`，不能直接写 `1GiB`。时间参数需要单位，例如 `30m`、`24h`；只有 `0` 可以不带单位。

## 监听、后端和存储

| 参数 | 默认 | 怎么用 |
| --- | --- | --- |
| `-listen` | `127.0.0.1:15000` | Mirror 接收请求的 IP 和端口。容器内要让其他容器访问时，可用 `0.0.0.0:15000`；这不会自动发布宿主机端口。 |
| `-backend` | 必填 | 先查、也负责写入的 Registry 地址，例如 `http://zot:5000`。只写协议、主机和端口，不加 `/v2/`、路径或凭据。 |
| `-cache-dir` | 必填 | Mirror 自己的落盘目录。容器内建议挂持久卷；目录必须可写，同一目录只交给一个 Mirror 进程，不能与 Zot 的数据目录共用。 |

## 允许回源的仓库和上游凭据

这组参数只管 **Mirror 到上游**。Mirror 仍先问 `-backend`；只有后端确认资源缺失且策略允许，才会使用 Mirror 缓存或回源。客户端凭据不会被转发给上游。

| 参数 | 默认 | 怎么用 |
| --- | --- | --- |
| `-allow-registry` | 不设置 | 只允许列出的上游 Registry 主机名回退。可以重复传；不传表示不启用白名单。 |
| `-deny-registry` | 不设置 | 禁止列出的上游 Registry 主机名回退。可以重复传，且优先于允许名单；常用于禁止 Mirror 回到自己的入口域名。 |
| `-private-dns-host` | 不设置 | 只给指定的上游 DNS 主机名开例外，允许它解析到 RFC 1918 私网 IPv4 或 IPv6 ULA。可重复传；不接受 IP 字面量，也不是关闭出站地址检查。普通公网回源不用配置。 |
| `-upstream-authfile` | 不设置（匿名回源） | 指向带内嵌凭据的 Docker/Skopeo `auth.json`。不自动读取默认登录文件，也不调用 credential helper。更新文件后可发送 `SIGHUP` 重载。 |
| `-upstream-authfile-watch` | `false` | Linux 上可选：通过 inotify 监听凭据文件变更，自动重载。需同时配置 `-upstream-authfile`；容器内只读挂载文件的父目录，更新时原子替换文件，不要替换父目录。 |
| `-upstream-auth-realm` | 无额外信任主机 | 为使用独立令牌服务的 Registry 增加一个可信主机，例如 `registry.example.com=auth.example.com`。可为不同 Registry 重复传；普通 Registry 和 Docker Hub 通常不用配。 |

凭据文件读不到或 JSON 整体无法解析时，保留上一份有效凭据。要移除凭据，更新为 `{"auths":{}}` 并重载。手动与自动轮换示例见 [README：可选上游认证](../README.md#可选上游认证)。

## Manifest：清单缓存什么时候刷新、能放多少

| 参数 | 默认 | 怎么用 |
| --- | --- | --- |
| `-cache-manifest-ttl` | `0`（不自动刷新） | 距上次成功回源到多久算过期。到期后的下一次回退请求才回源，不会定时刷新；回源失败不返回旧清单。上游 Tag 会变动时可设置，例如 `72h`。 |
| `-cache-manifest-bytes` | `67108864`（64 MiB） | 已保留的原始 Manifest 内容总字节上限，不是 SQLite 文件大小，也不包含镜像层。 |
| `-cache-manifest-entries` | `10000` | digest 记录和 Tag 绑定合计的条目上限。同一个仓库里，两个 Tag 指向同一个 digest，共计 3 条。 |

TTL 只控制刷新，不回收空间。Tag 指向新 digest 后，旧 digest 仍保留并占配额；超限时不能新增记录，回退请求返回 `507`。如果经常拉取变动的 Tag，不能指望缩短 TTL 来腾空间。

## Blob：正式缓存能放多少、什么时候回收

| 参数 | 默认 | 怎么用 |
| --- | --- | --- |
| `-cache-blob-bytes` | `17179869184`（16 GiB） | 下载和校验完成后，已发布 Blob 的总字节硬限额。 |
| `-cache-blob-entries` | `10000` | 已发布 Blob 的记录数硬限额。很多小层可能先撞到这项。 |
| `-cache-blob-soft-bytes` | `0`（跟随字节硬限额） | 正式缓存达到压力后，回收希望降到的字节数。可设得比硬限额低，给不能立刻淘汰的 Blob 留出余量。 |
| `-cache-blob-soft-entries` | `0`（跟随条目硬限额） | 条目数的回收目标。和字节软限额独立，任一超出都会尝试回收。 |
| `-cache-blob-ttl` | `0`（不保护） | Blob 自发布起多久内不能被淘汰。设为 `24h` 意味着至少保护 24 小时，不是 24 小时后删除。读取不续期。 |

启动和发布新 Blob 前会尝试向软水位回收，优先淘汰最久未读的 Blob，跳过正在读或仍在 TTL 保护期内的 Blob。超过软水位但仍装得进硬限额，可以发布；若无法腾出空间满足硬限额，则拒绝发布。没有定时 GC。Blob 的硬限额不包括下面的暂存预算，也不是缓存目录在磁盘上的硬限额。

## 回源下载：临时磁盘、并发与限速

| 参数 | 默认 | 怎么用 |
| --- | --- | --- |
| `-cache-staging-bytes` | `8589934592`（8 GiB） | 所有回源下载共用的 Blob 临时磁盘预留额度。多个上游同时下载，合计不能超这个数。 |
| `-cache-upstream-staging-bytes` | `4294967296`（4 GiB） | 每个逻辑 Registry 可用的 Blob 暂存额度，防止一个上游占满全局预算。 |
| `-cache-fetches` | `4` | 所有上游合计能同时进行的回源任务数。冷 Blob 下载和 Manifest 拉取或刷新共用。 |
| `-cache-upstream-fetches` | `2` | 单个逻辑 Registry 能同时进行的回源任务数；同时受全局任务数限制。 |
| `-upstream-requests-per-second` | `16` | 对单个上游 Registry 的持续 HTTP 请求速率。它限制的是每秒请求量，不是并发数或下载带宽。 |
| `-upstream-request-burst` | `16` | 单个上游 Registry 可以瞬时发出的 HTTP 请求数。突发额度用完后按持续速率补充。 |
| `-upstream-registries` | `128` | 进程最多保留多少个上游 Registry 的限速和认证状态。不是允许回源的 Registry 数量白名单。 |
| `-upstream-scopes-per-registry` | `64` | 单个 Registry 最多保留多少个可复用的仓库拉取认证会话。会频繁访问许多私有仓库时再考虑调大。 |

暂存预算在写文件前预留，下载结束后释放；预留不足返回 `507`。回源并发槽不足则返回 `503`。例如，同一上游的两个 2 GiB Blob 会用完默认的 4 GiB 单上游暂存预算；就算其他上游没占用全局预算，该上游的第三个大下载也得等下次请求重试。调大暂存预算不会提高代码中的单 Blob 大小上限（2 GiB）。Docker Hub 的别名共用逻辑上游配额。

### 示例：全局暂存 32 GiB、每上游 16 GiB

```bash
oci-mirror -listen=0.0.0.0:15000 \
  -backend=http://zot:5000 -cache-dir=/var/lib/oci-mirror/cache \
  -cache-staging-bytes=34359738368 \
  -cache-upstream-staging-bytes=17179869184
```

此例只放宽暂存空间；如果希望同时下载更多对象，还要核对 `-cache-fetches` 和 `-cache-upstream-fetches`。生产部署细节见 [PoC 运维说明](poc.md#持久缓存与配额)。
