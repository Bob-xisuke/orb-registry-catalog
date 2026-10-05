# 查询输入重复参数与 URL 转义取值分析（附回归用例）

本文基于当前基线源码，说明 `GET /v1/artifacts` 的输入边界：客户端提交的原始查询串（raw query string）如何经过**URL 解析、取值、规范化、校验、查询分派**成为 HTTP 响应。重点是此前未单独分析的两类取值规则——**同名参数重复出现时取哪一个值**与**百分号/`+` 转义如何改写参数名和参数值**——并把每条结论与源码依据、已有覆盖和本次新增验证对应起来。与 `docs/artifact-identity-analysis.md`（身份与标签语义）、`docs/storage-boundary-analysis.md`（三层职责边界）、`docs/registration-input-decoding-analysis.md`（POST 重复键与整数）互补。

- 源码版本：工作区当前提交（`d1e9251`）；Go 工具链 `go1.26.8`，gin `v1.10.0`
- 验证方式标注：
  - **[已有测试]** 基线测试已覆盖，注明函数与文件
  - **[新增回归]** 本次补充的用例，位于 `internal/api/query_input_test.go`
  - **[源码推导]** 由源码结构或标准库语义直接得出，无法或不必通过行为用例证明
- 本次只新增测试与本文档；生产代码、README、登记与健康检查入口、启动配置（`main.go`）、SQLite schema、`docs/` 下既有分析文档均未改动。所有新增用例通过公开 HTTP 入口（`httptest` 驱动 GET/POST）核对结果，不触碰存储层内部。

---

## 1. 总览：原始查询串到响应的五个阶段

入口：`queryArtifacts`（`internal/api/artifacts.go:187-226`）。一个原始查询串（如 `?repository=R&tag=a+b&tag=c`）成为响应，固定经过五个阶段：

1. **URL 解析**：gin 第一次读取查询参数时调用 `c.Request.URL.Query()`（gin `context.go:469-477` 的 `initQueryCache`），其实现是 `v, _ := url.ParseQuery(u.RawQuery)`——**标准库 `net/url` 负责按 `&` 切分、按 `=` 切键值、对键和值分别做 query 组件反转义，并把值按文档顺序 append 进 `map[key][]string`**；注意 `URL.Query()` 显式丢弃了 `ParseQuery` 的错误（`net/url/url.go:1144-1146`）。**[源码推导]**
2. **取值（首值规则）**：处理器不读数组，只读一个标量。`c.Query("repository")` 与 `c.GetQuery("tag"/"digest")` 最终都走 `GetQueryArray` 取切片，再由 `GetQuery` 返回 **`values[0]`**（gin `context.go:455-460`、`:481-485`），即**同名参数重复出现时取第一个值**。
3. **规范化**：`repository = strings.TrimSpace(...)`、`tag = strings.TrimSpace(...)`，`digest` **不修剪**（`artifacts.go:189-192`）。
4. **校验**：一个布尔表达式决定 400（`artifacts.go:194-198`），见 §6。
5. **分派与响应**：构造 `service.Query` 并按 tag / digest / 列表三选一（`artifacts.go:200-210`），调用 `svc.Query`（`:212`），把业务结果映射为 200 / 404 / 503（`:213-224`）。

关键对照（**[源码推导]**）：POST 端的 JSON 对象内重复键是 **map 解码、后值覆盖前值**，因此取**最后一次**出现（见 `docs/registration-input-decoding-analysis.md` §2）；GET 端是 **`[]string` 切片、gin 取 `values[0]`**，因此取**第一次**出现。同一服务的两个入口规则相反，差异完全来自"map 末值 vs 切片首值"这一取值结构，而非业务代码刻意选择。

---

## 2. 解析机制：net/url 如何切分与反转义

`url.ParseQuery` → `parseQuery`（`net/url/url.go:932` 起）对每一段：

- 按 `&` 切分；含未转义分号的段记为错误并跳过；空段跳过（`url.go:959-967`）。
- 按第一个 `=` 切出键、值；**没有 `=` 的段被当作"值为空字符串"的键**（标准库注释："A setting without an equals sign is interpreted as a key set to an empty value"，`url.go:927-928`）。这解释了 `?tag` 等价于 `?tag=`：键存在、值为 `""`。
- 对键和值分别调用 `QueryUnescape`（`url.go:976-985`），再 `m[key] = append(m[key], value)`（`url.go:988`）。**切片顺序就是参数在原始串中的文档顺序**，因此 gin 的 `values[0]` 即"第一个出现的值"。

