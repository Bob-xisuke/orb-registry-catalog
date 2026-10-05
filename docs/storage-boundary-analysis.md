# 存储接入边界分析（附回归用例）

本文以 `service.Store` 与 `NewRouterWithStore` 为中心，说明 `POST /v1/artifacts` 与 `GET /v1/artifacts` 从输入到响应的完整路径上，**HTTP 校验**、**Service 层**与**存储实现**三方的职责边界，并给出与每条结论对应的验证位置。与 `docs/artifact-identity-analysis.md`（身份与标签语义）互补，本文聚焦"接入一个自定义存储时，每一层负责什么、可以依赖什么"。

- 源码版本：工作区当前提交（`ccbf0fb`）
- 验证方式标注：
  - **[已有测试]** 基线测试已覆盖，注明函数与文件
  - **[新增回归]** 本次补充的用例，位于 `internal/api/storage_boundary_test.go`
  - **[源码推导]** 由源码结构直接得出，无法或不必通过行为用例证明
- 本次只新增测试与本文档；README、既有构造入口（`NewRouter`）、启动配置（`main.go`）、SQLite schema 与既有数据均未改动；登记规范化、三种查询的字段与顺序、标签指向语义沿用基线。

---

## 1. 总览：三层职责边界

从 HTTP 请求到响应，路径固定经过三层，每层只依赖下一层的契约：

| 层 | 位置 | 职责 | 不负责 |
|---|---|---|---|
| HTTP 适配层 | `internal/api/router.go`、`internal/api/artifacts.go` | 装配（`NewRouterWithStore`）、解码与校验输入、把业务哨兵错误映射为状态码与固定错误对象 | 不生成时间、不判断重复/冲突、不操作数据库 |
| 业务层 | `internal/service/service.go` | 生成 `pushed_at`、把存储三态与错误归类为业务哨兵（`ErrConflict`/`ErrNotFound`/`ErrStorage`） | 不做传输层校验（输入已被调用方校验）、不接触具体后端 |
| 存储实现 | `service.Store` 的实现（内置 SQLite 适配器在 `internal/store/store.go`，调用方可自带） | 唯一身份、原子写入、已提交读取、区分"未命中"与"后端故障" | 不决定 HTTP 状态码、不生成 `pushed_at` |

装配点 `NewRouterWithStore(st service.Store, health HealthCheck)`（`router.go:32-54`）只做三件事：用 `service.New(st)` 包出业务层（`router.go:37`）、注册 `/healthz` 与两个制品路由（`:39-48`）、注册兜底 404（`:50-52`）。**路由不负责关闭注入的存储**：函数注释明确"ownership and lifecycle stay with the caller"（`router.go:29-30`），且路由实现中没有任何 `Close` 调用——**[源码推导]**（`main.go:26` 的 `defer st.Close()` 也印证所有权在调用方）。

---

## 2. POST /v1/artifacts：输入到响应

### 2.1 HTTP 校验（`decodeArtifactInput`，`artifacts.go:56-110`）

- 只接受恰好一个 JSON 对象；`repository`/`tag` 修剪后非空；`digest` 不修剪、必须匹配 `^sha256:[0-9a-f]{64}$`；`signature_verified` 为布尔；`retention_days` 为 `1..3650` 整数；`size_bytes` 为非负整数；未知字段忽略。
- 任一失败 → **400 `InvalidArtifactInputError`**（`artifacts.go:160-164`），发生在任何存储调用之前。
- **[已有测试]** `TestAssemblyEntryErrorPriority`（`assembly_test.go:217-253`）用带调用计数的自定义存储证明：非法输入的 400 全程 `calls == 0`（`:231-233`），且存储故障时非法输入仍得 400 而非 503。

### 2.2 Service.Register 的前提与时间生成（`service.go:160-180`）

- **已校验输入前提**：`RegisterInput` 的注释明确"the service adds no transport-level validation; it assumes non-empty repository/tag, a well-formed digest and an in-range retention period"（`service.go:28-31`）。校验是 HTTP 层的职责，服务层不重复。
- **时间生成**：`PushedAt: time.Now().UTC().Format(time.RFC3339)` 在调用存储前由服务层写入记录（`service.go:168`）。客户端与存储实现都不参与该字段的产生。
- **[已有测试]** `TestRegisterArtifactCreated`（`artifacts_test.go:77-109`）断言 RFC3339/UTC；`TestRegisterCreatedThenDuplicate`（`service_test.go:59-97`）在服务层断言重复时原 `pushed_at` 不变。

