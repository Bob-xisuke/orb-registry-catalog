# 制品身份与标签指向分析（附回归用例）

本文基于当前基线源码，说明从 `POST /v1/artifacts` 接收请求到 `GET /v1/artifacts` 返回响应的完整链路，区分**不可修改的登记记录**与**可变化的标签指向**，并给出与每条结论对应的验证位置。

- 源码版本：工作区当前提交（`14733e7`）
- 验证方式标注：
  - **[已有测试]** 基线测试已覆盖，注明函数与文件
  - **[新增回归]** 本次补充的用例，位于 `internal/api/regression_test.go`
  - **[源码推导]** 由源码结构直接得出，无法或不必通过行为用例证明（如"全仓库不存在某类语句"）
- 本次只新增测试与本文档；README、`GET /healthz`、启动配置（`main.go`）、数据库 schema 与既有数据行为均未改动。

---

## 1. 总览：两类数据，两种可变性

服务在 SQLite 中维护两张表（`internal/store/store.go:167-189`）：

| 表 | 主键 / 唯一约束 | 含义 | 可变性 |
|---|---|---|---|
| `artifacts` | 自增 `id`；`UNIQUE (repository, digest)`（`store.go:181`） | 登记记录：仓库、摘要、登记时标签、签名结果、保留天数、体积、服务生成时间 | **插入后不可修改**。全仓库不存在针对该表的 `UPDATE`/`DELETE` 语句 **[源码推导]** |
| `tag_pointers` | `PRIMARY KEY (repository, tag)`（`store.go:188`） | 标签指向：`(仓库, 标签) -> 当前摘要` | **仅在新制品登记时**通过 upsert 移动（`store.go:91-96`）；重复提交不移动 |

关键区分：

- `artifacts.tag` 是该记录**首次登记时随请求提交的标签**，随记录固化，之后不再变化；
- `tag_pointers` 是独立的"标签当前指向"映射。按标签查询时，是用映射表 JOIN 登记表取记录（`store.go:146-153`），而不是读取记录上的 `tag` 列。
- 因此出现"记录 A 的 `tag` 字段仍是 `release`，但 `release` 当前已指向 B"是正常状态，二者描述的是不同事实。

---

## 2. POST /v1/artifacts 请求链路

入口：`registerArtifact`（`internal/api/artifacts.go:151-178`）。

### 2.1 解码与规范化（`decodeArtifactInput`，`artifacts.go:57-111`）

1. 只接受**恰好一个 JSON 对象**：先 `Decode` 到 `map[string]json.RawMessage`，再要求下一个 token 必须是 `io.EOF`（`artifacts.go:59-66`）。数组、标量、`null`、两个对象、截断 JSON 均为 400。
2. 未知字段忽略：请求解进 map 后只按名字取六个必填键（`artifacts.go:68-109`），多余键从不检查。**[已有测试]** `TestRegisterArtifactNormalizesAndIgnoresUnknownFields`、`TestRegisterDuplicateReturnsOriginalRecord`（`artifacts_test.go:111`、`:172`）
3. 规范化（仅两个字段做首尾空白修剪）：
   - `repository`：`strings.TrimSpace` 后非空（`artifacts.go:68-74`）
   - `tag`：`strings.TrimSpace` 后非空（`artifacts.go:76-82`）
   - `digest`：**不做修剪**，必须整体匹配锚定正则 `^sha256:[0-9a-f]{64}$`（`artifacts.go:25`、`:84-91`），带空白或大写十六进制一律 400。**[新增回归]** `TestClientErrorShapeAndMessages` 中带前导空白的 digest 用例。
4. 类型与取值域校验：
   - `signature_verified` 必须是 JSON 布尔（`artifacts.go:93-95`、`requiredBool:125-135`）
   - `retention_days` 必须是整数且 `1..3650`（`artifacts.go:97-102`）；小数、指数、字符串经 `json.Unmarshal` 进 `int64` 直接失败（注释见 `:137-139`）
   - `size_bytes` 必须是非负整数（`artifacts.go:104-109`）

### 2.2 服务生成时间（`artifacts.go:166`）

