# SQLite 登记事务可见性分析（附回归用例）

本文针对**内置 SQLite 适配器**，补齐 `POST /v1/artifacts` 登记事务在**尚未提交**与**中途回滚**两个阶段的可见性证据。既有两份说明（`docs/artifact-identity-analysis.md` §7、`docs/storage-boundary-analysis.md` §2.3）已描述"单事务写入"与"已提交读取"，本轮沿同一条 `POST → 存储提交`、`GET → 响应` 路径标出关键源码位置，并以同一仓库、同一标签、两个合法摘要 A、B 展示每个事务阶段与三种查询结果之间的确定对应关系。

- 源码版本：工作区当前提交（`5b2320c`）
- 适用范围：**仅内置 SQLite 适配器**（`internal/store`）。自定义存储若满足相同的"原子写入、已提交读取"表现，保证来自**其自身实现**（`service.Store` 接口注释中的约定，`internal/service/service.go:124-142`），本文不为自定义存储作证。
- 验证方式标注：
  - **[已有验证]** 基线测试已覆盖，注明函数与文件
  - **[新增验证]** 本次补充的用例，位于 `internal/api/sqlite_visibility_test.go`
  - **[源码推导]** 由源码结构直接得出（如"不存在某语句/某设置"），不通过行为用例证明
- 本次只新增本文档与一个测试文件；README、生产代码、SQLite schema（`store.go:148-170` 的 `schema` 常量）、既有数据库、启动配置（`main.go`）与两种路由构造入口（`NewRouter`/`NewRouterWithStore`）均未改动；`GET /healthz`、输入校验、冲突响应、顶层 `error` 对象与固定错误消息沿用既有行为。

---

## 1. 路径上的关键源码位置

### 1.1 写路径：POST /v1/artifacts → 存储提交

| 步骤 | 位置 | 内容 |
|---|---|---|
| 装配 | `internal/api/router.go:47` | `router.POST("/v1/artifacts", registerArtifact(svc))`，服务由 `service.New(st)` 包出（`router.go:37`） |
| 解码校验 | `internal/api/artifacts.go:160-164` | 非法输入 400，发生在任何存储调用之前 |
| 业务层 | `internal/service/service.go:161-169` | 生成 `pushed_at`（`:168`），调用 `store.Register`；自身不开事务 |
| 开启事务 | `internal/store/store.go:55` | `tx, err := s.db.Begin()` —— 登记的全部读写都在这一个显式事务内 |
| 回滚兜底 | `internal/store/store.go:59` | `defer tx.Rollback()` —— 任何错误返回路径（含标签更新失败）都会回滚；提交成功后的回滚是空操作 |
| 身份查询 | `store.go:61-63` | 事务内按 `(repository, digest)` 查既有记录 |
| 插入记录 | `store.go:66-71` | `INSERT INTO artifacts ...`（新记录分支） |
| 移动标签 | `store.go:72-77` | `INSERT INTO tag_pointers ... ON CONFLICT ... DO UPDATE`，失败返回 `"move tag pointer"` 包装错误（`:75-76`） |
| 提交 | `store.go:78-80` | `tx.Commit()` —— 此前两步对外均不可见，此刻同时可见 |
| 重复/冲突 | `store.go:86-92` | 全等返回原记录（无写语句）；差异返回冲突（无写语句） |
| 错误归类 | `service.go:170-175` | 存储错误**先**判为 `ErrStorage`（`:170-172`），其次才是冲突 |
| HTTP 映射 | `artifacts.go:174-183` | `ErrStorage` → 503 `storage_unavailable` + 固定 message（`:177-178`）；成功（含重复）→ 201 原记录（`:179-183`） |

### 1.2 读路径：GET /v1/artifacts → 响应

