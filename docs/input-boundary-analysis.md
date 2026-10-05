# 登记输入边界分析：重复键、整数表示与不可修改记录（附回归用例）

本文只聚焦 `POST /v1/artifacts` 的**输入边界**：客户端提交的**原始 JSON 文本**经过"解码 → 字段校验 → 登记判重"三个阶段，怎样成为一条**不可修改的记录**。重点说明两件基线未显式固定的事：

1. 同一对象内**重复字段名**与键名中的 **Unicode 转义**如何被识别；
2. JSON 数字如何**直接**成为有符号 64 位整数，边界值如何精确保存。

并把每条源码依据与通过公开 HTTP 入口观察到的结果一一对应。与既有两份文档互补，不重复其主题：身份与标签语义见 `docs/artifact-identity-analysis.md`，三层存储职责见 `docs/storage-boundary-analysis.md`。

- 源码版本：工作区当前提交（`afde97a`）
- 验证方式标注：
  - **[源码推导]** 由源码结构或 Go 标准库 `encoding/json` 的既定行为直接得出，无法或不必单靠黑盒用例区分实现机理
  - **[已有测试]** 基线测试已覆盖，注明函数与文件
  - **[新增回归]** 本次补充的用例，位于 `internal/api/input_boundary_test.go`
- 本次只新增测试与本文档；**未改动**任何生产代码、`README.md`、既有分析文档、路由构造入口、SQLite schema 与启动配置。所有用例只用 `httptest` 经公开 HTTP 入口（POST/GET `/v1/artifacts`、GET `/healthz`）提交原始请求文本，再经查询核对结果，不直接触碰存储内部。

---

## 1. 总览：原始 JSON 到不可修改记录的三阶段

`registerArtifact`（`internal/api/artifacts.go:158-185`）对一次登记请求严格按顺序做三件事，前一阶段失败就不会进入后一阶段：

| 阶段 | 位置 | 输入 → 输出 | 失败结果 |
|---|---|---|---|
| ① 解码 | `decodeArtifactInput`（`artifacts.go:56-65`） | 原始字节 → `map[string]json.RawMessage`，且恰为一个 JSON 值 | 400 |
| ② 字段校验 | `requiredString/Bool/Int` 与规范化（`artifacts.go:67-109`、`:116-156`） | 每个必填键的**原始片段** → 类型正确、取值合法的 `artifactInput` | 400 |
| ③ 登记判重 | `service.Service.Register`（`service.go:160-180`）→ `store.Register`（`store.go:54-93`） | 已校验字段 + 服务生成 `pushed_at` → 新建 / 重复 / 冲突 | 201 / 409 / 503 |

关键边界：

- 阶段①只把对象解成"键 → **原始 JSON 片段**"的映射，**不做任何类型转换**；类型校验被推迟到阶段②对单个片段再次 `json.Unmarshal` 时（`artifacts.go:126,138,152`）。这正是"重复键时前一个非法值不会导致失败"的机理来源（§2）。
- 阶段②全部通过之前，`registerArtifact` 不会调用 `svc.Register`（`artifacts.go:160-173`）。因此 400 永远先于 409 与 503（§5）。
- 阶段③一旦首次提交成功，记录以 `(repository, digest)` 为唯一身份落库且**不可修改**；重复提交只返回原记录。该语义的完整分析见身份文档，本文只在与"最后一次出现的值"相关处引用（§4）。

---

## 2. 阶段①解码：重复键、Unicode 转义、语法错误

### 2.1 解码目标是 RawMessage 映射：类型校验被推迟

```go
var raw map[string]json.RawMessage
decoder := json.NewDecoder(body)
if err := decoder.Decode(&raw); err != nil || raw == nil { … 400 }
if err := decoder.Decode(&struct{}{}); err != io.EOF { … 400 }
```
（`artifacts.go:58-65`）

