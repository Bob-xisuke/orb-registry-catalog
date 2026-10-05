# 登记请求重复字段与整数表示分析（附回归用例）

本文基于当前基线源码，说明 `POST /v1/artifacts` 的输入边界：客户端提交的原始 JSON 文本如何经解码、字段校验与登记判重，成为一条不可修改的登记记录（或被拒绝）。重点是两个此前未单独分析的解码行为——**同一对象内重复键**与**JSON 数字的整数表示**——并把每条结论与源码依据和可观察结果对应起来。

- 源码版本：工作区当前提交（`afde97a`）
- 验证方式标注：
  - **[已有测试]** 基线测试已覆盖，注明函数与文件
  - **[新增回归]** 本次补充的用例，位于 `internal/api/registration_input_test.go`
  - **[源码推导]** 由源码结构或标准库语义直接得出
- 本次只新增测试与本文档；生产代码、README、`docs/` 下既有分析文档均未改动。所有新增用例通过公开 HTTP 入口（`httptest` 驱动 POST/GET）提交原始请求文本，再经查询入口核对结果，不触碰存储层内部。

---

## 1. 解码入口：恰好一个 JSON 对象

入口：`registerArtifact`（`internal/api/artifacts.go:158-185`），第一步是 `decodeArtifactInput`（`artifacts.go:56-110`）：

1. 第一次 `Decode` 把请求体解进 `map[string]json.RawMessage`（`artifacts.go:58-62`）。**JSON 语法错误出现在任意位置**（截断、多余逗号、未加引号的记号等），这一次 Decode 直接失败，返回 400——此时根本不会进入任何字段检查，"后面还有合法的同名字段"无从谈起。
2. 第二次 `Decode` 要求读到 `io.EOF`（`artifacts.go:63-65`）：对象之后还有任何内容（垃圾字符、多余括号、第二个对象）同样是 400。
3. 只有两次 Decode 都通过，才按名字逐个取六个必填键校验（`artifacts.go:67-108`）。

解进 map 这一步带来两条标准库（`encoding/json`）语义，是本文后续所有结论的机制基础 **[源码推导]**，行为均由 **[新增回归]** 验证：

- **重复键后值覆盖前值**：解码器按文档顺序处理键值对，同名键后出现的值替换 map 中已有的值。因此 `requiredString`/`requiredBool`/`requiredInt`（`artifacts.go:120-156`）从 map 取到的只是**最后一次出现**的值，之前的出现已被丢弃。
- **键名先反转义再入 map**：JSON 字符串的转义规则同样适用于对象键名，`\uXXXX` 形式的键名按解码后的名称进入 map，字段匹配认的是解码后的名字，而非原始字节。

---

## 2. 重复键：同一对象内以最后一次出现为准

### 2.1 最后值合法 → 201，保存最后的值

前面出现的值即使是错误类型、`null` 或一个完全不同的诱饵身份（别的仓库、别的摘要、别的标签），也已在 map 解码阶段被覆盖，不参与任何校验与落库。规范化（`repository`/`tag` 的 `strings.TrimSpace`，`artifacts.go:71,79`）同样作用于最后的值。

**[新增回归]** `TestDuplicateKeysLastValueWins`：一个请求体内六个必填字段各出现两次——首次是诱饵仓库 `registry-demo/decoy`、诱饵摘要 B、诱饵标签 `decoy-tag` 或非法值（`"signature_verified":"yes"`、`"retention_days":null`、`"size_bytes":"1024"`），最后一次全部合法——返回 **201**，响应与查询均确认：记录身份是 `(registry-demo/dup, 摘要A)`，标签 `release` 指向摘要 A；诱饵仓库列表 **404**、诱饵标签 **404**、诱饵摘要 **404**，即第一次出现的值从未成为记录或标签指向。同用例还验证 `"repository":"  registry-demo/trim  "` 作为最后值时仍被 TrimSpace 为 `registry-demo/trim`。

### 2.2 最后值非法 → 400 InvalidArtifactInputError

镜像规则：前面出现过的合法值**不能挽救**一个非法的最后值。最后值为 `null`（`isJSONNull`，`artifacts.go:116-118`）、类型不符（`json.Unmarshal` 进目标类型失败，`artifacts.go:126,138,152`）或违反字段规则（`repository`/`tag` 修剪后为空 `:71-73,79-81`；`digest` 不匹配锚定正则 `^sha256:[0-9a-f]{64}$` `:24,87-89`；`retention_days` 超出 `1..3650` `:99-101`；`size_bytes` 为负 `:106-108`）时，返回 **400** 与 `InvalidArtifactInputError`，与只出现一次的非法值完全同路径。

