# SQLite 写锁争用下的登记结果分析（附回归用例）

本文针对**内置 SQLite 适配器**，补齐 `POST /v1/artifacts` 在**另一连接持续持有同一数据库写锁**期间，四种不同输入为何分别得到 **503 / 201 / 409 / 400** 的源码依据与回归证据。既有三份说明（`docs/artifact-identity-analysis.md`、`docs/storage-boundary-analysis.md`、`docs/sqlite-transaction-visibility-analysis.md`）已覆盖"输入先于存储""三态映射""未提交/已回滚不可见"；本轮沿同一条 `校验 → 身份查找 → 登记分支 → 响应映射` 路径，标出每一步在写锁争用下是否真的需要拿写锁，并在真实 SQLite 文件上经公开 HTTP 入口断言全部结果。

- 源码版本：工作区当前提交（`652aabe`）
- 适用范围：**仅内置 SQLite 适配器**（`internal/store`）。自定义存储若呈现相同的"原子写入、已提交读取、故障与未命中可分"表现，保证来自**其自身实现**（`service.Store` 接口注释，`internal/service/service.go:124-142`），本文不为自定义存储作证。
- 验证方式标注：
  - **[已有验证]** 基线测试已覆盖，注明函数与文件
  - **[新增验证]** 本次补充的用例，位于 `internal/api/sqlite_write_lock_test.go`
  - **[源码推导]** 由源码结构直接得出，不通过行为用例证明
- 本文只新增一个文档与一个测试文件；README、生产代码、SQLite schema（`store.go:148-170` 的 `schema` 常量）、既有数据库、启动配置（`main.go`）、两种路由构造入口（`NewRouter`/`NewRouterWithStore`，`router.go:21-54`）与既有输入规则均未改动；`GET /healthz`、冲突/未命中响应、顶层 `error` 对象与固定消息沿用既有行为。

---

## 1. 路径上的关键源码位置

### 1.1 输入校验先于一切存储调用

| 步骤 | 位置 | 内容 |
|---|---|---|
| 解码校验 | `internal/api/artifacts.go:40-44` | `input.ParseRegistration(c.Request.Body)`，非法即 400 返回（`:42`），handler 自身不解析 JSON |
| 校验规则 | `internal/input/input.go:55-113` | 必填、规范化、摘要 `sha256:`+64 位小写十六进制（`:90-93`）、保留天数、`size_bytes` 范围 |
| 400 映射 | `artifacts.go:42` | 固定 `code=InvalidArtifactInputError`、message `"request body is not a valid artifact registration"` |

关键次序：**400 发生在 `svc.Register` 被调用之前**（`artifacts.go:46` 才是服务调用），因此输入是否合法与数据库当前是否有写锁**完全无关**。**[源码推导]**；行为由 **[新增验证]** §3.6 在持锁窗口内提交非法摘要仍得 400 覆盖；既有侧有 **[已有验证]** `TestRegisterArtifactRejectsInvalidInput`（`artifacts_test.go:125`）、`TestStorageUnavailableStillValidatesInput`（`regression_test.go:545`，存储关闭时仍先校验）。

### 1.2 服务层：生成时间，委托存储，只认三态

| 步骤 | 位置 | 内容 |
|---|---|---|
| 生成 pushed_at | `internal/service/service.go:160-169` | `time.Now().UTC().Format(time.RFC3339)`（`:168`），随后调用 `store.Register` |
| 存储错误归类 | `service.go:170-172` | 存储返回**任意**非 nil 错误 → `ErrStorage`；检查先于状态检查 |
| 冲突归类 | `service.go:173-175` | `status == StatusConflict` → `ErrConflict` |
| 成功 | `service.go:176-179` | 其余按存储回传的记录与 created/duplicate 状态返回 |

服务层自身**不开事务、不触碰 database/sql**（包注释 `service.go:1-9`）。

### 1.3 存储事务：三个登记分支对写锁的需求不同

`Store.Register`（`internal/store/store.go:54-93`）的全部读写在一个显式事务内：

