# 制品身份与标签指向分析（基于源码）

本文回答三个问题：

1. 一次 `POST /v1/artifacts` 从进来到落库经历了什么；
2. 哪些数据是**不可修改的登记记录**，哪些是**可变化的标签指向**；
3. `retention_days` 与 `signature_verified` 被如何对待（仅保存/仅记录，不触发删除或校验）。

每条结论都标注源码位置（`文件:行号` 与函数名），并在第 9 节给出与回归用例的对照。

## 0. 证据分级约定

- **【源码推导】**：直接读源码可确定的行为，不依赖运行。
- **【实际验证】**：由自动化用例实际发起 HTTP 请求核对的行为，括注测试名。
- 推导与验证冲突时以验证为准；本文生成时二者一致。

源码基线：模块 `github.com/Bob-xisuke/orb-registry-catalog`，共四个包（`main.go`、`internal/api`、`internal/store`），数据库为 SQLite（modernc.org/sqlite 驱动）。

## 1. 数据模型：记录与指针是两张表

`internal/store/store.go` 的 `schema`（store.go:167-189）建两张表：

```
artifacts                          tag_pointers
─────────────────────────────────  ─────────────────────────────
id INTEGER AUTOINCREMENT           repository TEXT ─┐
repository TEXT        ─┐          tag        TEXT ─┼─ PRIMARY KEY
digest     TEXT        ├─ UNIQUE  digest     TEXT   （每个仓库每个标签一行）
tag        TEXT         │
signature_verified INT  │          digest 指向 artifacts 中的某条记录
retention_days     INT  │
size_bytes         INT  │
pushed_at          TEXT ─┘
```

- `artifacts` 的唯一约束是 `UNIQUE (repository, digest)`（store.go:181）——这是**制品身份**。
- `tag_pointers` 的主键是 `(repository, tag)`（store.go:187），保存“某仓库的某标签当前指向哪个 digest”——这是**标签指向**。
- 两表分离意味着：标签可以改指向，而记录行本身不动。

【源码推导】全库不存在针对 `artifacts` 表的 `UPDATE` 或 `DELETE` 语句；全库唯一的 `UPDATE` 子句是 `tag_pointers` 的 upsert（store.go:91-94）。`main.go` 只打开数据库并启动 HTTP 服务（main.go:22-30），没有启动任何后台 goroutine、定时器或清理任务。

## 2. `POST /v1/artifacts` 全链路

路由注册：`NewRouter`（internal/api/router.go:26）把 POST 交给 `registerArtifact`。

```
HTTP 请求
  │
  ▼
registerArtifact                 internal/api/artifacts.go:151
  ├─ decodeArtifactInput         artifacts.go:57   规范化 + 校验
  │    失败 → 400 InvalidArtifactInputError        artifacts.go:155
  ├─ 生成 pushed_at（服务端）      artifacts.go:166
  └─ Store.RegisterArtifact       internal/store/store.go:73
       ├─ Begin 事务                                          store.go:74
       ├─ 按 (repository, digest) 查现存记录                   store.go:80-82
       ├─ 不存在 → INSERT artifacts + UPSERT tag_pointers     store.go:85-96
       │           Commit → RegisterCreated                   store.go:97-100
       ├─ 已存在且四字段全等 → 直接返回原记录，不写入           store.go:105-109
       │                                          RegisterDuplicate
       └─ 已存在但四字段不同 → 不写入，冲突                    store.go:111
                                                  RegisterConflict
  │
  ▼ 结果映射（artifacts.go:168-176）
  error     → 503 storage_unavailable
  conflict  → 409 ArtifactConflictError
  created/duplicate → 201 + artifactResponse（artifacts.go:43-53）
```

### 2.1 规范化与校验（decodeArtifactInput，artifacts.go:57-111）

- 请求体必须是**恰好一个 JSON 对象**：首个 token 不是对象或对象之后还有第二个 token 都拒绝（artifacts.go:60-66）。
- `repository`、`tag`：`strings.TrimSpace` 去首尾空白，去后为空即非法（artifacts.go:72-74、79-81）。
- `digest`：必须精确匹配 `^sha256:[0-9a-f]{64}$`（artifacts.go:25、88-90），不 trim，大写/短一位/换算法前缀均拒绝。
- `signature_verified`：必须是 JSON 布尔（artifacts.go:93-95）。
- `retention_days`：必须能解码进 `int64`，拒绝小数/指数/字符串（artifacts.go:139-149），且 1–3650（artifacts.go:100-102）。
- `size_bytes`：`int64` 且非负（artifacts.go:104-109）。
- **未知字段忽略**：先解码进 `map[string]json.RawMessage`（artifacts.go:60），只按名字取已知键，多余键从不报错——这是结构使然，不是特例（artifacts.go:56 注释明示）。

