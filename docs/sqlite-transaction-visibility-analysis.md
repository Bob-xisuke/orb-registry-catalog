# SQLite 登记事务可见性分析（附回归用例）

本文基于当前基线源码，沿 `POST /v1/artifacts` 到存储提交、`GET /v1/artifacts` 到响应的完整路径，补齐既有两份文档尚未给出行为证据的两类事务阶段：

1. **事务尚未结束**：B 的记录行已插入、标签指针已更新，但事务还未 `COMMIT` 时，仓库查询、标签查询、摘要查询各自能看到什么；
2. **事务中途回滚**：B 的记录行已插入成功，随后标签更新语句失败，登记请求返回什么、回滚后三种查询各自能看到什么。

并明确**单次查询**与**跨多次请求**的可见性边界。

- 源码版本：工作区当前提交（`5b2320c`）
- 验证方式标注：
  - **[已有测试]** 基线测试已覆盖，注明函数与文件
  - **[新增回归]** 本次补充的用例，位于 `internal/api/sqlite_visibility_test.go`
  - **[源码推导]** 由源码结构直接得出，无法或不必通过行为用例证明
- 本次**只新增本文档与测试文件**；README、生产代码（`main.go`、`internal/api`、`internal/service`、`internal/store`）、SQLite schema、既有数据库文件与启动配置（`ADDR`/`DB_PATH`）均未改动；`GET /healthz`、两种路由构造入口（`NewRouter`、`NewRouterWithStore`）、输入校验、409 冲突响应沿用既有行为；错误响应继续使用单个顶层 `error` 对象与固定 message。
- 本文的事务证据**只针对内置 SQLite 适配器**（§7）。

---

## 1. 结论速览

固定布景：同一仓库 `R = "registry-demo/control"`、同一标签 `T = "release"`，两个合法摘要 A、B（`sha256:` 后接 64 个 `a` / `b`），先成功登记 A，再让 B 的写入进入指定事务阶段。

| # | 事务阶段 | POST 结果 | `?repository=R`（列表） | `&tag=T`（标签） | `&digest=B`（摘要） |
|---|---|---|---|---|---|
| 1 | B 的 INSERT 与标签 upsert 均已执行，**事务未提交** | （尚无响应，事务仍开启） | **200 `[A]`** | **200 A** | **404 `ArtifactNotFoundError`** |
| 2 | 阶段 1 的事务**提交**后 | — | **200 `[A, B]`**（首次登记顺序） | **200 B** | **200 B** |
| 3 | B 的 INSERT 成功，**标签 upsert 失败**，事务回滚 | **503 `storage_unavailable`** | **200 `[A]`** | **200 A** | **404 `ArtifactNotFoundError`** |

阶段 2 之后原样重试 A：仍 **201**，返回 A 的原记录（`pushed_at` 不变），标签仍指向 B（§4 步骤 3，**[已有测试]** `TestRegistrationSequenceIdentityAndTagPointer` 与 **[新增回归]** 均覆盖）。

阶段 1 与阶段 3 的差别仅在"事务如何结束"：提交让两处变化同时可见；回滚（含失败触发的自动回滚）让两处变化同时消失。读者在任何时刻都不会看到"记录与标签只变了一半"的状态。

---

## 2. 写入路径：POST /v1/artifacts 到存储提交

一次登记从 HTTP 到提交点固定经过三层，关键位置如下。

### 2.1 HTTP 适配层（`internal/api/artifacts.go`）

- 解码与校验：`decodeArtifactInput`（`artifacts.go:56-110`），失败 → **400 `InvalidArtifactInputError`**（`:160-164`），发生在任何存储调用之前。
- 调用业务层：`svc.Register(...)`（`artifacts.go:166-173`）。
- 结果映射（`:174-183`）：`ErrConflict` → **409**（`:175-176`）；其他非 nil 错误 → **503 `storage_unavailable`**，message 固定为 `"database is not available"`（`:177-178`）；成功（含首次与重复）→ **201** 与记录本身（`:179-182`）。

### 2.2 业务层（`internal/service/service.go`）

`Service.Register`（`service.go:160-180`）：

