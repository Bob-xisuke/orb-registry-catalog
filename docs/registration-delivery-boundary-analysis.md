# 登记请求取消、读取失败与响应交付边界分析（附回归用例）

既有基线已具备不可修改的制品登记、标签指针切换与重复提交处理（见 `docs/artifact-identity-analysis.md`、`docs/sqlite-transaction-visibility-analysis.md`）。本文补齐一次 `POST /v1/artifacts` 登记从**请求取消**到**响应交付**的边界：沿请求体解析、业务调用、事务提交、响应写出四个阶段说明实际行为，并把三件容易混为一谈的事分开——**请求上下文被取消**、**请求体读取失败**、**客户端未收到成功响应**。

- 源码版本：工作区当前提交（`bc92423`）
- 适用范围：除特别标注外，行为用例均在**内置 SQLite 适配器**（`internal/store`）上验证；对自定义 `service.Store` 的边界见 §8。
- 验证方式标注：
  - **[已有验证]** 基线测试已覆盖，注明函数与文件
  - **[新增验证]** 本次补充的用例，位于 `internal/api/registration_delivery_test.go`
  - **[源码推导]** 由源码结构直接得出（如"不存在某调用"），不通过行为用例证明
- 本次只新增本文档与一个测试文件；生产代码、README、`docs/` 下既有文档、SQLite schema、启动配置（`main.go`）、`GET /healthz`、输入规范化、错误对象形状与固定文案、既有数据库兼容性均未改动。

---

## 1. 一次登记请求的四个阶段（源码位置）

| 阶段 | 位置 | 内容 |
|---|---|---|
| 装配 | `internal/api/router.go:47` | `router.POST("/v1/artifacts", registerArtifact(svc))`；两个构造入口 `NewRouter`（`router.go:21-23`）与 `NewRouterWithStore`（`router.go:32-54`）挂的是同一处理函数 |
| ① 请求体解析 | `internal/api/artifacts.go:40` → `internal/input/input.go:78-89` | `input.ParseRegistration(c.Request.Body)`：第一次 `Decode` 解出单个 JSON 对象（`input.go:84`），第二次 `Decode` 要求读到 `io.EOF`（`input.go:87-89`）；任一失败即 `ErrInvalidInput` |
| ① 失败映射 | `artifacts.go:41-44` | 400 `InvalidArtifactInputError` + 固定 message，**发生在任何存储调用之前** |
| ② 业务调用 | `artifacts.go:46` → `internal/service/service.go:160-180` | 生成 `pushed_at`（`service.go:168`），调用 `store.Register`；存储错误归 `ErrStorage`（`:170-172`），内容冲突归 `ErrConflict`（`:173-175`） |
| ③ 事务提交 | `internal/store/store.go:70-96` | `Begin`（`:70`）→ 身份查询（`:76-79`）→ 插入记录（`:81-86`）→ 移动标签指针（`:87-92`）→ `tx.Commit()`（`:93-95`）；`defer tx.Rollback()`（`:74`）兜底，提交成功后的回滚是空操作。重复/冲突分支（`:101-107`）不写库 |
| ④ 响应写出 | `artifacts.go:53-56` | 首次登记与完全相同的重试共享 201，`c.JSON(http.StatusCreated, ...)` 在**事务提交之后**才执行；冲突 409、存储故障 503 分别在 `:48-49`、`:50-51` |

关键次序：**③ 严格先于 ④**。处理函数返回前，提交已经发生；响应写出只是结果的交付，不是结果本身。

---

## 2. 三件必须区分的事

### 2.1 请求上下文被取消 ≠ 处理被中止

登记路径上**没有任何代码读取请求上下文**：`registerArtifact` 只取 `c.Request.Body`（`artifacts.go:40`），从不调用 `c.Request.Context()`；业务层（`service.go:160-180`）不接收 context；SQLite 适配器全部使用非 context 方法——`s.db.Begin()`（`store.go:70`）、`tx.Exec`（`:81,:87`）、`tx.Commit`（`:93`）、`s.db.Query`/`QueryRow`（`:112,:135,:142`）。**[源码推导]**

