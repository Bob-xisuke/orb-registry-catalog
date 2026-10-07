# SQLite 写锁争用下登记入口的行为分析（附回归用例）

本文针对**内置 SQLite 适配器**，说明 `POST /v1/artifacts` 在**另一连接持有同一数据库写锁**时的行为：为什么新制品登记、相同重试与内容冲突在同一段持锁窗口内得到 503、201、409 三种不同结果，而非法输入仍是 400。既有三份说明（`docs/artifact-identity-analysis.md`、`docs/sqlite-transaction-visibility-analysis.md`、`docs/storage-boundary-analysis.md`）分别覆盖了身份语义、事务可见性与存储边界；本文沿同一条 `POST → 存储`、`GET → 响应` 路径补齐"写锁被占用"这一正交维度，**不改变既有读取可见性结论**。

- 源码版本：工作区当前提交（`652aabe`）
- 适用范围：**仅内置 SQLite 适配器**（`internal/store`）。自定义存储若呈现相同表现，保证来自**其自身实现**（`service.Store` 接口注释中的约定，`internal/service/service.go:124-142`），本文不为自定义存储作证。
- 验证方式标注：
  - **[已有验证]** 基线测试已覆盖，注明函数与文件
  - **[新增验证]** 本次补充的用例，位于 `internal/api/sqlite_write_lock_test.go`
  - **[源码推导]** 由源码结构直接得出（如"某分支无写语句"），不通过行为用例证明
- 本次只新增本文档与一个测试文件；README、生产代码、SQLite schema（`store.go:148-170` 的 `schema` 常量）、既有数据库、启动配置（`main.go`）与两种路由构造入口（`NewRouter`/`NewRouterWithStore`）均未改动；`GET /healthz`、输入校验规则、冲突响应、顶层 `error` 对象与固定错误消息沿用既有行为。

---

## 1. 写路径上的关键源码位置

`POST /v1/artifacts` 的处理沿四段推进：**输入校验 → 身份查找 → 登记分支 → 响应映射**。写锁争用只影响其中真正需要写锁的一段，这正是四种请求结果不同的根源。

### 1.1 输入校验（不触碰存储，不需要任何锁）

| 步骤 | 位置 | 内容 |
|---|---|---|
| 装配 | `internal/api/router.go:47` | `router.POST("/v1/artifacts", registerArtifact(svc))` |
| 解码校验 | `internal/api/artifacts.go:40-44` | `input.ParseRegistration(c.Request.Body)`；失败即 400 `InvalidArtifactInputError`，发生在**任何存储调用之前** |
| 摘要格式 | `internal/input/input.go:34,90-93` | 锚定正则 `^sha256:[0-9a-f]{64}$`，不匹配即 `ErrInvalidInput` |

### 1.2 身份查找（事务内的读，WAL 下不被写锁阻塞）

| 步骤 | 位置 | 内容 |
|---|---|---|
| 业务层入口 | `internal/service/service.go:160-169` | 生成 `pushed_at`（`:168`），调用 `store.Register`；自身不开事务 |
| 开启事务 | `internal/store/store.go:55` | `s.db.Begin()` —— 延迟事务，此时不取写锁 |
| 回滚兜底 | `store.go:59` | `defer tx.Rollback()` —— 任何错误返回路径都会回滚 |
| 身份查找 | `store.go:61-63` | 事务内 `SELECT ... WHERE repository = ? AND digest = ?` —— 这是**读**语句 |

数据库以 WAL 模式打开（`store.go:31`，`PRAGMA journal_mode=WAL`）：WAL 下读不阻塞写、写也不阻塞读，因此另一连接持有写锁时，本事务的 SELECT 照常执行。

### 1.3 登记分支（只有"新记录"分支需要写锁）

| 分支 | 位置 | 是否含写语句 |
|---|---|---|
| 新记录：插入 + 移动标签 + 提交 | `store.go:66-80`（`INSERT INTO artifacts`、`INSERT INTO tag_pointers ... ON CONFLICT ... DO UPDATE`、`tx.Commit()`） | **是** —— 需要取得 SQLite 全库唯一的写锁 |
| 全等判重：返回库中原记录 | `store.go:86-90` | **否** —— 只有比较，无写语句 |
| 内容冲突：返回冲突状态 | `store.go:92` | **否** —— 只有比较，无写语句 |

三个分支互斥，且后两个分支在函数内**不存在任何 INSERT/UPDATE/DELETE 语句**。**[源码推导]**

### 1.4 响应映射（错误归类与固定消息）