### 2.3 存储实现负责：唯一身份、原子写入、已提交读取

`Store` 接口契约（`service.go:124-142`）要求实现方保证：

1. **唯一身份**：`(repository, digest)` 唯一确定一条记录。SQLite 适配器用 `UNIQUE (repository, digest)` 兜底（`store.go:162`），查询条件始终带这两个键（`store.go:61-63`）。
2. **原子写入**：`Register` 把"写入记录 + 移动标签指针"作为一次原子操作。SQLite 适配器在同一显式事务内依次 `INSERT artifacts`（`store.go:66-71`）、upsert `tag_pointers`（`:72-77`）、`Commit`（`:78-80`），任一步失败经 `defer tx.Rollback()`（`:59`）回滚。
3. **已提交读取**：三个查询方法只返回已提交状态。SQLite 适配器每条查询是独立的 autocommit SELECT（`store.go:96-117`、`:120-124`、`:127-134`），WAL 模式（`store.go:31`）下读不阻塞写、也读不到未提交数据。
4. **未命中与故障可分**：`RecordByTag`/`RecordByDigest` 未命中返回 `ErrRecordNotFound`（`service.go:118-122`），后端自身故障返回**其他**非 nil 错误。SQLite 适配器在 `scanRecord` 中把 `sql.ErrNoRows` 映射为 `ErrRecordNotFound`（`store.go:140-141`），其余错误原样上抛（`:143`）。

> **边界声明**：以上四条中，"唯一身份、原子写入、已提交读取"对**任意** `service.Store` 实现而言只是接口注释中的约定（`service.go:124-142`），服务层与路由没有任何代码强制它们。本文引用的原子性证据全部来自 SQLite 适配器源码（`store.go:54-93`）与针对它的测试；`fakeStore`/`customStore`/`assemblyStore`/`boundaryStore` 只是按同一规则建模的测试替身。**不将接口约定或顺序用例视为所有存储都满足事务保证的证明**——自定义实现是否满足，取决于实现自身。

### 2.4 存储三态到业务结果的映射（`service.go:170-179`）

`Service.Register` 对 `Store.Register` 返回值的判读顺序固定：

1. **`err != nil` → `ErrStorage`**（`service.go:170-172`）。错误检查**先于**状态检查，因此存储同时返回状态（包括 `StatusCreated`/`StatusDuplicate`/`StatusConflict`）与错误时，结果固定为 `ErrStorage`。**[新增回归]** `TestRegisterStatusPlusErrorMapsTo503` 对三种状态逐一验证 HTTP 层得到 503 且未提交任何记录。
2. **`StatusConflict` → `ErrConflict`**（`service.go:173-175`）。
3. 其余 → `mapStatus`：`StatusDuplicate` → `OutcomeDuplicate`，否则 `OutcomeCreated`（`service.go:220-225`）。

### 2.5 业务结果到 HTTP 响应（`artifacts.go:174-183`）

| 业务结果 | HTTP | 错误对象 |
|---|---|---|
| `OutcomeCreated` / `OutcomeDuplicate` | **201** | 无；返回记录本身，重复时即原记录（原 `pushed_at`） |
| `ErrConflict` | **409** | `ArtifactConflictError`，message 为固定字面量（`artifacts.go:175-176`） |
| `ErrStorage` | **503** | `storage_unavailable`，message 固定为 `"database is not available"`（`artifacts.go:177-178`） |
| 输入非法（§2.1） | **400** | `InvalidArtifactInputError`（`artifacts.go:160-164`），不调用存储 |

- 首次登记与相同重试**均返回 201**（`artifacts.go:179-182` 注释：区分只留在服务结果内部）；重试保留原 `pushed_at`、不移动标签。**[已有测试]** `TestAssemblyEntryRegisterAndQuery`（`assembly_test.go:147-212`）、`TestRegisterDuplicateReturnsOriginalRecord`（`artifacts_test.go:172-189`）。
- 409 的固定 message 与错误对象形状：**[已有测试]** `TestAssemblyEntryRegisterAndQuery`（`assembly_test.go:196-197`）经 `assertErrorResponse` 核对。