因此"处理开始前上下文已取消"对服务端处理结果**无影响**：解析、登记、提交照常完成，201 照常生成。取消只可能影响客户端能否**收到**这个响应——那是交付问题，见 §2.3。

### 2.2 请求体读取失败 = 输入非法（400），与已读到的内容无关

`ParseRegistration` 的第二次 `Decode` 要求 `io.EOF`（`input.go:87-89`）：对象之后的**任何非 EOF 结果**——尾随垃圾、第二个对象，或**底层 reader 返回的非 EOF 错误**——都归 `ErrInvalidInput`。即使第一次 `Decode` 已经成功读出一个**完整且合法**的 JSON 对象，只要末尾检查读到非 EOF 错误，整个请求体即不可信（无法确认对象之后还有什么），处理在 ① 终止：400 `InvalidArtifactInputError`，不进入业务调用，不触碰存储。

### 2.3 客户端未收到成功响应 ≠ 登记未生效

201 的写出（`artifacts.go:55`）发生在 `tx.Commit()`（`store.go:93-95`）**之后**。写出失败（连接断开、对端重置）时：

- 事务**不会**因此回滚——`defer tx.Rollback()`（`store.go:74`）在提交成功后是空操作，且写出阶段没有任何代码路径回到存储层；
- 已提交的记录与标签指针保持已提交状态，后续查询如实返回；
- 服务端**不承诺**已断开的客户端最终看到哪个状态码——客户端可能看到 201、看到连接错误、或什么都没看到，这取决于交付时机，不属于服务端处理结果。

---

## 3. 场景一：处理开始前已取消的请求上下文

**[新增验证]** `TestCancelledRequestContextStillCompletesRegistration`，两个路由入口各跑一遍（内置 SQLite）。

前提：请求体完整合法、存储可用；请求上下文在 `ServeHTTP` **之前**已取消（用例先断言 `request.Context().Err() != nil`）。

结果：

- POST 返回 **201**，七字段回显，`pushed_at = tA`（服务端生成）；
- 仓库列表恰含这一条记录；
- 标签查询命中该摘要；
- 摘要查询返回的记录与 201 响应逐字段一致。

依据：§2.1——取消的上下文不在任何读取路径上，处理与未取消时逐字节相同。注意本用例的断言全部关于**服务端处理结果与后续可查询状态**；它不证明真实网络中已断开的客户端能收到这个 201（§2.3）。

---

## 4. 场景二：完整 JSON 对象之后读取返回非 EOF 错误

**[新增验证]** `TestBodyReadFailureAfterCompleteObjectReturns400`，两个路由入口各跑一遍（内置 SQLite）。

夹具：`readThenFailReader` 先完整交付一个**合法**的登记请求体（摘要 B），随后的读取一律返回非 EOF 错误——确定性地复现"对象已读完、末尾检查失败"，不依赖网络或时机。

序列与结果：

1. 先正常登记 A（201，`pushed_at = tA`）作为既有状态；
2. 提交上述请求体 → **400** `InvalidArtifactInputError` + 固定 message（`artifacts.go:41-44`）；
3. 事后经公开查询核对：仓库列表仍仅 `[A]`；A 记录七字段（含 `pushed_at`）逐字不变；标签仍指向 A；按 B 摘要查询 **404** `ArtifactNotFoundError`——B 是合法摘要身份，只是从未成为记录。

依据：§2.2。第一次 `Decode`（`input.go:84`）虽已解出完整对象，第二次 `Decode` 的非 EOF 错误（`input.go:87-89`）使整个请求体被判非法，处理在阶段 ① 终止，阶段 ②③ 从未执行，因此"不调用登记存储、不留新记录、原记录与标签不变"。

---

## 5. 场景三：事务提交成功后响应写出失败

**[新增验证]** `TestResponseWriteFailureAfterCommitKeepsCommittedRecord` 前半段，两个路由入口各跑一遍（内置 SQLite）。

夹具：`failOnWriteResponseWriter` 正常接收响应头与状态行，但 `Write` 一律返回错误——确定性地复现"提交完成、交付失败"，不依赖休眠或网络断开时机。gin 的 `Context.Render` 把写出错误记入 `c.Errors` 并中止渲染，不 panic、不回头触碰存储（gin v1.10.0 `context.go`；处理函数在 `c.JSON` 调用后即返回，`artifacts.go:55-56`）。

