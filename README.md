# orb-registry-catalog

把镜像仓库、制品摘要、标签、签名与保留策略记录成可查询的服务，支持按仓库与标签查询制品来源、签名状态并追溯标签的每一次指向变化。

## 运行要求

- Go 1.26 或以上
- SQLite（本服务自带存储，不需要外部数据库）

## 构建、测试与启动

```bash
go build ./...
go test ./...
go run .
```

服务默认监听 `127.0.0.1:8080`。可用环境变量覆盖：

| 变量 | 默认值 | 用途 |
|---|---|---|
| `ADDR` | `127.0.0.1:8080` | HTTP 监听地址 |
| `DB_PATH` | `orb-registry-catalog.db` | SQLite 数据库文件路径 |

## 已公开的入口

### `GET /healthz`

返回服务与存储状态。正常时 HTTP 200：

```json
{"status":"ok","database":"ok"}
```

存储不可用时 HTTP 503：

```json
{"error":{"code":"storage_unavailable","message":"database is not available"}}
```

### `POST /v1/artifacts`

登记一个制品。请求体必须是单个 JSON 对象，以下六个字段全部必填，未知字段忽略：

| 字段 | 类型 | 规则 |
|---|---|---|
| `repository` | string | 去除首尾空白后非空，区分大小写 |
| `digest` | string | `sha256:` 加 64 位小写十六进制字符 |
| `tag` | string | 去除首尾空白后非空，区分大小写 |
| `signature_verified` | bool | — |
| `retention_days` | int | 1 至 3650 |
| `size_bytes` | int | 非负有符号 64 位整数 |

首次登记返回 HTTP 201，响应为六个登记字段及服务生成的 `pushed_at`（UTC，RFC3339）。仓库与摘要确定记录的唯一身份，登记后不可修改：重复提交规范化后内容相同的请求仍返回 201 与原记录（`pushed_at` 不变）；内容不同返回 409 与 `ArtifactConflictError`。

不同摘要可复用同一仓库的已有标签：登记成功后标签指向新记录，旧记录继续可按摘要或仓库查询；重试旧记录不会改变标签当前指向。登记与标签更新在同一事务中完成。

### `GET /v1/artifacts`

查询制品。`repository` 必填，`tag` 与 `digest` 可选且不能同时提供，参数校验规则与登记一致：

- 仅 `repository`：返回该仓库全部记录，按首次登记先后排列。
- `repository` + `tag`：返回标签当前指向的记录。
- `repository` + `digest`：返回对应记录。

成功时统一返回 HTTP 200 与 `{"artifacts":[...]}`。参数非法返回 400 与 `InvalidArtifactInputError`；合法查询无结果返回 404 与 `ArtifactNotFoundError`。

## 错误约定

所有错误响应都是单个顶层 `error` 对象，包含 `code` 与 `message` 两个字符串字段；`message` 不包含 SQL、堆栈或文件路径。