| 步骤 | 位置 | 内容 |
|---|---|---|
| 存储错误归类 | `service.go:170-172` | `store.Register` 返回**任意**非 nil 错误（含写锁错误）→ `ErrStorage`；错误检查**先于**状态检查 |
| 冲突归类 | `service.go:173-175` | `StatusConflict` → `ErrConflict` |
| 成功（含重复） | `service.go:176-179` | 返回记录与 `OutcomeCreated`/`OutcomeDuplicate` |
| HTTP 映射 | `artifacts.go:46-56` | `ErrConflict` → 409 `ArtifactConflictError`（`:48-49`）；其他错误 → 503 `storage_unavailable` + 固定 message `"database is not available"`（`:50-51`）；成功 → 201 记录回显（`:52-56`，HTTP 不区分首次与重复） |

---

## 2. 持锁窗口内四种请求为何结果不同

前提：仓库 R 中制品 A 已提交、标签 T 指向 A；另一连接持有同一数据库的写锁，但**不改变任何制品记录与标签指向**。

### 2.1 新制品 B（合法、不同摘要）→ 503 `storage_unavailable`

B 的 `(R, digestB)` 身份查找不到既有记录，进入新记录分支（`store.go:66-80`）。`INSERT INTO artifacts`（`:66-71`）要求把事务升级为写事务，而 SQLite 全库同一时间只有一个写者；写锁正被另一连接持有，驱动立即返回锁错误。处置链：

1. `Register` 返回 `"insert artifact"` 包装错误（`store.go:70`）；
2. `defer tx.Rollback()`（`store.go:59`）回滚事务——B 未留下任何行；
3. 业务层把该错误归为 `ErrStorage`（`service.go:170-172`）；
4. HTTP 层返回 **503**、顶层 `error` 对象 `{"code":"storage_unavailable","message":"database is not available"}`（`artifacts.go:50-51`）。锁错误原文（如 `database is locked`）只留在服务端包装错误里，不进入响应体。

**[新增验证]** `TestSQLiteWriteLockContentionOutcomes` 阶段 2a；503 状态码映射本身另有 **[已有验证]** `TestStorageUnavailableReturns503`（`regression_test.go:307-335`，关闭数据库文件），但该用例不覆盖"锁被另一连接持有、释放后恢复"的场景。

### 2.2 原样重试 A → 201 原记录

重试 A 的身份查找命中既有行（`store.go:61-63`），四字段全等比较（`:86-90`）走 `StatusDuplicate` 分支：返回库中原记录，**没有任何写语句**，因此全程不需要写锁——另一连接是否持锁与结果无关。SELECT 在 WAL 下不被写锁阻塞（§1.2），事务正常完成。HTTP 层对首次与重复统一回 201（`artifacts.go:52-56`），响应记录来自库中既有行，`pushed_at` 保持首次登记时的值。**[新增验证]** 同一用例阶段 2b；无锁时的判重行为另有 **[已有验证]** `TestRegisterDuplicateReturnsOriginalRecord`（`artifacts_test.go:172-190`）。

### 2.3 仅改 A 的 `size_bytes`（输入仍合法）→ 409 `ArtifactConflictError`

与 §2.2 同理：身份查找与字段比较都是读（`store.go:61-63,86-92`），差异走 `StatusConflict` 分支，无写语句、不需要写锁。业务层映射为 `ErrConflict`（`service.go:173-175`），HTTP 层返回 **409** 与固定 message（`artifacts.go:48-49`）。持锁不会把冲突降级为 503，因为冲突判定在到达任何写语句之前已经完成。**[新增验证]** 同一用例阶段 2c；无锁时的冲突行为另有 **[已有验证]** `TestRegisterConflictOnDifferentContent`（`artifacts_test.go:192-204`）与序列用例的冲突子测试（`regression_test.go:217-252`）。

### 2.4 非法摘要 → 400 `InvalidArtifactInputError`

摘要不匹配锚定正则（`input.go:34,90-93`），`ParseRegistration` 在任何存储调用之前返回 `ErrInvalidInput`（`artifacts.go:40-44`）。校验不触碰存储，锁状态完全不参与。**[新增验证]** 同一用例阶段 2d；校验先于存储的既有证据：**[已有验证]** `TestStorageUnavailableStillValidatesInput`（`regression_test.go:545-576`，存储关闭时非法输入仍是 400）。

### 2.5 小结：差异来自"是否需要写锁"，不来自锁本身

| 请求 | 到达的分支 | 需要写锁？ | 持锁窗口内的结果 |
|---|---|---|---|
| 新制品 B | 新记录（`store.go:66-80`） | 是 | **503** `storage_unavailable` |
| 原样重试 A | 全等判重（`store.go:86-90`） | 否 | **201** 原记录，`pushed_at` 不变 |
| 改 A 的 `size_bytes` | 内容冲突（`store.go:92`） | 否 | **409** `ArtifactConflictError` |
| 非法摘要 | 输入校验（`artifacts.go:40-44`） | 否（不触碰存储） | **400** `InvalidArtifactInputError` |