### 2.2 `pushed_at` 由服务生成（artifacts.go:166）

```go
PushedAt: time.Now().UTC().Format(time.RFC3339),
```

请求结构体里根本没有 `pushed_at` 字段（artifactInput，artifacts.go:30-37）；客户端即使在 JSON 里带上也会按未知字段忽略。重复提交时这个新生成的值随调用进入存储层，但 duplicate 分支返回的是库里查出的 `existing`（store.go:109），新值被丢弃——这正是“重试返回原 `pushed_at`”的机制原因。

### 2.3 事务里的三种结果（RegisterArtifact，store.go:73-112）

| 情况 | 判定 | 写入 | 事务 | 返回 |
|---|---|---|---|---|
| 身份不存在 | `ErrNotFound`（store.go:84） | INSERT artifacts（85-90）+ UPSERT tag_pointers（91-96） | Commit（97） | 新记录，`RegisterCreated` |
| 身份存在且内容相同 | 四字段全等（105-108） | 无 | 不 Commit（defer Rollback 空事务，78） | 原记录，`RegisterDuplicate` |
| 身份存在但内容不同 | 任一不等 | 无（只有 SELECT） | 同上 | 空记录，`RegisterConflict` |

标签指针 upsert 语义（store.go:91-94）：`(repository, tag)` 已存在就把 `digest` 改成新值，否则插入。即**只有首次登记新记录时才可能移动标签**；重复与冲突路径都不碰 `tag_pointers`。

## 3. `GET /v1/artifacts` 全链路

路由：router.go:27，处理函数 `queryArtifacts`（artifacts.go:180-225）。

参数规范化与校验（artifacts.go:182-191）：

- `repository` 必填（trim 后非空）；
- `tag`、`digest` 至多一个，同时给 → 400；
- `tag` 做 trim，给了但 trim 后为空 → 400；`digest` 不 trim，必须匹配同一正则。

分派（artifacts.go:197-210）：

| 查询 | 存储函数 | SQL 语义 |
|---|---|---|
| 仅 repository | `ListArtifacts`（store.go:115-136） | `WHERE repository=? ORDER BY id`——按自增主键即**首次登记顺序** |
| repository + tag | `ArtifactByTag`（store.go:146-153） | `tag_pointers JOIN artifacts`，先查指针再取记录 |
| repository + digest | `ArtifactByDigest`（store.go:139-143） | 按身份精确取行 |

结果映射（artifacts.go:212-223）：`ErrNotFound` 或仓库存在但列表为空 → **404 ArtifactNotFoundError**；其他存储错误 → **503 storage_unavailable**；命中 → 200 `{"artifacts":[...]}`。注意标签查询的 JOIN 以指针为准：如果指针指向的记录行不存在会得到 404；而记录行只能被插入、不能被改删（第 1 节），所以正常操作下不会出现悬空指针。

## 4. 关键结论

### 4.1 登记记录不可修改【源码推导】

- 身份 `(repository, digest)` 唯一（store.go:181）；身份命中后没有更新分支：全等走 duplicate 回显原值（store.go:109），不等走 conflict 拒绝（store.go:111）。
- 记录上的 `tag`、`signature_verified`、`retention_days`、`size_bytes` 是**首次登记时的快照**，之后任何请求都改不动；`pushed_at` 同理且由服务生成。
- 已验证：冲突后按摘要回查，原四字段与 `pushed_at` 全部不变（`TestConflictMatrixKeepsRecordAndTag`）。

### 4.2 标签指向可变化，但只随“新记录首次登记”移动【源码推导 + 实际验证】

- 指向存于独立的 `tag_pointers` 表，新 digest 用同一标签登记时 upsert 改向（store.go:91-94），与记录插入在同一事务（第 4.5 节）。
- 被换下来的旧记录仍在 `artifacts` 中，按 digest 照常可取（`ArtifactByDigest` 不过滤标签）。
- 原样重试旧登记是 duplicate，不会把指针拉回（store.go:105-109 无写入）。
- 已验证：A→B 同标签后，标签命中 B、摘要仍可取回 A、重试 A 不回摆（`TestArtifactIdentityAndTagPointerSequence`，既有 `TestTagPointerMovesToNewDigestAndOldRecordSurvives` 也覆盖同一结论）。