| 分支 | 位置 | 执行的语句 | 是否需要写锁 |
|---|---|---|---|
| 开启事务 | `store.go:55-58` | `s.db.Begin()`（延迟事务，见 §2.1） | 否 |
| 回滚兜底 | `store.go:59` | `defer tx.Rollback()`，任何错误路径都回滚 | — |
| 身份查找 | `store.go:61-63` | 事务内 `SELECT … FROM artifacts WHERE repository=? AND digest=?` | **否（WAL 下读）** |
| 新身份插入 | `store.go:66-71` | `INSERT INTO artifacts …` | **是，第一条写语句** |
| 标签移动 | `store.go:72-77` | `INSERT INTO tag_pointers … ON CONFLICT … DO UPDATE` | 是 |
| 提交 | `store.go:78-80` | `tx.Commit()` | 是 |
| 查找本身出错 | `store.go:82-83` | 非 `ErrRecordNotFound` 的查询错误 → 包装返回 | — |
| **重复分支** | `store.go:86-90` | 四字段全等（tag/signature/retention/size）判定，**无任何写语句** | **否** |
| **冲突分支** | `store.go:92` | 身份已存在且内容不同 → `StatusConflict`，**无任何写语句** | **否** |

由此决定争用结果：**只有"新身份"分支会去拿写锁**；重复与冲突在一次身份读取之后即以纯内存比较结束，不进入任何 INSERT/upsert。**[源码推导]**；§3.3–3.5 的 **[新增验证]** 在同一把写锁下把三种输入并排打出 503/201/409。

### 1.4 读路径不申请写锁

`ListRecords`（`store.go:96-117`，直用 `s.db.Query`）、`RecordByDigest`（`:120-124`）、`RecordByTag`（`:127-134`，JOIN `tag_pointers`）都是连接上的独立 autocommit SELECT，从不 `Begin`；`sql.ErrNoRows` 在 `scanRecord`（`:136-146`）归一为 `service.ErrRecordNotFound`。所以"登记写不进去"的窗口里，三种 GET 始终照常工作并读到**提交前**状态。**[源码推导]**；行为由 **[新增验证]** §3.7 在持锁窗口逐请求断言，既有侧有 `docs/sqlite-transaction-visibility-analysis.md` §1.3 与 `TestSQLiteUncommittedRegistrationIsInvisible`。

### 1.5 错误到 HTTP 的映射与固定消息

`registerArtifact`（`artifacts.go:46-57`）的 switch：

| 服务结果 | 位置 | 状态 | code | 固定 message |
|---|---|---|---|---|
| `service.ErrConflict` | `artifacts.go:48-49` | 409 | `ArtifactConflictError` | `an artifact with this repository and digest already exists with different content` |
| 其他非 nil（即 `ErrStorage`） | `artifacts.go:50-51` | 503 | `storage_unavailable` | `database is not available` |
| nil（首次与重复相同） | `artifacts.go:52-55` | 201 | — | 存储记录七字段，`pushed_at` 为服务原值 |

消息由 `writeError`（`artifacts.go:20-22`）写成唯一顶层 `error` 对象（仅 `code`、`message`）；既有的 `assertErrorResponse`（`regression_test.go:72-97`）逐字固定消息并扫描 `sql/sqlite/insert/select/begin/.go///goroutine/stack` 等子串，锁错误原文、SQL、路径不外泄。**[已有验证]** 该助手；**[新增验证]** 本用例的 503/409/400 全部经它断言。

### 1.6 锁与忙等待配置：WAL 但没有 busy_timeout

1. 打开时仅执行 `PRAGMA journal_mode=WAL`（`store.go:31`）；全仓库没有第二个 PRAGMA，没有 `busy_timeout`、没有 `SetMaxOpenConns` 等池配置 **[源码推导]**（全仓搜索仅命中 `store.go:31`）。
2. modernc.org/sqlite 驱动在未设置 busy_timeout 时忙等待预算为 0：写者在场时，另一连接的写语句**立即**得到 `SQLITE_BUSY`（`database is locked`），不阻塞、不排队、不重试。
3. 因此 §3 持锁窗口内的 blocked POST 在约零等待后返回，用例无需计时即可确定 503；这一"立即失败"由 **[新增验证]** 用例在真实文件上稳定复现（`go test -count` 重复通过）。

---

## 2. 持锁窗口 × 登记输入结果总表