**[新增回归]** `TestDuplicateKeysLastValueInvalid`：18 个子例覆盖六个字段的 null/空白/错误类型/违反规则组合，每个子例在 400 后追加列表查询确认 **404**——被拒请求不留记录。

### 2.3 任意位置的语法错误 → 同一个 400

语法错误在 §1 第 1 步即终止处理，与字段内容无关；对象之后的尾随内容在 §1 第 2 步被拒绝。**[新增回归]** `TestDuplicateKeysSyntaxErrorStill400`：重复键后截断、键间双逗号、未加引号的记号后随一个**合法**的 `"size_bytes":5`、合法对象后尾随垃圾字符、合法对象后多一个 `}`——全部 **400** `InvalidArtifactInputError`，且仓库列表保持 **404**。**[已有测试]** `TestRegisterArtifactRejectsInvalidInput` 的 `malformed json`、`trailing object` 子例覆盖同类边界（`artifacts_test.go:125-170`）。

### 2.4 未知字段：继续忽略，但不能代替必填字段

解码进 map 后只按名字取六个必填键（`artifacts.go:67-108`），多余键从不检查，重复出现的未知键同样被忽略。但"名字相近"不等于"名字相同"：`repo` 不能满足缺失的 `repository`。**[新增回归]** `TestUnicodeEscapedKeyNamesDecodeBeforeMatching` 末段；**[已有测试]** `TestRegisterArtifactNormalizesAndIgnoresUnknownFields`（`artifacts_test.go:111-123`）。

---

## 3. 键名中的 Unicode 转义按解码后的名称识别

`"\u0072epository"` 解码后就是 `"repository"`，与其余五个字段的转义拼写一样被识别为必填字段；同一解码名的转义拼写与普通拼写（如 `"\u0074ag"` 与 `"tag"`）构成 §2 意义上的重复键，文档顺序决定哪个值生效。

**[新增回归]** `TestUnicodeEscapedKeyNamesDecodeBeforeMatching`：

- 六个必填键全部以 `\u` 转义拼写提交 → **201**，记录各字段与查询结果正常；
- `"\u0074ag":"escaped","tag":"plain"` → 落库标签为 `plain`；`"tag":"plain","\u0074ag":"escaped"` → 落库标签为 `escaped`，且标签查询只在 `escaped` 下命中、`plain` 下 **404**——两个拼写是同一个键。

---

## 4. 整数表示：JSON 数字直接解码为有符号 64 位整数

`requiredInt`（`artifacts.go:146-156`）把字段的 `json.RawMessage` 直接 `json.Unmarshal` 进 `int64`，全程不经过浮点：

### 4.1 合法边界：精确往返

- `size_bytes = 9007199254740993`（2^53+1，**超出 float64 精确表示范围**）与 `9223372036854775807`（int64 上限）→ **201**；按摘要、按标签查询均 **200** 且数字精确一致。
- 精确性的链路依据：SQLite 列为 `INTEGER`（`store.go:159-160`），写入 `store.go:66-69`、读出 `store.go:107-108,138-139` 均为 int64；响应由 `artifactResponse` 直接放置 int64（`artifacts.go:42-52`），gin 按整数序列化。
- **核对过程不转浮点**：**[新增回归]** 用例以 `json.Decoder.UseNumber()` 把响应数字保留为原始字面量（`json.Number`）逐字比较，并对原始响应体做 `"size_bytes":9007199254740993` 子串断言（`decodeBodyNumber`/`soleRecordNumber`/`jsonNumber`，`registration_input_test.go`）。

**[新增回归]** `TestIntegerBoundaryExactInt64` 覆盖上述全部，并附带一个只能由精确整数比较产生的判重证据：`9007199254740992` 与 `9007199254740993` 作为 float64 **相等**（都舍入到 2^53），但作为 int64 不等——同一身份改交前者得到 **409** `ArtifactConflictError`，证明登记判重的逐字段比较（`store.go:86-91`）是精确整数比较，从未经过浮点。

### 4.2 拒绝的形式：同一个 400

以下形式都无法 `Unmarshal` 进 int64 或违反取值域，全部返回 **400** `InvalidArtifactInputError` 且不留记录：

