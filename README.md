# firmware-releases

面向海上风机窄带链路的控制器固件发布与分发服务。运维平台发布固件时强制校验
SHA-256 摘要（不符即弃、绝不留存），已发布版本不可覆盖；设备端下载支持
HTTP Range 分段与断点续传，分段重组后与发布件逐字节一致。

## 设计要点

- **原子发布**：上传的 artifact 先流入数据卷下的 `.tmp/` 临时区并边收边算
  SHA-256；摘要与声明值一致后，才以 `mkdir`（O_EXCL 语义）原子占位版本目录、
  `rename` 移入成品并写入元数据。摘要不符、字段非法、版本重复时，临时文件
  立即删除，发布区不受任何污染。服务启动时会清理上次中断遗留的临时文件与
  半成品版本目录。
- **字节一致**：`ETag` 即发布时校验过的 SHA-256（带引号的强校验器）。下载由
  Go 标准库 `http.ServeContent` 提供 RFC 9110 分段语义，`Content-Length` /
  `Content-Range` 与实际字节严格一致。
- **持久化**：全部状态位于数据目录（容器内 `/data`，Compose 命名卷），
  重启后既有版本仍可下载。
- **严格 416**：不可满足或格式非法的单个 Range 一律返回 416 且必带
  `Content-Range: bytes */<size>`（含 `bytes=-0`、`bytes=abc` 等边界）。

## API

### 发布固件 — `POST /api/firmware/releases`

`multipart/form-data`，字段：`version`、`targetModel`、`sha256`（64 位十六进制）、
`artifact`（文件）。

```sh
SHA=$(sha256sum fw.bin | cut -d' ' -f1)
curl -F version=1.4.2 -F targetModel=wtg-controller-x9 -F sha256=$SHA \
     -F artifact=@fw.bin http://localhost:8080/api/firmware/releases
```

- `201`：发布成功，响应体为版本元数据 JSON，响应头带 `ETag`、`Location`。
- `400`：字段缺失、`version` 或 `sha256` 格式非法。
- `409`：版本已发布（已发布版本不可覆盖，原有字节不受影响）。
- `413`：超过上传大小限制（默认 1 GiB，`-max-upload` 可调）。
- `415`：非 multipart 请求。
- `422`：摘要不符——artifact 被丢弃，不落盘、不可下载。

### 下载固件 — `GET /api/firmware/releases/{version}/artifact`

```sh
# 完整下载（200）
curl -OJ http://localhost:8080/api/firmware/releases/1.4.2/artifact

# 闭区间 / 开放尾端 / 后缀（206，带 Content-Range）
curl -H 'Range: bytes=0-1048575'      .../artifact
curl -H 'Range: bytes=1048576-'       .../artifact
curl -H 'Range: bytes=-4096'          .../artifact

# 断点续传：ETag 未变才给分段，否则自动回退完整文件（200）
curl -H 'Range: bytes=1048576-' -H 'If-Range: "<sha256>"' .../artifact
```

- `200`：完整文件；`206`：分段；两者均带一致的 `ETag`、`Content-Length`，
  206 另带精确的 `Content-Range`，并总是带 `Accept-Ranges: bytes`。
- `416`：范围非法/不可满足，带 `Content-Range: bytes */<size>`。
- `404`：版本不存在；`400`：版本标识非法。支持 `HEAD`。

### 其他端点

- `GET /api/firmware/releases`：版本列表。
- `GET /api/firmware/releases/{version}`：版本元数据（含 `ETag` 头）。
- `GET /healthz`：健康检查。

## 运行（Docker Compose）

```sh
docker compose up -d --build app          # 默认宿主机端口 8080
FW_HOST_PORT=9090 docker compose up -d app # 宿主机端口可配置
docker compose restart app                 # 重启后既有版本仍可下载（命名卷持久化）
```

- `app` 服务配置了健康检查（`/server -healthcheck` 探测 `/healthz`）、
  持久卷 `firmware-data:/data`、可配置宿主机端口 `FW_HOST_PORT`。
- 容器内端口/数据目录可用 `PORT`、`DATA_DIR` 环境变量调整。

## 一次性验证服务 `verify`

`verify` 在 `app` 健康后运行：构建检查（`go build`/`go vet`）→ 代码测试
（`go test`）→ 针对活服务的 API 冒烟（发布 → 分段下载 → 重组校验 →
错误语义），以退出码报告结果（0 = 全部通过）：

```sh
docker compose up -d --build app
docker compose up --build --exit-code-from verify verify
echo $?   # 0 表示验证通过
```

## 本地开发

```sh
go test ./...                                  # 单元测试
go run ./cmd/server -addr :8080 -data-dir ./data
go run ./cmd/smoke                             # 对本地服务跑冒烟（APP_URL 可配）
```

## 仓库结构

```
Dockerfile            多阶段：build / verify / app（默认目标为应用镜像）
compose.yaml          app（健康检查、持久卷、可配置端口）+ verify（一次性）
cmd/server            服务入口（含 -healthcheck 探针模式）
cmd/smoke             API 冒烟：发布、分段重组、Range/ETag/If-Range、错误语义
internal/server       HTTP 实现（发布、下载、严格 416）与单元测试
scripts/verify.sh     verify 服务入口：构建检查 + 测试 + 冒烟
```
