# 登记请求交付边界分析（附回归用例）

本文针对 `POST /v1/artifacts` 的一次登记，补齐从**请求取消**到**响应交付**的边界说明。既有文档已覆盖输入解码规则（`docs/registration-input-decoding-analysis.md`）、身份与标签语义（`docs/artifact-identity-analysis.md`）、事务可见性（`docs/sqlite-transaction-visibility-analysis.md`）与写锁竞争（`docs/sqlite-write-lock-contention-analysis.md`）；本文沿同一条 `POST → 响应` 路径，按**请求体解析、业务调用、事务提交、响应写出**四个阶段说明实际行为，并把三件容易被混为一谈的事分开：**请求上下文被取消**、**请求体读取失败**、**客户端未收到成功响应**。

- 源码版本：工作区当前提交（`bc92423`）
- 适用范围：
  - **事务提交、提交后可见性、列表顺序、标签指向**等结论仅针对**内置 SQLite 适配器**（`internal/store`），经真实 SQLite 文件验证；自定义存储的相同表现来自**其自身实现**（`service.Store` 接口注释只是约定，`internal/service/service.go:124-142`），本文不为自定义存储作证。
  - **请求体解析、400/201 的 HTTP 映射**位于框架无关的 handler/input/service 层，与具体存储无关；两种路由构造入口（`NewRouter`、`NewRouterWithStore`）共用同一组处理函数，三个场景在两个入口上各验证一遍。
- 验证方式标注：
  - **[已有验证]** 基线测试已覆盖，注明函数与文件
  - **[新增验证]** 本次补充的用例，位于 `internal/api/registration_delivery_test.go`
  - **[源码推导]** 由源码结构直接得出（如"处理路径上不读取请求上下文"），不通过行为用例证明
- 本次只新增本文档与一个测试文件；README、生产代码、SQLite schema、既有数据库、启动配置（`main.go`）与两种路由构造入口均未改动；`GET /healthz`、输入规范化、顶层 `error` 对象与固定错误消息沿用既有行为。

---

## 1. 一次登记的处理阶段与关键源码位置

### 1.1 写路径：POST /v1/artifacts 的四个阶段

| 阶段 | 位置 | 内容 | 该阶段失败时的行为 |
|---|---|---|---|
| ① 请求体解析 | `internal/api/artifacts.go:40` → `internal/input/input.go:78-137` | `input.ParseRegistration(c.Request.Body)`：首对象解码（`input.go:82-86`）、末尾检查要求 `io.EOF`（`input.go:87-89`）、逐字段校验（`input.go:91-136`） | 任何偏差（含**读取失败**）→ `ErrInvalidInput` → **400** `InvalidArtifactInputError`（`artifacts.go:41-44`），**不进入业务层** |
| ② 业务调用 | `artifacts.go:46` → `internal/service/service.go:160-180` | 生成 `pushed_at`（`service.go:168`），调用 `store.Register`；存储错误 → `ErrStorage`（`service.go:170-172`），内容冲突 → `ErrConflict`（`service.go:173-175`） | `ErrStorage` → **503** `storage_unavailable`（`artifacts.go:50-51`）；`ErrConflict` → **409** `ArtifactConflictError`（`artifacts.go:48-49`） |
| ③ 事务提交 | `internal/store/store.go:69-108` | `Begin`（`:70`）、`defer tx.Rollback()` 兜底（`:74`）、身份查询（`:76-78`）、插入记录（`:81-86`）、标签 upsert（`:87-92`）、`Commit`（`:93-95`）；重复/冲突分支无任何写语句（`:101-107`） | 提交前任何错误都经 `:74` 回滚（见 `docs/sqlite-transaction-visibility-analysis.md`）；**提交成功后的 `Rollback` 是空操作** |
| ④ 响应写出 | `artifacts.go:55`（201）、`artifacts.go:20-22`（错误响应统一走 `writeError`） | `svc.Register` 成功返回后，`c.JSON(http.StatusCreated, ...)` 渲染并写出响应体 | 写出失败只影响**交付**：此时阶段 ③ 的 `Commit` 已经完成，服务端没有任何代码把写出失败转成回滚 |

两个结构性事实：