| 形式 | 例 | 拒绝位置 |
|---|---|---|
| 超出 int64 上限 | `9223372036854775808` | `json.Unmarshal` 溢出失败（`artifacts.go:152`） |
| 负体积 | `-1` | `size_bytes < 0`（`artifacts.go:106-108`）**[已有测试]** `size negative` |
| 字符串数字 | `"1024"` | 类型不符（`artifacts.go:152`） |
| 数值等于整数的小数写法 | `1024.0`、`30.0` | `json.Unmarshal` 拒绝小数进 int64（注释见 `artifacts.go:144-145`） |
| 数值等于整数的指数写法 | `1e3`、`3e1` | 同上 |
| `retention_days` 越界 | `0`、`3651` | 区间检查（`artifacts.go:99-101`）**[已有测试]** `retention zero`/`retention too large` |

**[新增回归]** `TestIntegerFormsRejected`（7 个子例：两个字段各自的溢出、字符串、整数值小数、整数值指数）；**[已有测试]** `TestRegisterArtifactRejectsInvalidInput` 覆盖负体积、字符串 retention、小数 retention（`artifacts_test.go:146-152`）。

### 4.3 边界端点与合法零值

- `retention_days` 的 `1` 与 `3650` 是闭区间端点，保持合法（`artifacts.go:99-101`）：**[新增回归]** `TestIntegerBoundaryExactInt64` 分别以 1 和 3650 登记成功并精确读回。
- `signature_verified:false` 与 `size_bytes:0` 是合法零值，不与 `null` 混淆（`isJSONNull` 注释，`artifacts.go:112-118`）：**[已有测试]** `TestRegistrationNullAndMissingRequiredFields`（`regression_test.go:400-440`）。

---

## 5. 判重、冲突与错误优先级

1. **规范化后相同内容重提 → 201 与原 `pushed_at`**。判重在存储层逐字段比较 `tag/signature_verified/retention_days/size_bytes`（`store.go:86-91`），全等返回原记录、不写库、不移动标签指向；`pushed_at` 由服务端在调用存储前生成（`service.go:168`），不参与比较。**[已有测试]** `TestRegisterDuplicateReturnsOriginalRecord`；**[新增回归]** `TestIntegerBoundaryExactInt64` 对 2^53+1 体积的原样重提断言 201 与原 `pushed_at`。
2. **内容不同 → 409 `ArtifactConflictError`**（`store.go:92` → `service.go:173-175` → `artifacts.go:175-176`），不写入任何内容。**[已有测试]** `TestRegisterConflictOnDifferentContent`；**[新增回归]** 整数相邻值冲突（§4.1）。
3. **非法输入优先于冲突与存储故障**。处理器先 `decodeArtifactInput`（`artifacts.go:160-164`）再 `svc.Register`（`:166`），400 在任何身份查找与存储访问之前。**[新增回归]** `TestRejectedDuplicateBodyLeavesCommittedStateUntouched`：请求携带既有 `(repository, digest)` 身份与不同内容、但最后值 `size_bytes:null`——返回 **400 而非 409**，且既有记录七字段逐字不变、标签指向不变、列表不增、请求携带的新标签从未落库；`TestIntegerBoundaryInvalidWinsOverStorageFailure`：关闭底层 SQLite 后，溢出与整数值小数仍是 **400 而非 503**，合法边界值才降级为 **503**。**[已有测试]** `TestStorageUnavailableStillValidatesInput`、`TestAssemblyEntryErrorPriority` 覆盖同一优先级的其他非法形式。
4. **错误对象与固定消息沿用现有约定**：`writeError` 输出仅含字符串 `code`、`message` 的单个 `error` 对象（`artifacts.go:38-40`），message 为处理器内静态字面量（`:162,176,178`）。**[新增回归]** 全部用例经 `assertErrorResponse` 断言形状、固定文案与无内部信息泄漏。

---

## 6. 不受影响面

本次只新增测试与文档，以下行为保持原样：

- **GET /v1/artifacts** 查询链路与 **GET /healthz**（`router.go:39-45`）未改动；**[已有测试]** `TestQueryValidation`、`TestQueryNotFound`、`router_test.go` 及 `TestAssemblyHealthCheckIndependent` 持续通过。
- **两种路由构造入口**：`NewRouter`（`router.go:21-23`）与 `NewRouterWithStore`（`router.go:32-54`）共享同一组处理函数，解码边界不在 SQLite 装配里 **[源码推导]**。**[新增回归]** `TestDuplicateAndIntegerBoundaryOnAssemblyEntry` 在 `NewRouterWithStore` + 调用方自带内存存储上复验：重复键最后值生效、2^53+1 精确往返、`/healthz` 正常。
- **已有 SQLite 文件兼容性**：schema 为 `CREATE TABLE IF NOT EXISTS`（`store.go:148-170`），未改动；**[已有测试]** `TestRestartPreservesRecordsOrderAndPointers`、`TestSQLiteEntryStaysCompatibleWithExistingData` 持续通过。