`QueryUnescape` → `unescape(s, encodeQueryComponent)`（`url.go:89`、`:106-175`）在 query 模式下：

- `%XX` 解码为字节 `0xXX`（键名、值都适用）；`%` 后不是两位十六进制则该次反转义返回 `EscapeError`。
- **`+` 解码为空格**（`unescapedPlusSign = ' '`，query 模式专有，`url.go:148-152`、`:166-168`）；要表示真正的加号须写 `%2B`，表示空格可写 `+` 或 `%20`。

以上均为 **[源码推导]**，行为由本文 §3–§5、§7 的 **[新增回归]** 验证。

---

## 3. 重复参数：同名出现多次，一律取首个值

`GetQuery` 只返回 `values[0]`（gin `context.go:455-460`），处理器随后只对这一个值规范化、校验、分派。因此对 `repository`、`tag`、`digest` 三个参数，**后续出现的值既不参与校验，也不参与分派**。

### 3.1 后续合法值不能补救非法首值

| 参数 | 首值（生效） | 后续值（被忽略） | 结果 |
|---|---|---|---|
| tag | 不存在的标签 | 合法标签 | **404**（按首值查空） |
| tag | 空白 / 仅参数名 | 合法标签 | **400**（首值 TrimSpace 后为空） |
| digest | 不匹配 `^sha256:[0-9a-f]{64}$` | 合法摘要 | **400**（首值不过正则） |
| repository | 空白 | 合法仓库 | **400**（首值 TrimSpace 后为空） |

依据：取值在 gin 层即固定为首值（`context.go:455-460`）；空白 tag / 非法 digest / 空 repository 的拒绝在 `artifacts.go:194-198`。**[新增回归]** `TestDuplicateQueryParametersFirstValueWins`（`query_input_test.go:71`）。

### 3.2 后续非法值不能覆盖合法首值

镜像规则：首值合法时，后续即使是空白值、仅参数名或畸形摘要，也被完全忽略——请求按首值正常命中 **200**。例：`tag=release&tag=%20%20`、`tag=release&tag`（第二个无值）、`digest=<A>&digest=sha256:xyz` 都按首值命中。**[新增回归]** 同用例（`query_input_test.go:71`）。

### 3.3 两个合法值时，排列决定查询目标

`digest=<A>&digest=<B>` 命中 A 的不可修改原记录；交换为 `digest=<B>&digest=<A>` 命中 B。tag 同理：`tag=<不存在>&tag=release` 按首值得 **404**，`tag=release&tag=<不存在>` 命中当前指向。**[新增回归]** `TestDuplicateQueryParametersFirstValueWins` 用同仓库 A、B 两条记录逐字核对返回的是哪一条（含各自的 `pushed_at`）。

### 3.4 与 POST 端"末值生效"的对照

POST 请求体是 JSON，解进 `map[string]json.RawMessage` 后同名键以后值覆盖前值，故取**最后一次**（`docs/registration-input-decoding-analysis.md` §2）。GET 取切片首值。两端都不是"遇到重复就报 400"，但生效位置相反——这是本次必须区分清楚的取值规则。**[源码推导]**

---

## 4. 参数名：百分号解码后相同即同名；未知参数忽略；区分大小写

参数名匹配发生在**百分号解码之后**：`parseQuery` 先对键 `QueryUnescape` 再作为 map 键（`url.go:976-977`、`:988`）。

1. **解码后同名按重复参数处理**：`%74ag` 解码为 `tag`（`%74` = `t`），因此 `?…&%74ag=other&tag=release` 与 `?…&tag=other&tag=release` 是同一个 `tag` 参数的两次出现，文档顺序决定 `values[0]`。两种拼写谁在前，首值就是谁。**[新增回归]** `TestPercentDecodedParameterNamesCollide`（`query_input_test.go:155`）。
2. **未知参数继续忽略**：处理器只按名字读取 `repository`/`tag`/`digest` 三个键（`artifacts.go:189-192`），其余键（无论明文 `unknown=x` 还是解码后才成形的 `%65xtra=y` → `extra=y`）从不检查，也不影响"是否提供了 tag/digest"的判定。**[新增回归]** 同用例的 unknown 段；**[已有测试]** 登记侧未知字段忽略见 `TestRegisterArtifactNormalizesAndIgnoresUnknownFields`（`artifacts_test.go:111`）。
3. **参数名区分大小写**：标准库不做大小写折叠，map 以解码后的精确字符串为键。`Tag`、`Repository` 是与 `tag`、`repository` 不同的、未知的键：
   - `?repository=R&Tag=release` 中 `Tag` 被忽略 → 退化为**仓库列表**（200），而不是标签查询；
   - `?Repository=R&tag=release` 中小写 `repository` 缺失 → **400**，大写 `Repository` 不能顶替。
   **[新增回归]** `TestPercentDecodedParameterNamesCollide`；**[已有测试]** 仓库值本身区分大小写见 `TestQueryValidation` 的 `uppercase repository` 子例（`artifacts_test.go:268-276`，大写值合法但无记录 → 404）。