结果：

- 服务端生成的状态行是 **201**（写状态行成功、写响应体失败）；
- 后续经**正常**连接的公开查询：按 A 摘要 **200**，记录七字段完整，`pushed_at = tA` 从该查询响应取得（201 响应体从未送达任何人，原始推送时间只能由已提交状态证明）。

两条边界声明（对应 §2.3）：

1. **写出失败不是事务回滚**：提交（`store.go:93-95`）先于写出（`artifacts.go:55`），写出阶段不存在任何回滚路径；本用例以"写出失败后查询仍返回已提交记录"为证。
2. **不承诺已断开客户端收到某个状态码**：本用例只断言服务端可观察状态（生成的状态行 + 后续 GET 结果），不断言客户端收到了什么；"客户端未收到 201"与"登记未生效"不能互推。

---

## 6. 场景四：首条登记未交付之后的完整序列

**[新增验证]** `TestResponseWriteFailureAfterCommitKeepsCommittedRecord` 后半段，紧接 §5 的未交付首条登记（A，`pushed_at = tA` 经查询取得），两个路由入口各跑一遍：

1. 同仓库、同标签登记另一摘要 B → **201**，标签指针移向 B；
2. **原样重试首条请求**（A 的原始请求体）→ **201**，响应 `pushed_at` 仍为 `tA`（`store.go:101-105` 全等判重返回库中原记录；HTTP 不区分首次与重复，`artifacts.go:53-56`）；
3. 仓库列表按首次登记顺序含 `[A, B]` 两条；标签仍指向 B（重复路径无写语句，指针不回移）；按 A、B 摘要查询均 **200** 且各字段不变；
4. 改变 A 的 `size_bytes` 再提交 → **409** `ArtifactConflictError` + 固定 message（`store.go:107` → `service.go:173-175` → `artifacts.go:48-49`）；
5. 合法查询一个从未登记的摘要 → **404** `ArtifactNotFoundError` + 固定 message（`artifacts.go:72-73` 的查询侧映射）；
6. 4、5 之后状态不变：A 记录逐字段（含 `tA`）不变、标签仍指 B、列表仍 `[A, B]`。

这一序列证明：未交付的首条登记与正常交付的登记在后续所有公开行为上**不可区分**——重复判重、标签移动、冲突检测、未命中都以已提交状态为准，与"当初的 201 是否送达"无关。

---

## 7. 服务端处理结果 × 客户端接收结果对照

| 情形 | 服务端处理结果 | 服务端生成的响应 | 客户端实际收到 |
|---|---|---|---|
| 上下文已取消，处理完成（§3） | 记录已提交，标签已移动 | 201 + 完整记录 | **不作承诺**：取决于连接是否仍可交付 |
| 完整对象后读取失败（§4） | 无任何写入，既有状态不变 | 400 `InvalidArtifactInputError` | 通常能收到（连接尚可用于响应），但本文不以此为契约 |
| 提交后写出失败（§5、§6） | 记录已提交，标签已移动 | 状态行 201 已生成，响应体交付失败 | **不作承诺**：客户端不得把"没收到 201"当作"未登记" |

客户端侧的可靠做法（由 §6 的行为支撑，不是新增契约）：未收到明确响应时，用相同请求体**重试**——完全相同的重试返回 201 与原始 `pushed_at`，不会制造重复记录或移动标签。

---

## 8. 适用范围与对自定义存储的边界

- **事务提交先于响应写出、提交后写出失败不回滚**：阶段次序（§1）由 `internal/api` 与 `internal/service` 的源码结构决定，对任何 `service.Store` 实现成立；但"提交"本身的语义（原子性、持久性）由存储实现决定，本文的可观察验证在**内置 SQLite** 上进行（`store.go:70-96`，WAL 见 `docs/sqlite-transaction-visibility-analysis.md`）。
- **上下文取消不中止处理**：来自"处理路径不读取请求上下文"这一源码事实（§2.1），与存储实现无关 **[源码推导]**；行为验证在内置 SQLite 上进行。
- **读取失败判 400**：`input.ParseRegistration` 是传输无关的共享入口（`input.go:78-89`），与存储实现无关。
- 自定义 `service.Store`（经 `NewRouterWithStore` 注入）若呈现相同的提交语义，保证来自**其自身实现**；接口注释中的约定（`service.go:124-142`）不构成业务层或路由的强制。本轮测试只使用内置 SQLite，所有事后状态均经公开 GET 响应核对，不把自定义存储的接口约定当成实际保证。

