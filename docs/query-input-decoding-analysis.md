# 查询请求重复参数与 URL 转义取值分析（附回归用例）

本文基于当前基线源码，说明 `GET /v1/artifacts` 的输入边界：请求行里的原始查询串（raw query）如何经**参数解析、百分号解码、规范化与校验、查询分派**，成为 `artifacts` 响应或一个错误对象。记录身份（`(repository, digest)` 唯一、登记后不可修改）与存储边界已由 `docs/artifact-identity-analysis.md`、`docs/storage-boundary-analysis.md` 覆盖，本文不再重复；本文聚焦此前未单独分析的两件事——**同名参数重复出现时的取值规则**与**查询串 URL 转义（含 `+` 与 `%2B`）的取值规则**。

- 源码版本：工作区当前提交（`d1e9251`），工具链 go1.26.8 linux/amd64；标准库引用以该工具链 `net/url` 源码为准，第三方引用以 `go.mod` 锁定的 `github.com/gin-gonic/gin v1.10.0` 为准。
- 验证方式标注：
  - **[已有测试]** 基线测试已覆盖，注明函数与文件
  - **[新增回归]** 本次补充的用例，全部位于 `internal/api/query_input_test.go`
  - **[源码推导]** 由源码结构、标准库或 gin 语义直接得出（无对应用例）
- 本次只新增测试与本文档；生产代码、`main.go` 启动配置、README、`docs/` 下既有分析文档均未改动。所有新增用例只经公开 HTTP 入口（`httptest` 驱动 GET，POST 仅用于播种既有记录）核对目标记录、错误对象与固定错误消息，不触碰存储层内部。

---

## 1. 从原始请求到响应：四个阶段

路由在装配时注册 `router.GET("/v1/artifacts", queryArtifacts(svc))`（`internal/api/router.go:48`）；两种装配入口 `NewRouter`（`router.go:21-23`）与 `NewRouterWithStore`（`router.go:32-54`）挂的是同一个处理函数。一个 GET 请求依次经历：

1. **参数解析（gin → 标准库）**。处理函数 `queryArtifacts`（`internal/api/artifacts.go:187-226`）通过 gin 上下文取三个参数：
   - `c.Query("repository")`（`artifacts.go:189`）：gin 的 `Context.Query` 只是 `GetQuery` 丢弃布尔值（gin `context.go:426-428`）；
   - `c.GetQuery("tag")`、`c.GetQuery("digest")`（`artifacts.go:190,192`）：gin `GetQuery` 取 `GetQueryArray` 结果的**第一个值** `values[0]`（gin `context.go:455-460`）。
   - 二者都在首次使用时由 `initQueryCache` 惰性调用 `c.Request.URL.Query()` 构建缓存（gin `context.go:469-485`）。`(*url.URL).Query` 调用 `ParseQuery` 并**丢弃其返回的 error**（标准库 `net/url/url.go:1144-1147`、`932-936`）。
2. **百分号解码（标准库）**。`parseQuery` 逐个键值对处理：按 `&` 切分、按**第一个** `=` 切出键与值，然后对键和值**分别**调用 `QueryUnescape`，最后 `m[key] = append(m[key], value)` 按出现顺序追加（`url.go:959-991`）。`QueryUnescape`（`url.go:89-92`）在查询分量模式下：`%XX` 按十六进制解码为字节（解码循环 `url.go:170-174`）；字面 `+` 解码为空格（`url.go:144-145,160-165,175-176`）；`%2B` 解码为字面加号（同一解码循环，`%2B` 先按 `%XX` 解码，`url.go:170-174`）。
3. **规范化与校验（处理函数）**。`repository` 取首个值后 `strings.TrimSpace`（`artifacts.go:189`）；`tag` 取首个值后 `TrimSpace`，但用 `GetQuery` 的布尔值保留"参数是否出现"（`artifacts.go:190-191`）；`digest` 取首个值、**不修剪**，直接交给锚定正则 `^sha256:[0-9a-f]{64}$`（`artifacts.go:24,192,195`）。一个布尔表达式集中决定 400（`artifacts.go:194-198`）。
4. **查询分派与响应**。校验通过后构造 `service.Query`，按 `hasTag`/`hasDigest` 三选一分派（`artifacts.go:200-210`），调用 `svc.Query`（`internal/service/service.go:186-218`）：标签查 `RecordByTag`、摘要查 `RecordByDigest`、都没有则 `ListRecords`（`service.go:191-206`；存储契约 `service.go:137-142`）。命中由 `artifactResponse` 组装为 `{"artifacts":[...]}`（`artifacts.go:42-52,218-224`）；未命中 404、存储故障 503（`artifacts.go:213-217`）。