- 必须是**恰好一个** JSON 顶层值：首个 `Decode` 失败或得到 `null`（`raw == nil`）为 400；其后再 `Decode` 必须得到 `io.EOF`，即不允许两个对象、尾随标量等。**[已有测试]** `TestRegisterArtifactRejectsInvalidInput`（`artifacts_test.go:125-170`，含数组、标量、`null`、尾随对象、截断 JSON）。
- map 的值类型是 `json.RawMessage`——**原样保留的 JSON 字节**。所以阶段①解对象时，`7` 与 `"ok"` 都只是字节，不发生"数字能否变成字符串"这类判断；判断只在阶段②按目标类型 `json.Unmarshal` 时发生。**[源码推导]**：`RawMessage` 实现 `MarshalJSON/UnmarshalJSON` 为原样透传，标准库在解对象阶段不触碰其内部词法类型。

### 2.2 同一对象内重复键：只保留最后一次出现的片段

Go 的 `encoding/json` 解对象到 map 时，对每个键做普通的 map 赋值；同名键再次出现即**覆盖**前一个值，且不报错（RFC 8259 §4 允许这种行为，Go 选择"最后一个胜出"）。因此阶段①结束后，每个解码后的键名在 `raw` 中**只剩最后一次出现的 RawMessage**，前面的片段既不保存也不校验。**[源码推导]**：处理器只 `Decode` 到 map 一次并随后只读 map（`artifacts.go:60,67-105`），代码中没有任何"收集同名键全部出现"的路径；最后胜出由标准库的 map 覆盖语义决定。

由此得到可观察规则，全部经 **[新增回归]** 固定：

- **前一个值类型不符、最后一个值合法 → 201，保存最后值**。六个必填字段逐一验证：如 `{"repository":7,"repository":"x"}`、`{"signature_verified":"true","signature_verified":true}` 均 201，且取回的记录每一字段都是最后值。用例：`TestDuplicateKeysFirstBadLastGood`（`input_boundary_test.go:115`）。
- **最后一个值为 null、类型不符或违反字段规则 → 400 `InvalidArtifactInputError`**，无论同名键前面是否出现过合法值。用例：`TestDuplicateKeysLastBadRejected`（`:149`，覆盖六个字段的末值 null、末值错类型，以及 repository/tag 修剪后为空、digest 不匹配、retention `0`/`3651`、size `-1` 等末值违规；每个 400 后追加列表查询确认无记录）。
- **最终身份与标签指向来自最后值**（规范化仍沿用 TrimSpace 等既有规则）：
  - repository 重复：记录只落在最后（修剪后的）仓库名下，早先仓库名查询为 404；
  - digest 重复：身份是最后摘要，早先摘要查不到；
  - tag 重复：最后标签既固化为新记录的 `tag` 字段，又只移动**最后标签**的指针；前一个标签仍指旧记录；
  - 用早先的"冲突型"仓库名/标签、最后给规范化后与存量一致的值，结果是 **201 且返回原 `pushed_at`**（识别为重复，而非冲突或新建）。
  - 用例：`TestDuplicateKeysIdentityAndTagFromLastValue`（`:205`，四个子例）。
- **登记判重比较的也是最后值**：已存在身份重提时，内容字段（tag/size 等）的最后一次出现决定"相同"还是"冲突"——首个匹配、末值不同 → 409；首个不同、末值匹配 → 201 原记录。用例：`TestDuplicateKeysDedupUsesLastValue`（`:339`）。

### 2.3 键名中的 Unicode 转义：按**解码后**的名称识别

JSON 字符串键允许 `\uXXXX` 形式的转义。解码器在把键用作 map 键之前，先把转义解析成对应的 Unicode 字符：例如 `\u0074ag` 先变为键 `tag`，再进入 map。因此字段识别发生在**解码后的名称**上，而不是字节字面量上。**[源码推导]**：阶段①目标是普通 `map[string]…`，键名解析（含 `\uXXXX`、代理对）由标准库在构造 map 键之前完成；处理器随后以普通字符串 `"tag"` 等取值（`artifacts.go:67-105`），对键的字面写法无感知。

**[新增回归]** `TestUnicodeEscapedKeyNames`（`:403`）固定：