处理层在调用存储层前生成 `PushedAt: time.Now().UTC().Format(time.RFC3339)`。客户端无法提供该字段（未知字段会被忽略），它是登记记录的一部分。**[已有测试]** `TestRegisterArtifactCreated` 断言其为 RFC3339 且时区为 UTC（`artifacts_test.go:98-108`）。

### 2.3 重复判定与事务（`Store.RegisterArtifact`，`store.go:73-112`）

全部操作在一个显式事务里（`tx, err := s.db.Begin()`，`store.go:74`）：

1. 以 `(repository, digest)` 查现有记录（`store.go:80-82`）。身份由这两个字段确定，schema 层也有 `UNIQUE(repository, digest)` 兜底（`store.go:181`）。
2. **无记录 → RegisterCreated**（`store.go:84-100`）：同一事务内依次
   - `INSERT INTO artifacts ...`（`:85-90`）
   - `INSERT INTO tag_pointers ... ON CONFLICT (repository, tag) DO UPDATE SET digest = excluded.digest`（`:91-96`）——新记录登记成功时，标签指向随之移动到新摘要；
   - `Commit`（`:97-99`）。
3. **有记录 → 逐字段比较**（`store.go:105-108`）：只比较 `tag`、`signature_verified`、`retention_days`、`size_bytes` 四项：
   - 全等 → 返回库中**原记录**与 `RegisterDuplicate`，不执行任何写操作（`:109`），标签指向不移动；
   - 任一不等 → `RegisterConflict`（`:111`），事务经 `defer tx.Rollback()`（`:78`）回滚，不写入任何内容。

### 2.4 相同判定包含什么，为什么不含服务生成时间

- 参与判定：身份键 `repository`、`digest`（决定查哪条），加内容四元组 `tag / signature_verified / retention_days / size_bytes`（`store.go:105-108`）。
- **不参与**：`pushed_at`。它由服务端在处理层生成（`artifacts.go:166`），不接受客户端输入，也不在比较列表中。因此两次内容相同但到达时间不同的提交，第二次只能被识别为重复并返回原 `pushed_at`，服务时间差异不可能制造冲突。**[已有测试]** `TestRegisterDuplicateReturnsOriginalRecord`（`artifacts_test.go:187-189`）；**[新增回归]** `TestRegistrationSequenceIdentityAndTagPointer` 中原样重试与规范化重试两次断言 `pushed_at` 不变。

### 2.5 处理层结果到 HTTP 响应（`artifacts.go:159-177`）

| 存储层结果 | HTTP | 响应 |
|---|---|---|
| `RegisterCreated` / `RegisterDuplicate` | **201** | 均返回记录本身（`artifactResponse`，`artifacts.go:43-53`）；重复时即原记录，`pushed_at` 为首次登记值 |
| `RegisterConflict` | **409** | `ArtifactConflictError`（`artifacts.go:172-175`） |
| 存储层返回 error | **503** | `storage_unavailable`（`artifacts.go:168-171`） |
| 解码/校验失败 | **400** | `InvalidArtifactInputError`（`artifacts.go:153-157`），发生在任何存储写入之前，不留记录 |

注意 201 不区分"新建"与"重复"：公开契约只有 201，区分仅存在于存储层内部枚举（`store.go:58-65`）。

---

## 3. GET /v1/artifacts 查询链路

入口：`queryArtifacts`（`artifacts.go:180-225`）。

1. 参数规范化与校验（`artifacts.go:182-191`）：
   - `repository` 必填、TrimSpace 后非空；
   - `tag`（TrimSpace）与 `digest`（**不修剪**，过同一锚定正则）至多给一个；给了空 tag 或非法 digest 都是 400。
2. 分派（`artifacts.go:197-210`）：
   - 仅 repository → `ListArtifacts`：`WHERE repository = ? ORDER BY id`（`store.go:115-136`），即**按首次登记顺序**返回全部记录；
   - `tag` → `ArtifactByTag`：`tag_pointers JOIN artifacts` 取当前指向（`store.go:146-153`）；
   - `digest` → `ArtifactByDigest`：按身份精确取记录（`store.go:139-143`）。