与登记入口的关键取向差异在此先点明：POST body 解进 `map[string]json.RawMessage`，**重复 JSON 键是后者覆盖前者**（见 `docs/registration-input-decoding-analysis.md` §2）；GET 查询串解进 `url.Values`（`map[string][]string`）并以 `values[0]` 取值，**重复查询参数是第一个出现者生效**。两者规则相反，且都不是"取最后值"在两个入口上的统一约定。

---

## 2. 重复参数：同名多次出现时以第一个值为准

### 2.1 机制：追加成切片，读取首元素

`parseQuery` 对每个键值对 `m[key] = append(m[key], value)`（`url.go:988`），从不覆盖；读取侧 `Values.Get` 返回 `vs[0]`（`url.go:891-897`），gin `GetQuery` 同样返回 `values[0]`（gin `context.go:457`）。因此第 2、3 次出现的值**根本不进入处理函数**，既不参与校验，也不参与分派 **[源码推导]**，行为由 **[新增回归]** 验证。

### 2.2 合法首值即查询目标；后续值既不能改目标，也不能补救首值未命中

`repository`、`tag`、`digest` 三个被识别参数都遵守同一规则：

- `tag=alpha&tag=beta` → 命中 alpha 指向的记录；颠倒顺序命中 beta；出现三次时第三值被忽略。`digest=A&digest=B` 同理，返回的是首值摘要的记录。**[新增回归]** `TestRepeatedQueryParameterFirstValueWins`。
- `repository=<存在的仓库>&repository=<别的仓库>&tag=alpha` 在**存在的仓库**内命中；镜像写法 `repository=<不存在的仓库>&repository=<存在的仓库>&tag=alpha` 是 **404** 而非 200——首值已经把查询范围定在空仓库，后值无法补救。同上用例。
- `tag=missing&tag=release`（首标签不存在）与 `digest=C&digest=A`（C 未登记）保持 **404**：后值即使能命中也不改变结果。**[新增回归]** `TestRepeatedQueryParameterInvalidFirstValue` 的 `tag first misses then hits`、`digest first misses then hits` 子例。

### 2.3 首值非法 → 同一个 400；后续合法值不能补救，后续非法值也不能覆盖

校验只看首值（`artifacts.go:194-198`），因此：

- 首值非法、后续全合法，仍是 **400** `InvalidArtifactInputError` / `"query parameters are not valid"`：`repository=&repository=<合法>`（首值为空）、`repository=%20%20&...`（首值修剪后为空）、`repository&repository=...`（首个仅有参数名）、`tag=&tag=release`、`tag=%20%20&tag=release`、`tag&tag=release`、`digest=sha256:xyz&digest=<合法摘要>`、`digest&digest=<合法摘要>`、`digest=+<合法摘要>&...`（首值解码出前导空格、又不修剪，正则不匹配）。**[新增回归]** `TestRepeatedQueryParameterInvalidFirstValue` 的 9 个非法首值子例。
- 首值合法、后续非法，**不降级为 400**：`tag=release&tag=`、`tag=release&tag=%20%20`、`digest=<合法>&digest=sha256:xyz`、`repository=<合法>&repository=%20` 全部按首值正常命中 **200**；后续值不会"污染"首值。同用例的合法首值子例。