1. **阶段 ④ 严格在阶段 ③ 之后**。`svc.Register` 返回成功即意味着 `store.go:93-95` 的 `Commit` 已成功（重复/冲突分支则没有任何写入）。因此"响应写出失败"永远发生在"事务结果已定"之后，二者之间没有补偿逻辑。**[源码推导]**
2. **整条路径不读取请求上下文**。解析直接读 `c.Request.Body`（`artifacts.go:40`）；`Service.Register` 的签名不接收上下文（`service.go:160`）；存储用 `s.db.Begin()` 而非 `BeginTx`（`store.go:70`），全部读写方法（`store.go:111-147`）亦无 `QueryContext`/`ExecContext`。请求上下文的取消不会中断其中任何一个阶段。**[源码推导]**（`net/http` 是否为已断开的连接调用处理函数，属于标准库与部署层行为，不在本仓库代码范围内。）

### 1.2 读路径：核对结果所用的公开入口

后续所有"核对状态"都经 `GET /v1/artifacts`（装配于 `router.go:48`）：参数解析 `artifacts.go:64` → 业务分派 `service.go:186-218` → 存储读取（列表 `store.go:111-131`、摘要 `store.go:134-138`、标签 `store.go:141-147`）→ 命中 200（`artifacts.go:77-81`）、未命中 404（`artifacts.go:72-73`）、故障 503（`artifacts.go:74-75`）。每条读都是独立 autocommit SELECT（读方法均直用 `s.db`，无 `Begin`）**[源码推导]**，与 `docs/sqlite-transaction-visibility-analysis.md` §1.2 一致。

---

## 2. 三件必须区分的事

### 2.1 请求上下文被取消 ≠ 处理中止

客户端断开（或主动取消）会把取消信号放进请求上下文，但本服务的登记路径**没有任何一处消费这个信号**（§1.1 事实 2）。因此，只要处理函数已经开始执行：

- 请求体照常读取、校验；
- 业务照常调用、事务照常提交；
- 服务端照常生成 201 响应并尝试写出。

"上下文已取消"是一个**客户端侧/传输层事实**，不是处理信号。已取消上下文是否会导致响应无法送达客户端，是交付问题（§2.3），与登记是否完成无关。**[新增验证]** 场景一（§3）。

### 2.2 请求体读取失败 = 输入非法（400），与"内容不合法"同一路径

`ParseRegistration` 把传输层读失败与 JSON 语法/字段问题归为同一类：

- 首对象解码期间读失败 → `decoder.Decode` 返回该错误 → `ErrInvalidInput`（`input.go:82-86`）；
- **完整 JSON 对象已经读到**、但末尾确认读返回**非 EOF 错误** → 末尾检查（`input.go:87-89`）要求恰好 `io.EOF`，非 EOF 错误同样判负 → `ErrInvalidInput`。原因：无法确认对象之后不存在第二个 JSON 值，与"对象后有多余内容"同等处理。

两种情况的 HTTP 结果都是 **400 `InvalidArtifactInputError`**（`artifacts.go:41-44`），且 `return` 发生在 `svc.Register`（`artifacts.go:46`）之前——**不调用登记存储**，不留下新记录，既有记录与标签指针不变。**[新增验证]** 场景二（§4）；400 不触碰存储的错误优先级另有 **[已有验证]** `TestAssemblyEntryErrorPriority`（`internal/api/assembly_test.go:217-253`，自定义存储调用计数为零）。

### 2.3 客户端未收到成功响应 ≠ 登记未发生

响应写出（`artifacts.go:55`）在事务提交（`store.go:93-95`）之后执行。写出失败（连接断开、对端重置）时：

- **已提交状态不受任何影响**：`defer tx.Rollback()`（`store.go:74`）在 `Commit` 成功后是空操作，服务端不存在"写出失败 → 回滚"的代码路径。**[源码推导]** + **[新增验证]** 场景三（§5）；
- **不能承诺已断开的客户端收到了某个 HTTP 状态码**：状态行与响应体是否离站取决于传输层，服务端只能保证"生成了 201 并尝试写出"。本文与用例不断言断开客户端"看到了什么"——那是不可核对的；
- **客户端的安全补救是原样重试**：重复判定（`store.go:101-106`）全等时返回库中原记录，HTTP 层对首次与重复统一给 201（`artifacts.go:52-56` 注释），因此重试拿到**原 `pushed_at`**、不产生第二条记录、标签指针不回移。这使"没收到 201 就重试同一请求体"成为幂等操作。**[新增验证]** 场景三阶段 3。