1. 调用存储前生成 `PushedAt: time.Now().UTC().Format(time.RFC3339)`（`service.go:168`）；
2. 调用 `s.store.Register(...)`（`:161-169`）；
3. `err != nil` → 固定归类为 `ErrStorage`（`:170-172`，错误先于状态判定）；
4. `StatusConflict` → `ErrConflict`（`:173-175`）；
5. 其余 → 201（`:176-179`）。

业务层不接触事务，事务的开始、提交与回滚全部在存储实现内部。

### 2.3 存储层事务（`internal/store/store.go`）

`Store.Register`（`store.go:54-93`）把"写入记录 + 移动标签指针"放在**一个显式事务**里，顺序固定：

| 行 | 语句 | 事务阶段 |
|---|---|---|
| `store.go:55` | `tx, err := s.db.Begin()` | 事务开始 |
| `store.go:59` | `defer tx.Rollback()` | 注册兜底：任何返回路径未提交即回滚；提交后再调用是 no-op |
| `store.go:61-63` | `SELECT ... FROM artifacts WHERE repository=? AND digest=?` | 事务内查重 |
| `store.go:66-71` | `INSERT INTO artifacts (...) VALUES (...)` | **写 1：登记记录** |
| `store.go:72-77` | `INSERT INTO tag_pointers ... ON CONFLICT (repository, tag) DO UPDATE SET digest = excluded.digest` | **写 2：移动标签指针**（A 之后登记 B 时走 `DO UPDATE` 分支） |
| `store.go:78-80` | `tx.Commit()` | 提交；只有提交成功才返回 `StatusCreated`（`:81`） |

由此可直接得出两条结构性质：

- **写 1 与写 2 之间不存在提交点**（两条 `Exec` 之间没有 `Commit`），且 `Commit` 是事务内最后一步——**[源码推导]**。因此阶段 1（写 2 已执行、未提交）在生产代码里是一次正常登记内部的真实时刻，只是持续时间极短。
- **写 2 失败时写 1 必然回滚**：`:75-76` 的失败分支直接 `return ..., fmt.Errorf("move tag pointer: %w", err)`，`defer tx.Rollback()`（`:59`）随即撤销写 1 已插入的行——**[源码推导]**，其端到端行为由 §6 的 **[新增回归]** 在真实 POST 上验证。

schema 约束：`artifacts` 上 `UNIQUE (repository, digest)`（`store.go:162`），`tag_pointers` 主键 `(repository, tag)`（`store.go:168`）。

---

## 3. 读取路径：GET /v1/artifacts 到响应

### 3.1 适配层与业务层

- 参数校验与分派：`queryArtifacts`（`artifacts.go:187-210`），非法参数 → 400（`:194-198`）。
- 调用 `svc.Query`（`artifacts.go:212`）；结果映射（`:213-223`）：`ErrNotFound` → **404 `ArtifactNotFoundError`**，固定 message `"no artifact matches this query"`（`:214-215`）；其他错误 → **503**（`:216-217`）；命中 → **200 `{"artifacts":[...]}`**（`:218-223`）。
- `Service.Query`（`service.go:186-218`）按查询种类调用一个存储方法（`:191-206`），再把 `ErrRecordNotFound`/其他错误/空列表分别归类（`:208-217`）。

### 3.2 存储层：每次查询一条独立的 autocommit SELECT

- 列表：`ListRecords` 的 `SELECT ... WHERE repository = ? ORDER BY id`（`store.go:97-98`）；
- 摘要：`RecordByDigest` 的 `SELECT ... WHERE repository = ? AND digest = ?`（`store.go:121-123`）；
- 标签：`RecordByTag` 的 `tag_pointers JOIN artifacts`（`store.go:128-133`），命中的前提是指针与被指记录**同时**处于已提交状态。

三者都直接使用连接池句柄 `s.db`（`store.go:97,121,128`），**不开启显式事务**；每条语句在自己的 autocommit 事务里执行，语句结束快照即释放。`sql.ErrNoRows` 在 `scanRecord` 中映射为 `service.ErrRecordNotFound`（`store.go:140-141`），其余错误原样上抛（`:143`）。