这与单值场景的拒绝路径完全相同：单值非法的基线覆盖见 **[已有测试]** `TestQueryValidation`（`internal/api/artifacts_test.go:258-286`，`blank repository`/`blank tag`/`malformed digest`）与 `TestQueryEmptyAndBareParameters`（`internal/api/regression_test.go:494-540`）。

### 2.4 与登记入口的对照

POST 重复 JSON 键以**最后**值为准、先前非法值可被最后合法值挽救（`docs/registration-input-decoding-analysis.md` §2，**[已有测试]** `TestDuplicateKeysLastValueWins`）；GET 重复查询参数以**首个**值为准、后续值对结果没有任何影响力。这是两种传输格式（JSON 对象 vs `url.Values` 多值映射）的解析语义差异，不是处理函数刻意为之的业务规则 **[源码推导]**。

---

## 3. 参数名先百分号解码再匹配；匹配区分大小写；未知参数继续忽略

### 3.1 按解码后的参数名识别

`parseQuery` 在把键值对放入 map 前先对**键**调用 `QueryUnescape`（`url.go:974-980`），map 的键是解码后的字符串；处理函数按字面名 `"repository"`/`"tag"`/`"digest"` 取值（`artifacts.go:189-192`）。因此：

- `%72epository`（`%72`='r'）、`%74ag`（`%74`='t'）、`%64igest`（`%64`='d'）分别就是三个已知参数，单独出现即触发对应分派：`...&%74ag=release` 是标签查询、`...&%64igest=<摘要>` 是摘要查询。
- 转义可出现在参数名任意位置：`%74a%67` 同样解码为 `tag` **[源码推导]**（逐 `%XX` 解码，`url.go:170-174`）。
- 解码后同名的两种拼写构成 §2 意义上的重复参数，首值规则照样按文档顺序生效：`%74ag=missing&tag=release` → **404**（首值缺失）；`tag=release&%74ag=missing` → **200** 命中。`%72epository=<空仓库>&repository=<存在的仓库>` 同理为 404。

**[新增回归]** `TestPercentDecodedParameterNamesAndCaseSensitivity` 的 6 个碰撞子例。

### 3.2 参数名区分大小写

map 按键的精确字符串索引，没有大小写归一：`Tag`、`TAG`、`Repository`、`DIGEST` 都是**未知参数**，不是已知参数的别名。

- `repository=<repo>&Tag=release`：tag 实际缺失，请求**合法地退化为仓库列表**（200、两条记录），而不是标签查询（一条记录）——用列表与单查的记录数差异把"被忽略"钉死。
- `Repository=<repo>&tag=release`：必需的 `repository` 缺失 → **400**，大小写变体不能顶替。
- 已知参数后的大小写重复（`tag=release&TAG=other`、`digest=A&DIGEST=B`）不改变目标。

**[新增回归]** 同用例的 `Tag=` 列表、`Repository=` 400 与大小写重复段。值的大小写敏感（仓库名 `TEAM/APP` 合法但无匹配 → 404，而非 400）由 **[已有测试]** `TestQueryValidation` 的 `uppercase repository` 子例覆盖（`artifacts_test.go:268-276`）。

### 3.3 未知参数继续忽略，但不能代替已知参数

处理函数只按三个名字取值，任何其他键（含其重复形式 `unknown=1&unknown=2`、空值 `frob=`）都不影响分派与校验。**[新增回归]** 同用例末段（未知参数重复仍 200 单记录）。但"名字相近"不等于"名字相同"：`Repository=` 不能满足缺失的 `repository`（§3.2 的 400）。"未知键不检查、只按名取已知键"这一取数方式与登记侧 `decodeArtifactInput` 同源（`artifacts.go:67-108`，**[已有测试]** `TestRegisterArtifactNormalizesAndIgnoresUnknownFields` 覆盖的是登记侧）；查询侧的未知参数忽略此前没有直接用例，本次为首次覆盖。

### 3.4 畸形百分号转义：该键值对不入 map（边界备注）