| 步骤 | 位置 | 内容 |
|---|---|---|
| 装配 | `router.go:48` | `router.GET("/v1/artifacts", queryArtifacts(svc))` |
| 参数校验分派 | `artifacts.go:194-210` | 校验 400；分派为仓库/标签/摘要三种业务查询 |
| 业务层 | `service.go:186-218` | 未命中（含包装的 `ErrRecordNotFound`、空列表）→ `ErrNotFound`（`:209-214`），其他错误 → `ErrStorage`（`:211-212`） |
| 仓库列表 | `store.go:96-117` | 直接在 `s.db` 上 `Query`（`:97-98`），`WHERE repository = ? ORDER BY id`，无 `Begin` |
| 摘要查询 | `store.go:120-124` | 直接在 `s.db` 上 `QueryRow`，无 `Begin` |
| 标签查询 | `store.go:127-134` | 直接在 `s.db` 上 JOIN `tag_pointers` 与 `artifacts`，无 `Begin` |
| 未命中映射 | `store.go:136-146` | `sql.ErrNoRows` → `service.ErrRecordNotFound`（`:140-141`） |
| HTTP 映射 | `artifacts.go:212-224` | 命中 → 200 `{"artifacts":[...]}`；未命中 → 404 `ArtifactNotFoundError`（`:214-215`）；故障 → 503（`:216-217`） |

三个读方法都直接使用 `s.db`（`store.go:97`、`:121`、`:128`），**从不调用 `Begin`**——每条 SELECT 都是独立的 autocommit 语句，读事务只在该语句执行期间存在。**[源码推导]**

### 1.3 为什么未提交数据不可见

1. 数据库以 WAL 模式打开（`store.go:31`，`PRAGMA journal_mode=WAL`）；
2. SQLite 只读已提交数据，脏读必须显式 `PRAGMA read_uncommitted=1` 才可能出现——全仓库不存在该设置，除 `journal_mode=WAL` 外也没有任何隔离级别设置 **[源码推导]**；
3. 每条读查询是独立 autocommit SELECT（§1.2），语句开始时取得最近一次提交点的快照；写事务在 `Commit`（`store.go:78`）之前不会产生新的提交点；
4. WAL 下读不阻塞写、写（全库仅一个写者）也不阻塞读，因此"登记进行中"的并发 GET 不会等待、也不会读到半成品，而是立即得到提交前的旧状态。

---

## 2. 事务阶段 × 查询结果总表

同一仓库 R、同一标签 T，A 已成功登记并提交；B 的登记事务随后发生。三个查询阶段的确定结果：

| B 的事务阶段 | `GET ?repository=R`（列表） | `GET ?repository=R&tag=T` | `GET ?repository=R&digest=B` |
|---|---|---|---|
| **未提交**（记录已插入、标签已 upsert，但 `Commit` 未执行，`store.go:66-77` 之后、`:78` 之前） | **200**，仅 `[A]`，按 `id` 首次登记顺序 | **200**，命中 **A**（JOIN 不到未提交的指针行） | **404** `ArtifactNotFoundError`（未提交行对 SELECT 不存在） |
| **已提交**（`store.go:78-80` 完成） | **200**，`[A, B]`（首次登记顺序） | **200**，命中 **B** | **200**，命中 **B** 原记录 |
| **已回滚**（标签更新失败，经 `store.go:59` 回滚） | **200**，仅 `[A]` | **200**，命中 **A** | **404** `ArtifactNotFoundError`（被回滚的插入如同从未发生） |

POST 本身在回滚场景的结果：**503 `storage_unavailable`**，message 固定 `"database is not available"`（`service.go:170-172` → `artifacts.go:177-178`）。

- 已提交列的行为为 **[已有验证]**：`TestSQLiteRegisterAndQuery`（`internal/service/sqlite_test.go:45-97`）、`TestRegistrationSequenceIdentityAndTagPointer`（`internal/api/regression_test.go:137-253`）。
- 未提交列与已回滚列此前没有用例，本轮由 **[新增验证]** 两个用例补齐（§3、§4）。

---

## 3. 场景一：未提交事务期间的读取（确定序列）

用例 `TestSQLiteUncommittedRegistrationIsInvisible`（`sqlite_visibility_test.go`），全程在真实 SQLite 文件上经公开 HTTP 入口断言。

- 固定数据：R = `registry-demo/control`，T = `release`；A = `sha256:` + 64 个 `a`（`size_bytes=1024`），B = `sha256:` + 64 个 `b`（`size_bytes=2048`）；`signature_verified=true, retention_days=30`；`tA`、`tB` 取实际返回值逐字比较，不硬编码。

### 3.1 阶段 0：登记 A（已提交基线）

经 `POST /v1/artifacts` 登记 A：**201**，记录七字段回显，`pushed_at = tA`。此后标签 T 指向 A。

### 3.2 阶段 1：B 的写入挂在未提交事务中