---

## 5. 参数值：修剪边界与 `+` / `%2B` 的不同记录

### 5.1 仓库名与标签修剪首尾空白；摘要不修剪

- `repository`、`tag` 在取值后 `strings.TrimSpace`（`artifacts.go:189-191`）。因此首尾空白（包括由 `+`、`%20` 解码出的空格）在比较前被去掉：`tag=+a+b` 解码为 `" a b"`，修剪后等价于标签 `a b`；`tag=%20a%2Bb%20` 解码为 `" a+b "`，修剪后等价于标签 `a+b`。
- `digest` **不做修剪**，必须整体匹配锚定正则 `^sha256:[0-9a-f]{64}$`（`artifacts.go:24`、`:192,195`）；带空白或大写十六进制一律 400。**[已有测试]** `TestQueryValidation` 的 malformed digest 子例（`artifacts_test.go:267`）；**[新增回归]** 大写摘要在 `TestMissingVersusEmptyQueryParameters` 的 `digest uppercase hex` 子例（`query_input_test.go:291`）。

### 5.2 `+` 解码为空格，`%2B` 解码为加号：两种标签指向不同记录

query 组件中 `+` 与 `%20` 都解码为空格，`%2B` 才解码为真正的 `+`（`net/url/url.go:148-152`）。因此标签字面量 `"a b"` 与 `"a+b"` 是 `tag_pointers` 中两个不同的键：

| 查询串 | 解码后的标签 | 指向 |
|---|---|---|
| `tag=a+b` | `a b` | 登记为 `"a b"` 的记录 |
| `tag=a%20b` | `a b` | 同上 |
| `tag=+a+b` | `" a b"` → TrimSpace → `a b` | 同上 |
| `tag=a%2Bb` | `a+b` | 登记为 `"a+b"` 的另一条记录 |
| `tag=a%20%2Bb` | `a +b`（无此键） | **404** |

`url.QueryEscape("a+b")` 的输出正是 `a%2Bb`，即"含加号标签"的规范客户端写法。**[新增回归]** `TestPlusAndPercent2BTagDistinctRecords`（`query_input_test.go:210`）：同仓库登记摘要 A（标签 `a b`）与摘要 C（标签 `a+b`）两条记录，逐字核对两种编码各自命中的记录、标签回显与 `pushed_at`，并确认二者互不串扰、列表仍按首次登记顺序为 `[A, C]`。

> 对比：POST 端标签是 JSON 字符串，`+` 就是普通加号字符，不存在 query 式解码。同样的字符序列在两个入口的语义差异完全来自"JSON 字符串 vs query 组件反转义"。**[源码推导]**

---

## 6. 缺失参数与空值的差别

判定集中在一个表达式（`artifacts.go:194-198`）：

```go
if repository == "" || (hasTag && hasDigest) ||
    (hasTag && tag == "") || (hasDigest && !digestPattern.MatchString(digest)) {
    // 400 InvalidArtifactInputError
}
```

`hasTag`/`hasDigest` 来自 `GetQuery` 的第二个返回值（gin `context.go:455-460`）：**键存在即为 `true`，哪怕值是空字符串**；键完全缺失才是 `false`。由此区分两类情况：

**唯一的列表查询**：`repository` 修剪后非空，且 `tag`、`digest` **都完全缺失**（`hasTag == false && hasDigest == false`）→ `QueryByRepository`（`artifacts.go:200,208-209`）。

**以下全部为 400 `InvalidArtifactInputError`，不得退化为列表或单条件查询**：

