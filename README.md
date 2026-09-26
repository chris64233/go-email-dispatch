# go-email-dispatch

抑制规则感知的批量邮件投递服务（Go 1.23，零第三方依赖）。

核心能力：活动启动时锁定模板版本并冻结收件人集合；工作者以有期限租约领取发送项；
以持久化的“发送授权点”为界裁决抑制事件与投递的竞态；临时失败按策略重试、永久失败
进入终态；回执与取消均幂等，统计由状态实时推导、绝不重复累加。

## 运行测试

    go test ./...
    go test -race ./...

## 领域模型

| 概念 | 说明 |
| --- | --- |
| `Template` | 邮件模板，`PutTemplate` 同 ID 重复调用版本号 +1 |
| `Campaign` | 投递活动：`draft → running → cancelled` |
| `Recipient` | 收件人，启动时随活动一起冻结 |
| `Delivery` | 发送项，ID 即幂等键 `sha256(campaignID ␀ recipientID)` |
| `Lease` | 有期限租约，`Token` 是栅栏令牌 |
| `Authorization` | 持久化的发送授权点（授权时刻、尝试序号、锁定的模板版本） |
| `Suppression` | 抑制事件：全局退订 / 地址退信 / 活动级抑制 |
| `AuditEntry` | 发送项级审计日志，解释每一次状态流转 |

发送项状态机：

    pending ──领取授权──▶ leased ──sent──▶ sent（终态）
      ▲  ▲                  │
      │  └──退避到期────────┤──transient_failure（未达上限）──▶ pending（带 NextAttemptAt）
      │                     ├──transient_failure（达上限）──▶ failed（终态）
      │                     └──permanent_failure──────────▶ failed（终态）
      ├──授权前命中抑制─────▶ suppressed（终态）
      └──活动取消───────────▶ cancelled（终态）

## 关键语义

### 1. 启动即冻结

`StartCampaign` 在一个原子写入内完成三件事：把当前模板版本锁进
`Campaign.TemplateVersion`、冻结收件人集合、为每个收件人生成稳定幂等键的发送项。
之后 `PutTemplate` 发布新版本、`AddRecipients` 追加收件人都不会影响已启动的活动
（后者直接返回 `ErrCampaignNotDraft`）。

### 2. 发送授权点 vs 抑制事件

每次 `Claim` 成功都会落一条持久化的 `Authorization` 记录。所有状态变更都在
`Store.Update` 的串行写临界区内完成，因此“抑制事件”与“授权”的先后顺序由持久化
写入顺序严格确定，不存在检查与授权之间的竞态窗口：

- 授权**之前**已生效（`EffectiveAt <= AuthorizedAt`）的全局退订、地址退信、
  活动级抑制 → 发送项直接判为 `suppressed` 终态，并写审计；
- 授权**之后**才生效的抑制 → 不改变本次投递结果（邮件可能已经发出），
  回执落地时追加 `late_suppression` 审计条目，保留完整解释。

### 3. 租约栅栏

`Claim` 返回 `LeaseToken` 与到期时间。`Receipt` 必须携带匹配的令牌：

- 令牌不匹配、租约已过期、发送项不在租出状态 → 分别返回
  `ErrStaleLease` / `ErrDeliveryNotLeased`，状态不变；
- 租约过期后可被其他工作者重新领取，产生新的尝试序号与新令牌，
  **旧工作者的迟到回执无法覆盖新尝试**；
- `SweepExpiredLeases` 可由定时器周期调用：运行中的活动把过期项退回
  `pending`，已取消的活动收口为 `cancelled`。

### 4. 重试与终态

- `transient_failure`：未达 `MaxAttempts` 时退回 `pending` 并按
  `Backoff(attempt)` 设置 `NextAttemptAt`，退避窗口内不可领取；达到上限转入
  `failed` 终态；
- `permanent_failure`：立即进入 `failed` 终态；
- 终态发送项永远不会再被领取。

### 5. 幂等性

- **重复回执**：按 `receiptID` 去重，返回与首次处理完全一致的结果
  （`Duplicate=true`），即使第二次携带不同的 outcome 也返回首次的稳定结果；
- **重复取消**：`CancelCampaign` 对已取消活动直接返回当前状态，不产生任何变更；
- **统计**：`Stats` 由发送项当前状态实时推导，而非增量计数器，
  从结构上杜绝重复累加。

### 6. 取消语义

`CancelCampaign` 后不再授权任何新邮件（`Claim` 返回 `ErrCampaignNotActive`）：
未授权的 `pending` 项与已过期租约立即收口为 `cancelled`；仍在有效租约内的在途项
允许其回执正常落地（授权早于取消，属合法发送），并追加 `sent_after_cancel` 审计。

## API 一览

```go
store, _ := emaildispatch.NewFileStore("state.json") // 或 NewMemoryStore()
svc := emaildispatch.NewService(store, emaildispatch.DefaultConfig())

svc.PutTemplate("tpl-1", "<html>…")                        // 模板，版本自增
svc.CreateCampaign("c1", "九月活动", "tpl-1", recipients)   // 草稿
svc.AddRecipients("c1", more)                               // 仅草稿期
svc.StartCampaign("c1")                                     // 锁定模板版本 + 冻结受众

items, _ := svc.Claim("c1", "worker-1", 100)                // 领取 + 授权点
svc.Receipt("c1", items[0].DeliveryID, items[0].LeaseToken,
            "rcpt-001", emaildispatch.OutcomeSent, "250 OK") // 回执

svc.AddSuppression(emaildispatch.SuppressionInput{          // 抑制事件
    Type: emaildispatch.SuppressionGlobalUnsubscribe,
    Email: "user@example.com", Reason: "user opted out",
})
svc.CancelCampaign("c1")                                    // 幂等取消
stats, _ := svc.Stats("c1")                                 // 实时统计
```

## 错误类型

所有错误均为哨兵错误，可用 `errors.Is` 判定：

| 错误 | 含义 |
| --- | --- |
| `ErrNotFound` | 活动 / 发送项 / 模板不存在 |
| `ErrCampaignNotDraft` | 操作要求草稿态（启动、追加收件人） |
| `ErrCampaignNotActive` | 操作要求运行态（领取） |
| `ErrStaleLease` | 租约令牌失效或不匹配（旧工作者回执） |
| `ErrDeliveryNotLeased` | 发送项不在租出状态 |
| `ErrDeliveryTerminal` | 发送项已在终态 |
| `ErrInvalidOutcome` | 回执 outcome 非法 |
| `ErrInvalidArgument` | 参数非法 |

## 持久化

`FileStore` 在每次状态变更后把完整状态以 JSON 原子写入（临时文件 + rename），
进程重启后通过同一路径恢复：活动快照、发送项、授权点、抑制事件、回执去重表
全部持久化。`MemoryStore` 供测试使用；`Store` 是接口，可替换为数据库实现，
只要保证 `Update` 回调的原子性即可维持上述全部语义。

## 代码结构

    types.go       领域类型、状态机、幂等键
    errors.go      哨兵错误
    store.go       Store 接口、MemoryStore、FileStore（JSON 原子落盘）
    service.go     全部业务语义：冻结、授权点、栅栏、重试、幂等、统计
    service_test.go / store_test.go   行为测试与持久化恢复测试