3. 结果映射（`artifacts.go:212-223`）：
   - `store.ErrNotFound`（`store.go:53`、由 `scanArtifact` 在 `sql.ErrNoRows` 时返回，`:159-161`）或列表为空 → **404 `ArtifactNotFoundError`**；
   - 其他存储错误 → **503 `storage_unavailable`**；
   - 命中 → **200** `{"artifacts":[...]}`，单记录查询也包装成长度 1 的数组。

仓库名、标签均区分大小写，源码中没有任何折叠大小写的处理。**[已有测试]** `TestQueryValidation` 的 `uppercase repository` 子例（`artifacts_test.go:268`）。

---

## 4. 完整请求序列（尚无记录的仓库，两个合法摘要、同一标签）

下列序列与新增用例 `TestRegistrationSequenceIdentityAndTagPointer` 一一对应，可直接执行验证。

- 仓库 `R = "registry-demo/control"`，标签 `T = "release"`
- 摘要 A = `sha256:` + 64 个 `a`；摘要 B = `sha256:` + 64 个 `b`
- A 的体积 1024，B 的体积 2048；其余字段相同（`signature_verified=true, retention_days=30`）
- `tA`、`tB` 为服务生成的 RFC3339 UTC 时间，测试中保存实际返回值并逐字比较，不硬编码。

### 步骤 1：登记 A（首次）

请求：

```http
POST /v1/artifacts
{"repository":"registry-demo/control","digest":"sha256:aaa…a（64 个 a）",
 "tag":"release","signature_verified":true,"retention_days":30,"size_bytes":1024}
```

- 状态码：**201**
- 关键响应字段：六个登记字段原样回显；`pushed_at = tA`（服务生成）。

### 步骤 2：登记 B（同仓库、同标签、不同摘要）

请求体同上，`digest` 换 B、`size_bytes` 换 2048。

- 状态码：**201**，`pushed_at = tB`（≠ `tA`）。
- 存储层在同一事务内插入 B 并把 `(R, release)` 的指向 upsert 为 B（`store.go:85-99`）。

### 步骤 3：原样重试 A

重复步骤 1 的请求体。

- 状态码：**201**（不是 200，也不是 409）。
- 响应是**原记录**：`pushed_at` 仍为 `tA`（用例逐字断言）。
- 不执行任何写语句，标签指向不移动。

### 每步之后的查询结果

| 查询 | 步骤 1 后 | 步骤 2 后 | 步骤 3 后 |
|---|---|---|---|
| `GET /v1/artifacts?repository=R`（列表） | 200，`[A]`，ORDER BY id | 200，`[A, B]`（首次登记顺序） | 200，仍只有 `[A, B]` 两条，顺序不变 |
| `GET /v1/artifacts?repository=R&tag=release` | 200，命中 A | 200，命中 **B**（JOIN tag_pointers） | 200，仍命中 **B**（重试不把指针移回 A） |
| `GET /v1/artifacts?repository=R&digest=A` | 200，A（`tA`） | 200，A（`tA`，旧记录仍可取回） | 200，A（`tA`） |

以上全部由 `TestRegistrationSequenceIdentityAndTagPointer` 断言：列表恰为两条且顺序 `A,B`；标签查询命中 B；摘要查询取回的 A 记录七字段逐字等于首次响应；重试后标签仍指 B。**[已有测试]** 亦覆盖同一事实的子集：`TestTagPointerMovesToNewDigestAndOldRecordSurvives`（`artifacts_test.go:206-237`）、`TestListArtifactsInRegistrationOrder`（`:239-256`）。

### 步骤 4：首尾空白规范化后的重复提交

```http
POST /v1/artifacts
{"repository":"  registry-demo/control  ","digest":"sha256:aaa…a",
 "tag":"  release  ","signature_verified":true,"retention_days":30,
 "size_bytes":1024,"unexpected":"ignored"}
```

- 状态码：**201**；repository/tag 经 TrimSpace 后身份与内容四元组不变，未知键 `unexpected` 被忽略。
- 返回原记录，`pushed_at = tA`；列表与标签指向均无变化。

依据：`artifacts.go:68-82`（修剪）、`:57-111`（未知字段不读取）、`store.go:105-109`（全等判重）。**[已有测试]** `TestRegisterDuplicateReturnsOriginalRecord`；**[新增回归]** 序列用例第 4 段。