生产流程中，B 的插入与标签移动发生在 `Store.Register` 的 `store.go:66-77`，而事务直到 `:78` 才提交；并发的其他 HTTP 请求正可能落在这个窗口。为了**确定性地复现该窗口**，测试用第二个 `database/sql` 连接打开同一个 SQLite 文件，执行与 `store.go:66-77` **逐字相同的两条写语句**后停住，不执行 Commit：

1. `INSERT INTO artifacts ...`（B 记录，`pushed_at=tB`）；
2. `INSERT INTO tag_pointers ... ON CONFLICT ... DO UPDATE`（T 移向 B）；
3. 不提交，保持事务打开。

随后测试在**第三个连接**上反复执行一条 no-op 写，直到得到 `database is locked`：WAL 下全库只有一个写者，该失败从连接外部证明挂起事务此刻真实持有写锁（`waitForPendingWriteLock`），因此接下来的 GET 确实与一个在途登记并发，而非与已完成的登记并发。**[新增验证]**

事务打开期间，经生产路由发出的三种查询（对应 §2 未提交列）：

- 仓库查询 **200**，数组仅含 A；
- 标签查询 **200**，命中 A，且其 `pushed_at = tA`；
- 按 A 摘要查询 **200**，A 记录七字段与阶段 0 响应**逐字一致**（含 `tag`、`pushed_at`）；
- 按 B 摘要查询 **404 `ArtifactNotFoundError`**、固定 message（`artifacts.go:214-215`）——B 是合法摘要身份，只是匹配不到任何**已提交**记录。

依据：§1.3 的四点；插入与 upsert 同处一个未提交事务（`store.go:66-77`），读侧的 autocommit SELECT 取不到其中任何一个。

### 3.3 阶段 2：提交 B，新查询原子地观察到两处变化

挂起事务执行 Commit（等价于生产的 `store.go:78-80`）。之后**新发出**的独立查询：

- 仓库查询 **200**，数组为 `[A, B]`：A 先登记故 `id` 更小，`ORDER BY id`（`store.go:98`）保证顺序；A 的 `pushed_at` 仍为 `tA`；
- 标签查询 **200**，命中 B，七字段与 B 登记内容逐字一致（`pushed_at = tB`）；
- 按 A 摘要 **200** 返回 A 原记录；按 B 摘要 **200** 返回 B 原记录。

记录与指针在同一个提交点同时可见，不存在"列表有 B 但标签仍指 A"或反向的中间状态。**[新增验证]** 该用例；已提交后的同事实亦有 **[已有验证]** `TestRegistrationSequenceIdentityAndTagPointer`、`TestSQLiteRegisterAndQuery`。

### 3.4 阶段 3：B 提交后重试 A——201 原记录，标签仍指 B

再次原样提交 A 的请求体：**201**（`store.go:86-90` 全等判重，返回库中原记录；HTTP 不区分首次与重复，`artifacts.go:179-182`），响应记录的 `pushed_at` 仍为 `tA`、七字段不变；标签 T 仍指向 **B**（重复路径无任何写语句，指针不回移）；列表仍为 `[A, B]`。**[新增验证]** 该用例在"B 先处于未提交窗口、再提交"的序列末尾断言；**[已有验证]** `TestRegistrationSequenceIdentityAndTagPointer` 第 3 段（`regression_test.go:179-188`）。

---

## 4. 场景二：记录已写入、标签更新失败 → 回滚

用例 `TestSQLiteRolledBackRegistrationIsInvisible`（`sqlite_visibility_test.go`）。这次故障注入在**生产 POST 路径内部**，不模拟服务层错误。

### 4.1 故障注入方式（仅存在于临时测试库）

测试经第二个连接在临时数据库文件内创建一个仅针对标签表 UPDATE 路径的触发器：

```sql
CREATE TRIGGER fail_tag_move BEFORE UPDATE ON tag_pointers
BEGIN
    SELECT RAISE(ABORT, 'injected tag pointer update failure');
END
```

该触发器不属于 `schema` 常量（`store.go:148-170`），生产代码与既有数据库均无此对象；它由测试在 `t.TempDir()` 的临时文件中运行时创建、随后 `DROP TRIGGER` 清除。A 首次登记时 `(R, T)` 指针行尚不存在，upsert 走 INSERT 路径，不受影响；登记 B 时指针行已存在，`ON CONFLICT ... DO UPDATE`（`store.go:72-75`）走 UPDATE 路径，触发器令该语句失败——精确制造"B 的记录已写入（`store.go:66-71` 成功）但标签更新失败（`:72-77` 出错）"。