| 情况 | 触发的条件 |
|---|---|
| `repository` 缺失 | `c.Query` 得 `""`（`artifacts.go:189`） |
| `repository` 修剪后为空（`repository=`、`repository`、`repository=%20%20`） | `repository == ""` |
| `tag` 修剪后为空（`tag=`、`tag`、`tag=%20%20`） | `hasTag && tag == ""` |
| `digest` 仅有参数名（`digest`） | `hasTag`/`hasDigest` 为真且空串不过正则 |
| `digest` 为空值或不符格式（`digest=`、大写、短、错前缀） | `hasDigest && !MatchString` |
| `tag` 与 `digest` 同时存在（含两者皆空、一空一合法） | `hasTag && hasDigest` |
| 完全无参数、只有未知参数 | `repository == ""` |

- **[已有测试]** `TestQueryValidation`（`artifacts_test.go:258-286`）覆盖缺 repository、tag+digest、空 tag、畸形 digest；`TestQueryEmptyAndBareParameters`（`regression_test.go:498-540`）系统覆盖 bare/empty/whitespace 及"不能退化为列表"。
- **[新增回归]** `TestMissingVersusEmptyQueryParameters`（`query_input_test.go:276`）在**已种入两条记录的非空仓库**上重跑 14 个非法形态——若错误地退化为列表会返回 200/两条而被抓到——并以"完全省略 tag、digest 得 200 两条"作为对照，另核合法命中 200 与合法未命中 404。

注意 `tag` 与 `digest` 同时存在时即使其中一个值为空，也是 400 而非按另一个单条件查询：`hasTag && hasDigest` 先于值内容成立。**[已有测试]** `TestClientErrorShapeAndMessages`（`regression_test.go:358`）、`TestQueryEmptyAndBareParameters` 的 both-empty 子例。

---

## 7. 同仓库两条摘要：参数排列决定查询目标

`TestDuplicateQueryParametersFirstValueWins` 与 `TestQueriesDoNotMutateState` 共用同一固定场景（`seedTwoDigestRepo`，`query_input_test.go:42`）：仓库 `registry-demo/qinput`，先登记摘要 A（标签 `release`，体积 1024），再登记摘要 B（同标签 `release`，体积 2048），标签 `release` 当前指向 B，A 仍以其 `(repository, digest)` 身份保留。

| 查询 | 参数排列如何决定目标 | 结果 |
|---|---|---|
| `?repository=R` | 无 tag/digest → 列表 | **200**，`artifacts` 为 `[A, B]`，`ORDER BY id` 首次登记顺序（`store.go:96-117`） |
| `?repository=R&tag=release` | 标签查**当前指向** | **200**，命中 **B**（`tag_pointers JOIN artifacts`，`store.go:127-134`） |
| `?repository=R&digest=<A>` | 摘要查**不可修改的原记录** | **200**，命中 **A**，七字段含 `pushed_at` 逐字等于首次响应（`store.go:120-124`） |
| `?repository=R&digest=<A>&digest=<B>` | 首值选 A | **200**，命中 **A** |
| `?repository=R&digest=<B>&digest=<A>` | 首值选 B | **200**，命中 **B** |
| `?repository=R&tag=missing&tag=release` | 首值选缺失标签 | **404**，后续合法值不补救 |

- 命中统一为 **200** 与 `{"artifacts":[...]}`，单记录查询也包装成长度 1 的数组（`artifacts.go:218-223`）。
- 列表顺序、标签当前指向、摘要原记录三条语义的基线依据见 `docs/artifact-identity-analysis.md` §3–§4；**[已有测试]** `TestListArtifactsInRegistrationOrder`（`artifacts_test.go:239`）、`TestTagPointerMovesToNewDigestAndOldRecordSurvives`（`:206`）；**[新增回归]** 上表的重复参数排列由 `query_input_test.go:71`、`:321` 逐条断言。

---

## 8. 查询不改变任何状态

`Service.Query` 只调用三个只读存储方法（`internal/service/service.go:186-218`）：`ListRecords`、`RecordByTag`、`RecordByDigest`；SQLite 适配器中三者都只有一条 autocommit SELECT（`internal/store/store.go:96-134`），全文件没有任何由查询路径触发的 INSERT/UPDATE/DELETE。**[源码推导]**

**[新增回归]** `TestQueriesDoNotMutateState`（`query_input_test.go:321`）在种入 A、B 后混合发起命中、合法未命中、重复参数、百分号转义名等 9 个查询，随后断言：列表仍恰为 `[A, B]` 且顺序不变；A、B 两记录七字段（含两个不同的 `pushed_at`）逐字等于首次登记响应；标签 `release` 仍指向 B 且其 `pushed_at` 不变。

---

## 9. 错误对象、错误优先级与一个标准库边界