---

## 3. 写入失败的归类与"无需写入"的依据

1. **写入失败归为存储错误**：`store.Register` 对任何失败都返回非 nil 的包装错误（`store.go:57,70,75,79,83`），业务层不做错误文本匹配，统一归为 `ErrStorage`（`service.go:170-172`）——锁错误、约束错误、连接错误走同一条路径，HTTP 层因此只暴露固定的 `storage_unavailable` 与 `"database is not available"`（`artifacts.go:50-51`）。响应体不含锁错误原文、SQL 语句或文件路径，由 `assertErrorResponse` 的固定 message 与泄漏子串扫描核对（`regression_test.go:72-97`）。
2. **重试与冲突无需写入**：判重与冲突分支在源码中只含字段比较与返回语句（`store.go:86-92`），没有任何 INSERT/UPDATE/DELETE，也不执行 `Commit`（只读事务由 `store.go:59` 的兜底回滚收尾，空操作）。因此它们的成功**不依赖写锁可得性**，在持锁窗口内与无锁时行为一致。**[源码推导]** + **[新增验证]** 阶段 2b/2c。
3. **失败请求不改变已提交状态**：503 路径经 `store.go:59` 回滚，400/409 路径本就无写语句；四种请求之后经三种查询核对，仓库仍仅有 A、标签仍指向 A、按 A 摘要取回原记录、按 B 摘要 404。**[新增验证]** 每次请求后的 `assertOnlyCommittedA`。

---

## 4. 边界声明（与既有说明一致，不外推）

1. **读取可见性结论不变**：本文不修改 `docs/sqlite-transaction-visibility-analysis.md` 的任何结论。WAL 下读不被写锁阻塞，因此持锁期间三种查询照常返回 200/404；但**不能把"读取成功"推导成"写入一定成功"**——读路径（`store.go:96-134`，直用 `s.db` 的 autocommit SELECT）不需要写锁，写路径的新记录分支需要，§2.1 的 503 正是二者差异的体现。
2. **不推广到自定义存储**：上述"单写者""锁错误即 503"的表现属于内置 SQLite 适配器。经 `NewRouterWithStore`（`router.go:32-54`）注入的自定义 `service.Store` 若呈现相同保证，保证来自其自身实现；接口注释只约定"原子写入、已提交读取、未命中与故障可分"（`service.go:124-142`），业务层与路由没有任何代码强制锁行为。**[源码推导]**
3. **持锁连接不改变业务数据**：场景中另一连接只写 `service_metadata` 以占据写锁，制品记录与标签指向在整个持锁窗口内不变；这与"并发登记事务挂起"（可见性文档场景一）是两个不同维度。

---

## 5. 回归用例场景（`internal/api/sqlite_write_lock_test.go`）

用例 `TestSQLiteWriteLockContentionOutcomes`，全程在真实 SQLite 文件上经公开 HTTP 入口断言；第二个 `database/sql` 连接只做测试夹具工作（占据/释放写锁、探测锁状态），不通过它读取或断言任何业务结果。

- 固定数据：R = `registry-demo/control`，T = `release`；A = `sha256:` + 64 个 `a`（`size_bytes=1024`），B = `sha256:` + 64 个 `b`（`size_bytes=2048`）；`signature_verified=true, retention_days=30`；`pushed_at` 取实际返回值逐字比较，不硬编码。

### 5.1 阶段 0：登记 A（已提交基线）

经 `POST /v1/artifacts` 登记 A：**201**，`pushed_at = tA`；标签 T 指向 A。

### 5.2 阶段 1：另一连接取得写锁（不动业务数据）

测试连接开启事务并执行 `INSERT INTO service_metadata (key, value) VALUES ('write-lock-holder', 'held')`——只写元数据表，**不触碰 `artifacts` 与 `tag_pointers`**。随后 `waitForPendingWriteLock`（`sqlite_visibility_test.go:56-69`）在第三个连接上反复执行 no-op 写，直到得到 `database is locked`：从连接外部证明写锁此刻确实被持有。**[新增验证]** 持锁事实本身。

### 5.3 阶段 2：持锁窗口内的四个请求与逐次查询核对

依次发出 §2 的四个请求（B 新登记、A 原样重试、A 改 `size_bytes`、非法摘要），断言 503/201/409/400 及各自的固定 code 与 message；**每个请求之后**都经 GET 入口核对：仓库列表 **200** 仅 `[A]`、标签查询 **200** 命中 A（`pushed_at = tA`）、按 A 摘要 **200** 返回七字段原记录、按 B 摘要 **404** `ArtifactNotFoundError`。命中查询均为 200 且保持既有 `{"artifacts":[...]}` 数组结构；所有失败请求均未改变已提交状态。**[新增验证]**