`%` 后不是两位十六进制（如 `%zz`）时 `QueryUnescape` 返回错误，`parseQuery` 记录错误后 `continue`——**该键值对不追加进 map**（`url.go:974-987`）；而 `URL.Query` 丢弃解析错误（`url.go:1145`）。因此 `tag=%zz` 或 `%74ag%zz=x` 在处理函数看来等同于该参数**未出现**：在仓库合法时退化为列表，而不是 400。**[源码推导]** 本次未为该边界保留回归用例；它不属于本文验收结论，仅记录与"未知参数忽略"的相邻关系，避免误判为"畸形转义应当 400"。

---

## 4. 解码后的规范化：修剪边界，以及 `+` 与 `%2B` 指向不同记录

### 4.1 各参数的修剪边界

| 参数 | 存在性判定 | 取值后处理 | 非法判定 |
|---|---|---|---|
| `repository` | `Query`（不区分缺失与空） | `TrimSpace`（`artifacts.go:189`） | 修剪后为空 → 400（`:194`） |
| `tag` | `GetQuery` 布尔值区分缺失（`:190`） | `TrimSpace`（`:191`） | 出现且修剪后为空 → 400（`:195`） |
| `digest` | `GetQuery` 布尔值（`:192`） | **不修剪** | 不匹配锚定正则 → 400（`:24,195`） |

- 仓库名与标签在 URL 解码**之后**做首尾空白修剪；摘要在解码后原样匹配正则，前导/尾随空白（无论写成 `+` 还是 `%20`）都使其落在 `sha256:` 形态之外 → **400**，而不是修剪后命中、也不是 404。这与登记入口"digest 从不修剪"一致（**[已有测试]** `TestClientErrorShapeAndMessages` 的前导空格登记段，`regression_test.go:348-353`）。
- **[新增回归]** `TestPlusVersusPercent2BTagDecoding` 的 `digest=+<摘要>`、`digest=<摘要>+`、`digest=%20<摘要>` 三个 400 子例；`TestRepeatedQueryParameterInvalidFirstValue` 的 `digest spaced first`。

### 4.2 缺失与空值的差别

存在性由 `GetQuery` 的第二个返回值保留，取值为空字符串不代表缺失（gin `context.go:443-460`；处理函数 `artifacts.go:190-195`）：

| 情形 | 观察 | 结果 |
|---|---|---|
| `repository` 完全缺失 | `Query` 得 `""` | 400（`:194`） |
| `repository=` / `repository` / `repository=%20` | 空或修剪后为空 | 400 |
| `tag` 完全缺失（仅有仓库） | `hasTag=false` | 合法：仓库列表 |
| `tag=` / `tag` / `tag=%20` | 出现但修剪后为空 | 400（`:195`） |
| `digest=` / `digest` / `digest=<空白>` | 出现但空/不符格式 | 400 |
| tag 与 digest **同时出现** | 两个布尔值都为真 | 400，即使一个或两个值为空（`:194`） |

任何一种 400 都**不能退化为列表查询**。完整矩阵由 **[已有测试]** `TestQueryEmptyAndBareParameters`（`regression_test.go:494-540`，13 个子例）与 `TestQueryValidation`（`artifacts_test.go:262-285`）覆盖；本次新增的是这些形态在**重复参数首值**位置上的同样结论（§2.3）以及存储故障下的优先级（§6）。

### 4.3 `+` 解码为空格、`%2B` 解码为加号：两个标签指向两条记录

查询分量解码规则（`url.go:160-176`）：字面 `+` → 空格（等价 `%20`）；`%2B` → 字面 `+`（先按普通 `%XX` 解码）。登记侧对标签只做 TrimSpace（`artifacts.go:79-81`），因此标签 `"a b"`（含空格）与 `"a+b"`（含加号）可以各自登记为不同标签、指向不同摘要；查询时两种写法不可互换：

- `tag=a+b` 与 `tag=a%20b` 都解码为 `a b` → 命中含空格标签的记录；
- `tag=a%2Bb` 解码为 `a+b` → 命中含加号标签的另一条记录；两条记录互不为别名。