一句话区分：**请求上下文被取消**不影响处理（§2.1）；**请求体读取失败**使处理在阶段 ① 以 400 终止、不触碰存储（§2.2）；**客户端未收到成功响应**发生在处理全部完成之后，只影响交付、不影响已提交状态（§2.3）。

---

## 3. 场景一：处理开始前已取消请求上下文

用例 `TestCancelledContextRegistrationCompletes`（`registration_delivery_test.go`），在 `NewRouter` 与 `NewRouterWithStore` 两个入口上各跑一遍，均使用内置 SQLite 真实文件。

- 固定数据：R = `registry-demo/control`，T = `release`，A = `sha256:` + 64 个 `a`；`signature_verified=true, retention_days=30, size_bytes=1024`。
- 构造请求时**先取消上下文再交给路由**（`context.WithCancel` + 立即 `cancel()`），请求体是完整合法的 A 登记 JSON，存储可用。
- 断言（全部经公开入口）：
  1. 服务端生成 **201**，响应体含记录与 `pushed_at = tA`（服务端结果，经 `httptest.ResponseRecorder` 核对）；
  2. 随后一次**新的、未取消的** `GET ?repository=R`：**200**，数组恰为 `[A]` 一条；
  3. `GET ?repository=R&tag=T`：**200**，命中 A 且 `pushed_at = tA`。

依据：§1.1 事实 2（处理路径不读取请求上下文）。**[新增验证]**

边界说明：本场景只覆盖"处理函数已开始执行"之后的取消。真实部署中 `net/http` 是否在连接已断开时仍调用处理函数、以及反向代理的取消传播策略，都不在本仓库代码范围内，本文不作结论。

---

## 4. 场景二：请求体读取返回非 EOF 错误

用例 `TestBodyReadFailureRejectsWithoutStoreWrite`（`registration_delivery_test.go`），两个入口各跑一遍。测试夹具 `failAfterReader` 先按字节交出给定内容、随后每次读都返回注入的非 EOF 错误，确定性地模拟"传输中断"，不依赖休眠或网络断开的时机。

- 阶段 0：经正常 POST 登记 A 并提交，标签 T 指向 A（`pushed_at = tA`）。
- 阶段 1（**完整对象 + 末尾读失败**）：B 的完整合法 JSON 对象全部交出后，确认末尾的那次读返回注入错误。断言：
  - POST 结果 **400 `InvalidArtifactInputError`**、固定 message（`artifacts.go:41-44`）；
  - 仓库列表 **200** 仍仅 `[A]`；标签查询 **200** 仍命中 A、`pushed_at = tA`；按 A 摘要 **200** 七字段逐字不变；按 B 摘要 **404 `ArtifactNotFoundError`**（B 是合法摘要身份，只是没有任何已提交记录）。
- 阶段 2（**对象中途读失败**）：B 的 JSON 只交出一半即注入错误。断言与阶段 1 完全相同——同一 400 路径，同样不留痕迹。

"不调用登记存储"由源码结构保证（`artifacts.go:41-44` 的 `return` 在 `:46` 之前）**[源码推导]**；"不留下新记录、原有记录和标签保持不变"经公开 GET 核对 **[新增验证]**。错误优先级（400 先于任何存储访问）另有 **[已有验证]** `TestAssemblyEntryErrorPriority`、`TestStorageUnavailableStillValidatesInput`（`regression_test.go:545-576`）。

---

## 5. 场景三：事务提交成功后响应写出失败

用例 `TestResponseWriteFailureKeepsCommittedRecord`（`registration_delivery_test.go`），两个入口各跑一遍。测试夹具 `failingWriter` 是一个 `Write` 必败的 `http.ResponseWriter`，确定性模拟"响应体写出期间连接断开"，不依赖真实网络。

### 5.1 阶段 1：A 提交，但 201 无法交付

A 的首次登记经 `failingWriter` 发出。断言：