### 4.2 生产代码的实际处置链

1. `Register` 在标签 upsert 处收到错误，返回包装错误（`store.go:75-76`）；
2. `defer tx.Rollback()`（`store.go:59`）回滚整个事务——已插入的 B 记录一并撤销；
3. 业务层把**任意**非 nil 存储错误归为 `ErrStorage`（错误检查先于状态检查，`service.go:170-172`）；
4. HTTP 层返回 **503**、顶层 `error` 对象 `{"code":"storage_unavailable","message":"database is not available"}`（`artifacts.go:177-178`）；触发器的注入文本只留在服务端包装错误里，不进入响应体（用例经 `assertErrorResponse` 的固定 message 与泄漏子串扫描核对）。

### 4.3 回滚后的读结果（对应 §2 已回滚列）

- 登记 B 的 POST：**503 `storage_unavailable`**；
- 仓库查询 **200**，仍仅 `[A]`；
- 标签查询 **200**，仍命中 A，`pushed_at = tA`；
- 按 A 摘要 **200**，A 原记录七字段不变；
- 按 B 摘要 **404 `ArtifactNotFoundError`**。

回滚对读者等价于"这次登记从未发生"。**[新增验证]**

### 4.4 回滚后存储仍可用

`DROP TRIGGER` 移除故障后，再次经 POST 登记同一 B：**201**；随后列表为 `[A, B]`、标签命中 B。回滚只结束该事务，不污染连接池与 schema。**[新增验证]** 同一用例的恢复段；503 状态码映射本身另有 **[已有验证]** `TestStorageUnavailableReturns503`（`regression_test.go:307-335`，关闭数据库文件）与 `TestSQLiteStorageFailureIsNotNotFound`（`sqlite_test.go:102-126`），但二者不覆盖"事务中途失败并回滚后旧状态仍可读"。

---

## 5. 单次查询与跨请求的边界

1. **单次查询读到的是它自己开始时的已提交状态**：每个 GET 对应一条 autocommit SELECT（`store.go:97-98`、`:121-123`、`:128-133`），处理函数返回即结束，服务层与适配层都不持有跨请求事务或快照（处理函数中无 `Begin`，无隔离级别设置）**[源码推导]**。
2. **两次独立查询之间没有快照承诺**：因此"前一次仓库列表只有 A，两次查询之间 B 提交，后一次标签查询命中 B"是**合法结果**，不是不一致——两个请求各自取自己开始时的提交点。场景一用例刻意保留了提交前捕获的列表响应对象 `preCommitList`，在提交后把它与一次新的标签查询并排断言：旧响应仍只有 A、新标签查询命中 B（§3.3 末段）。**[新增验证]**
3. 反过来，**在单个未提交事务的窗口内**（§3.2），同一次并发交错中的任意个查询都只能读到 A——它们各自的语句快照都早于 B 的提交点。边界只存在于**提交点**，不存在于"记录写入"与"标签更新"两条语句之间（二者同事务，对外不可分）。
4. 重复与冲突路径不产生任何写入（`store.go:86-92`），对并发读者同样等价于"什么都没发生"。**[已有验证]** `TestRejectedRegistrationLeavesCommittedStateUntouched`（`regression_test.go:447-492`）从已提交状态侧佐证；本轮新增用例从事务内部状态侧补齐。

---

## 6. 各阶段字段不变性核对

场景一在每个阶段（A 提交后、B 未提交期间、B 提交后、重试 A 后）都按摘要取回 A 并逐字段核对：`repository/digest/tag/signature_verified/retention_days/size_bytes/pushed_at` 与首次 201 响应完全一致；B 提交后取回的 B 与挂起事务中写入的值（含测试生成的 `tB`）逐字一致。依据：`artifacts` 表插入后不存在任何 `UPDATE`/`DELETE` 语句，列表与单记录查询只是读出既有行（`store.go:96-134`）**[源码推导]**；行为由 §3 各阶段 **[新增验证]** 覆盖，已提交侧另有 **[已有验证]** `TestRegistrationSequenceIdentityAndTagPointer` 冲突子测试与 `TestRestartPreservesRecordsOrderAndPointers`（`regression_test.go:580-620`）。

---

## 7. 对自定义存储的边界声明