- 数据库以 WAL 模式打开（`store.go:31`，`PRAGMA journal_mode=WAL`）。WAL 下读不阻塞写，且读连接只能看到语句开始时**最后一个已提交**的快照，读不到任何连接尚未提交的页——**[源码推导]**（SQLite WAL 的读已提交语义；代码中没有任何跨语句读事务或隔离级别设置）。
- 因此：阶段 1 中，标签指针虽已在写事务内被 upsert 为 B，但 JOIN 读连接看不到该行版本，标签查询仍 JOIN 到已提交的 A；B 的摘要行同样不可见 → 404。不存在"标签查到一个尚不可见的 B"这一中间结果。
- **[已有测试]** 已提交后的读取行为：`TestSQLiteRegisterAndQuery`（`sqlite_test.go:45-97`，三种查询 + 顺序 + 标签移动）、`TestListArtifactsInRegistrationOrder`（`artifacts_test.go:239-256`）、`TestTagPointerMovesToNewDigestAndOldRecordSurvives`（`artifacts_test.go:206-237`）。阶段 1 的"未提交即不可见"此前没有行为证据，由本次 **[新增回归]** 补齐。

---

## 4. 场景一：B 的事务未提交期间与提交之后

对应用例 `TestUncommittedArtifactTransactionInvisibleUntilCommit`（`sqlite_visibility_test.go`）。

### 4.1 测试手法（为什么能"暂停"事务）

生产 `Store.Register` 把事务封闭在一次函数调用内，外部无法让它停在写 2 与 `Commit` 之间。用例因此：

1. 用公开入口正常登记 A（真实 POST，已提交）；
2. 在**同一 SQLite 文件**上打开第二个独立的 `database/sql` 句柄（`sqliteVisibilityHandle`），执行与 `store.go:66-77` **逐字相同的两条写语句**（INSERT B、upsert 标签），但持有事务**不提交**（`parkUncommittedB`）；
3. 所有读取仍通过真实路由发出（`queryList`/`queryByTag`/`queryByDigest` → `Service.Query` → 适配器的 `s.db` 连接）。

第二句柄不改变任何生产配置：WAL 是数据库文件级属性，所有连接一致。由此复现的正是生产事务在 `store.go:77` 与 `:78` 之间的那一刻。B 的 `pushed_at` 使用固定字面量 `2026-01-02T03:04:05Z`（TEXT 列接受任意字符串），使"提交后读到的就是该事务写入的那一行"可以逐字断言。

### 4.2 各步骤与确定结果

**步骤 1 — 登记 A**：POST → **201**，保存返回的 `pushed_at = tA`。

**步骤 2 — 阶段 1（未提交）**，三种查询经完整 HTTP 链路：

| 查询 | 结果 | 核对点 |
|---|---|---|
| 列表 | **200 `[A]`** | 仅一条；A 七字段与首次响应逐字一致，`pushed_at = tA` |
| 标签 | **200 A** | JOIN 命中已提交的指针行；记录七字段同上 |
| `digest=B` | **404 `ArtifactNotFoundError`** | 固定 message（`assertErrorResponse`） |
| `digest=A` | **200 A** | 已提交的 A 不受未提交写影响，`pushed_at = tA` |

**步骤 3 — 提交 B**（写事务 `Commit`），随后**新发出**的查询：

| 查询 | 结果 | 核对点 |
|---|---|---|
| 列表 | **200 `[A, B]`** | `ORDER BY id` 的首次登记顺序；A、B 各自七字段逐字核对，`pushed_at` 分别为 `tA`、固定 B 时间 |
| 标签 | **200 B** | 指针移动随提交一并可见，命中记录的 `pushed_at` 为 B 的值 |
| `digest=A` | **200 A** | 旧记录仍在，字段与 `tA` 不变 |
| `digest=B` | **200 B** | 新记录按身份可取回 |

**步骤 4 — 提交 B 后原样重试 A**：POST → **201**，响应是 A 的**原记录**（`pushed_at = tA`，七字段逐字一致）；重复路径不执行任何写语句（`store.go:86-92`），标签查询仍命中 **B**。

> 阶段 1 的全部断言由 `assertOnlyAIsVisible` 统一执行（列表/标签/摘要三查询 + A 原记录字段），步骤 3、4 在用例正文中逐一断言。

---

## 5. 单次查询与跨请求的边界

服务**不**在多个 HTTP 请求之间持有事务或快照：处理函数本身无状态，每次 GET 都新调用一次存储方法、从连接池取连接执行一条 autocommit SELECT（§3.2），响应返回后该语句的快照即释放。代码中不存在会话事务或隔离级别的设置——**[源码推导]**。

