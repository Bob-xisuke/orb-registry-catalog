# 存储接入边界分析（附回归用例）

本文以 `service.Store` 端口与 `NewRouterWithStore` 装配入口为中心，说明 `POST /v1/artifacts` 与 `GET /v1/artifacts` 从输入到响应的完整路径上，**HTTP 校验、业务层、存储实现**三方的职责边界，并给出每条结论的源码依据与验证位置。登记规范化、三种查询的字段与顺序、标签指向语义等既有行为沿用基线，不在本文重复展开（见 `docs/artifact-identity-analysis.md`）。

- 源码版本：工作区当前提交（`ccbf0fb`）
- 验证方式标注：
  - **[已有测试]** 基线测试已覆盖，注明函数与文件
  - **[新增回归]** 本次补充的用例，位于 `internal/api/assembly_test.go`
  - **[源码推导]** 由源码结构直接得出，不通过行为用例证明
- 本次只新增测试与本文档；README、`NewRouter`/`NewRouterWithStore` 构造入口、启动配置（`main.go`）、SQLite schema 与既有数据行为均未改动。

---

## 1. 边界总览：三层职责划分

从 HTTP 请求到响应，路径固定经过三层，每层的职责互不越界：

| 层 | 位置 | 职责 | 不负责 |
|---|---|---|---|
| HTTP 层 | `internal/api/artifacts.go`、`internal/api/router.go` | 解码与校验请求、调用业务层、把业务哨兵错误映射为状态码与固定错误对象 | 不做业务判定，不直接触碰存储 |
| 业务层 | `internal/service/service.go` | 持有已校验输入前提、生成登记时间、把存储结果分类为 `ErrConflict`/`ErrNotFound`/`ErrStorage` | 不做传输层校验，不感知 HTTP |
| 存储实现 | `service.Store` 端口（`service.go:137-142`）；自带适配器 `internal/store/store.go` | 唯一身份判定、原子写入、已提交读取、未命中与后端故障的区分 | 不决定 HTTP 状态码 |

装配关系：`NewRouterWithStore(st service.Store, health HealthCheck)`（`router.go:32-54`）把任意 `service.Store` 实现与独立的健康检查接进同一路由；`NewRouter`（`router.go:21-23`）只是用自带 SQLite 存储及其 `Ping` 调用同一入口的兼容包装。`*store.Store` 满足端口由编译期断言固定（`store.go:18`）。

---

## 2. POST /v1/artifacts：输入到响应

入口：`registerArtifact`（`artifacts.go:158-185`）。

### 2.1 HTTP 校验：业务层与存储之前的闸门

`decodeArtifactInput`（`artifacts.go:56-110`）完成全部传输层校验：恰好一个 JSON 对象、六个必填字段、repository/tag 修剪后非空、digest 匹配锚定正则、`retention_days` 在 1..3650、`size_bytes` 非负。任一失败即 400 `InvalidArtifactInputError`（`artifacts.go:160-164`），**此时尚未调用业务层，更不会触及存储**。

**[已有测试]** `TestAssemblyEntryErrorPriority`（`assembly_test.go:218`）用带调用计数的自定义存储证明：非法输入在存储健康时不产生任何存储调用，在存储故障时 400 仍优先于 503。

### 2.2 Service.Register 的前提与时间生成

`RegisterInput` 的文档注释明确声明业务层前提：输入已由调用方校验并规范化，业务层不做传输级校验（`service.go:28-39`）。`Service.Register`（`service.go:160-180`）只做两件事：

1. 在调用存储前生成 `PushedAt: time.Now().UTC().Format(time.RFC3339)`（`service.go:168`）——推送时间由业务层生成，客户端无法提供，存储实现收到的是完整记录；
2. 把存储返回的 `(Record, RegisterStatus, error)` 分类为业务结果（见 2.4）。

### 2.3 存储实现负责：唯一身份、原子写入

`Store.Register` 的接口约定（`service.go:129-132`）要求：写入记录与移动标签指针是一次原子操作；相同内容重提交返回 `StatusDuplicate` 且不写；同身份不同内容是 `StatusConflict` 也不写。自带 SQLite 适配器在单个显式事务内完成查重、INSERT 与标签 upsert（`store.go:54-93`），身份唯一性另有 schema 的 `UNIQUE (repository, digest)` 兜底（`store.go:162`）。