- 六个必填键全部用 `\u0072epository`、`\u0064igest`、`\u0074ag`、`\u0073ignature_verified`、`\u0072etention_days`、`\u0073ize_bytes` 这类转义书写，仍 201 并正确组装记录；
- 同一个键一半写字面 `tag`、一半写转义 `\u0074ag`，二者解码后是**同一个键**，仍按最后一次出现取值（两种先后顺序各测一次，失败的首值标签查不到）；
- 转义出的**不同**名称仍是未知字段：`\u0074ag2` 解码为 `tag2`，被忽略但**不能代替**必填的 `tag` → 400。

### 2.4 任意位置的 JSON 语法错误：整体 400，不接受部分对象

对象解码是全有或全无的：词法/语法错误使整个 `Decode` 失败，返回的是零值 map，不会保留出错点之前已解析的键。因此**不能**因为后面（或前面）有同名合法字段就接受请求。**[源码推导]**：`decoder.Decode(&raw)` 一旦返回错误，处理器立即返回 `errInvalidInput`（`artifacts.go:60-62`），不读取任何部分结果。

**[新增回归]** `TestSyntaxErrorAnywhereRejects`（`:484`）：合法同名标签后紧跟非法值（`"tag":"a","tag":,`）、非法值后再给合法同名标签（`"tag":,"tag":"a"`）、截断对象（缺右花括号）、合法重复键后的尾随逗号、数字前导零（`01`）——均为同一个 400，且仓库列表维持 404。基线另有 `{"repository":` 等用例（**[已有测试]** `TestClientErrorShapeAndMessages`，`regression_test.go:345`）。

---

## 3. 阶段②字段校验：六个必填字段与整数表示

阶段②从 map 中按名字取六个必填键（`artifacts.go:67-109`）。未知键从不读取——继续被忽略，但缺失必填键照样 400（**[已有测试]** `TestRegisterArtifactNormalizesAndIgnoresUnknownFields`，`artifacts_test.go:111`；**[新增回归]** §2.3 的 `tag2` 用例）。

### 3.1 缺键与 null 等价；合法的 false / 0 不受影响

`isJSONNull` 把字面 `null` 与缺键同等拒绝（`artifacts.go:116-118`，各 `required*` 的 `!ok || isJSONNull(field)`，`:122,134,148`），因为 `null` 解进任何 Go 值都不报错、只会留下零值，不拦就会把 `null` 的 `size_bytes` 静默当成 0。注意区分：**显式 `false` 与 `0` 是合法值，不是 null**。

- **[已有测试]** `TestRegistrationNullAndMissingRequiredFields`（`regression_test.go:400-440`）：六字段 null/缺失均 400；`false` 签名与零体积登记成功。
- **[新增回归]** `TestFalseSignatureAndZeroSizeStayLegal`（`input_boundary_test.go:605`）再次确认 `signature_verified=false`、`size_bytes=0` 均 201 并原样取回。

规范化与取值域规则保持基线不变：repository/tag 经 `strings.TrimSpace` 后非空（`artifacts.go:71-81`）；digest 不修剪、须整体匹配锚定正则 `^sha256:[0-9a-f]{64}$`（`:24,87`）；retention 与 size 的范围见下。

### 3.2 JSON 数字直接解成 int64：不经过浮点数

两个整数字段共用 `requiredInt`：

```go
var value int64
if err := json.Unmarshal(field, &value); err != nil { return 0, errInvalidInput }
```
（`artifacts.go:146-156`）

目标 Go 类型是 **`int64`**。标准库把 JSON **整数字面量直接按十进制累积成整数**填入 `int64`，路径上不存在 `float64`；只有当数字是小数/指数词法，或整数超出 `int64` 可表示范围时才报错。**[源码推导]**：函数注释明确"rejects fractional, exponential and string forms because encoding/json refuses to unmarshal them into an integer"（`artifacts.go:144-145`）；是否经过浮点由解码目标类型 `int64` 决定，全处理器没有把整数字段解进 `float64`/`any` 的路径。