同一仓库 R、同一标签 T；A 已成功登记并提交；连接 2 持有该数据库的写锁但**不写 `artifacts`、不写 `tag_pointers`**（夹具只对 `service_metadata` 做一次 INSERT，见 §3.2）。窗口内四种登记输入与每次后的查询结果：

| 窗口内输入 | 走到的分支 | 需要的写语句 | POST 结果 | 每次后的仓库列表 | 标签查询 | 按 B 摘要查询 |
|---|---|---|---|---|---|---|
| 新身份 B（合法摘要、新内容） | 新登记（`store.go:65-77`） | INSERT + upsert，争抢写锁失败 | **503** `storage_unavailable` | **200** 仅 `[A]` | **200** 命中 A，`pushed_at=tA` | **404** `ArtifactNotFoundError` |
| 原样重试 A | 重复（`:86-90`） | 无（读取后全等比较） | **201** 原记录，`pushed_at` 不变 | **200** 仅 `[A]` | **200** 仍指 A | （A 摘要）**200** 原记录 |
| 仅把 A 的 `size_bytes` 改值 | 冲突（`:92`） | 无（比较不等即返回） | **409** `ArtifactConflictError` | **200** 仅 `[A]` | **200** 仍指 A | （B 摘要）**404** |
| 非法摘要 `sha256:not-hex` | 到不了服务（`artifacts.go:40-44`） | 无（校验先行） | **400** `InvalidArtifactInputError` | **200** 仅 `[A]` | **200** 仍指 A | **404** |

释放写锁后：同样的 B 请求 → **201**；列表按首次登记顺序返回 `[A, B]`；标签查询命中 B；B 摘要取回 B 记录；此后再原样重试 A 仍 **201** 且返回 A 原记录、`pushed_at=tA`，标签**不回移**。

- 503 与 409 的既有侧分别有 **[已有验证]** `TestStorageUnavailableReturns503`（`regression_test.go:307`，关闭数据库）与 `TestRegisterConflictOnDifferentContent`（`artifacts_test.go:192`），但二者都不覆盖"写锁在持、三种登记并排分化"；201 重复有 `TestRegisterDuplicateReturnsOriginalRecord`（`artifacts_test.go:172`）。分化序列本轮由 **[新增验证]** 一个用例补齐（§3）。

### 2.1 为什么延迟 BEGIN 不报错、报错点是 INSERT

写者在场时另一个登记请求的实际执行点（用真实 SQLite 文件的临时探针测得，结论并入本节，探针不保留）：

1. `s.db.Begin()`（`store.go:55`）对 modernc 驱动是**延迟事务**：此刻不获取任何锁，立即成功返回；
2. 事务内身份 SELECT（`store.go:61-63`）在 WAL 下允许一个写者与多个读者并发，**正常执行**——查 B 返回 `sql.ErrNoRows`、查 A 计数为 1，均不阻塞；
3. 新登记分支的第一条写语句 `INSERT INTO artifacts`（`store.go:66-71`）要求把事务升级为写者，而写锁已被连接 2 持有且忙等待预算为 0（§1.6）→ 立即 `database is locked (SQLITE_BUSY)`；该错误不是 `ErrRecordNotFound`，经 `store.go:82-83` 包装、`service.go:170-172` 归为 `ErrStorage`、`artifacts.go:50-51` 映射为 503。
4. `defer tx.Rollback()`（`store.go:59`）结束这个一条业务语句都没写成的事务。

重复分支停在第 2 步后的内存全等比较（`store.go:86-90`）、冲突分支停在 `:92`、非法输入停在 handler（`artifacts.go:42`）——三者都**到不了第 3 步**，所以写锁对它们不可见。

---

## 3. 持锁期间四分支的确定序列（新增用例）

用例 `TestSQLiteWriteLockContentionDistinguishesRegisterOutcomes`（`sqlite_write_lock_test.go`），全程在真实 SQLite 文件上经公开 HTTP 入口断言。夹具复用 `newVisibilityRouter`（`sqlite_visibility_test.go:33-48`，生产 `*store.Store` + 第二个连接池）与 `waitForPendingWriteLock`（`:56-69`，用另一连接的写探测从外部证明写锁在场）。