注意边界：**"原子写入"是端口对接入实现的要求，不是业务层或路由提供的机制**。业务层只转发一次 `Store.Register` 调用；某个自定义实现是否真在事务中完成写入，业务层无法强制（见 §5）。

### 2.4 存储结果到响应的映射

`Service.Register` 的分类顺序（`service.go:170-179`）与 HTTP 映射（`artifacts.go:174-183`）：

| 存储返回 | 业务结果 | HTTP | code |
|---|---|---|---|
| `err != nil`（无论 status 是什么） | `ErrStorage`（`service.go:170-172`，**错误优先于状态**） | 503 | `storage_unavailable` |
| `StatusConflict`（err 为 nil） | `ErrConflict`（`service.go:173-175`） | 409 | `ArtifactConflictError` |
| `StatusCreated` / `StatusDuplicate` | `OutcomeCreated` / `OutcomeDuplicate`（`mapStatus`，`service.go:220-225`） | **201** | — |

- 首次登记与相同重试**都返回 201**（`artifacts.go:179-183` 的注释明确：created/duplicate 区分只留在业务结果内）；重试返回的是存储交还的原记录，`pushed_at` 保持首次登记值，标签指针不移动。**[已有测试]** `TestAssemblyEntryRegisterAndQuery`（`assembly_test.go:148`）、`TestRegisterCreatedThenDuplicate`（`service_test.go:59`）。
- 错误优先是固定行为：存储同时返回状态（重复或冲突）与非 nil 错误时，`service.go:170-172` 先判 `err != nil`，结果只能是 `ErrStorage` → 503 `storage_unavailable`，不可能是 201 或 409。**[新增回归]** `TestAssemblyRegisterStatusWithErrorIsStorage`（`assembly_test.go:363`）分别构造 `StatusDuplicate`+error 与 `StatusConflict`+error，断言均为 503 及固定 code/message。

---

## 3. GET /v1/artifacts：三种查询与错误分类

入口：`queryArtifacts`（`artifacts.go:187-226`）。

1. 参数校验（`artifacts.go:189-198`）：repository 必填、tag 与 digest 至多其一、digest 过同一正则；失败即 400 `InvalidArtifactInputError`，不触存储。
2. 分派（`artifacts.go:200-210` → `service.go:191-206`）：仅 repository → `Store.ListRecords`；tag → `Store.RecordByTag`；digest → `Store.RecordByDigest`。
3. 错误分类（`service.go:208-217`）与 HTTP 映射（`artifacts.go:213-217`）：

| 存储返回 | 业务结果 | HTTP | code |
|---|---|---|---|
| `errors.Is(err, ErrRecordNotFound)` 为真（**含包装后的未命中**） | `ErrNotFound`（`service.go:209-210`） | 404 | `ArtifactNotFoundError` |
| 其他非 nil error | `ErrStorage`（`service.go:211-212`） | 503 | `storage_unavailable` |
| err 为 nil 但列表为空 | `ErrNotFound`（`service.go:213-214`） | 404 | `ArtifactNotFoundError` |
| 命中 | — | 200 | `{"artifacts":[...]}` |

- 未命中哨兵是 `ErrRecordNotFound`（`service.go:118-122`），其注释明确要求存储实现用它表示"无匹配已提交记录"，而用**其他**非 nil 错误表示后端自身故障，二者不得混淆。自带适配器在 `scanRecord` 中把 `sql.ErrNoRows` 翻译成该哨兵（`store.go:140-141`）。
- 分类用 `errors.Is`，因此实现用 `fmt.Errorf("...: %w", ErrRecordNotFound)` 包装后仍被识别为未命中 → 404，而不会误判为存储故障。**[新增回归]** `TestAssemblyWrappedMissIsNotFound`（`assembly_test.go:348`）让自定义存储在标签与摘要查询中返回包装后的未命中，断言均为 404 `ArtifactNotFoundError`。
- 空列表（err 为 nil、零条记录）同样是 404。**[已有测试]** `TestEmptyRepositoryListIsNotFound`（`service_test.go:304`）；HTTP 侧 `TestAssemblyEntryRegisterAndQuery` 的未知仓库子例（`assembly_test.go:206-207`）。
- 故障与未命中不混淆：**[已有测试]** `TestStorageFailureIsNotNotFound`（`service_test.go:314`）对每个查询方法强制故障（包括"身份本就不存在"的情形），断言全部为 `ErrStorage`；HTTP 侧 503 见 `TestAssemblyEntryErrorPriority`（`assembly_test.go:242-252`）与 `TestStorageUnavailableReturns503`（`regression_test.go:307`）。