因此边界行为如下，均由 **[新增回归]** `TestIntegerExactBoundaries`（`:511`）与 `TestIntegerRejectedForms`（`:546`）固定：

| `size_bytes` 原始文本 | 含义 | 结果 | 观察方式 |
|---|---|---|---|
| `9007199254740993` | 2^53+1，float64 已无法精确表示的首个整数 | **201**，GET **200** 且数字逐位一致 | 比较 HTTP 报文中的**原始数字文本** |
| `9223372036854775807` | 2^63-1，有符号 64 位上限（含） | **201**，GET **200** 且逐位一致 | 同上 |
| `9223372036854775808` | 上限 +1，超出 int64 | **400** | 解码报错 |
| `-9223372036854775809` | 低于 int64 下限 | **400** | 解码报错 |
| `-1` | 负体积（整数本身合法，业务范围非法） | **400** | `artifacts.go:106` 的 `< 0` 检查 |
| `"4096"` | 字符串数字 | **400** | 词法类型不符 |
| `1.0` | 数值等于整数的**小数**写法 | **400** | 非整数词法，拒绝隐式截断 |
| `1e3` / `1E3` | 数值等于整数的**指数**写法 | **400** | 非整数词法，拒绝隐式换算 |

> **核对本身不先转浮点**：测试辅助 `assertRawInteger` 用正则 `"字段":(-?[0-9]+)` 直接从 HTTP 响应体抽取**数字文本**逐字比较（`input_boundary_test.go:96-109`），刻意不使用把 JSON 解进 `map[string]any` 的 `decodeBody`——后者会把数字变成 `float64`，反而无法分辨 2^53 以上是否丢精度。若实现退化为经 `float64` 存取，`9007199254740993` 会变成 `9007199254740992`，该断言立即失败。

`retention_days` 与 size 走同一个 `requiredInt`，随后做不同的范围检查：

- 合法边界 **`1` 与 `3650` 保持 201**（闭区间，`artifacts.go:99`）；**`0` 与 `3651` 返回同一个 400**。
- 小数 `1.0`、指数 `1e3`、字符串 `"30"` 同样 400，绝不截断或换算成整数。
- 用例：`TestIntegerRejectedForms`（`input_boundary_test.go:546`，retention 的合法/非法边界与三类非法写法）。
- 基线已覆盖部分取值域：**[已有测试]** `TestRegisterArtifactRejectsInvalidInput`（`artifacts_test.go:146-151`，retention `0`、`3651`、`1.5`、字符串与 size 负值/小数）。

---

## 4. 阶段③登记判重：最后值如何进入不可修改记录

阶段②产出的 `artifactInput` 被组装为 `service.RegisterInput`（`artifacts.go:166-173`）；`pushed_at` 由服务层在调用存储前生成（`service.go:168`，UTC RFC3339），客户端无法提供。进入存储后（`store.go:54-93`）：

1. 按规范化后的 `(repository, digest)` 查现有记录——这两个**最后值**决定身份；
2. 不存在 → 同一事务内插入记录并 upsert 标签指针（`store.go:66-81`），标签指针取最后 tag；
3. 存在且 `tag/signature_verified/retention_days/size_bytes` 全等 → 返回**原记录** `StatusDuplicate`（`store.go:86-91`），即 **201 + 原 `pushed_at`**，不写库、不移指针；
4. 存在但任一不等 → `StatusConflict`（`store.go:92`），回滚，不写任何内容。

与输入边界直接相关、本次补齐的两点：

- **规范化后相同内容重提 → 201 与原 `pushed_at`**：即便请求用重复键先给了会冲突的仓库名/标签，只要**最后值**修剪后与存量身份和内容一致，即判为重复（`TestDuplicateKeysIdentityAndTagFromLastValue` 的"normalized last values are a duplicate"子例，`:205`）。
- **内容不同 → 409 `ArtifactConflictError`**：比较用的是最后值（`TestDuplicateKeysDedupUsesLastValue`，`:339`）。完整的标签移动、旧记录仍可按摘要查询、跨仓库隔离等语义，见身份文档及其用例（**[已有测试]** `TestRegistrationSequenceIdentityAndTagPointer`，`regression_test.go:137`）。