因此承诺的边界是：

- **单次查询内**：一条 SELECT 读到一个一致的已提交快照（语句级），不会在一条语句内看到半个事务；
- **两次独立查询之间**：没有共同快照。若 B 的提交恰好发生在两次 GET 之间，前一次回答旧状态、后一次回答新状态，**都是合法结果**，不构成读取不一致。

**[新增回归]** `TestIndependentQueriesObserveCommitBetweenThem` 用确定时序展示这一合法交错：

1. B 的事务保持未提交，发出查询 1（列表）→ **200 `[A]`**；
2. 查询 1 已结束之后，写事务才 `Commit`；
3. 再独立发出查询 2（标签）→ **200 B**，且 `pushed_at` 为该事务写入的 B 时间；
4. `digest=A` 仍取回 A 的原记录（`pushed_at = tA` 不变），查询 1 已完成的响应不会被追溯改变。

"前一次列表只有 A、后一次标签命中 B"正是跨请求无快照承诺的预期表现；服务不对并发交错作更强保证（既有文档 §7 第 3 条亦持此边界）。

---

## 6. 场景二：B 的记录已写入、标签更新失败 → 503 与回滚

对应用例 `TestTagPointerMoveFailureRollsBackRegistrationWith503`（`sqlite_visibility_test.go`）。

### 6.1 故障注入手法

在临时数据库文件上安装两个**测试期临时触发器**（不属于生产 schema，生产 `schema` 常量与既有数据库文件均未改动）：对 `tag_pointers` 的 `BEFORE INSERT` 与 `BEFORE UPDATE` 各 `RAISE(FAIL, ...)`。登记 B 时标签 upsert 命中 A 已建立的 `(R, T)` 指针行、走 `DO UPDATE` 分支，于是**写 2 在数据库层真实失败**；`BEFORE INSERT` 触发器同时覆盖全新标签路径。随后发生的一切都在生产代码内：

```
POST B
 └─ Service.Register (service.go:161)            生成 tB（随回滚丢弃）
    └─ Store.Register (store.go:55)              Begin
       ├─ INSERT artifacts 写 B     (store.go:66-71)   ← 成功
       ├─ upsert tag_pointers       (store.go:72-77)   ← 触发器 FAIL，返回非 nil error
       ├─ return error              (store.go:75-76)
       └─ defer tx.Rollback()       (store.go:59)      ← 撤销已插入的 B 行
 └─ err != nil → ErrStorage        (service.go:170-172)
    └─ 503 storage_unavailable，固定 message (artifacts.go:177-178)
```

失败与回滚在同一次 `Register` 调用内连续完成，外部读者**不存在**"写 1 可见、写 2 失败"的观察窗口——这正是原子性，而不是一个可被 GET 观察到的阶段。

### 6.2 确定结果

| 观察 | 结果 |
|---|---|
| 登记 B 的 POST | **503**，顶层 `error` 对象：`code = storage_unavailable`、`message = "database is not available"`（由 `assertErrorResponse` 核对对象恰含两字段且无 SQL/路径泄漏） |
| 回滚后列表 | **200 `[A]`**；A 七字段与首次响应逐字一致，`pushed_at = tA` |
| 回滚后标签 | **200 A**；指针仍指 A，`pushed_at = tA` |
| 回滚后 `digest=B` | **404 `ArtifactNotFoundError`**（B 行已随回滚消失，而非残留一条"无标签"记录） |
| 回滚后 `digest=A` | **200 A**，原记录不变 |

即阶段 3 的外部可观察状态与阶段 1 相同（§1 表），区别只是写者得到了 503 而不是挂起。

**[已有测试]** 的相邻覆盖（不构成本场景证据，仅边界相邻）：关闭整个 SQLite 后三种查询与 POST 均 503 的 `TestStorageUnavailableReturns503`（`regression_test.go:307-335`）、服务层 `TestSQLiteStorageFailureIsNotNotFound`（`sqlite_test.go:102-126`）、状态与错误同返时错误优先的 `TestRegisterStatusPlusErrorMapsTo503`（`storage_boundary_test.go:142-167`）。它们不制造"单事务内写 2 失败"，因此回滚原子性的直接行为证据由本次用例首次给出。

---

## 7. 适用边界：内置 SQLite 与自定义存储