- 服务端生成了 **201** 状态行（`httptest` 记录的服务端结果）；
- 响应体**一个字节都没有写出**（`Write` 全部失败）——客户端视角是"什么都没收到"；
- 随后一次新的 `GET ?repository=R`：**200**，数组恰为 `[A]`，A 的 `pushed_at`（记为 `tA`，取自该查询响应，因为 201 响应体从未送达）非空；`GET ?repository=R&tag=T` 命中 A。

**写出失败不是事务回滚**：记录与标签指针在提交点已经落库，交付失败发生在其之后（§1.1 事实 1）。**[新增验证]**

### 5.2 阶段 2：同仓库同标签登记第二摘要 B

经健康连接登记 B（`size_bytes=2048`）：**201**，`pushed_at = tB`；标签 T 移向 B。

### 5.3 阶段 3：原样重试首条 A 请求

这正是"没收到 201 的客户端"会做的补救。断言：

- 重试返回 **201**，`pushed_at` 为**原 `tA`**（重复判定返回库中原记录，`store.go:101-106`；首次与重复共用 201，`artifacts.go:52-56`）；
- 仓库列表 **200**，按首次登记顺序为 `[A, B]`（`store.go:111-131` 的 `ORDER BY id`）；
- 标签查询 **200** 仍命中 **B**（重试无任何写语句，指针不回移）；
- 按 A、B 两个摘要查询均 **200**，各返回原记录七字段。

### 5.4 阶段 4：改变 A 的体积 → 409

仅把 A 的 `size_bytes` 改为 2048 再提交：**409 `ArtifactConflictError`**、固定 message（`service.go:173-175` → `artifacts.go:48-49`），不写任何内容。

### 5.5 阶段 5：合法查询不存在的摘要 → 404

按合法但从未登记的摘要 C 查询：**404 `ArtifactNotFoundError`**、固定 message（`service.go:209-210` → `artifacts.go:72-73`）。

### 5.6 阶段 4、5 之后的状态核对

列表仍为 `[A, B]` 且顺序不变；标签仍命中 B（`pushed_at = tB`）；按 A 摘要取回的记录与阶段 1 提交的内容**逐字一致**（含 `pushed_at = tA`）。**[新增验证]**

---

## 6. 服务端处理结果与客户端接收结果对照

| 场景 | 服务端处理结果 | 服务端生成 | 客户端接收 | 后续公开查询 |
|---|---|---|---|---|
| 处理开始前上下文已取消（§3） | 登记完成、事务提交 | 201 + 记录 | 不承诺（取决于传输层） | 列表恰 `[A]`，标签指 A |
| 完整对象后末尾读失败（§4） | 阶段 ① 以 400 终止，**未触碰存储** | 400 `InvalidArtifactInputError` | 正常连接下即该 400 | 列表仅 `[A]`，标签指 A，B 摘要 404 |
| 对象中途读失败（§4） | 同上 | 同上 | 同上 | 同上 |
| 提交后响应写出失败（§5.1） | 登记完成、事务已提交 | 201（状态行），响应体未送达 | 不承诺——**不得解释为回滚** | 列表恰 `[A]`，标签指 A |
| 写出失败后原样重试（§5.3） | 判重，返回原记录 | 201 + 原 `pushed_at` | 正常连接下即该 201 | 列表 `[A, B]`，标签指 B，两摘要均 200 |

原则：**服务端处理结果**由源码阶段（§1.1）决定，可经后续公开查询核对；**客户端接收结果**取决于传输层交付，服务端无法承诺已断开的客户端看到任何状态码。分析、日志与客户端重试策略都应建立在"处理结果可经查询核对"之上，而不是"客户端一定收到了某个响应"之上。

---

## 7. 对自定义存储的边界声明

- §3–§5 中**事务提交、提交后可见、列表顺序、标签指向、重试返回原 `pushed_at`** 等结论，由内置 SQLite 适配器的源码（`store.go:69-147`）与对真实 SQLite 文件的行为用例共同支撑。
- 对于经 `NewRouterWithStore`（`router.go:32-54`）注入的自定义 `service.Store`：接口注释约定"Register 原子写入、重复返回库中记录、查询读已提交状态"（`service.go:124-142`），但业务层与路由没有任何代码强制这些行为；自定义存储若呈现相同保证，**保证来自其自身实现**。本文不把接口约定或 SQLite 上的结果外推为所有存储的证明，与 `docs/storage-boundary-analysis.md` §2.3、`docs/sqlite-transaction-visibility-analysis.md` §7 的声明一致。
- 与存储无关的部分（请求体解析失败 → 400 且不进入业务层、201/409/404 的 HTTP 映射、处理路径不读取请求上下文）位于 handler/input/service 层，对两种路由入口与任何后端同样成立；本轮用例在两个入口上分别验证（子测试 `NewRouter`/`NewRouterWithStore`）。