### 步骤 5：分别改变 A 的四个可比较字段 → 全部 409，状态不变

在步骤 3 的库状态下，仅改 A 请求中的一个字段，各发一次：

| 变体 | 与原记录的差异 | 状态码 / code |
|---|---|---|
| 改标签 `"tag":"other"` | `tag` 不等 | **409 / `ArtifactConflictError`** |
| 改签名结果 `"signature_verified":false` | `signature_verified` 不等 | **409 / `ArtifactConflictError`** |
| 改保留天数 `"retention_days":31` | `retention_days` 不等 | **409 / `ArtifactConflictError`** |
| 改体积 `"size_bytes":2048` | `size_bytes` 不等 | **409 / `ArtifactConflictError`** |

每次冲突后用例同时核对（`TestRegistrationSequenceIdentityAndTagPointer` 的 `conflicts` 子测试）：

1. `digest=A` 查询返回的 A 记录七字段与首次响应**逐字一致**（含 `tag` 仍为 `release`、`pushed_at=tA`）——登记记录未被修改；
2. `tag=release` 仍命中 **B** 且其 `pushed_at=tB`——标签指向未受影响；
3. 列表仍是 `[A, B]` 两条，顺序不变；
4. 被拒请求体中的 `"other"` 标签从未落库：`tag=other` 返回 **404 `ArtifactNotFoundError`**。

冲突分支不执行 INSERT/upsert，事务回滚（`store.go:78`、`:111`）。**[已有测试]** `TestRegisterConflictOnDifferentContent` 仅覆盖改标签一种（`artifacts_test.go:192-204`）；签名/保留/体积三种及"冲突后状态不变"由 **[新增回归]** 补齐。

### 步骤 6：另一仓库使用相同摘要与标签，互不影响

在仓库 `R2 = "registry-demo/two"` 用相同的摘要 A、标签 `release` 登记：**201**，且与 R 中的 A 互不冲突——身份键包含 repository（`store.go:80-82`、`:181`），标签指向按仓库分区（`store.go:187`）。

用例 `TestRepositoriesDoNotInterfere` 断言：

- 两个仓库内 `tag=release` 各自 JOIN 回自己仓库的记录；
- R2 内对 A 制造 409 不改变 R 中 A 的任何字段；
- R2 随后登记 B 把 `release` 移到 B，R 的 `release` 仍指 A；
- 两个仓库的列表互不含对方记录（计数分别为 1 和 2）。

---

## 5. 保留天数仅被保存，签名结果仅被记录

- `retention_days` 在全代码库中只出现于：请求校验（`artifacts.go:97-102`）、透传写入（`artifacts.go:164` → `store.go:87-88`）、重复判定比较（`store.go:107`）、查询回显（`artifacts.go:49`、`store.go:127,149,158`）与 schema 列定义（`store.go:178`）。**没有任何代码基于它调度删除或过滤结果。**
- `signature_verified` 同理：校验为布尔、写入、参与全等比较、回显（`artifacts.go:93-95,48,163`；`store.go:87,106`）。**服务不执行任何密码学验证**：业务代码不 import 任何 `crypto/*` 包，也没有校验签名的调用点；该字段只是客户端申报结果的记录。**[源码推导]**（对全仓库 `crypto|x509|Ticker|go func|Delete|Expire` 的检索无业务命中；`go.mod` 中 `golang.org/x/crypto` 仅为间接依赖。）
- 没有 goroutine、定时器或任何后台任务：`main.go:12-31` 只做打开数据库、挂路由、`Run` 阻塞。**不会发生自动删除。**

行为侧佐证：**[新增回归]** 序列用例把两字段当作普通登记数据——改变它们产生 409，且原值原样可取回；没有任何测试或代码路径会让旧记录随时间消失。README 已公开声明"仅登记，不触发自动删除"（`README.md:48`），本次实现与该声明一致。

---

## 6. 错误返回路径与错误对象约定

错误统一由 `writeError` 输出（`artifacts.go:39-41`）：单个顶层 `error` 对象，仅含字符串 `code`、`message`。