---

## 7. 结论—依据—验证对照表

| # | 结论 | 源码依据 | 验证 |
|---|---|---|---|
| 1 | 请求体必须是恰好一个 JSON 对象；任意位置语法错误或尾随内容都是同一个 400 | `artifacts.go:58-65` | [已有] 非法输入表驱动用例；[新增] `TestDuplicateKeysSyntaxErrorStill400` |
| 2 | 同一对象内重复键以最后一次出现的值为准；前面非法值不影响合法最后值 | map 解码语义 + `artifacts.go:120-156` 只取 map 值 | [源码推导] + [新增] `TestDuplicateKeysLastValueWins` |
| 3 | 最后值为 null、类型不符或违反字段规则 → 400 `InvalidArtifactInputError`，不留记录 | `artifacts.go:67-108,116-118` | [新增] `TestDuplicateKeysLastValueInvalid`（18 子例） |
| 4 | 最终身份与标签指向来自最后值；诱饵首次出现不落库 | `store.go:61-77`（按解码后值查写） | [新增] `TestDuplicateKeysLastValueWins` 的 404 核对 |
| 5 | 键名 Unicode 转义按解码后名称识别；转义与普通拼写构成重复键 | JSON 字符串反转义适用于键名 | [源码推导] + [新增] `TestUnicodeEscapedKeyNamesDecodeBeforeMatching` |
| 6 | 未知字段忽略，但不能代替必填字段 | `artifacts.go:67-108` 只按名取六键 | [已有] 规范化用例；[新增] 同上末段 |
| 7 | JSON 数字直接解码为 int64；2^53+1 与 int64 上限精确登记、精确读回 | `artifacts.go:146-156`；`store.go:159-160,66-69,107-108`；`artifacts.go:42-52` | [新增] `TestIntegerBoundaryExactInt64`（json.Number 逐字 + 原始响应子串） |
| 8 | 溢出、负体积、字符串数字、整数值小数/指数写法 → 400 | `artifacts.go:152,106-108,99-101` | [已有] 负体积/retention 越界等；[新增] `TestIntegerFormsRejected` |
| 9 | retention 端点 1/3650 合法；false 签名与零体积合法 | `artifacts.go:99-101,112-118` | [已有] null/缺失用例；[新增] 边界用例 |
| 10 | 判重是精确整数比较：float64 下相等的相邻 int64 触发 409 | `store.go:86-91` | [新增] `TestIntegerBoundaryExactInt64` 冲突段 |
| 11 | 相同内容重提 201 原 `pushed_at`；内容不同 409 `ArtifactConflictError` | `store.go:86-92`；`service.go:160-180`；`artifacts.go:175-183` | [已有] 重复/冲突用例；[新增] 整数边界重提与冲突 |
| 12 | 非法输入优先于冲突（400>409）与存储故障（400>503）；被拒请求不改记录与标签指向 | `artifacts.go:160-166` 先校验后登记 | [已有] 存储不可用校验用例；[新增] `TestRejectedDuplicateBodyLeavesCommittedStateUntouched`、`TestIntegerBoundaryInvalidWinsOverStorageFailure` |
| 13 | 错误对象仅 code/message 两字符串，message 为固定字面量 | `artifacts.go:38-40,162,176,178` | [已有]+[新增] 全部错误断言经 `assertErrorResponse` |
| 14 | 两种路由入口共享解码边界；GET 入口、/healthz、SQLite 兼容性不变 | `router.go:21-54`；`store.go:148-170` | [源码推导] + [已有] 重开/兼容用例；[新增] `TestDuplicateAndIntegerBoundaryOnAssemblyEntry` |

---

## 8. 复现方式

```bash
go test ./...                          # 全部用例（基线 + 新增回归）
go test ./internal/api -run 'TestDuplicateKeys|TestUnicodeEscapedKeyNames|TestRejectedDuplicateBody|TestIntegerBoundary|TestIntegerForms|TestDuplicateAndIntegerBoundary' -v
go vet ./...
```

新增用例全部经由公开 HTTP 入口提交原始请求文本（`httptest`），再经 `GET /v1/artifacts` 核对结果；整数精确性核对使用 `json.Number` 保留原始字面量，不经过 float64。任何保持公开契约的内部重构都应持续通过这些用例。