**[新增回归]** `TestPlusVersusPercent2BTagDecoding`（同一仓库播种 `a b`→摘要A、`a+b`→摘要B、`latest`→摘要C）。

解码先于修剪（§4.1）还带来两个可观察的边界：

- `tag=+latest+` 解码为 `" latest "`，修剪为 `latest` → **200** 命中；
- `tag=%2Blatest%2B` 解码为 `"+latest+"`，`+` 不是空白、不被修剪，该标签不存在 → **404**（合法查询无结果，而非 400）；
- `repository=++<repo>++` 解码出两侧空格并被修剪 → 命中该仓库的列表，顺序不变。

均在同一用例内断言。

---

## 5. 参数排列如何决定查询目标；查询不改变任何记录

### 5.1 三种分派与同存拒绝

`hasTag`/`hasDigest` 只取决于**是否出现**，值的合法性另算；分派是互斥三选一（`artifacts.go:200-210`）：

- 仅 `repository` → `QueryByRepository` → `ListRecords`，SQL 为 `ORDER BY id`，即首次登记顺序（`store.go:96-99`；服务层 `service.go:204-205`）。
- 加 `tag` → `QueryByTag` → `RecordByTag`，经 `tag_pointers` JOIN `artifacts` 取**当前**指针（`store.go:126-134`）；指针只在新登记时由 upsert 移动（`store.go:72-77`）。
- 加 `digest` → `QueryByDigest` → `RecordByDigest`，按 `(repository, digest)` 主键形态直查**不可修改的原记录**（`store.go:119-124`）。
- tag 与 digest 同存 → 400，与参数书写先后无关（`artifacts.go:194`）。

**[新增回归]** `TestParameterArrangementSelectsTargetAndQueriesAreReadOnly`：同一仓库先登记 A（`release`）、再登记 B（同标签），然后断言——列表按登记顺序返回 A、B；`tag=release` 返回 B 及其自己的 `pushed_at`（当前指向）；`digest=A` 返回 A 原始记录与原始 `pushed_at`（标签移走后原记录不变）；`tag=...&digest=...` 与颠倒顺序两种排列均为 400。**[已有测试]** `TestTagPointerMovesToNewDigestAndOldRecordSurvives`（`artifacts_test.go:206-237`）、`TestListArtifactsInRegistrationOrder`（`artifacts_test.go:239-256`）、`TestRegistrationSequenceIdentityAndTagPointer`（`regression_test.go:137-253`）覆盖同一身份模型的基线行为；单值形态的同存 400 见 `TestQueryValidation` 的 `tag and digest` 与 `TestClientErrorShapeAndMessages`（`regression_test.go:358-359`）。

### 5.2 查询是只读操作

`Service.Query` 只调用三个读方法（`service.go:191-206`），存储实现侧它们全是 `SELECT`（`store.go:96-134`），不含任何写路径；登记与标签移动只发生在 `Register`（`store.go:54-93`）。**[新增回归]** 同用例在一串读请求（命中、首值未命中、400 形态、`%2B`/`+` 标签、跨仓库未命中、纯列表）前后，对列表、标签视图、两条摘要视图的**完整响应体做快照比对**，并复核标签仍指向 B、A 的七字段与 `pushed_at` 不变——查询不改变记录、推送时间或标签指向 **[源码推导 + 新增回归]**。

---

## 6. 错误优先级与错误对象

处理函数先做参数校验、后调用服务（`artifacts.go:194-198` 在 `:212` 之前），所以：