---

## 4. 健康检查与存储生命周期

- `GET /healthz` 只依据注入的 `HealthCheck`（`router.go:39-45`）：未提供检查（nil）或检查返回 nil 时，返回现有 200 响应体 `{"status":"ok","database":"ok"}`；检查返回非 nil 错误时为 503 `storage_unavailable`。健康检查在每次请求时重新调用（`HealthCheck` 类型注释，`router.go:12-16`）。
- 健康检查与制品操作互不阻断：制品链路从不调用健康检查，健康检查也不触碰存储。**[已有测试]** `TestAssemblyHealthCheckIndependent`（`assembly_test.go:260`）证明探针故障只降级 `/healthz`、存储故障只降级制品入口。
- **未提供健康检查时存储故障**的组合此前无覆盖：**[新增回归]** `TestAssemblyNoHealthCheckStoreFailure`（`assembly_test.go:385`）以 `NewRouterWithStore(st, nil)` 装配并令存储故障，断言健康请求为 200 原样响应体，而合法制品请求（登记与列表）为 503 `storage_unavailable`。
- 路由**不负责关闭注入的存储**：`NewRouterWithStore` 的注释明确所有权与生命周期留在调用方（`router.go:25-31`）；源码中路由与处理函数没有任何 `Close` 调用点，关闭只发生在 `main.go:26` 的 `defer` 与各测试自身的清理中。**[源码推导]**
- 自带 SQLite 入口的健康路径：**[已有测试]** `TestHealthzReportsOK`（`router_test.go:12`）覆盖 200；`TestStorageUnavailableReturns503`（`regression_test.go:307`）覆盖关闭数据库后的 503。

---

## 5. 边界声明：接口约定不等于所有实现的证明

以下三点是本文全部结论的适用范围，刻意不作外推：

1. **接口注释是约定，不是证明。** `service.Store` 的文档（`service.go:124-136`）要求实现满足"写入与标签移动原子完成""未命中用 `ErrRecordNotFound`、故障用其他错误"。自带 SQLite 适配器满足这些约定（事务见 `store.go:54-93`，哨兵翻译见 `store.go:140-141`），且自定义实现可凭此接入（**[已有测试]** `TestAlternativeStorePlugsIn`，`custom_store_test.go:89`）。但业务层与路由无法在调用点强制某个第三方实现真的满足事务保证——这是接入方对端口契约的承诺，不是框架验证过的事实。**[源码推导]**
2. **顺序用例只证明被测实现。** 本文引用的全部行为用例都是单连接顺序执行，证明的是自带 SQLite 适配器与测试内自定义存储的行为；它们不构成"所有 `service.Store` 实现都满足原子写入/已提交读取"的证据，也不覆盖并发交错场景。
3. **业务层分类规则与实现无关。** 2.4 与 §3 的映射表（错误优先于状态、`errors.Is` 识别包装未命中、空列表即未命中）写在 `service.go` 与 `artifacts.go` 中，对任何接入实现一视同仁；新增回归用自定义存储驱动的正是这层与实现无关的规则。

---

## 6. 错误响应形状

所有错误响应由 `writeError` 输出（`artifacts.go:38-40`）：单个顶层 `error` 对象，恰含字符串 `code` 与 `message` 两字段；message 是处理器中的静态字面量，不含 SQL、堆栈或文件路径。本文新增用例与既有回归一样，统一经 `assertErrorResponse`（`regression_test.go:72-97`）核对状态码、code、固定 message 与无泄漏子串扫描，逐条对应：