---

## 3. GET /v1/artifacts：输入到响应

### 3.1 HTTP 校验与分派（`artifacts.go:187-210`）

- `repository` 必填且修剪后非空；`tag`（修剪）与 `digest`（不修剪、过同一锚定正则）至多给一个，空标签或非法 digest → **400 `InvalidArtifactInputError`**（`artifacts.go:194-198`），不调用存储。
- 分派：`QueryByTag` / `QueryByDigest` / `QueryByRepository`（`artifacts.go:200-210`）。

### 3.2 存储结果到业务结果的映射（`service.go:186-218`）

`Service.Query` 对三种查询统一判读（`service.go:208-217`）：

1. **`errors.Is(err, ErrRecordNotFound)` → `ErrNotFound`**（`:209-210`）。`errors.Is` 沿错误链匹配，因此存储用 `fmt.Errorf("...: %w", ErrRecordNotFound)` **包装**后的未命中仍被识别为未命中，而不是误判为故障。**[新增回归]** `TestWrappedRecordNotFoundStillMapsTo404`：自定义存储在标签与摘要查询中返回包装后的 `ErrRecordNotFound`，HTTP 层仍为 404 `ArtifactNotFoundError` 且固定 message，已提交记录不受影响。
2. **其他非 nil 错误 → `ErrStorage`**（`:211-212`）。故障永远不会被当成未命中。**[已有测试]** `TestStorageFailureIsNotNotFound`（`service_test.go:314-365`）、`TestSQLiteStorageFailureIsNotNotFound`（`sqlite_test.go:102-126`）。
3. **空列表 → `ErrNotFound`**（`:213-214`）。仓库无记录时 `ListRecords` 返回空而非错误，业务层把它归为未命中。**[已有测试]** `TestEmptyRepositoryListIsNotFound`（`service_test.go:304-309`）。

### 3.3 业务结果到 HTTP 响应（`artifacts.go:212-224`）

| 业务结果 | HTTP | 错误对象 |
|---|---|---|
| 命中 | **200** | 无；`{"artifacts":[...]}`，单记录查询也包装成长度 1 数组 |
| `ErrNotFound`（含包装后的未命中、空列表） | **404** | `ArtifactNotFoundError`，message 固定为 `"no artifact matches this query"`（`artifacts.go:214-215`） |
| `ErrStorage` | **503** | `storage_unavailable`，message 固定为 `"database is not available"`（`artifacts.go:216-217`） |
| 参数非法（§3.1） | **400** | `InvalidArtifactInputError`（`artifacts.go:196-197`），不调用存储 |

- 未包装未命中的 404：**[已有测试]** `TestQueryNotFound`（`artifacts_test.go:288-307`）、`TestClientErrorShapeAndMessages`（`regression_test.go:340-368`）。
- 503 的实际触发：**[已有测试]** `TestStorageUnavailableReturns503`（`regression_test.go:307-335`，关闭 SQLite 后三种查询均 503）、`TestAssemblyEntryErrorPriority`（`assembly_test.go:242-252`，自定义存储注入故障）。

---

## 4. 健康检查与存储的相互独立（`router.go:39-45`）

- 判定逻辑只有一个：`health != nil && health() != nil` → **503 `storage_unavailable`**（`router.go:40-42`）；否则（包括**未提供健康检查**）→ **200** `{"database":"ok","status":"ok"}`（`:44`）。
- 健康检查与制品操作互不依赖：检查失败时制品照常，存储故障时健康检查照常。**[已有测试]** `TestAssemblyHealthCheckIndependent`（`assembly_test.go:259-304`）覆盖"探针失败但存储正常"与"存储故障但探针正常"两个方向。
- **未提供健康检查**（`NewRouterWithStore(st, nil)`）时，即使存储故障，健康请求仍为 200，合法制品请求为 503。**[新增回归]** `TestNilHealthCheckWithFailingStore`：装配时传 nil 探针，存储注入故障后 `/healthz` 仍为 200 且响应体为既有固定 JSON，POST 与三种 GET 均为 503 `storage_unavailable`。
- 错误响应形状与固定 message 由 `assertErrorResponse`（`regression_test.go:72-97`）核对：error 对象恰含 `code`/`message` 两个字符串字段，message 不泄漏 SQL/路径/堆栈。