---

## 9. 结论—依据—验证对照表

| # | 结论 | 源码依据 | 验证 |
|---|---|---|---|
| 1 | 登记路径不读取请求上下文；处理开始前已取消的上下文不影响解析、登记与提交 | `artifacts.go:40-56` 无 Context 调用；`store.go:70,81,87,93` 均非 ctx 方法 | [源码推导] + [新增] `TestCancelledRequestContextStillCompletesRegistration`（两入口） |
| 2 | 取消上下文下的合法登记：201、列表唯一记录、标签指向、摘要可查 | 同上 | [新增] 同用例 |
| 3 | 完整 JSON 对象之后读取返回非 EOF 错误 → 400 `InvalidArtifactInputError`，不调用存储、不留记录、既有记录与标签不变 | `input.go:84,87-89`；`artifacts.go:41-44` | [新增] `TestBodyReadFailureAfterCompleteObjectReturns400`（两入口） |
| 4 | 事务提交严格先于响应写出；写出失败不回滚，后续查询返回已提交记录 | `store.go:93-95` vs `artifacts.go:55`；`store.go:74` 提交后回滚为空操作 | [源码推导] + [新增] `TestResponseWriteFailureAfterCommitKeepsCommittedRecord` 前半段（两入口） |
| 5 | 不承诺已断开客户端收到某状态码；服务端处理结果与客户端接收结果是两件事 | 写出阶段无存储路径（`artifacts.go:53-56`） | [源码推导]；用例只断言服务端可观察状态 |
| 6 | 未交付首条登记之后：登记 B 移标签、原样重试 A 得 201 与原 `pushed_at`、列表按首次登记序 `[A,B]`、标签仍指 B、两摘要各 200 | `store.go:101-105`（判重无写）、`:87-92`（指针移动）；`artifacts.go:53-56` | [新增] 同用例后半段；[已有] `TestRegistrationSequenceIdentityAndTagPointer`（`regression_test.go:137-253`）覆盖已交付情形 |
| 7 | 改变 A 的体积 → 409 `ArtifactConflictError`；查询未登记摘要 → 404 `ArtifactNotFoundError`；二者之后状态不变 | `store.go:107`；`service.go:173-175`；`artifacts.go:48-49,72-73` | [新增] 同用例末段；[已有] `TestClientErrorShapeAndMessages`（`regression_test.go:340-368`） |
| 8 | 两个路由构造入口行为一致 | `router.go:21-23,32-54,47` | [新增] 三个用例均经 `eachSQLiteEntry` 在 `NewRouter` 与 `NewRouterWithStore` 上各跑一遍 |
| 9 | 提交语义与可观察验证仅针对内置 SQLite；自定义存储的保证来自其自身实现 | `store.go:70-96`；`service.go:124-142` | [源码推导]；本轮测试只用内置 SQLite 并经公开查询核对 |

---

## 10. 复现方式

```bash
go test ./...                          # 全部用例（基线 + 新增回归）
go test ./internal/api -run 'TestCancelledRequestContextStillCompletesRegistration|TestBodyReadFailureAfterCompleteObjectReturns400|TestResponseWriteFailureAfterCommitKeepsCommittedRecord' -v
go test -race -count=3 ./internal/api  # 稳定性核对
go vet ./...
```

三个新增用例的被测对象始终是生产装配（`store.Open` 真实 SQLite 文件 + 两个公开路由入口），所有事后状态经公开 `GET /v1/artifacts` 响应断言。三个边界情形分别由确定性夹具复现——处理前已取消的 `context.Context`、先交付完整对象再失败的 `io.Reader`、写体必败的 `http.ResponseWriter`——不依赖休眠、并发交错或网络断开时机，因此任何保持公开契约的内部重构都应持续通过。