| 场景 | 状态码 / code / message | 验证 |
|---|---|---|
| 包装未命中（tag/digest） | 404 / `ArtifactNotFoundError` / `no artifact matches this query` | [新增] `TestAssemblyWrappedMissIsNotFound` |
| 状态与错误同时返回（重复/冲突） | 503 / `storage_unavailable` / `database is not available` | [新增] `TestAssemblyRegisterStatusWithErrorIsStorage` |
| 无健康检查 + 存储故障时的制品请求 | 503 / `storage_unavailable` / `database is not available` | [新增] `TestAssemblyNoHealthCheckStoreFailure` |
| 无健康检查时的 `/healthz` | 200 / `{"status":"ok","database":"ok"}` | [新增] 同上；[已有] `TestHealthzReportsOK` |

---

## 7. 结论—依据—验证对照表

| # | 结论 | 源码依据 | 验证 |
|---|---|---|---|
| 1 | HTTP 校验在业务层与存储之前；非法输入 400 且不调用存储 | `artifacts.go:56-110,160-164,189-198` | [已有] `TestAssemblyEntryErrorPriority`（调用计数为 0） |
| 2 | 业务层假设已校验输入；`pushed_at` 由业务层在调用存储前生成（UTC RFC3339） | `service.go:28-39,168` | [已有] `TestRegisterCreatedThenDuplicate`；[源码推导] 客户端字段在解码时被忽略 |
| 3 | 唯一身份、原子写入、已提交读取是存储实现在端口契约下的责任 | `service.go:124-142`；适配器 `store.go:54-93,162` | [已有] `TestAlternativeStorePlugsIn`、`TestSQLiteEntryStaysCompatibleWithExistingData`；边界见 §5 |
| 4 | 首次登记与相同重试均 201；重试保留原 `pushed_at`、不移动标签 | `artifacts.go:179-183`；`service.go:220-225` | [已有] `TestAssemblyEntryRegisterAndQuery`、`TestRegisterCreatedThenDuplicate` |
| 5 | 内容冲突 → `ErrConflict` → 409 `ArtifactConflictError` | `service.go:173-175`；`artifacts.go:175-176` | [已有] `TestAssemblyEntryRegisterAndQuery`、`TestRegisterConflictLeavesRecordAndTagUntouched`（`service_test.go:99`） |
| 6 | 状态与错误同时返回时错误优先 → `ErrStorage` → 503 `storage_unavailable` | `service.go:170-172`；`artifacts.go:177-178` | [新增] `TestAssemblyRegisterStatusWithErrorIsStorage` |
| 7 | 包装后仍可 `errors.Is` 识别的 `ErrRecordNotFound`、空列表 → `ErrNotFound` → 404 `ArtifactNotFoundError` | `service.go:118-122,209-210,213-214`；`artifacts.go:214-215` | [新增] `TestAssemblyWrappedMissIsNotFound`；[已有] `TestEmptyRepositoryListIsNotFound` |
| 8 | 其他存储错误 → `ErrStorage` → 503 `storage_unavailable`，故障不误判为未命中 | `service.go:211-212`；`artifacts.go:216-217` | [已有] `TestStorageFailureIsNotNotFound`、`TestStorageUnavailableReturns503` |
| 9 | `/healthz` 只依据健康检查：未提供或返回 nil → 200；失败 → 503；不阻断制品操作 | `router.go:12-16,39-45` | [新增] `TestAssemblyNoHealthCheckStoreFailure`；[已有] `TestAssemblyHealthCheckIndependent`、`TestHealthzReportsOK` |
| 10 | 路由不关闭注入的存储，生命周期归调用方 | `router.go:25-31`；全仓库无路由侧 `Close` 调用点 | [源码推导] |
| 11 | 错误响应为单个 error 对象（恰含 code/message），message 为固定字面量、不泄漏内部细节 | `artifacts.go:38-40` 及各静态字面量 | [已有]+[新增] 全部错误断言经 `assertErrorResponse` |

---

## 8. 复现方式

```bash
go test ./...                          # 全部用例（基线 + 新增回归）
go test ./internal/api -run 'TestAssemblyWrappedMissIsNotFound|TestAssemblyRegisterStatusWithErrorIsStorage|TestAssemblyNoHealthCheckStoreFailure' -v
go vet ./...
```

新增用例全部经由公开 HTTP 入口（`httptest` 驱动 POST/GET）与公开的 `NewRouterWithStore` 装配入口，自定义存储只依赖 `internal/service` 的导出契约，不触碰自带适配器内部，因此任何保持公开契约的内部重构都应持续通过。