---

## 5. 结论—依据—验证对照表

| # | 结论 | 源码依据 | 验证 |
|---|---|---|---|
| 1 | 路由不关闭注入的存储，所有权在调用方 | `router.go:29-30`；路由实现无 `Close` 调用 | [源码推导]；`main.go:26` 佐证 |
| 2 | HTTP 校验失败为 400，且不调用存储 | `artifacts.go:56-110,160-164,194-198` | [已有] `TestAssemblyEntryErrorPriority` 调用计数为 0 |
| 3 | Service 假设输入已校验，不重复传输层校验 | `service.go:28-31` | [源码推导] |
| 4 | `pushed_at` 由服务层生成（UTC RFC3339），存储与客户端不参与 | `service.go:168` | [已有] `TestRegisterArtifactCreated`、`TestRegisterCreatedThenDuplicate` |
| 5 | 首次登记与相同重试均 201；重试保留原 `pushed_at`、不移动标签 | `service.go:220-225`；`store.go:86-91` | [已有] `TestAssemblyEntryRegisterAndQuery`、`TestRegisterDuplicateReturnsOriginalRecord` |
| 6 | 内容冲突 → `ErrConflict` → 409 `ArtifactConflictError`（固定 message） | `service.go:173-175`；`artifacts.go:175-176` | [已有] `TestAssemblyEntryRegisterAndQuery` |
| 7 | 存储同时返回状态与错误 → 固定 `ErrStorage` → 503 `storage_unavailable` | `service.go:170-172`（错误先于状态检查） | [新增] `TestRegisterStatusPlusErrorMapsTo503`（三种状态逐一） |
| 8 | 标签/摘要查询的未命中（含包装后的 `ErrRecordNotFound`）→ `ErrNotFound` → 404 `ArtifactNotFoundError` | `service.go:209-210`（`errors.Is` 沿链匹配） | [已有] `TestQueryNotFound`（未包装）；[新增] `TestWrappedRecordNotFoundStillMapsTo404`（包装） |
| 9 | 空列表 → `ErrNotFound` → 404 | `service.go:213-214` | [已有] `TestEmptyRepositoryListIsNotFound` |
| 10 | 其他存储错误 → `ErrStorage` → 503，不误判为未命中 | `service.go:211-212`；`store.go:140-143` | [已有] `TestStorageFailureIsNotNotFound`、`TestSQLiteStorageFailureIsNotNotFound`、`TestStorageUnavailableReturns503` |
| 11 | 健康检查独立于存储：nil 检查 → 200；检查失败 → 503 `storage_unavailable`，均不阻止制品操作 | `router.go:39-45` | [已有] `TestAssemblyHealthCheckIndependent`；[新增] `TestNilHealthCheckWithFailingStore` |
| 12 | 唯一身份、原子写入、已提交读取是存储实现的职责；SQLite 适配器满足它们 | 约定：`service.go:124-142`；实现：`store.go:54-93,96-146,162,167` | [已有] `TestSQLiteRegisterAndQuery` 等针对 SQLite 适配器；[源码推导]：不将接口约定或顺序用例视为所有存储都满足事务保证的证明 |
| 13 | 错误响应为单个 error 对象，恰含 `code`/`message`，message 为固定字面量、不泄漏内部细节 | `artifacts.go:38-40` 及各静态字面量；`router.go:41,44` | [源码推导] + [已有] `assertErrorResponse` 在各用例中核对 |

---

## 6. 复现方式

```bash
go test ./...                          # 全部用例（基线 + 新增回归）
go test ./internal/api -run 'TestWrappedRecordNotFoundStillMapsTo404|TestRegisterStatusPlusErrorMapsTo503|TestNilHealthCheckWithFailingStore' -v
go vet ./...
```

新增用例全部经由公开 HTTP 入口（`httptest` 驱动 POST/GET），自定义存储只实现导出的 `service.Store` 契约，不依赖 `internal/store` 的任何类型，因此任何保持公开契约的内部重构都应持续通过。