1. **非法输入 → 400 `InvalidArtifactInputError`**，发生在任何存储访问之前；重复参数首值非法走同一路径（§2.3）。
2. **合法查询无结果 → 404 `ArtifactNotFoundError`**：存储层 `ErrRecordNotFound` 与"查到零条"都映射为服务层 `ErrNotFound`（`service.go:208-214`），处理函数映射为 404（`artifacts.go:214-215`）。**[已有测试]** `TestQueryNotFound`（`artifacts_test.go:288-307`）。
3. **存储故障 → 503 `storage_unavailable`**（`artifacts.go:216-217`；服务层 `service.go:211-212`）。**[已有测试]** `TestStorageUnavailableReturns503`（`regression_test.go:305-335`）。
4. **存储故障时非法输入仍优先 400**：关闭底层 SQLite 后，`repository=&...`、`tag=&...`、`tag=%20%20&...`、`digest=sha256:xyz&...`、`digest&...` 等重复首值非法形态仍返回 400；只有合法查询才降级为 503。**[新增回归]** `TestInvalidQueryFirstValueWinsOverStorageFailure`。**[已有测试]** `TestStorageUnavailableStillValidatesInput`（`regression_test.go:542-576`，查询段 `:565-568`）与装配入口上的 `TestAssemblyEntryErrorPriority`（`assembly_test.go:214-253`）覆盖单值形态。
5. **错误对象与固定消息沿用现有约定**：`writeError` 输出仅含字符串 `code`、`message` 的单个 `error` 对象（`artifacts.go:38-40`），查询消息是处理器内静态字面量 `"query parameters are not valid"`（`:196`）、404 为 `"no artifact matches this query"`（`:215`）、503 为 `"database is not available"`（`:217`）。**[新增回归]** 全部错误断言经既有的 `assertErrorResponse`（`regression_test.go:69-97`）核对状态码、错误对象形状、固定文案与无内部信息泄漏。

---

## 7. 两种路由构造入口结论一致；不受影响面

- `NewRouter`（SQLite 自带装配，`router.go:21-23`）与 `NewRouterWithStore`（调用方自带 `service.Store` + 独立健康检查，`router.go:32-54`）共用同一个 `queryArtifacts`，解析/校验/分派边界不在 SQLite 装配里 **[源码推导]**。**[新增回归]** `TestQueryInputRulesOnAssemblyEntry` 在调用方自带内存存储（`assemblyStore`，无 SQLite、无 `database/sql`）上复验：tag/digest/repository 首值生效与首值未命中 404、转义参数名碰撞、首值非法 400、合法首值不被后值污染、`Tag=` 被忽略而成列表、tag+digest 同存 400、未知重复参数忽略、`tag=a+b` 与 `tag=a%2Bb` 分别命中空格标签与加号标签；`/healthz` 保持 200，健康检查不参与查询链路。
- 本次未改动面：登记入口 `POST /v1/artifacts` 与 `GET /healthz`（`router.go:39-48`）、`main.go` 启动配置（`ADDR`/`DB_PATH`）、README 的公开契约描述、`docs/` 下四份既有分析文档、SQLite schema（`store.go:148-170`，仍为 `CREATE TABLE IF NOT EXISTS`）。**[已有测试]** `go test ./...` 全量基线（含 `router_test.go`、`storage_boundary_test.go`、`sqlite_visibility_test.go`）在新增用例后持续通过。

---

## 8. 结论—依据—验证对照表