| 路径 | 触发条件 | 状态码 | code | 源码 |
|---|---|---|---|---|
| POST 入参非法 | 非单个 JSON 对象、缺字段、空白仓库/标签、digest 不匹配、retention 越界、size 为负、类型错误 | 400 | `InvalidArtifactInputError` | `artifacts.go:153-157`，校验 `:57-149` |
| GET 参数非法 | 缺/空 repository、tag 与 digest 同给、空 tag、digest 不过正则 | 400 | `InvalidArtifactInputError` | `artifacts.go:187-191` |
| 合法查询无结果 | `ErrNotFound` 或仓库列表为空 | 404 | `ArtifactNotFoundError` | `artifacts.go:213-214`；`store.go:53,159-161` |
| 存储不可用 | 登记过程任意 DB error；查询中的非 NotFound error；健康检查 Ping 失败 | 503 | `storage_unavailable` | `artifacts.go:168-171`、`:215-216`；`router.go:19-22` |
| 未知路由 | NoRoute | 404 | `route_not_found` | `router.go:29-31` |

- 所有 `message` 都是处理器里的**静态字符串字面量**（`artifacts.go:155,173,189,214,216`；`router.go:20,30`）；存储层的错误信息只进入服务端 `fmt.Errorf` 包装（如 `store.go:76,89,95,102,119`），从不写进 HTTP 响应，因此响应不含 SQL、堆栈或文件路径。**[源码推导]** + **[新增回归]** `assertErrorResponse` 对全部 400/404/409/503 响应断言：error 对象恰有两个字段，并对 message 做 `sql/sqlite/insert/select/begin/.go// /goroutine/stack` 子串扫描。
- 400 发生在存储写入之前；**[已有测试]** `TestRegisterArtifactRejectsInvalidInput` 每个子例后追加列表查询，确认不留记录（`artifacts_test.go:163-167`）。
- 503 的实际触发验证：**[新增回归]** `TestStorageUnavailableReturns503` 先登记一条数据，再 `st.Close()` 关闭底层 SQLite，随后对 POST、GET 三种查询和 `/healthz` 逐一断言 503 + `storage_unavailable` + 固定 message（此前只有 `/healthz` 的成功路径有测试，见 `router_test.go:12`）。
- 404 的三类无结果（未知仓库、未知标签、未知摘要）：**[已有测试]** `TestQueryNotFound`（`artifacts_test.go:288-307`）；**[新增回归]** 另核对了 message 文案与错误对象形状。

---

## 7. 事务原子性与查询可见性边界

1. **登记与标签移动是原子的**。INSERT 记录与 upsert 标签指向在同一显式事务提交（`store.go:74-99`）；任何一步失败都走回滚（`:78`）。查询侧不存在"读到记录但标签缺失"或"标签指向一个尚不可见的摘要"的中间状态——后者若发生，按标签的 JOIN（`store.go:150-152`）会落空并返回 404，而原子性使其不可能出现。
2. **单次查询读取的是已提交状态**。每个 GET 只发一条 autocommit SELECT（列表 `store.go:116-117`、摘要 `:140-142`、标签 JOIN `:147-152`）；数据库以 WAL 模式打开（`store.go:23`），读不会阻塞写，也不会读到未提交数据。
3. **多个独立查询之间没有快照承诺**。服务不在多个 HTTP 请求间持有事务或快照（处理函数每次调用即结束）。因此连续两次 GET 之间若有别的客户端完成登记，第二次查询可能观察到新记录或新的标签指向——上文步骤表中"步骤 1 后标签指 A / 步骤 2 后指 B"正是这一边界的正常体现。**[源码推导]**：无会话事务/隔离级别设置；行为上由新增序列用例在单连接内验证"同一客户端前后两次查询能观察到已提交的指针移动"，但并发交错场景未做、也无需承诺更强保证。
4. 冲突与重复路径都不产生写入（`store.go:109,111` + 回滚），对并发读者等价于"什么都没发生"。

---

## 8. 重开同一数据库后的持久性

`store.Open` 对 schema 使用 `CREATE TABLE IF NOT EXISTS`（`store.go:27`、`:167-189`），不清理既有数据；WAL 已提交的事务落盘。重开同一路径后：