### 4.3 “相同判定”包含什么，为什么不含 `pushed_at`【源码推导】

判定分两层：

1. **身份**：`(repository, digest)` 决定查哪条记录（store.go:80-82）；
2. **内容相同**：仅比较 `tag`、`signature_verified`、`retention_days`、`size_bytes` 四个字段（store.go:105-108）。

不参与比较的：

- `pushed_at`：它是服务端每次请求临时生成的时间戳（artifacts.go:166），不是客户端声明的制品内容；若把它纳入比较，任何重试都会因时间不同而误报冲突。duplicate 分支回显库存值而丢弃新生成值，保证重试幂等。
- `repository`、`digest`：是查找键本身，不需要再比。

规范化先于判定：`repository`/`tag` 比较的是 trim 后的值（artifacts.go:72、80），所以带首尾空白的重试仍算相同。

### 4.4 `retention_days` 仅被保存，`signature_verified` 仅被记录【源码推导 + 实际验证】

证据链：

1. 两者只是表列（store.go:177-178），登记时写入（store.go:87-88），查询时回显（store.go:148-149、artifactResponse artifacts.go:48-49）。
2. 它们唯一影响行为的地方是冲突判定中的相等比较（store.go:106-107）。
3. 代码里没有任何基于 `signature_verified` 取值的分支（true/false 都走同一条登记路径），也没有任何读取 `retention_days` 计算到期/删除的逻辑——全库无 `DELETE`、无定时器、无 goroutine（对全仓搜索 `DELETE|ticker|cron|go func|time.After` 的结果为空）。
4. 公开路由只有 `GET /healthz`、`POST /v1/artifacts`、`GET /v1/artifacts`（router.go:18-27），不存在删除入口。

因此：服务不会因保留天数到期自动删除记录，也不会验证签名或因签名为 false 拒绝登记。已验证的可观测代理行为：`signature_verified:false`、`retention_days:1` 可正常登记并原样回显；对登记 URL 发 `DELETE`/`PUT` 得到路由级 404，记录随后仍可查（`TestRetentionAndSignatureAreRecordedButNotActedOn`）。“未来时刻不会被自动删除”这一点本身无法用短时测试穷尽，标注为源码推导。

### 4.5 原子性与可见性边界【源码推导】

- **登记与标签移动原子**：插入记录与 upsert 指针在同一事务（store.go:74、85-99），读者不可能看到“记录有了但标签没动”或反之；任一步失败事务回滚（defer rollback，store.go:78）。
- **冲突/重复不产生写操作**：无需回滚数据，只是结束只读事务。
- **单次查询读到已提交状态**：三个查询函数各自直接通过 `s.db` 发起单条语句（store.go:117、140、147），SQLite 连接处于自动提交，每条语句看到的是该语句开始时已提交的快照（WAL 模式，store.go:23）。
- **多个独立查询之间没有统一快照**：连续发两个 HTTP 查询是两条独立语句、两个独立请求，中间可能有其他登记提交；服务不提供跨查询的读一致性。需要“同一时刻视图”的调用方不能依赖两次 GET 的组合结果。

### 4.6 重开数据库后持久保留【实际验证】

数据库是文件型 SQLite（`DB_PATH`，main.go:17-20；`Open` 用 `CREATE TABLE IF NOT EXISTS`，store.go:27），WAL 下提交即落盘。重开后记录、`pushed_at`、标签指针都保留：既有 `TestRecordsSurviveRestart` 验证单记录场景，新增 `TestTagPointerAndRecordsSurviveRestart` 额外验证 A→B 换向后重开，列表顺序、标签指向 B、摘要可取回 A、原时间戳不变。

## 5. 完整请求序列（新仓库、同标签、两个摘要）

以下每一步都是对公开入口的真实调用形态；自动化版本见 `TestArtifactIdentityAndTagPointerSequence`。示例仓库 `team/demo`（调用前无任何记录），标签统一为 `v1`，`pushed_at` 为示例值。

摘要：
- A = `sha256:` + 64 个 `a`
- B = `sha256:` + 64 个 `b`

### 步骤 1：登记 A → 201

请求：

```http
POST /v1/artifacts
{"repository":"team/demo","digest":"<A>","tag":"v1",
 "signature_verified":true,"retention_days":30,"size_bytes":1024}
```