| # | 结论 | 源码依据 | 验证 |
|---|---|---|---|
| 1 | 同名查询参数重复出现以**第一个**值为准；后值不校验、不分派 | `url.go:988` 追加；`url.go:891-897` 与 gin `context.go:455-460` 取 `[0]` | [源码推导] + [新增] `TestRepeatedQueryParameterFirstValueWins`、`TestRepeatedQueryParameterInvalidFirstValue` |
| 2 | 合法首值决定目标；后值既不能改目标，也不能补救首值的 404 | gin `context.go:457`；`artifacts.go:200-210` | [新增] 两个重复参数用例 |
| 3 | 首值非法 → 400，后随合法值不能补救；首值合法时后随非法值不能覆盖 | `artifacts.go:194-198` 只校验首值 | [已有] `TestQueryValidation`、`TestQueryEmptyAndBareParameters`；[新增] `TestRepeatedQueryParameterInvalidFirstValue` |
| 4 | GET 取首值与 POST 重复 JSON 键取尾值是两种传输格式的解析差异 | `url.Values` 多值映射 vs `map[string]json.RawMessage`（`artifacts.go:56-65`） | [源码推导]；[已有] 登记侧 `TestDuplicateKeysLastValueWins` |
| 5 | 参数名先百分号解码再匹配；解码后同名的拼写按重复参数处理 | `url.go:974-980,988` | [新增] `TestPercentDecodedParameterNamesAndCaseSensitivity` |
| 6 | 参数名区分大小写；大小写变体是未知参数（`Tag=` 退化为列表，`Repository=` 400） | map 精确字符串索引；`artifacts.go:189-192` | [新增] 同用例；值大小写 [已有] `TestQueryValidation/uppercase repository` |
| 7 | 未知参数（含重复、空值）继续忽略，但不能顶替缺失的已知参数 | `artifacts.go:189-192` 只取三个名字 | [新增] `TestPercentDecodedParameterNamesAndCaseSensitivity` 末段；登记侧同源忽略见 [已有] `TestRegisterArtifactNormalizesAndIgnoresUnknownFields` |
| 8 | 畸形 `%XX` 的键值对不入 map，等同参数未出现；错误被 `URL.Query` 丢弃 | `url.go:974-987,1144-1147` | [源码推导]（无保留用例） |
| 9 | repository/tag 解码后 TrimSpace；digest 不修剪、原样匹配锚定正则 | `artifacts.go:189-195,24` | [新增] `TestPlusVersusPercent2BTagDecoding`；[已有] `TestClientErrorShapeAndMessages` |
| 10 | 缺失与空值不同：仓库缺失/空为 400；tag 缺失是列表，tag 空/裸参数为 400；digest 空/裸/格式错为 400；tag+digest 同存恒 400 | gin `context.go:443-460`；`artifacts.go:190-195` | [已有] `TestQueryEmptyAndBareParameters`（13 子例）；[新增] 重复首值与存储故障段 |
| 11 | `+`→空格、`%2B`→加号；标签 `a b` 与 `a+b` 指向不同记录；`+latest+` 命中、`%2Blatest%2B` 为 404 | `url.go:160-176`；`artifacts.go:191` | [新增] `TestPlusVersusPercent2BTagDecoding` |
| 12 | 列表按首次登记顺序；标签查当前指针；摘要查不可修改原记录；tag+digest 两种排列均 400 | `artifacts.go:200-210`；`store.go:96-99,119-134,72-77` | [新增] `TestParameterArrangementSelectsTargetAndQueriesAreReadOnly`；[已有] 标签/顺序/序列基线用例 |
| 13 | 查询只读：不改变记录、`pushed_at` 与标签指向 | `service.go:191-206` 仅读方法；`store.go:96-134` 仅 SELECT | [源码推导] + [新增] 同用例快照比对 |
| 14 | 合法无结果 404；存储故障 503；故障时非法输入优先 400；错误对象仅 code/message 固定文案 | `artifacts.go:194-217,38-40`；`service.go:208-214` | [已有] 404/503/优先级基线；[新增] `TestInvalidQueryFirstValueWinsOverStorageFailure` |
| 15 | 两种路由构造入口行为一致；登记入口、/healthz、启动配置、README、既有文档不变 | `router.go:21-54` 共享处理函数 | [源码推导] + [新增] `TestQueryInputRulesOnAssemblyEntry`；[已有] 全量 `go test ./...` |

---

## 9. 复现方式

```bash
go test ./...
go test ./internal/api -run 'TestRepeatedQueryParameter|TestPercentDecodedParameterNames|TestPlusVersusPercent2B|TestParameterArrangement|TestInvalidQueryFirstValueWinsOverStorageFailure|TestQueryInputRulesOnAssemblyEntry' -v
go vet ./...
```

新增用例全部经由公开 HTTP 入口提交原始请求目标（`httptest.NewRequest` 携带未再加工的查询串），再经同一 GET 入口与错误响应核对结果；只读性以命中视图完整响应体快照前后比对确认。任何保持公开契约的内部重构都应持续通过这些用例。