- 固定数据：R = `registry-demo/control`，T = `release`；A = `sha256:`+64 个 `a`（`size_bytes=1024`），B = `sha256:`+64 个 `b`（`size_bytes=2048`）；`signature_verified=true, retention_days=30`；`tA` 取自首次 201 响应逐字比较。

### 3.1 阶段 0：登记 A（已提交基线）

经 `POST /v1/artifacts` 登记 A：**201**，七字段回显，`pushed_at=tA`；标签 T 指向 A。

### 3.2 阶段 1：连接 2 只持写锁，不碰业务表

为确定性地复现"写者在场但业务行不变"，夹具连接 `Begin` 后对 schema 内的 `service_metadata` 表（`store.go:149-152`，**无任何生产代码读取**）执行一次：

```sql
INSERT INTO service_metadata (key, value) VALUES ('lock-contention-probe', 'held');
```

该 INSERT 无论表是否为空都会把事务升级为保留写者（对比：对空表的零行 UPDATE 不保证持锁，故不用 UPDATE）；它既不向 `artifacts` 也不向 `tag_pointers` 写一行。随即 `waitForPendingWriteLock` 在又一连接上反复 no-op 写，直到收到 `database is locked`——从连接外部证明此刻全库唯一写锁确实被持有，故随后的 POST 确实与在途写者并发，而非与空窗口并发。用例结束经 `tx.Rollback()`（及 cleanup）撤销夹具行，临时库随 `t.TempDir()` 移除。**[新增验证]**

### 3.3 持锁期间：新身份 B → 503，已提交状态不变

POST 合法的 B：**503 `storage_unavailable`**，message 固定 `"database is not available"`（错误归类与映射见 §1.2、§1.5、§2.1）。随后：

- 仓库查询 **200**，数组仅 `[A]`；
- 标签查询 **200**，命中 A 且 `pushed_at=tA`；
- 按 B 摘要查询 **404 `ArtifactNotFoundError`**——B 是合法身份，只是匹配不到任何**已提交**行（`store.go:140-141` → `service.go:209-210` → `artifacts.go:72-73`），**不是 503**：未命中与存储故障在服务层就是两个哨兵。

### 3.4 持锁期间：原样重试 A → 201 原记录

同一请求体重发 A：**201**（身份 SELECT 在 WAL 下读成功，四字段全等走重复分支，无写语句），响应七字段与阶段 0 逐字一致、`pushed_at` 仍为 `tA`；标签查询仍指 A；列表仍仅 `[A]`。**[新增验证]** 持锁场景；既有侧 **[已有验证]** `TestRegisterDuplicateReturnsOriginalRecord`（`artifacts_test.go:172`）证明无锁时的同一行为，`TestRegistrationSequenceIdentityAndTagPointer`（`regression_test.go:137-253`）覆盖 B 提交后的重试。

### 3.5 持锁期间：仅改 A 的 size_bytes → 409

同摘要、同标签、合法其余字段，仅 `size_bytes=2048`：身份查找照常读到 A，比较不等 → `StatusConflict`（`store.go:92`），全程无写语句：**409 `ArtifactConflictError`** 与固定 message；列表、标签不变。既有侧 **[已有验证]** `TestRegisterConflictOnDifferentContent`（`artifacts_test.go:192`）及 `artifact-identity-analysis.md` §4 步骤 5（四字段分别改均 409）。

### 3.6 持锁期间：非法摘要 → 400，与锁无关

提交 `digest="sha256:not-hex"`（其余合法）：`ParseRegistration` 在 `input.go:90-93` 拒绝，handler 在触达服务前返回 **400 `InvalidArtifactInputError`**；列表、标签同样不变。**[新增验证]** 持锁窗口内的 400；既有侧 **[已有验证]** `TestStorageUnavailableStillValidatesInput`（`regression_test.go:545`）证明存储故障时校验仍先行。

### 3.7 每次失败后仓库仍只有 A、标签仍指 A

§3.3–3.6 的**每一次**请求之后都立即发出仓库列表、标签查询与 B 摘要查询并断言：列表恒为 `[A]`、标签恒命中 A（`pushed_at=tA`）、B 摘要恒 404。失败请求既不提交业务行也不移动指针——503 的事务在 INSERT 处失败并经 `store.go:59` 回滚，409/400/201-重复本就无写语句。**[新增验证]**