- 登记记录与 `pushed_at` 保留；
- 列表仍按 `id`（首次登记顺序）返回；
- `tag_pointers` 中的当前指向保留。

**[已有测试]** `TestRecordsSurviveRestart`（`artifacts_test.go:309-331`）验证标签查询与 `pushed_at`；**[新增回归]** `TestRestartPreservesRecordsOrderAndPointers` 补齐：重开后列表顺序 `[A,B]`、两个 `pushed_at` 逐字不变、标签仍指 B、按摘要仍可取回 A。启动配置（`ADDR`/`DB_PATH`，`main.go:13-20`）本次未改动。

---

## 9. 结论—依据—验证对照表

| # | 结论 | 源码依据 | 验证 |
|---|---|---|---|
| 1 | 身份 = `(repository, digest)`，schema 层唯一 | `store.go:80-82,181` | [已有] `TestTagPointerMoves…`；[新增] 序列/跨仓库用例 |
| 2 | 登记记录插入后不可修改（无 UPDATE/DELETE） | 全仓库仅 `store.go:93` 一条 UPDATE，且只作用于 `tag_pointers` | [源码推导]；[新增] 冲突后七字段逐字不变 |
| 3 | 标签指向仅在**新记录登记**时移动；重复与冲突不移动 | `store.go:91-96,105-111` | [已有] `TestTagPointerMoves…`；[新增] 序列用例 |
| 4 | 重复判定比较 tag/签名/保留/体积四项，全等返回 201 原记录 | `store.go:105-109` | [已有] `TestRegisterDuplicate…`；[新增] 四变体 409 |
| 5 | `pushed_at` 服务端生成（UTC RFC3339），不参与判定、永不改变 | `artifacts.go:166`；`store.go:105-108`（比较中无该项） | [已有] `TestRegisterArtifactCreated`、`…Duplicate…`；[新增] 序列/重开用例 |
| 6 | repository/tag 修剪；digest 不修剪、必须匹配小写正则；未知字段忽略 | `artifacts.go:59-91` | [已有] 规范化/非法输入表驱动用例；[新增] 带空白 digest、规范化重试 |
| 7 | retention 仅保存（1..3650 校验），无自动删除 | 见 §5 引用 | [源码推导] + [新增] 改 retention 触发 409 且原值不变 |
| 8 | signature 仅记录客户端申报，服务不验证签名 | 见 §5 引用 | [源码推导] + [新增] 改签名触发 409 且原值不变 |
| 9 | 列表按首次登记顺序；标签查当前指向；摘要查身份记录 | `store.go:117,140-152` | [已有] 列表/标签用例；[新增] 序列每步查询 |
| 10 | 400/404/409/503 路径与 code | 见 §6 表 | [已有] 400/404/409；[新增] 503（关闭 DB）、message 与无泄漏断言 |
| 11 | 错误对象仅 code/message 两个字符串，message 不泄漏内部信息 | `artifacts.go:39-41` 及各静态字面量 | [源码推导] + [新增] `assertErrorResponse` 形状与子串扫描 |
| 12 | 登记与标签移动原子；单次查询读已提交；独立查询间无快照 | `store.go:74-99,23`；处理函数无跨请求事务 | [源码推导] + [新增] 序列中前后查询观察到已提交移动 |
| 13 | 仓库间身份与标签隔离 | `store.go:181,187` 及全部 WHERE 条件含 repository | [新增] `TestRepositoriesDoNotInterfere` |
| 14 | 重开库后记录、推送时间、顺序、指向保留 | `store.go:23,27,117` | [已有] `TestRecordsSurviveRestart`；[新增] 顺序与双时间戳断言 |

---

## 10. 复现方式

```bash
go test ./...                          # 全部用例（基线 + 新增回归）
go test ./internal/api -run 'TestRegistrationSequenceIdentityAndTagPointer|TestRepositoriesDoNotInterfere|TestStorageUnavailableReturns503|TestClientErrorShapeAndMessages|TestRestartPreservesRecordsOrderAndPointers' -v
go vet ./...
```

新增用例全部经由公开 HTTP 入口（`httptest` 驱动 POST/GET），不直接调用存储层内部方法，因此任何保持公开契约的内部重构都应持续通过。
