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

## 路由装配入口

HTTP 层只依赖业务层的存储契约 `service.Store`，路由构造不打开数据库，也不关闭调用方传入的存储：

- `api.NewRouter(st service.Store) *gin.Engine`：只装配制品入口。自备存储只需满足 `service.Store`（`Register`、`ListRecords`、`RecordByDigest`、`RecordByTag`），不必实现 `Ping`，也不必使用 SQLite 或 `database/sql`；此时不提供 `GET /healthz`（与其他未知路径一样返回 `route_not_found`）。
- `api.NewRouterWithHealth(st service.Store, check api.HealthChecker) *gin.Engine`：在同样的制品入口之外，额外以调用方提供的 `func() error` 装配 `GET /healthz`，每次检查只依据该函数的返回值，成功为 nil、失败为任意 error。健康检查与制品登记/查询互不前置：检查失败不阻断仍可成功的制品请求，存储故障也不替代检查结果。

`NewRouter` 的既有调用方式保持可用；默认启动仍按 `ADDR` 与 `DB_PATH` 打开 SQLite，并以 `st.Ping` 作为健康检查，数据库句柄由 `main.go` 自行关闭。

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

登记一个制品。请求体是单个 JSON 对象，必填字段：`repository`、`digest`、`tag`、`signature_verified`、`retention_days`、`size_bytes`，未知字段忽略。校验规则：

- `repository`、`tag`：去除首尾空白后非空，区分大小写
- `digest`：`sha256:` 加 64 位小写十六进制字符
- `signature_verified`：布尔值
- `retention_days`：1 至 3650 的整数（仅登记，不触发自动删除）
- `size_bytes`：非负有符号 64 位整数

首次登记返回 HTTP 201，响应为六个登记字段及服务生成的 `pushed_at`（UTC，RFC3339）。记录以 `(repository, digest)` 为唯一身份，登记后不可修改：

- 规范化后内容完全相同的重复提交返回 201 和原记录，`pushed_at` 不变，标签指向不变
- 内容不同返回 HTTP 409 与 `ArtifactConflictError`
- 不同摘要复用同一标签时，标签指向新记录，旧记录仍可按摘要查询

输入非法时返回 HTTP 400 与 `InvalidArtifactInputError`，不留下任何记录。

### `GET /v1/artifacts`

查询制品。`repository` 必填，`tag` 与 `digest` 可选其一、不能同时提供，参数沿用登记校验规则：

- 仅 `repository`：返回该仓库全部记录，按首次登记先后排列
- 加 `tag`：返回该标签当前指向的记录
- 加 `digest`：返回对应记录

成功时 HTTP 200，响应为 `{"artifacts":[...]}`。参数非法返回 HTTP 400 与 `InvalidArtifactInputError`；合法查询无结果返回 HTTP 404 与 `ArtifactNotFoundError`。

## 错误约定

所有错误响应都是单个顶层 `error` 对象，包含 `code` 与 `message` 两个字符串字段；`message` 不包含 SQL、堆栈或文件路径。