- 本文的提交/回滚/读已提交证据来自三处：内置适配器源码（`store.go:31,54-93,96-146`）、SQLite WAL 的数据库语义，以及针对**真实 SQLite 文件**的本次用例。
- `service.Store` 接口注释（`service.go:124-142`）只**约定**"Register 把写记录与移动指针作为一次原子操作""查询只返回已提交状态"，服务层与路由没有任何代码强制它们（与 `docs/storage-boundary-analysis.md` §2.3 的边界声明一致）。
- 因此：**自定义存储是否提供相同的未提交不可见、失败回滚与读已提交保证，取决于其自身实现**；本文不为自定义存储背书。用例中的第二 `database/sql` 句柄与触发器是 SQLite 专属测试夹具，也不用于自定义存储。

---

## 8. 结论—依据—验证对照表

| # | 结论 | 源码依据 | 验证 |
|---|---|---|---|
| 1 | 写记录与移动标签在同一显式事务，两条写语句之间无提交点 | `store.go:55,66-80` | [源码推导]；提交后两侧同时可见见场景一用例 |
| 2 | 事务未提交时，列表/标签只见已提交的 A，B 摘要为 404 | `store.go:31,66-77`（未提交）、`:97-98,121-133`（读） | [新增回归] `TestUncommittedArtifactTransactionInvisibleUntilCommit` 步骤 2 |
| 3 | 提交后新查询看到 `[A,B]`（首次登记顺序）、标签指 B、两个摘要各取原记录 | `store.go:78-81,97-98,128-133` | [新增回归] 步骤 3；[已有] `TestSQLiteRegisterAndQuery` |
| 4 | 提交后重试 A 仍 201 且返回原记录、`pushed_at` 不变、标签仍指 B | `store.go:86-92`；`service.go:176-179` | [新增回归] 步骤 4；[已有] 序列/`TestTagPointerMoves…` |
| 5 | 标签 upsert 失败 → POST 503 `storage_unavailable`，已插入的 B 随 `defer Rollback` 撤销 | `store.go:59,72-77`；`service.go:170-172`；`artifacts.go:177-178` | [新增回归] `TestTagPointerMoveFailureRollsBackRegistrationWith503` |
| 6 | 回滚后列表仅 A、标签指 A、B 摘要 404，A 字段与 `pushed_at` 不变 | 同 5 | [新增回归] 同用例（`assertOnlyAIsVisible`） |
| 7 | 每条 GET 是独立 autocommit SELECT，语句结束即释放快照 | `store.go:97,121,128`（均用 `s.db`，无显式事务） | [源码推导]；[新增回归] `TestIndependentQueriesObserveCommitBetweenThem` |
| 8 | 两次独立查询之间提交导致"前列表 A、后标签 B"是合法结果 | 处理函数无跨请求事务；`store.go:31` | [新增回归] 同跨请求用例 |
| 9 | 未命中（含未提交行）经 `sql.ErrNoRows → ErrRecordNotFound → ErrNotFound` 成为 404，故障成为 503 | `store.go:140-143`；`service.go:208-212`；`artifacts.go:214-217` | [已有] `TestQueryNotFound`、`TestSQLiteStorageFailureIsNotNotFound`；[新增] 两场景的 404/503 |
| 10 | 错误响应为单个顶层 `error` 对象、固定 message、不泄漏 SQL/路径 | `artifacts.go:38-40,177-178,214-215` | [新增回归] `assertErrorResponse`；[已有] 各错误用例 |
| 11 | 相同保证对自定义存储仅为接口约定，是否满足取决于其自身实现 | `service.go:124-142` | [源码推导]（§7） |

---

## 9. 复现方式

```bash
go test ./...                          # 全部用例（基线 + 新增回归）
go test ./internal/api -run 'TestUncommittedArtifactTransactionInvisibleUntilCommit|TestIndependentQueriesObserveCommitBetweenThem|TestTagPointerMoveFailureRollsBackRegistrationWith503' -v
go vet ./...
```

三个新增用例全部在 `t.TempDir()` 下的**真实 SQLite 文件**上运行；读取一律经由公开 HTTP 入口（`httptest` 驱动 POST/GET）与生产的路由、业务层、内置适配器，第二句柄仅用于制造并控制事务阶段，触发器仅安装在该临时文件上。
