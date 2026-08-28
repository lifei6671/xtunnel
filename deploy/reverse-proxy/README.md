# XTunnel HTTPS/WSS 前置代理示例

这里的 Caddy 与 Nginx 示例只适用于 **host-native + loopback upstream** 拓扑：
前置代理和 `xtunnel-server` 运行在同一个主机网络命名空间，前置代理在公网可达
地址监听 HTTPS/WSS，XTunnel HTTP Ingress 只监听 `127.0.0.1:8081`。

```text
Public Client
    │ HTTPS / WSS
    ▼
Caddy or Nginx 0.0.0.0:443
    │ HTTP/1.1
    ▼
XTunnel HTTP Ingress 127.0.0.1:8081
```

这些文件不能原样用于两个独立容器的 Bridge Network。那种拓扑中 XTunnel 看到的
TCP Peer 不是 loopback，必须按实际固定代理地址重新设计
`http_ingress.trusted_proxies`；禁止为省事信任整个私网或 `0.0.0.0/0`。

## 配置变量

两个示例使用同一组变量：

- `XTUNNEL_PUBLIC_HOST`：公网 DNS 主机名，不带端口，例如
  `app.tunnel.example.com`。通配域名需要与实际证书和 DNS 一致。
- `XTUNNEL_HTTPS_PORT`：前置代理监听端口，生产通常为 `443`。
- `XTUNNEL_HTTP_INGRESS`：明文 HTTP upstream authority，不带 scheme，推荐
  `127.0.0.1:8081`。
- `XTUNNEL_TLS_CERT_FILE`：代理进程可读的 PEM 证书链绝对路径。
- `XTUNNEL_TLS_KEY_FILE`：代理进程可读的 PEM 私钥绝对路径。

证书和私钥由部署环境提供，不得复制到仓库、镜像层、命令行参数或日志。私钥文件
应只允许代理服务身份读取。

Server 配置应保留 loopback 边界：

```yaml
http_ingress:
  listen: "127.0.0.1:8081"
  trusted_proxies:
    - "127.0.0.1/32"
```

如果代理改用 IPv6 loopback，则 upstream、监听地址和可信代理必须一起改为 `::1`，
不能只修改其中一项。

## Caddy

`Caddyfile` 使用 Caddy 原生 `{$VAR}` 环境变量替换。配置显式把 upstream 限制为
HTTP/1.1，保留完整 Host authority，覆盖 `X-Forwarded-For/Proto/Host`，并启用
WebSocket 与 `100ms` 有限刷新间隔的流式转发。该间隔与 XTunnel HTTP Ingress
一致，也保留客户端断开向 upstream 的取消传播；`Origin` 不做改写，按客户端值
透明传递。公网请求头必须在 `10s` 内读完；upstream transport 不设置方向独立的
读写超时，由 XTunnel 的双向共享 `1h` 空闲时钟统一关闭真正空闲的隧道。

```sh
export XTUNNEL_PUBLIC_HOST=app.tunnel.example.com
export XTUNNEL_HTTPS_PORT=443
export XTUNNEL_HTTP_INGRESS=127.0.0.1:8081
export XTUNNEL_TLS_CERT_FILE=/etc/xtunnel/tls/fullchain.pem
export XTUNNEL_TLS_KEY_FILE=/etc/xtunnel/tls/privkey.pem

caddy validate --config deploy/reverse-proxy/Caddyfile --adapter caddyfile
caddy run --config deploy/reverse-proxy/Caddyfile --adapter caddyfile
```

## Nginx

Nginx 不直接展开这些环境变量。先只替换列出的五个部署变量，保留 `$http_host`、
`$remote_addr`、`$scheme` 和 `$http_upgrade` 等 Nginx 运行时变量：

```sh
export XTUNNEL_PUBLIC_HOST=app.tunnel.example.com
export XTUNNEL_HTTPS_PORT=443
export XTUNNEL_HTTP_INGRESS=127.0.0.1:8081
export XTUNNEL_TLS_CERT_FILE=/etc/xtunnel/tls/fullchain.pem
export XTUNNEL_TLS_KEY_FILE=/etc/xtunnel/tls/privkey.pem

envsubst '${XTUNNEL_PUBLIC_HOST} ${XTUNNEL_HTTPS_PORT} ${XTUNNEL_HTTP_INGRESS} ${XTUNNEL_TLS_CERT_FILE} ${XTUNNEL_TLS_KEY_FILE}' \
  < deploy/reverse-proxy/nginx.conf.template \
  > /run/xtunnel/nginx.conf

nginx -t -c /run/xtunnel/nginx.conf
nginx -c /run/xtunnel/nginx.conf
```

该模板是包含 `events`/`http` 的完整 main config。使用官方 Nginx 容器入口时，应将
它挂载为 `/etc/nginx/templates/nginx.conf.template`，并把
`NGINX_ENVSUBST_OUTPUT_DIR` 设为 `/etc/nginx`；不要生成到只允许 `http` 子级指令的
默认 `conf.d`。镜像版本与不可变摘要由实际部署或 CI 单独固定，本目录不维护第二套
Compose 或镜像版本权威。

模板使用 `client_max_body_size 0` 关闭 Nginx 自身的请求体大小裁决。请求体唯一上限
由 `configs/server.schema.json` 的 `limits.max_http_body_bytes` 定义，并由 XTunnel
HTTP Ingress 返回稳定的 `413 REQUEST_BODY_TOO_LARGE`；不要在前置代理另设更小的
固定值覆盖该契约。

公网请求头必须在 `10s` 内读完。`large_client_header_buffers 4 1m` 保证单个 Header
可以达到 Server Schema 允许的最大 `1 MiB`，但不会替代 XTunnel 对所有 Header
聚合大小的最终裁决。

标准 Nginx HTTP Proxy 只能分别设置 upstream 读、写超时，不能精确表达 XTunnel
“任一方向有字节进展就同时刷新”的共享空闲时钟。模板因此把两个代理侧 ceiling
设为 `24d`：正常双向静默会先被 XTunnel 的 `1h` 空闲时钟关闭，而不会被前置代理
提前截断。若业务允许持续超过 24 天的严格单向 WebSocket 流，必须让对端周期性发送
反向 heartbeat，或者使用上面的 Caddy 示例；不要把 `24d` 解释成 XTunnel 的业务
空闲策略。

## 验证下限

语法校验只是第一步。上线前至少通过以下真实链路检查：

1. 使用受信证书完成 HTTPS 握手，并确认 XTunnel upstream 收到的是明文 HTTP/1.1。
2. 使用非默认测试端口时，Origin 仍收到完整 `Host: host:port`。
3. 客户端注入伪造 `X-Forwarded-*` 后，Origin 只收到代理覆盖并由 XTunnel 重建的
   单组 `X-Forwarded-For/Proto/Host`；`Origin` Header 保持不变。
4. WSS 返回 `101 Switching Protocols` 后，客户端和 Origin 可以双向传输帧。
5. 小块 streaming response 与大请求体不会被前置代理整包缓冲。
6. 客户端在响应未结束时断开后，前置代理会取消 upstream 请求并关闭连接。

Caddy 的 `flush_interval 100ms` 是有限刷新间隔，不得改为负值；负值低延迟模式会在
客户端提前断开时继续执行 upstream 请求，可能延迟 XTunnel WorkConn 与 ACTIVE Lease
归还。Caddy 不设置 upstream 读写 ceiling；Nginx 的 `24d` 只是其方向独立超时模型下
的外部安全上限。两者都不得替代 XTunnel 的双向共享 `1h` 空闲裁决。