错误统一由 `writeError` 输出（`artifacts.go:38-40`）：单个顶层 `error` 对象，仅含字符串 `code`、`message`，message 为处理器内静态字面量（查询非法为 `"query parameters are not valid"`，`:196`；未命中 `"no artifact matches this query"`，`:215`；存储故障 `"database is not available"`，`:217`）。

| 情况 | HTTP | code | 源码 |
|---|---|---|---|
| 参数非法（§6 全部形态、重复参数首值非法） | **400** | `InvalidArtifactInputError` | `artifacts.go:194-198` |
| 合法查询无结果（未知仓库/标签/摘要） | **404** | `ArtifactNotFoundError` | `artifacts.go:213-215`；`service.go:208-214` |
| 存储故障 | **503** | `storage_unavailable` | `artifacts.go:216-217` |

1. **非法输入优先于存储故障（400 > 503）**：校验在 `artifacts.go:194-198`，存储访问在 `:212`，400 发生在任何存储调用之前。关闭底层 SQLite 后，空 tag、畸形 digest（含"畸形首值 + 合法次值"）仍为 **400**；只有合法查询才降级为 **503**。**[新增回归]** `TestInvalidQueryWinsOverStorageFailure`（`query_input_test.go:390`）；**[已有测试]** `TestStorageUnavailableStillValidatesInput`（`regression_test.go:545-576`）、`TestAssemblyEntryErrorPriority`（`assembly_test.go:217-253`）。
2. **合法查询无结果为 404，不是 503**：未命中与后端故障由存储层用 `ErrRecordNotFound` 与其他错误区分（`service.go:208-212`）。**[已有测试]** `TestQueryNotFound`（`artifacts_test.go:288`）。
3. **错误对象形状与无泄漏**：**[新增回归]** 全部新增错误断言经既有 `assertErrorResponse`（`regression_test.go:72-97`）核对 code、固定 message 与两字段形状。

**一个必须如实记录的标准库边界——畸形百分号转义不是 400**：当某一段的键或值 `QueryUnescape` 失败（如 `%zz`），`parseQuery` 记录错误但 **`continue` 跳过该段、不加入 map**（`net/url/url.go:976-987`），而 `URL.Query()` 又丢弃该错误（`url.go:1144-1146`）。处理器因此根本看不到这个参数：

- `?repository=R&tag=%zz`：tag 段被丢弃 → 等同只有 repository → **200 列表**，而非 400；
- `?repository=R&digest=%zz`：同理 → **200 列表**；
- `?repository=%zz&tag=release`：repository 段被丢弃 → repository 缺失 → **400**；
- 被丢弃的段**不占切片位置**，故 `tag=release&tag=%zz` 与 `tag=%zz&tag=release` 中存活的 `release` 都是 `values[0]`，都命中。

这不是处理器的主动放行，而是"参数未通过解析即不存在"。使用 `url.QueryEscape`/`url.Values.Encode` 构造查询的合法客户端不会产生畸形转义；本文据此**记录基线行为而非将其包装成校验承诺**。**[源码推导]** + **[新增回归]** `TestMalformedPercentEscapeBaseline`（`query_input_test.go:360`）。

---

## 10. 两种路由构造入口结论一致

`NewRouter`（`router.go:21-23`，SQLite + 自身 Ping）与 `NewRouterWithStore`（`router.go:32-54`，调用方自带存储 + 独立健康检查）共享同一组处理函数：`router.GET("/v1/artifacts", queryArtifacts(svc))` 只注册一次（`router.go:48`）。查询的解析、取值、规范化、校验都在 gin 与处理器内，与使用哪种存储无关。**[源码推导]**

**[新增回归]** `TestQueryInputOnAssemblyEntry`（`query_input_test.go:428`）在 `NewRouterWithStore` + 调用方自带内存存储（`assemblyStore`，`assembly_test.go:31`）上复验：重复 tag 首值生效、`%74ag` 与 `tag` 同名碰撞、`a+b`（→空格）404 而 `a%2Bb`（→加号）命中、存储故障时空 tag 仍为 400 而合法查询为 503、A 记录只读不变、`/healthz` 独立且保持既有 200 响应。关键案例因此在两种入口下结论一致。**[已有测试]** 同入口的通用错误优先级见 `TestAssemblyEntryErrorPriority`（`assembly_test.go:217`）。

---

## 11. 结论—依据—验证对照表

