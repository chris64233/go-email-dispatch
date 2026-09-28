# go-email-dispatch

抑制感知（suppression-aware）的批量邮件投递服务：管理活动（campaign）生命周期、冻结模板版本与受众、以有期限租约驱动工作者投递，并以**持久化发送授权点**为边界处理与领取/确认并发的抑制事件。支持活动**暂停/恢复**：暂停围栏已领取未授权的邮件，恢复后沿用冻结快照继续，已授权邮件在暂停期间照常完成。

开发环境：Go 1.23.0，仅用标准库。

## 核心设计

### 1. 启动时锁定模板版本与受众

- 活动创建后为 `draft`；`start` 时锁定 `template_version`、冻结收件人集合，并为每个收件人生成投递项。
- 启动后模板版本与受众不可变；重复启动返回 `conflict`。
- 随每次租约下发冻结的模板版本与变量快照，工作者不需要（也不应当）回查可变配置。

### 2. 冻结受众，发送前权威复查

受众集合在启动时冻结，但**真正发送以前**在持久化授权点重新检查三类抑制：

| 抑制类型 | 作用范围 |
| --- | --- |
| `global_unsubscribe` 全局退订 | 地址，对所有活动生效 |
| `bounce` 地址退信 | 地址，对所有活动生效 |
| `campaign_suppression` 活动抑制 | 地址 × 指定活动 |

地址经规范化（去空白、小写）后比较；抑制事件支持回填 `occurred_at`（如延迟到达的退信 webhook）。

### 3. 有期限租约 + fencing 幂等

- 投递幂等键 `dispatch_key = sha256(campaign_id, address)`，同一活动 × 收件人在任意重试下稳定不变。
- 领取（lease）产生随机不可猜测的 `lease_token`、单调递增的 `attempt` 与 `lease_expires_at`。
- 租约到期且无回执时，失联工作者的任务可被新工作者以**新 attempt、新 token** 重新领取。
- 旧工作者迟到的回执返回 `stale_attempt`（fencing token），**绝不覆盖新尝试**；每次 attempt 的租约、决策、回执分别留存为审计轨迹。
- 同一 attempt 的重复回执返回首次处理时的稳定结论（`duplicate=true`）。

### 4. 持久化“发送授权点”是唯一裁决边界

工作者在真正调用邮件提供商**之前**必须调用授权接口。授权决策（放行或拒绝）先落库再返回：

- 授权**以前**已生效（`occurred_at <= 授权时刻`）的抑制或活动取消 → 拦截投递，任务进入 `suppressed` / `canceled` 终态。
- 授权**以后**才录入的抑制 → 不回滚投递结果（邮件可能已发出），但在回执中记录解释性审计条目（事件 id、类型、授权时间、生效时间），可在投递审计接口查到。
- 重复授权返回同一持久化决策（按 auth id 稳定）。

### 5. 重试、终态、取消

- 回执三类：`success` / `temporary_failure` / `permanent_failure`。
- 临时失败按指数退避进入 `retry_wait`（可配 `max_attempts`、初始/最大退避、乘数），退避到期才可再领取；达到次数上限转为终态 `failed`。
- 永久失败直接 `failed`；成功为 `sent`。终态不可再领取。
- 活动取消：`pending`/`retry_wait` 项立即进入 `canceled`，不再接受新领取；在途未授权项在授权点被拒（`campaign_canceled`）；已授权项如实等待回执。重复取消幂等（返回 `changed=false`），统计不会重复累加。

### 6. 暂停 / 恢复

暂停（`pause`）与恢复（`resume`）都在**单个持久化事务**内完成活动状态与在途任务的迁移，重复调用幂等（`changed=false`）。

**暂停时：**

- 活动 `running → paused`，立即停止新领取（暂停期间领取返回 `campaign_not_running`）。
- `pending` / `retry_wait` 项挂起为 `paused`（非终态，退避计时也一并挂起）。
- 已领取但**尚未授权**的租约被**围栏（fence）**：在其当前 attempt 上提前持久化一份不可变的 `granted=false, reason=campaign_paused` 授权决策，任务转为 `paused`。旧工作者随后调用授权只拿到这份稳定拒绝，提交回执得到 `invalid_state`，**不可能再发出邮件**。
- **已经授权**的邮件不受影响：暂停期间照常投递并接收回执（成功/失败/重试均如实落库）。

**恢复时：**

- 活动 `paused → running`；`paused` 项回到 `pending`，沿用启动时冻结的**原收件人与模板版本/变量快照**，由新工作者以**新 attempt、新 token** 领取。被围栏 attempt 上的拒绝决策保留在审计轨迹中。
- 暂停期间新生效的退订/退信无需特殊处理：恢复后领取的新 attempt 仍在授权点按事件生效时间复查，照样拦截。
- 旧租约 token 属于被取代的 attempt：授权永远返回 `campaign_paused` 拒绝决策，回执得到 `stale_attempt`——过期工作者无法再取得授权。
- 暂停期间已进入终态的邮件（`sent`/`failed`/`suppressed`/`canceled`）恢复后**不会重复发送**；所有暂停/恢复/领取/授权/回执请求都保持幂等。