拒绝的请求（400 或 409）不新增记录，也不改变既有记录与标签指向——除各新用例内的查询核对外，基线另有 **[已有测试]** `TestRejectedRegistrationLeavesCommittedStateUntouched`（`regression_test.go:447`）。

---

## 5. 判定优先级：非法输入先于冲突与存储故障

阶段②先于阶段③，故对外的状态码优先级固定为 **400（输入非法）＞ 409（内容冲突）＞ 503（存储不可用）**：

- 源码上，`decodeArtifactInput` 返回错误时处理器直接写 400 并 `return`（`artifacts.go:160-164`），`svc.Register` 根本不会被调用。
- **非法输入压过 409**：对一个**已存在且内容不同**的身份（本应 409），只要末值非法（末值 `null`、size 超 int64、`1e3`、末值标签为 null、retention 给字符串等），一律 400；事后查询证明记录、`pushed_at` 与标签指针逐字未变、请求里的新标签未落库。**[新增回归]** `TestInvalidInputBeatsConflictAndStorage`（`input_boundary_test.go:632`）。
- **非法输入压过 503**：关闭底层 SQLite 后，同一非法请求仍是 400，而合法请求才降级为 503。**[新增回归]** 同用例的"invalid beats 503 with store closed"子例；基线另有 **[已有测试]** `TestStorageUnavailableStillValidatesInput`（`regression_test.go:545`）、`TestAssemblyEntryErrorPriority`（`assembly_test.go:217`，以调用计数证明非法输入对存储 `calls == 0`）。

---

## 6. 错误对象与固定消息沿用既有约定

本次所有新增用例复用基线的 `assertErrorResponse`（`regression_test.go:72-97`）：错误体是单个顶层 `error` 对象，恰含 `code`、`message` 两个字符串字段；`message` 为处理器里的静态字面量，且不允许出现 `sql/sqlite/insert/select/begin/.go// /goroutine/stack` 等内部细节。输入边界涉及的三组响应不变：

| 情况 | HTTP | code | 固定 message |
|---|---|---|---|
| 解码/字段校验失败（重复键末值非法、语法错、整数越界/错词法等） | 400 | `InvalidArtifactInputError` | `request body is not a valid artifact registration`（`artifacts.go:162`） |
| 已存在身份、内容不同 | 409 | `ArtifactConflictError` | `an artifact with this repository and digest already exists with different content`（`artifacts.go:176`） |
| 存储不可用 | 503 | `storage_unavailable` | `database is not available`（`artifacts.go:178`） |

错误形状的既有分析见身份文档 §6；本文不改变任何错误码、文案或状态码。

---

## 7. 结论—依据—验证对照表