---

## 8. 结论—依据—验证对照表

| # | 结论 | 源码依据 | 验证 |
|---|---|---|---|
| 1 | 登记处理沿"解析 → 业务 → 事务提交 → 响应写出"顺序执行，响应写出严格在事务提交之后 | `artifacts.go:40,46,55`；`store.go:93-95` | [源码推导]；[新增] 场景三阶段 1 |
| 2 | 处理路径不读取请求上下文；处理开始前已取消上下文的合法登记仍完成，服务端生成 201，后续 GET 查到唯一记录且标签指向它 | `artifacts.go:40`；`service.go:160`；`store.go:70,111-147`（无 `BeginTx`/`QueryContext`） | [源码推导] + [新增] `TestCancelledContextRegistrationCompletes`（两入口） |
| 3 | 完整 JSON 对象已读到、末尾确认读返回非 EOF 错误 → 400 `InvalidArtifactInputError`；对象中途读失败同路径 | `input.go:82-89`；`artifacts.go:41-44` | [新增] `TestBodyReadFailureRejectsWithoutStoreWrite`（两入口、两种失败位置） |
| 4 | 400 在任何存储调用之前返回；读取失败的登记不留下新记录，原有记录与标签逐字不变 | `artifacts.go:41-44` 先于 `:46` | [源码推导] + [新增] 同用例的 GET 核对；[已有] `TestAssemblyEntryErrorPriority` |
| 5 | 提交成功后响应写出失败不是回滚：记录与标签保持已提交状态，后续 GET 可查 | `store.go:74`（提交后回滚为空操作）、`:93-95`；`artifacts.go:55` | [源码推导] + [新增] `TestResponseWriteFailureKeepsCommittedRecord` 阶段 1（两入口） |
| 6 | 不能承诺已断开的客户端收到某个 HTTP 状态码；服务端只能保证生成并尝试写出 | 写出在 `artifacts.go:55`，交付属传输层 | [源码推导]；用例只断言服务端结果与后续查询 |
| 7 | 未收到 201 后的原样重试是幂等补救：201 + 原 `pushed_at`，列表按首次登记顺序含两条，标签仍指第二条，两摘要均 200 | `store.go:101-106,111-131`；`artifacts.go:52-56` | [新增] 同用例阶段 2–3（两入口，内置 SQLite） |
| 8 | 改变首条记录的体积 → 409 `ArtifactConflictError`；合法查询不存在的摘要 → 404 `ArtifactNotFoundError`；二者均不改变已提交状态 | `service.go:173-175,209-210`；`artifacts.go:48-49,72-73` | [新增] 同用例阶段 4–6；[已有] `TestRegistrationSequenceIdentityAndTagPointer` 冲突子测试 |
| 9 | 上述事务与可见性结论仅适用于内置 SQLite；自定义存储的相同保证来自其自身实现，接口注释不构成强制 | `service.go:124-142`；`router.go:32-54` | [源码推导]；本轮用例仅针对内置 SQLite |

---

## 9. 复现方式

```bash
go test ./...                            # 全部用例（基线 + 新增回归）
go test ./internal/api -run 'TestCancelledContextRegistrationCompletes|TestBodyReadFailureRejectsWithoutStoreWrite|TestResponseWriteFailureKeepsCommittedRecord' -v
go test -race -count=5 ./internal/api    # 稳定性核对
go vet ./...
```

新增用例的被测对象始终是生产装配（`store.Open` 真实 SQLite 文件 + `NewRouter`/`NewRouterWithStore`），所有可观察结果都经公开 HTTP 入口（`httptest` 驱动 POST/GET）断言；三个测试夹具——预先取消的请求上下文、按字节交出后注入读失败的 `failAfterReader`、`Write` 必败的 `failingWriter`——都是确定性的进程内构造，不依赖休眠、真实网络断开或时序运气。因此任何保持公开契约与既有处理顺序的内部重构都应持续通过。