**与取消的区别：**暂停是可逆的 `paused` 挂起态，拦截原因记为 `campaign_paused`；取消是不可逆终态，未持有项直接 `canceled`。暂停中的活动也可以取消，取消后不可恢复。

### 任务状态机

```
pending ──lease──▶ leased ──authorize(granted)──▶ authorized ──receipt success──▶ sent (终态)
  ▲                   │                                ├── permanent_failure ──▶ failed (终态)
  │                   │                                └── temporary_failure ──▶ retry_wait
  │                   │                                                              │ 退避到期
  │                   │                                                              └──lease(attempt+1)
  │                   ├── authorize(denied: suppression) ──▶ suppressed (终态)
  │                   └── authorize(denied: canceled)    ──▶ canceled (终态)
  │                   │
  └──── resume ─── paused ◀──── pause（pending/retry_wait 直接挂起；
                                   leased 未授权项在当前 attempt 落 campaign_paused 拒绝后挂起）

注：authorized 等已越过授权点的邮件不受 pause 影响，暂停期间照常完成；
    暂停中的活动若被取消，paused 项直接进入 canceled 终态。
```

## 代码结构

| 文件 | 内容 |
| --- | --- |
| `model.go` | 领域模型、状态枚举、重试策略（duration 以毫秒 JSON 序列化） |
| `errors.go` | 统一领域错误类型与错误码（`Error.Code`） |
| `store.go` | `Store` 事务边界接口 + `MemoryStore` 持久化实现（租约、授权点、fencing、审计） |
| `service.go` | 应用服务：校验、时钟注入、编排存储事务 |
| `api.go` | HTTP Handler（Go 1.22+ 方法路由） |
| `cmd/emaildispatch/main.go` | 可运行服务入口（默认 `:8080`，`LISTEN_ADDR` 可覆盖） |

> `MemoryStore` 以互斥锁串行化事务、进程内持久化，满足单节点语义与全部测试。`Store` 接口即事务边界：SQL 实现应在单个数据库事务内完成 `LeaseTasks` / `Authorize` / `CompleteReceipt` 的相同读写集合（授权决策与状态迁移同事务提交）。

## 运行

```bash
go run ./cmd/emaildispatch
# 测试
go test ./...
go test -race ./...
```

## HTTP 接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/v1/campaigns` | 创建活动（草稿），可带 `retry_policy` |
| POST | `/v1/campaigns/{id}/start` | 启动：锁定模板版本、冻结受众 |
| GET | `/v1/campaigns/{id}` | 查询活动（含冻结快照） |
| POST | `/v1/campaigns/{id}/leases` | 领取发送项（有期限租约） |
| POST | `/v1/campaigns/{id}/pause` | 暂停活动（幂等） |
| POST | `/v1/campaigns/{id}/resume` | 恢复活动（幂等） |
| POST | `/v1/campaigns/{id}/dispatches/{key}/authorize` | 持久化发送授权点 |
| POST | `/v1/campaigns/{id}/dispatches/{key}/receipts` | 提交发送回执 |
| GET | `/v1/campaigns/{id}/dispatches/{key}` | 投递项审计轨迹 |
| POST | `/v1/suppressions` | 录入抑制事件 |
| POST | `/v1/campaigns/{id}/cancel` | 取消活动（幂等） |
| GET | `/v1/campaigns/{id}/stats` | 统计查询 |

错误响应统一形如 `{"error":{"code":"...","message":"..."}}`，状态码：400（校验/非法回执）、404（不存在）、409（状态冲突、租约失效、fencing、活动未运行）。

### 端到端示例

