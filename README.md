# go-email-dispatch

抑制感知（suppression-aware）的批量邮件投递服务：管理活动（campaign）生命周期、冻结模板版本与受众、以有期限租约驱动工作者投递，并以**持久化发送授权点**为边界处理与领取/确认并发的抑制事件。

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

### 6. 合规保留（freeze / retain）

运营人员可在发送前按**活动、收件人范围、原因、有效期**提交保留规则（`rule_id` 由调用方指定，活动内唯一）：

- `freeze` 冻结：有效期内暂停发送。提交时尚未授权的 `pending`/`retry_wait` 项立即进入 `held`；在途（`leased`）未授权项在**授权点**按当前规则被拒（`compliance_hold`）并进入 `held`。解除或到期后回到 `pending` 重新走领取→授权。
- `retain` 保留依据：不阻止发送，但规则生效后**新生成的邮件**（领取批次与授权决策）必须携带不可变依据（规则号、动作、原因）；依据在授权点快照落库，之后解除/失效/新规则都不改变已落库决策。

关键语义：

- **已授权决定不可变**：授权落库之后提交的规则不回滚发送（邮件可能已发出），仅在回执上留解释性审计条目；规则失效后同样不能影响已完成的发送。
- **尚未授权重新判断**：授权点在抑制复查之前先查当前有效规则，迟到的旧工作者只能拿到持久化的稳定结论，不能绕过当前规则。
- **新旧决定不混批**：单次领取（lease）只返回同一保留依据指纹的任务；依据变化即结束本批次，仍被 `freeze` 命中的任务不出现在批次中。
- **幂等与冲突**：相同规则号且范围/动作/原因/有效期语义一致（地址经规范化）→ 200 返回原记录（`created=false`）；任何差异 → `409 conflict`。解除幂等（`changed=false`）。
- **有效期**：`ttl_ms` 与 `expires_at_ms` 二选一，`expires_at` 必须晚于 `effective_at`；到期惰性转为 `expired` 并释放其 `held` 项。已解除的规则保持 `released`。
- **审计与隐私**：投递项查询返回活动、收件人、模板版本、当前规则依据与保留原因；合规审计日志中的收件地址统一经 `MaskAddress` 脱敏（`alice@x.com` → `a****@x.com`），敏感地址不进入普通日志。

### 任务状态机

```
pending ──lease──▶ leased ──authorize(granted)──▶ authorized ──receipt success──▶ sent (终态)
                      │                                ├── permanent_failure ──▶ failed (终态)
                      │                                └── temporary_failure ──▶ retry_wait
                      │                                                              │ 退避到期
                      │                                                              └──lease(attempt+1)
                      ├── authorize(denied: suppression) ──▶ suppressed (终态)
                      ├── authorize(denied: canceled)    ──▶ canceled (终态)
                      └── authorize(denied: freeze)      ──▶ held ──release/expiry──▶ pending
                                                          (pending/retry_wait 也可被 freeze 直接置为 held)
```

## 代码结构

| 文件 | 内容 |
| --- | --- |
| `model.go` | 领域模型、状态枚举、重试策略（duration 以毫秒 JSON 序列化） |
| `errors.go` | 统一领域错误类型与错误码（`Error.Code`） |
| `store.go` | `Store` 事务边界接口 + `MemoryStore` 持久化实现（租约、授权点、fencing、审计） |
| `service.go` | 应用服务：校验、时钟注入、编排存储事务 |
| `api.go` | HTTP Handler（Go 1.22+ 方法路由） |
| `audit.go` | 合规审计日志接口与地址脱敏（`MaskAddress`） |
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
| POST | `/v1/campaigns/{id}/dispatches/{key}/authorize` | 持久化发送授权点 |
| POST | `/v1/campaigns/{id}/dispatches/{key}/receipts` | 提交发送回执 |
| GET | `/v1/campaigns/{id}/dispatches/{key}` | 投递项审计轨迹 |
| POST | `/v1/suppressions` | 录入抑制事件 |
| POST | `/v1/campaigns/{id}/retention-rules` | 提交合规保留规则（幂等，冲突 409） |
| GET | `/v1/campaigns/{id}/retention-rules` | 列出规则（含已解除/失效的历史） |
| GET | `/v1/campaigns/{id}/retention-rules/{rule}` | 查询单条规则 |
| POST | `/v1/campaigns/{id}/retention-rules/{rule}/release` | 解除规则（幂等） |
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