### 5.4 阶段 3：释放写锁并确认

测试事务 `Rollback` 释放写锁；随后在夹具连接上执行同一条 no-op 探测写，**必须成功**——从连接外部证明锁已释放（与阶段 1 的"必须失败"互为对照）。**[新增验证]**

### 5.5 阶段 4：释放后同一 B 请求恢复成功

再次提交与阶段 2 完全相同的 B 请求体：**201**；仓库列表按首次登记顺序返回 `[A, B]`（`ORDER BY id`，`store.go:98`），A 的 `pushed_at` 仍为 `tA`；标签查询命中 B；按 A 摘要仍完整取回原记录。随后原样重试 A：**201** 原记录、`pushed_at` 不变，标签仍指向 B（判重分支无写语句，指针不回移）。**[新增验证]**；无锁时的同事实另有 **[已有验证]** `TestRegistrationSequenceIdentityAndTagPointer`（`regression_test.go:137-253`）。

---

## 6. 结论—依据—验证对照表

| # | 结论 | 源码依据 | 验证 |
|---|---|---|---|
| 1 | 输入校验先于任何存储调用；非法摘要恒为 400，与锁状态无关 | `artifacts.go:40-44`；`input.go:34,90-93` | [新增] 阶段 2d；[已有] `TestStorageUnavailableStillValidatesInput` |
| 2 | 身份查找是事务内的读，WAL 下不被另一连接的写锁阻塞 | `store.go:31,55,61-63` | [源码推导]；[新增] 阶段 2b/2c 在持锁窗口内成功返回 |
| 3 | 新记录分支含 INSERT/upsert/Commit，需要写锁；持锁时驱动返回锁错误 | `store.go:66-80` | [新增] 阶段 2a |
| 4 | 任意存储错误（含锁错误）归为 `ErrStorage` → 503 `storage_unavailable`、固定 message，不泄漏锁错误原文/SQL/路径 | `service.go:170-172`；`artifacts.go:50-51` | [新增] 阶段 2a 经 `assertErrorResponse` 核对；[已有] `TestStorageUnavailableReturns503` |
| 5 | 判重与冲突分支无写语句、不需要写锁：持锁时重试 A 仍 201 原记录（`pushed_at` 不变），改 `size_bytes` 仍 409 | `store.go:86-92`；`service.go:173-179`；`artifacts.go:48-56` | [源码推导] + [新增] 阶段 2b/2c；[已有] 判重/冲突基线用例 |
| 6 | 503/400/409 均不改变已提交状态：每次请求后列表仅 A、标签指 A、A 摘要取回原记录、B 摘要 404 | `store.go:59`（回滚）；`store.go:86-92`（无写） | [新增] 每次请求后的 `assertOnlyCommittedA` |
| 7 | 持锁与释放阶段确实成立：锁被持有时第三方探测写失败，释放后成功 | WAL 单写者语义；探测助手 `sqlite_visibility_test.go:56-69` | [新增] 阶段 1 与阶段 3 |
| 8 | 释放后同一 B 请求 201；列表 `[A, B]` 按首次登记顺序、标签命中 B、A 摘要完整取回；重试 A 仍 201 原记录且标签不回移 | `store.go:66-80,86-90,98` | [新增] 阶段 4；[已有] `TestRegistrationSequenceIdentityAndTagPointer` |
| 9 | 读取成功不能推导写入成功：读路径不需要写锁，新记录写路径需要 | 读 `store.go:96-134`（无 `Begin`）vs 写 `store.go:66-80` | [源码推导]；[新增] 持锁期间查询 200 与登记 503 并存 |
| 10 | 相同保证在自定义存储上来自其自身实现，本文结论不外推 | `service.go:124-142`；`router.go:32-54` | [源码推导]；本轮测试仅针对内置 SQLite |

---

## 7. 复现方式

```bash
go test ./...                                                   # 全部用例（基线 + 新增回归）
go test ./internal/api -run TestSQLiteWriteLockContentionOutcomes -v
go test -race -count=5 ./internal/api -run TestSQLiteWriteLockContentionOutcomes
go vet ./...
```

新增用例的被测对象始终是生产装配（`store.Open` 真实文件 + `NewRouter`），所有可观察结果都经公开 HTTP 入口（`httptest` 驱动 POST/GET）断言；第二个 `database/sql` 连接只做两件测试夹具工作——写一行 `service_metadata` 以占据写锁、用 no-op 写探测锁状态——不通过它读取或断言任何业务结果。因此任何保持公开契约与 SQLite 锁语义的内部重构都应持续通过。