```bash
# 1. 创建 + 启动
curl -s -XPOST localhost:8080/v1/campaigns -d '{"name":"welcome"}'
# -> {"id":"cmp_xxx", ...}

curl -s -XPOST localhost:8080/v1/campaigns/cmp_xxx/start -d '{
  "template_version": "tpl-42",
  "recipients": [{"address":"a@x.com","vars":{"name":"A"}},{"address":"off@x.com"}]
}'

# 2. 抑制（全局退订）
curl -s -XPOST localhost:8080/v1/suppressions -d '{
  "type":"global_unsubscribe","address":"off@x.com","reason":"unsubscribe link"
}'

# 3. 工作者领取
curl -s -XPOST localhost:8080/v1/campaigns/cmp_xxx/leases -d '{
  "worker_id":"w1","count":10,"lease_duration_ms":60000
}'
# -> {"tasks":[{"dispatch_key":"dk_...","lease_token":"lease_...","attempt":1,...}]}

# 4. 发送前授权（off@x.com 在此被拒：granted=false, reason=global_unsubscribe）
curl -s -XPOST localhost:8080/v1/campaigns/cmp_xxx/dispatches/dk_.../authorize \
  -d '{"lease_token":"lease_..."}'

# 5. 授权放行后提交回执（token 也可放 ?lease_token= 或 X-Lease-Token 头）
curl -s -XPOST localhost:8080/v1/campaigns/cmp_xxx/dispatches/dk_.../receipts -d '{
  "lease_token":"lease_...",
  "result":"success",
  "provider_message_id":"pm-123"
}'
# 重复提交 -> 200, {"duplicate":true,...}，统计不重复累加

# 6. 统计 / 暂停 / 恢复 / 取消 / 审计
curl -s localhost:8080/v1/campaigns/cmp_xxx/stats
# 暂停：未授权在途项被围栏（授权点返回 granted=false, reason=campaign_paused），已授权项可继续完成
curl -s -XPOST localhost:8080/v1/campaigns/cmp_xxx/pause -d '{}'
# 恢复：围栏项沿用冻结快照以新 attempt 重新领取；重复调用 {"changed":false}
curl -s -XPOST localhost:8080/v1/campaigns/cmp_xxx/resume -d '{}'
curl -s -XPOST localhost:8080/v1/campaigns/cmp_xxx/cancel -d '{}'
curl -s localhost:8080/v1/campaigns/cmp_xxx/dispatches/dk_...
```

### 请求/响应要点

- 时长字段（`initial_backoff_ms`、`max_backoff_ms`、`lease_duration_ms`、`occurred_at_ms`）一律用**毫秒整数**。
- `retry_policy` 缺省：最多 3 次尝试、30s 初始退避、10m 上限、2 倍乘数。
- `stats` 计数完全由持久化任务状态派生（含 `total_attempts`、各状态计数、`finished`），重复回执/重复暂停/恢复/取消不产生增量。
- 统计字段 `paused` 为当前处于暂停挂起态的邮件数；`paused_intercepted` 为**累计**因暂停围栏而未获授权的次数（多轮暂停会累加；同一次暂停对同一在途 attempt 至多计一次），与 `canceled`（主动取消）、`sent`（实际发送）相区分。

## 错误码

| code | 含义 | HTTP |
| --- | --- | --- |
| `validation_error` | 入参不合法 | 400 |
| `invalid_receipt` | 回执结果取值非法 | 400 |
| `not_found` | 活动/投递项不存在 | 404 |
| `conflict` | 状态冲突（重复启动等） | 409 |
| `campaign_not_running` | 活动非 running（如取消后领取） | 409 |
| `invalid_lease` | token 与任何租约都不匹配 | 409 |
| `lease_expired` | 租约已过期，操作被拒 | 409 |
| `stale_attempt` | 回执来自被新 attempt 取代的旧租约 | 409 |
| `invalid_state` | 当前任务状态不允许该操作（如未授权先回执） | 409 |

## 测试覆盖

`go test ./...` 包含（均支持注入假时钟精确控制时序）：

- 模板锁定、受众冻结、重复地址/重复启动校验；
- `dispatch_key` 稳定性；
- 领取→授权→回执→统计主流程、重复授权/重复回执的稳定结果；
- 租约过期接管与 fencing（旧回执不覆盖新尝试），审计轨迹完整；
- 三类抑制在授权点拦截、被拒后发送报错；
- 授权与抑制的时序竞争：授权前拦截、授权后仅审计；回填事件以 `occurred_at` 裁决；
- 临时失败退避、退避到期前不可领取、预算耗尽终态、永久失败终态；
- 取消后不授权/不领取、重复取消幂等、统计稳定；
- 暂停：不可领取、已领取未授权项被围栏（`campaign_paused` 拒绝持久化且稳定）、被拒后回执报错；已授权邮件暂停期间照常完成、重复回执幂等；
- 恢复：沿用冻结收件人/模板/变量以新 attempt 重新领取，旧 token 永远拿不到新授权、旧回执 `stale_attempt`；暂停期间的退订/退信恢复后在授权点拦截；终态邮件不重复发送；暂停后取消不可恢复；
- 统计区分 `paused` / `paused_intercepted` / `canceled` / `sent`，多轮暂停拦截按轮累加；非法状态迁移（draft 暂停等）返回冲突；
- HTTP 端到端与错误码/状态码映射；
- 并发场景（`-race`）：同一回执 32 路并发仅生效一次；取消与领取并发后不存在任何新授权；暂停/恢复/领取/授权/回执 30 轮交错并发后每封邮件恰好成功一次。