- 状态码：**201**
- 关键响应：`digest=<A>`、`tag="v1"`、`signature_verified=true`、`retention_days=30`、`size_bytes=1024`、`pushed_at=T1`（服务生成，RFC3339 UTC）。
- 后续查询：`GET /v1/artifacts?repository=team/demo` → 200，列表 1 条；`...&tag=v1` → A。

### 步骤 2：登记 B（同标签）→ 201，标签移动

请求同上，digest 换 `<B>`，其余字段与 A 一致。

- 状态码：**201**，`pushed_at=T2`（T2 由服务生成；与 T1 不同，但**不参与**任何判定）。
- 同一事务内插入 B 并把 `v1` 指针 upsert 到 B（store.go:85-99）。
- 后续查询：
  - `...&tag=v1` → 200，命中 **B**（经 tag_pointers JOIN）；
  - `...&digest=<A>` → 200，仍取回 **A**（旧记录未被改删）；
  - `GET ...?repository=team/demo` → 200，**两条**，顺序 **A 在前、B 在后**（`ORDER BY id`）。

### 步骤 3：原样重试 A → 201，返回原记录

请求体与步骤 1 **逐字节相同**。

- 状态码：**201**（duplicate 同样返回 201，不是 200/409）。
- 响应为库存原记录：`pushed_at` 仍为 **T1**（本次新生成的时间戳在 store.go:109 被丢弃），四字段与步骤 1 一致。
- 标签指针**不动**：`...&tag=v1` 仍命中 B；列表仍只有 A、B 两条且顺序不变。

## 6. 409 矩阵：四字段单独变化都冲突且不改动数据

在步骤 1 之后，保持身份 `(team/demo, <A>)` 与其余三个字段不变，只改一个字段，重复登记。四种请求都返回：

- 状态码：**409**；
- 响应体：`{"error":{"code":"ArtifactConflictError","message":"an artifact with this repository and digest already exists with different content"}}`（artifacts.go:172-174，固定文案）。

| 变化字段 | 请求差异 | 存储层依据 |
|---|---|---|
| 标签 | `"tag":"v2"` | existing.Tag != a.Tag（store.go:105） |
| 签名结果 | `"signature_verified":false` | store.go:106 |
| 保留天数 | `"retention_days":7` | store.go:107 |
| 体积 | `"size_bytes":2048` | store.go:108 |

每次 409 后的不变量（用例逐步核对）：

- `...&tag=v1` 仍命中 **A**（标签指向不变）；
- `...&digest=<A>` 的四字段仍是 true/30/1024、tag 仍 `v1`（记录不变）；
- 仓库列表仍只有 1 条记录（冲突路径无 INSERT）。

注意标签的 409 语义：即使请求写的是另一个标签 `v2`，服务也**不会**把 `v2` 指向 A——身份已存在的记录不可借重复登记改写，任何字段差异一律整体冲突。

## 7. 规范化重复、未知字段、跨仓库隔离

### 7.1 首尾空白规范化后仍判重【实际验证】

对 A 的重试把 `repository` 写成 `" team/demo "`、`tag` 写成 `" v1 "`：

- 经 trim 后身份与四字段不变（artifacts.go:72、80）→ **201 + 原记录**，`pushed_at=T1`，指针不移动、记录数不增加。
- 关键边界：**digest 不做 trim**（artifacts.go:88 直接匹配原值），所以空白只允许出现在 `repository`、`tag` 两端。

### 7.2 未知字段忽略【实际验证】

请求中加入 `"unexpected":123` 或客户端自造的 `"pushed_at"`：

- 多余键不参与解码（artifacts.go:60），请求仍 **201**；
- 客户端伪造的 `pushed_at` 不会生效，响应中的时间仍为服务生成的 T1（`TestClientCannotOverridePushedAt`）。

### 7.3 另一仓库使用相同摘要与标签互不影响【实际验证】

在 `team/other` 用**相同的 digest A、相同标签 v1** 登记：

- 身份含 repository，是一条**新记录** → **201**；
- `tag_pointers` 主键也含 repository，两条指针各自独立；
- `team/demo` 列表仍是 A、B 两条；两仓库 `...&tag=v1` 分别命中自己的 A；
- 在 `team/demo` 中按 `<B>` 之外不存在的摘要查询仍 404，仓库间无串扰。

对应用例：`TestSameDigestAndTagInOtherRepositoryIndependent`。

## 8. 错误返回路径

所有错误沿用同一形状（writeError，artifacts.go:39-41）：顶层单个 `error` 对象，`code`、`message` 均为字符串。