本文所有可见性结论（未提交不可见、回滚等同未发生、单次查询读已提交、跨请求无快照承诺）对内置 SQLite 适配器由源码（§1）与对真实 SQLite 文件的行为用例（§3、§4）共同支撑。对于调用方经 `NewRouterWithStore`（`router.go:32-54`）注入的自定义 `service.Store`：

- 接口契约只在注释中要求"Register 把记录写入与标签移动作为一次原子操作""查询返回已提交状态、未命中与故障可分"（`service.go:124-142`），业务层与路由没有任何代码强制事务行为；
- 因此自定义存储若呈现相同保证，**保证来自其自身实现**；本轮测试不把接口约定或 SQLite 上的顺序结果外推为所有存储的证明。这一边界与 `docs/storage-boundary-analysis.md` §2.3 的声明一致。

---

## 8. 结论—依据—验证对照表

| # | 结论 | 源码依据 | 验证 |
|---|---|---|---|
| 1 | 登记的记录插入与标签移动在同一显式事务内，提交点唯一 | `store.go:55,59,66-80` | [源码推导]；[已有] `TestSQLiteRegisterAndQuery` 提交后原子可见 |
| 2 | 提交前，事务内的记录行与标签 upsert 对其他连接的三种查询均不可见：列表仅 A、标签指 A、按 B 摘要 404 | `store.go:66-77` vs `:78-80`；读方法无 `Begin`（`:97,:121,:128`）；WAL `:31` | [新增] `TestSQLiteUncommittedRegistrationIsInvisible` 阶段 1 |
| 3 | 提交后新查询原子看到 `[A,B]`、标签指 B、两个摘要各取回原记录，顺序按首次登记 | `store.go:78-80,98,127-134` | [新增] 同用例阶段 2；[已有] 序列/SQLite 用例 |
| 4 | 标签更新失败时 POST 为 503 `storage_unavailable`，回滚后列表仅 A、标签指 A、B 摘要 404 | `store.go:59,72-76`；`service.go:170-172`；`artifacts.go:177-178` | [新增] `TestSQLiteRolledBackRegistrationIsInvisible` |
| 5 | 回滚后存储仍可正常登记，连接池与 schema 未被污染 | `store.go:59`（回滚仅结束本事务） | [新增] 同用例恢复段 |
| 6 | 每条 GET 是独立 autocommit 读；两次查询间提交导致"前列表只有 A、后标签命中 B"合法 | 读方法均直用 `s.db` 无 `Begin`；处理函数无跨请求事务 | [源码推导] + [新增] 场景一中提交前后响应并排断言 |
| 7 | 各阶段 A 原记录七字段与 `pushed_at` 不变；B 提交后重试 A 仍 201 原记录、标签仍指 B | `artifacts` 无 UPDATE/DELETE；`store.go:86-90`；`artifacts.go:179-182` | [新增] 场景一各阶段 + 阶段 3；[已有] 序列/重开用例 |
| 8 | 未提交/回滚行的摘要查询是 404 `ArtifactNotFoundError`（未命中），不是 503（故障） | `store.go:140-141`；`service.go:209-210`；`artifacts.go:214-215` | [新增] 两个新用例的 B 摘要断言 |
| 9 | 503/404 响应沿用顶层 `error` 对象与固定 message，不泄漏注入的 SQL 文本 | `artifacts.go:38-40,177-178,214-215` | [新增] 经 `assertErrorResponse` 核对；[已有] 该助手的既有调用 |
| 10 | 相同保证在自定义存储上来自其自身实现，接口约定不构成强制 | `service.go:124-142`；`router.go:32-54` | [源码推导]；本轮测试仅针对内置 SQLite |

---

## 9. 复现方式

```bash
go test ./...                          # 全部用例（基线 + 新增回归）
go test ./internal/api -run 'TestSQLiteUncommittedRegistrationIsInvisible|TestSQLiteRolledBackRegistrationIsInvisible' -v
go test -race -count=5 ./internal/api  # 含跨连接并发交错的稳定性核对
go vet ./...
```

新增用例的被测对象始终是生产装配（`store.Open` 真实文件 + `NewRouter`），所有可观察结果都经公开 HTTP 入口（`httptest` 驱动 POST/GET）断言；第二个 `database/sql` 连接只做两件测试夹具工作——按生产 SQL 原样挂起一个登记事务（场景一），以及在临时库内安装/删除一次性触发器（场景二）——不通过它读取或断言任何业务结果。因此任何保持公开契约与 SQLite 事务语义的内部重构都应持续通过。