| # | 结论 | 源码依据 | 验证 |
|---|---|---|---|
| 1 | 同名查询参数重复出现时取**首个**值（gin 取 `values[0]`）；后续值不校验、不分派 | gin `context.go:455-460,469-485`；`net/url/url.go:988` | [新增] `TestDuplicateQueryParametersFirstValueWins` |
| 2 | 后续合法值不能补救非法首值；后续非法值不能覆盖合法首值 | `artifacts.go:189-198`；gin `context.go:455-460` | [新增] 同上 |
| 3 | GET 首值生效与 POST JSON 末值生效相反，源于切片首值 vs map 末值 | gin 取首值；`encoding/json` map 覆盖 | [源码推导]；POST 见登记输入文档 §2 |
| 4 | 参数名百分号解码后相同即按同名参数处理（`%74ag`=`tag`） | `net/url/url.go:976-977,988` | [新增] `TestPercentDecodedParameterNamesCollide` |
| 5 | 未知参数（明文或解码后成形）继续忽略，但不能顶替缺失的必填参数 | `artifacts.go:189-192` 只读三键 | [已有] 登记侧忽略未知字段；[新增] 同上 unknown 段 |
| 6 | 参数名区分大小写：`Tag`/`Repository` 是不同未知键 | 标准库无大小写折叠；map 精确键 | [已有] 值大小写子例；[新增] 同上大小写段 |
| 7 | repository/tag 修剪首尾空白；digest 不修剪、须匹配锚定正则 | `artifacts.go:24,189-195` | [已有] `TestQueryValidation`；[新增] 大写/空白子例 |
| 8 | `+`/`%20` 解码为空格，`%2B` 解码为加号；两种标签指向不同记录 | `net/url/url.go:148-152,166-168`；`store.go:127-134` | [新增] `TestPlusAndPercent2BTagDistinctRecords` |
| 9 | 缺失参数与空值不同：仅完全省略 tag、digest 为列表；空值/bare/同给均 400，不退化为列表 | `artifacts.go:194-209`；`net/url/url.go:927-928` | [已有] `TestQueryEmptyAndBareParameters`；[新增] `TestMissingVersusEmptyQueryParameters` |
| 10 | 列表按首次登记顺序；标签查当前指向；摘要查不可修改原记录 | `store.go:96-134` | [已有] 顺序/标签用例；[新增] §7 排列用例 |
| 11 | 命中 200 + artifacts 数组（单记录也包长度 1 数组） | `artifacts.go:218-223` | [已有]+[新增] 各命中用例 |
| 12 | 查询只读：不改记录、推送时间、列表顺序或标签指向 | `service.go:186-218`；`store.go:96-134` 仅 SELECT | [源码推导] + [新增] `TestQueriesDoNotMutateState` |
| 13 | 非法查询 400、合法无结果 404、存储故障 503；400 优先于 503 | `artifacts.go:194-217` | [已有] 404/503/优先级；[新增] `TestInvalidQueryWinsOverStorageFailure` |
| 14 | 畸形百分号转义的段被 net/url 跳过且错误被 `URL.Query()` 丢弃，按"参数不存在"处理，非处理器主动放行 | `net/url/url.go:976-988,1144-1146` | [源码推导] + [新增] `TestMalformedPercentEscapeBaseline` |
| 15 | 两种路由构造入口共享查询边界，关键案例结论一致；/healthz、README、启动配置、schema 不变 | `router.go:21-54` | [源码推导] + [新增] `TestQueryInputOnAssemblyEntry`；[已有] assembly 用例 |

---

## 12. 复现方式

```bash
go test ./...                          # 全部用例（基线 + 新增回归）
go test ./internal/api -run 'TestDuplicateQueryParametersFirstValueWins|TestPercentDecodedParameterNamesCollide|TestPlusAndPercent2BTagDistinctRecords|TestMissingVersusEmptyQueryParameters|TestQueriesDoNotMutateState|TestMalformedPercentEscapeBaseline|TestInvalidQueryWinsOverStorageFailure|TestQueryInputOnAssemblyEntry' -v
go vet ./...
```

新增用例全部经由公开 HTTP 入口（`httptest` 驱动 GET，POST 仅用于种入记录），以手工拼好的已编码片段控制重复参数的文档顺序与转义写法（`queryPair`/`queryString`，`query_input_test.go:30-37`），再经 GET 入口核对命中记录、错误对象、固定 message 与状态不变性，不依赖存储层内部类型。任何保持公开契约的内部重构都应持续通过这些用例。