| 场景 | 状态码 | code | 触发位置 |
|---|---|---|---|
| 请求非法（坏 JSON、缺字段、越界、类型错；查询参数非法） | 400 | `InvalidArtifactInputError` | artifacts.go:155、189 |
| 合法查询无结果（未知仓库/标签/摘要；空仓库列表） | 404 | `ArtifactNotFoundError` | artifacts.go:213-214 |
| 存储不可用（存储层返回任何 error） | 503 | `storage_unavailable` | artifacts.go:169、216；healthz 同 code（router.go:19-22） |

- 非法登记在进入存储层之前就被拒绝（artifacts.go:153-157），**不留下任何记录**（既有 `TestRegisterArtifactRejectsInvalidInput` 每次拒绝后回查 404）。
- 503 的触发与文案：存储层错误经 `fmt.Errorf` 包装（如 store.go:76、89），但 API 层只看 `err != nil`，对外统一输出固定文案 `"database is not available"`，内部错误文本不透出。用例在关闭数据库句柄后发起 POST、GET 与 /healthz，核对均为 503 + storage_unavailable（`TestStorageUnavailableWhenDatabaseClosed`）。
- 消息不泄露：五条对外文案（artifacts.go:155、169、173、189、214、216）均为不含 SQL、表名、堆栈、文件路径的固定英文句；用例对 400/404/409/503 四类响应断言不含 `SELECT`/`INSERT`/`UPDATE`/`sql`/`.go`/路径分隔符/`goroutine` 等标记（`TestErrorMessagesDoNotLeakInternals`）。

## 9. 结论—源码—测试 追溯表

| 结论 | 源码依据 | 既有测试（已覆盖） | 本次新增用例 |
|---|---|---|---|
| 规范化（trim）与未知字段忽略 | artifacts.go:57-111 | `TestRegisterArtifactNormalizesAndIgnoresUnknownFields` | 7.1 判重场景、`TestClientCannotOverridePushedAt` |
| 非法输入 400 且不落记录 | artifacts.go:153-157 | `TestRegisterArtifactRejectsInvalidInput`、`TestQueryValidation` | — |
| 身份=(repository,digest)，四字段判重，pushed_at 不参与 | store.go:80-82、105-109；artifacts.go:166 | `TestRegisterDuplicateReturnsOriginalRecord` | `TestArtifactIdentityAndTagPointerSequence` |
| 冲突 409，记录与指针都不变 | store.go:105-111 | `TestRegisterConflictOnDifferentContent`（仅 tag 一维） | `TestConflictMatrixKeepsRecordAndTag`（四维 + 不变量回查） |
| 标签随新记录移动、旧记录留存、重试不回摆 | store.go:91-96、109 | `TestTagPointerMovesToNewDigestAndOldRecordSurvives` | 完整序列用例 |
| 列表按首次登记顺序 | store.go:117 | `TestListArtifactsInRegistrationOrder` | 序列中核对两条与顺序 |
| 合法查询无结果 404 | artifacts.go:212-214 | `TestQueryNotFound` | — |
| 存储不可用 503 | artifacts.go:169、216 | 无（缺失） | `TestStorageUnavailableWhenDatabaseClosed` |
| 错误消息不含内部信息 | artifacts.go:39-41 及各固定文案 | 仅断言行/码 | `TestErrorMessagesDoNotLeakInternals` |
| 保留天数仅保存、签名仅记录、无删除/校验动作 | store.go:177-178、全库无 DELETE/调度；router.go:26-27 | 无 | `TestRetentionAndSignatureAreRecordedButNotActedOn` |
| 登记与标签移动同事务 | store.go:74-99 | 间接覆盖（序列表现） | 序列用例的联合断言 |
| 重开后记录/时间/指针保留 | store.go:23、27 | `TestRecordsSurviveRestart`（单记录） | `TestTagPointerAndRecordsSurviveRestart`（含换向后指针） |
| 跨仓库身份与指针隔离 | store.go:181、187 | 无 | `TestSameDigestAndTagInOtherRepositoryIndependent` |

“多个独立查询之间不保证同一快照”（第 4.5 节）属并发调度下的源码推导，本次未做依赖时序的脆弱用例。

## 10. 本次交付边界

- 未改动：`README.md`、`GET /healthz` 行为、`main.go` 启动配置（`ADDR`/`DB_PATH`）、数据库 schema 与任何既有数据；公开请求/响应契约保持不变。
- 新增：本文档（源码推导与已验证行为分开标注）与 `internal/api` 下针对公开 HTTP 入口的回归用例，全部通过 `go test ./...`。