# 6. 统计 / 取消 / 审计
curl -s localhost:8080/v1/campaigns/cmp_xxx/stats
curl -s -XPOST localhost:8080/v1/campaigns/cmp_xxx/cancel -d '{}'
curl -s localhost:8080/v1/campaigns/cmp_xxx/dispatches/dk_...
```

### 合规保留示例

```bash
# 冻结某收件人 24 小时（尚未授权的邮件进入 held，在途未授权项授权点被拒）
curl -s -XPOST localhost:8080/v1/campaigns/cmp_xxx/retention-rules -d '{
  "rule_id":"HOLD-2026-001",
  "action":"freeze",
  "scope":["alice@x.com"],
  "reason":"legal request #4471",
  "ttl_ms":86400000
}'
# 相同内容重复提交 -> 200 {"created":false,...}；范围/有效期变化 -> 409

# 解除（幂等）：held 项回到 pending，可重新领取授权
curl -s -XPOST localhost:8080/v1/campaigns/cmp_xxx/retention-rules/HOLD-2026-001/release -d '{}'

# 仅保留不可变发送依据：之后领取的任务 basis 与授权决策 basis 均带规则号与原因
curl -s -XPOST localhost:8080/v1/campaigns/cmp_xxx/retention-rules -d '{
  "rule_id":"RETAIN-7", "action":"retain",
  "scope":["bob@x.com"], "reason":"consent evidence", "ttl_ms":604800000
}'

# 历史查询（含已解除/已失效规则）与单条规则
curl -s localhost:8080/v1/campaigns/cmp_xxx/retention-rules
curl -s localhost:8080/v1/campaigns/cmp_xxx/retention-rules/HOLD-2026-001
```

### 请求/响应要点

- 时长字段（`initial_backoff_ms`、`max_backoff_ms`、`lease_duration_ms`、`occurred_at_ms`）一律用**毫秒整数**。
- 保留规则提交：`action` 为 `freeze` 或 `retain`；`scope` 为地址数组；`ttl_ms` 与 `expires_at_ms` 二选一，可选 `effective_at_ms`（不得晚于当前时刻）。首次创建返回 201（`created=true`），相同重复提交返回 200（`created=false`），内容冲突返回 409。
- 领取响应每个任务含 `basis`（`rule_id`/`action`/`reason`），无依据批次不带该字段；授权决策对象同样含 `basis` 快照。
- `retry_policy` 缺省：最多 3 次尝试、30s 初始退避、10m 上限、2 倍乘数。
- `stats` 计数完全由持久化任务状态派生（含 `total_attempts`、各状态计数、`finished`），重复回执/重复取消不产生增量。

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

保留规则相关错误沿用统一格式：相同规则号但范围/有效期变化返回 `conflict`（409）；规则不存在返回 `not_found`（404）；动作、范围、原因或有效期非法返回 `validation_error`（400）。授权被冻结规则拦截时决策 `reason=compliance_hold`、`granted=false`（HTTP 仍为 200，决策已持久化）。

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
- HTTP 端到端与错误码/状态码映射；
- 并发场景（`-race`）：同一回执 32 路并发仅生效一次；取消与领取并发后不存在任何新授权。
- 合规保留：规则边界校验；相同规则号幂等返回原记录、范围/动作/原因/有效期变化冲突；`freeze` 立即冻结未授权项、在途项授权点拦截并进入 `held`、解除幂等且恢复重新判断；到期释放 `held` 且不回滚已完成发送；`retain` 依据随批次与授权决策快照、新旧依据不混批；授权后规则变化仅审计；冻结/解除/授权 32 路并发（`-race`）下同一收件人至多一个明确结果；审计日志地址脱敏；规则/投递项历史查询与 HTTP 映射。