### 3.8 释放写锁后：B → 201，列表 [A,B]，标签移到 B；再重试 A 不回移

`tx.Commit()` 释放写锁后重发同一 B 请求：**201**；列表按 `ORDER BY id`（`store.go:98`）返回 `[A, B]`，且 A 的 `pushed_at` 仍为 `tA`；标签查询命中 B；A、B 摘要各自取回原记录。此后再原样重试 A 仍 **201**，返回 A 原记录与 `tA`，标签**仍指 B**（重复路径无写语句，指针不回移）。**[新增验证]** 同用例末段；既有侧 **[已有验证]** `TestRegistrationSequenceIdentityAndTagPointer` 第 3 段（`regression_test.go:179-188`）、顺序与持久性另有 `TestRestartPreservesRecordsOrderAndPointers`（`regression_test.go:580-620`）。

---

## 4. 写入失败为何归存储错误，重试与冲突为何无需写入

1. **错误先于状态归类**：`Service.Register` 先判 `err != nil` → `ErrStorage`（`service.go:170-172`），再判 `StatusConflict`（`:173-175`）。锁争用表现为 INSERT 返回的非空错误，**在到达冲突判定之前**就被归为存储错误；而真正的冲突要求身份查找**成功**且比较不等，根本不产生错误。这一优先级既有 **[已有验证]** `TestSQLiteStorageFailureIsNotNotFound`（`internal/service/sqlite_test.go`）、`TestSharedEntryErrorPriorityOverStorageFailure`（`shared_input_test.go:366`）从服务两侧佐证，本轮新增用例在 HTTP 侧给出锁争用这一具体来源。
2. **重复与冲突是"读 + 内存比较"**：`store.go:86-92` 两个分支在身份 SELECT（`:61-63`）之后只有字段比较，没有、也不需要任何 INSERT/UPDATE/upsert；WAL 保证读者不被唯一写者阻塞，所以二者在持锁期间照常得到 201/409。**[源码推导]** + **[新增验证]** §3.4–3.5。
3. **回滚让失败请求等同未发生**：新登记的 INSERT 失败时 `defer tx.Rollback()`（`store.go:59`）连同一并撤销；它与既有可见性文档中"标签更新失败→整事务回滚"（`sqlite-transaction-visibility-analysis.md` §4）是同一机制，只是触发点从"自身事务中途失败"换成"开事务时写锁已被他人持有"。
4. **错误面恒定**：三种错误（503/409/400）与 404 都沿用顶层 `error` 对象与固定 message（`artifacts.go:20-22`）；`database is locked`、`SQLITE_BUSY`、表名、路径只留在服务端包装错误里，经 `assertErrorResponse` 的泄漏子串扫描核对（`regression_test.go:91-96`）。

---

## 5. 读得成功不代表写得成功（对既有可见性结论的边界）

既有说明确立了"WAL 下读不阻塞写、写不阻塞读，未提交/已回滚数据对其他连接不可见"。本轮在此之上明确一条**不对称**边界，避免把读取成功外推为写入会成功：

1. 持锁窗口内，登记请求的**身份读取始终成功**（读到提交前的旧状态：B 查无、A 查到），但同一请求的**写入分支会立即失败**——读路径（`store.go:97-134`）与写路径（`:55-80`）对锁的需求不同；
2. 因此不能用"持锁期间 GET 仍 200"推导"POST 也会成功"，也不能反过来用 POST 503 怀疑 GET 的结果：窗口内 GET 拿到的旧状态是 SQLite 读已提交语义的正确答案，不是故障；
3. 本节结论**仅限内置 SQLite 适配器**：它依赖 WAL（`store.go:31`）、单一写者与未配置 busy_timeout（§1.6）三个具体事实；接口层不强制自定义存储具备同样的读写锁分离，自定义存储的争用语义由其自身实现负责（§6）。

---

## 6. 对自定义存储的边界声明

本文所有争用与可见性结论（写锁在持时新登记 503、重复 201、冲突 409、非法 400；读不被写者阻塞；失败回滚不留痕）对内置 SQLite 适配器由源码（§1）与真实文件行为用例（§3）共同支撑。对于经 `NewRouterWithStore`（`router.go:32-54`）注入的自定义 `service.Store`：