| # | 结论 | 源码依据 | 验证 |
|---|---|---|---|
| 1 | 登记按"解码 → 字段校验 → 登记判重"顺序，前阶段失败不进入后阶段 | `artifacts.go:56-65,67-109,160-173` | [源码推导]；[已有] `TestAssemblyEntryErrorPriority`；[新增] `TestInvalidInputBeatsConflictAndStorage` |
| 2 | 对象解进 `map[string]json.RawMessage`，解对象时不做类型判断 | `artifacts.go:59-60`；`RawMessage` 原样透传 | [源码推导]；行为由结论 3 反证 |
| 3 | 重复键只留最后片段：前坏后好 → 201 存最后值；末值 null/错类型/违规 → 400 | `artifacts.go:60,116-156`；标准库 map 覆盖语义 | [新增] `TestDuplicateKeysFirstBadLastGood`、`TestDuplicateKeysLastBadRejected` |
| 4 | 最终身份（仓库、摘要）与标签指向来自最后值，规范化规则不变 | `artifacts.go:71-81`；`store.go:61-77` | [新增] `TestDuplicateKeysIdentityAndTagFromLastValue` |
| 5 | 登记判重比较最后值：末值不同→409；末值匹配→201 原记录 | `store.go:86-92` | [新增] `TestDuplicateKeysDedupUsesLastValue` |
| 6 | 键名按解码后名称识别：`\uXXXX` 转义键被识别；字面与转义同名视为一键；转义未知键不能替代必填键 | `artifacts.go:60,67-105`；标准库先解析键名再入 map | [新增] `TestUnicodeEscapedKeyNames` |
| 7 | 任意位置语法错误整体 400，不接受部分对象，不因后续同名字段合法而接受 | `artifacts.go:60-62` | [新增] `TestSyntaxErrorAnywhereRejects`；[已有] `TestClientErrorShapeAndMessages` |
| 8 | 未知字段继续忽略但不替代必填字段 | `artifacts.go:67-109`（不读取多余键） | [已有] 规范化用例；[新增] 转义 `tag2` 子例 |
| 9 | 缺键与显式 null 等价拒绝；合法 `false`/`0` 保持可登记 | `artifacts.go:116-118,122,134,148` | [已有] `TestRegistrationNullAndMissingRequiredFields`；[新增] `TestFalseSignatureAndZeroSizeStayLegal` |
| 10 | 整数直接解进 int64、不经浮点：2^53+1 与 2^63-1 精确保存并逐位取回 | `artifacts.go:146-156` | [源码推导] + [新增] `TestIntegerExactBoundaries`（按原始数字文本核对） |
| 11 | 超 int64 上/下限、负体积、字符串数字、小数 `1.0`、指数 `1e3/1E3` → 400 | `artifacts.go:151-153,106` | [新增] `TestIntegerRejectedForms` |
| 12 | retention `1`/`3650` 合法，`0`/`3651` 同一 400 | `artifacts.go:99` | [新增] `TestIntegerRejectedForms`；[已有] `TestRegisterArtifactRejectsInvalidInput` |
| 13 | 非法输入优先于 409 与 503，拒绝后记录与标签指向不变 | `artifacts.go:160-164`；`service.go:170-175` | [新增] `TestInvalidInputBeatsConflictAndStorage`；[已有] `TestRejectedRegistrationLeavesCommittedStateUntouched`、`TestStorageUnavailableStillValidatesInput` |
| 14 | 规范化后相同重提 201 返原 `pushed_at`；内容不同 409 | `store.go:86-92`；`service.go:168` | [新增] 重复键重复子例；[已有] `TestRegisterDuplicateReturnsOriginalRecord`、序列用例 |
| 15 | 错误对象形状、固定 message 与无泄漏约定不变 | `artifacts.go:38-40,162,176,178` | [源码推导] + 各新用例经 `assertErrorResponse` 核对 |

---

## 8. 未改动面与复现方式

以下保持原样，本次未新增也未修改其行为：`GET /v1/artifacts` 的三种查询、`GET /healthz`、两种路由构造入口（`NewRouter`、`NewRouterWithStore`，`router.go:21,32`）、已有 SQLite 文件兼容性（`CREATE TABLE IF NOT EXISTS`，`store.go:35`；**[已有测试]** `TestRestartPreservesRecordsOrderAndPointers`、`TestSQLiteEntryStaysCompatibleWithExistingData`）。

```bash
go test ./...          # 全部用例（基线 + 新增回归）
go test ./internal/api -run 'TestDuplicateKeys|TestUnicodeEscapedKeyNames|TestSyntaxErrorAnywhereRejects|TestIntegerExactBoundaries|TestIntegerRejectedForms|TestFalseSignatureAndZeroSizeStayLegal|TestInvalidInputBeatsConflictAndStorage' -v
go vet ./...
```

新增用例全部经公开 HTTP 入口提交**原始请求文本**（重复键、`\uXXXX` 转义键、大整数与非法数字写法均逐字发送），再通过 `GET /v1/artifacts` 的列表/标签/摘要查询核对可观察结果；大整数以响应中的原始数字文本比对，核对过程不经过浮点数。任何保持公开契约的内部重构都应持续通过。