- 接口契约只在注释中要求"Register 把记录写入与标签移动作为一次原子操作""查询返回已提交状态、未命中与故障可分"（`service.go:124-142`），业务层与路由没有任何代码强制其锁模型、隔离级别或忙等待策略；
- 自定义存储若呈现 503/201/409/400 的同样分化，保证来自**其自身实现**；本轮测试不把 SQLite 上的锁结论外推为任意存储的证明。与 `docs/storage-boundary-analysis.md` §2.3 及 `docs/sqlite-transaction-visibility-analysis.md` §7 的声明一致。

---

## 7. 结论—依据—验证对照表

| # | 结论 | 源码依据 | 验证 |
|---|---|---|---|
| 1 | 输入校验先于服务与存储：持锁期间非法摘要仍 400，且不留痕 | `artifacts.go:40-44`；`input.go:90-93` | [新增] §3.6；[已有] `TestStorageUnavailableStillValidatesInput` |
| 2 | 延迟 BEGIN 不持锁；身份 SELECT 在 WAL 下持锁期间可读；争用失败点是新登记分支第一条 INSERT（立即 SQLITE_BUSY） | `store.go:55,61-63,66-71`；WAL `:31`；无 busy_timeout（全仓搜索） | [源码推导]；[新增] §3.2–3.3 |
| 3 | 持锁期间新身份 B 得 503 `storage_unavailable`，固定 message，锁错误原文不外泄 | `store.go:66-71,82-83`；`service.go:170-172`；`artifacts.go:50-51` | [新增] §3.3；[已有] `TestStorageUnavailableReturns503`（关库来源） |
| 4 | 持锁期间原样重试 A 得 201 原记录、`pushed_at` 不变，因重复分支无写语句 | `store.go:86-90`；`artifacts.go:52-55` | [新增] §3.4；[已有] `TestRegisterDuplicateReturnsOriginalRecord` |
| 5 | 持锁期间仅改 size_bytes 得 409，冲突分支同样无写语句 | `store.go:92`；`service.go:173-175`；`artifacts.go:48-49` | [新增] §3.5；[已有] `TestRegisterConflictOnDifferentContent` |
| 6 | 每次失败后列表仅 A、标签指 A、B 摘要 404（读不被写者阻塞，读已提交） | 读方法直用 `s.db` 无 `Begin`（`store.go:97,121,128`）；`:140-141` | [新增] §3.7；[已有] 可见性用例与 `TestQueryNotFound` |
| 7 | 释放后 B 得 201，列表 `[A,B]` 按 id 序，标签移到 B；再重试 A 仍 201 原记录、标签不回移 | `store.go:78-80,98,72-77,86-90` | [新增] §3.8；[已有] `TestRegistrationSequenceIdentityAndTagPointer` |
| 8 | 存储错误归类先于冲突归类，故锁争用（非 nil 错误）不可能被报成 409 | `service.go:170-175` | [已有] `TestSQLiteStorageFailureIsNotNotFound` 等；[新增] §3.3/§3.5 并排 |
| 9 | 读成功不蕴含写成功：该不对称仅限内置 SQLite（WAL、单一写者、无 busy_timeout） | `store.go:31,55-80,96-134` | [源码推导]；[新增] §3；自定义存储不推广（§6） |

---

## 8. 复现方式

```bash
go test ./...                          # 全部用例（基线 + 新增回归）
go test ./internal/api -run 'TestSQLiteWriteLockContentionDistinguishesRegisterOutcomes' -v
go test -race -count=5 ./internal/api  # 含跨连接并发交错的稳定性核对
go vet ./...
```

新增用例的被测对象始终是生产装配（`store.Open` 真实文件 + `NewRouter`），所有可观察结果都经公开 HTTP 入口（`httptest` 驱动 POST/GET）断言；第二个 `database/sql` 连接只做两件夹具工作——对 `service_metadata` 的一次 INSERT 以保留写锁（§3.2），以及提交/回滚以释放它——不通过夹具连接读取或断言任何业务结果。因此任何保持公开契约与 SQLite 锁语义的内部重构都应持续通过。
