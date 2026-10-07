package emaildispatch

import (
	"encoding/json"
	"math"
	"time"
)

// CampaignStatus 是活动的生命周期状态。
type CampaignStatus string

const (
	// CampaignDraft 已创建但尚未启动：模板版本与受众均未冻结。
	CampaignDraft CampaignStatus = "draft"
	// CampaignRunning 已启动：模板版本与受众快照不可变，可以领取与授权发送。
	CampaignRunning CampaignStatus = "running"
	// CampaignCanceled 已取消：不再授权任何新邮件。
	CampaignCanceled CampaignStatus = "canceled"
)

// TaskState 是单个投递项（活动 × 收件人）的状态。
type TaskState string

const (
	TaskPending    TaskState = "pending"    // 等待首次领取
	TaskLeased     TaskState = "leased"     // 已被某个工作者租约持有，尚未授权
	TaskAuthorized TaskState = "authorized" // 已越过持久化发送授权点，允许实际投递
	TaskRetryWait  TaskState = "retry_wait" // 临时失败，等待退避后重试
	TaskHeld       TaskState = "held"       // 合规冻结：尚未授权，等待解除/失效后重新判断
	TaskSent       TaskState = "sent"       // 终态：成功
	TaskFailed     TaskState = "failed"     // 终态：永久失败或重试耗尽
	TaskSuppressed TaskState = "suppressed" // 终态：授权点被抑制拦截
	TaskCanceled   TaskState = "canceled"   // 终态：活动取消，未再投递
)

// IsTerminal 报告状态是否为终态。
func (s TaskState) IsTerminal() bool {
	switch s {
	case TaskSent, TaskFailed, TaskSuppressed, TaskCanceled:
		return true
	}
	return false
}

// ReceiptResult 是工作者（或下游邮件提供商）回执的结论。
type ReceiptResult string

const (
	// ResultSuccess 投递成功。
	ResultSuccess ReceiptResult = "success"
	// ResultTemporaryFailure 临时失败，按 RetryPolicy 安排重试。
	ResultTemporaryFailure ReceiptResult = "temporary_failure"
	// ResultPermanentFailure 永久失败（硬退信、被拒等），直接进入终态。
	ResultPermanentFailure ReceiptResult = "permanent_failure"
)

// RetentionAction 是合规保留规则对命中收件人采取的动作。
type RetentionAction string

const (
	// RetentionFreeze 冻结：规则有效期内暂停发送，尚未授权的邮件在授权点被拒并进入 held。
	RetentionFreeze RetentionAction = "freeze"
	// RetentionRetain 保留依据：不阻止发送，但规则生效后新生成的邮件必须携带不可变的保留依据。
	RetentionRetain RetentionAction = "retain"
)

// RetentionRuleStatus 是保留规则的生命周期状态。
type RetentionRuleStatus string

const (
	RuleActive   RetentionRuleStatus = "active"   // 生效中（仍在有效期内）
	RuleReleased RetentionRuleStatus = "released" // 已被运营人员解除
	RuleExpired  RetentionRuleStatus = "expired"  // 已超过有效期（惰性派生）
)

// RetentionRule 是按活动、收件人范围、原因与有效期保存的合规保留规则。
// 规则号（RuleID）由调用方提供并在活动内唯一；重复提交按内容做幂等/冲突判定。
type RetentionRule struct {
	RuleID      string              `json:"rule_id"`
	CampaignID  string              `json:"campaign_id"`
	Action      RetentionAction     `json:"action"`
	Scope       []string            `json:"scope"` // 规范化后的收件地址集合
	Reason      string              `json:"reason"`
	EffectiveAt time.Time           `json:"effective_at"`
	ExpiresAt   time.Time           `json:"expires_at"`
	Status      RetentionRuleStatus `json:"status"`
	CreatedAt   time.Time           `json:"created_at"`
	ReleasedAt  time.Time           `json:"released_at,omitempty"`
}

// EffectiveAtTime 报告规则在 at 时刻是否处于有效期且未被解除。
func (r RetentionRule) EffectiveAtTime(at time.Time) bool {
	if r.Status != RuleActive {
		return false
	}
	return !r.EffectiveAt.After(at) && r.ExpiresAt.After(at)
}

// Matches 报告规范化地址是否落在规则范围内。
func (r RetentionRule) Matches(address string) bool {
	for _, a := range r.Scope {
		if a == address {
			return true
		}
	}
	return false
}

// RetentionBasis 是随新邮件携带的不可变发送依据；授权落库后即不再变化。
type RetentionBasis struct {
	RuleID string          `json:"rule_id"`
	Action RetentionAction `json:"action"`
	Reason string          `json:"reason"`
}

// SuppressionType 是抑制事件类型。
type SuppressionType string

const (
	// SuppressionGlobalUnsubscribe 全局退订：地址对所有活动生效。
	SuppressionGlobalUnsubscribe SuppressionType = "global_unsubscribe"
	// SuppressionBounce 地址退信：地址对所有活动生效。
	SuppressionBounce SuppressionType = "bounce"
	// SuppressionCampaign 活动级抑制：仅对指定活动生效。
	SuppressionCampaign SuppressionType = "campaign_suppression"
)

// Recipient 是被冻结的受众条目。Vars 为透传给模板的变量快照。
type Recipient struct {
	Address string            `json:"address"`
	Vars    map[string]string `json:"vars,omitempty"`
}

// RetryPolicy 描述临时失败的重试策略。
type RetryPolicy struct {
	// MaxAttempts 最大尝试次数（含首次），达到后仍临时失败则进入终态 failed。
	MaxAttempts int `json:"max_attempts"`
	// InitialBackoff 首次临时失败后的退避时长。
	InitialBackoff time.Duration `json:"initial_backoff"`
	// MaxBackoff 退避上限。
	MaxBackoff time.Duration `json:"max_backoff"`
	// Multiplier 指数退避乘数。
	Multiplier float64 `json:"multiplier"`
}

// DefaultRetryPolicy 是缺省重试策略：最多 3 次、30s 起步、上限 10m、2 倍指数退避。
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:    3,
		InitialBackoff: 30 * time.Second,
		MaxBackoff:     10 * time.Minute,
		Multiplier:     2,
	}
}

func (p RetryPolicy) withDefaults() RetryPolicy {
	d := DefaultRetryPolicy()
	if p.MaxAttempts > 0 {
		d.MaxAttempts = p.MaxAttempts
	}
	if p.InitialBackoff > 0 {
		d.InitialBackoff = p.InitialBackoff
	}
	if p.MaxBackoff > 0 {
		d.MaxBackoff = p.MaxBackoff
	}
	if p.Multiplier > 0 {
		d.Multiplier = p.Multiplier
	}
	if d.MaxBackoff < d.InitialBackoff {
		d.MaxBackoff = d.InitialBackoff
	}
	return d
}

// Backoff 返回第 failedAttempt 次尝试（1 起）临时失败后的等待时长。
func (p RetryPolicy) Backoff(failedAttempt int) time.Duration {
	if failedAttempt < 1 {
		failedAttempt = 1
	}
	d := float64(p.InitialBackoff) * math.Pow(p.Multiplier, float64(failedAttempt-1))
	if d > float64(p.MaxBackoff) {
		d = float64(p.MaxBackoff)
	}
	return time.Duration(d)
}

// MarshalJSON 以毫秒暴露退避时长。
func (p RetryPolicy) MarshalJSON() ([]byte, error) {
	type alias struct {
		MaxAttempts      int     `json:"max_attempts"`
		InitialBackoffMS int64   `json:"initial_backoff_ms"`
		MaxBackoffMS     int64   `json:"max_backoff_ms"`
		Multiplier       float64 `json:"multiplier"`
	}
	return json.Marshal(alias{
		MaxAttempts:      p.MaxAttempts,
		InitialBackoffMS: p.InitialBackoff.Milliseconds(),
		MaxBackoffMS:     p.MaxBackoff.Milliseconds(),
		Multiplier:       p.Multiplier,
	})
}

// UnmarshalJSON 接受以毫秒表示的退避时长。
func (p *RetryPolicy) UnmarshalJSON(data []byte) error {
	type alias struct {
		MaxAttempts      int     `json:"max_attempts"`
		InitialBackoffMS int64   `json:"initial_backoff_ms"`
		MaxBackoffMS     int64   `json:"max_backoff_ms"`
		Multiplier       float64 `json:"multiplier"`
	}
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	p.MaxAttempts = a.MaxAttempts
	p.InitialBackoff = time.Duration(a.InitialBackoffMS) * time.Millisecond
	p.MaxBackoff = time.Duration(a.MaxBackoffMS) * time.Millisecond
	p.Multiplier = a.Multiplier
	return nil
}

// Campaign 是活动聚合。
type Campaign struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	Status          CampaignStatus `json:"status"`
	TemplateVersion string         `json:"template_version,omitempty"`
	// Recipients 在 StartCampaign 时冻结，启动前为 nil。
	Recipients  []Recipient `json:"recipients,omitempty"`
	RetryPolicy RetryPolicy `json:"retry_policy"`
	CreatedAt   time.Time   `json:"created_at"`
	StartedAt   time.Time   `json:"started_at,omitempty"`
	CanceledAt  time.Time   `json:"canceled_at,omitempty"`
}

// CampaignSpec 是创建活动的入参。
type CampaignSpec struct {
	Name        string      `json:"name"`
	RetryPolicy RetryPolicy `json:"retry_policy,omitempty"`
}

// StartSpec 是启动活动的入参；模板版本与受众在此时锁定/冻结。
type StartSpec struct {
	TemplateVersion string      `json:"template_version"`
	Recipients      []Recipient `json:"recipients"`
}

// Task 是一个投递项的可变状态。投递幂等键 DispatchKey 由活动与收件人稳定派生。
type Task struct {
	CampaignID     string    `json:"campaign_id"`
	Recipient      string    `json:"recipient"`
	DispatchKey    string    `json:"dispatch_key"`
	State          TaskState `json:"state"`
	Attempt        int       `json:"attempt"`
	LeaseToken     string    `json:"lease_token,omitempty"`
	LeasedBy       string    `json:"leased_by,omitempty"`
	LeaseExpiresAt time.Time `json:"lease_expires_at,omitempty"`
	NextAttemptAt  time.Time `json:"next_attempt_at,omitempty"`
	AuthID         int64     `json:"auth_id,omitempty"`
	AuthorizedAt   time.Time `json:"authorized_at,omitempty"`
	// HeldByRule 非空时表示任务正被某条合规冻结规则持有；解除/失效后清空并回到 pending。
	HeldByRule    string    `json:"held_by_rule,omitempty"`
	FailureReason string    `json:"failure_reason,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// LeasedTask 是领取成功后返回给工作者的发送项视图。
type LeasedTask struct {
	DispatchKey     string            `json:"dispatch_key"`
	CampaignID      string            `json:"campaign_id"`
	Recipient       string            `json:"recipient"`
	TemplateVersion string            `json:"template_version"`
	Vars            map[string]string `json:"vars,omitempty"`
	Attempt         int               `json:"attempt"`
	LeaseToken      string            `json:"lease_token"`
	LeaseExpiresAt  time.Time         `json:"lease_expires_at"`
	// Basis 是本批新生成邮件携带的不可变保留依据；无匹配规则时为 nil。
	// 同一批次返回的所有任务 Basis 相同（无依据批次统一为 nil），避免新旧决定混批。
	Basis *RetentionBasis `json:"basis,omitempty"`
}

// AuthDecision 是持久化的“发送授权点”记录。
// 无论放行还是拒绝都会落库，作为抑制事件时序判定的边界。
type AuthDecision struct {
	ID           int64              `json:"id"`
	Attempt      int                `json:"attempt"`
	Granted      bool               `json:"granted"`
	At           time.Time          `json:"at"`
	Reason       string             `json:"reason,omitempty"`
	Suppressions []SuppressionEvent `json:"suppressions,omitempty"`
	// Basis 是授权时刻快照下来的保留依据；授权后规则变化不影响该决策。
	Basis *RetentionBasis `json:"basis,omitempty"`
}

// 授权拒绝原因码。
const (
	ReasonCampaignCanceled   = "campaign_canceled"
	ReasonCampaignSuppressed = "campaign_suppressed"
	ReasonGlobalUnsubscribe  = "global_unsubscribe"
	ReasonBounce             = "bounce"
	ReasonComplianceHeld     = "compliance_hold"
)

// ReceiptRecord 是某次尝试回执的持久化记录；Audit 保留授权点之后事件的解释性信息。
type ReceiptRecord struct {
	Attempt           int           `json:"attempt"`
	Result            ReceiptResult `json:"result"`
	ProviderMessageID string        `json:"provider_message_id,omitempty"`
	Code              string        `json:"code,omitempty"`
	Message           string        `json:"message,omitempty"`
	ReceivedAt        time.Time     `json:"received_at"`
	Audit             []string      `json:"audit,omitempty"`
}

// AttemptRecord 是一次租约尝试（attempt）的完整轨迹。
type AttemptRecord struct {
	Number         int            `json:"number"`
	WorkerID       string         `json:"worker_id"`
	Token          string         `json:"token"`
	LeasedAt       time.Time      `json:"leased_at"`
	LeaseExpiresAt time.Time      `json:"lease_expires_at"`
	Decision       *AuthDecision  `json:"decision,omitempty"`
	Outcome        *ReceiptRecord `json:"outcome,omitempty"`
}

// DispatchDetail 是一个投递项的审计视图。
type DispatchDetail struct {
	Task            Task      `json:"task"`
	CampaignID      string    `json:"campaign_id"`
	Recipient       Recipient `json:"recipient"`
	TemplateVersion string    `json:"template_version"`
	// CurrentBasis 是查询时刻仍对该收件人有效的保留依据（可能晚于已落库决策，仅作展示）。
	CurrentBasis *RetentionBasis  `json:"current_basis,omitempty"`
	Attempts     []*AttemptRecord `json:"attempts"`
}

// SuppressionEvent 是抑制事件；OccurredAt 为生效时间，可早于录入时间。
type SuppressionEvent struct {
	ID         int64           `json:"id"`
	Type       SuppressionType `json:"type"`
	Address    string          `json:"address,omitempty"`
	CampaignID string          `json:"campaign_id,omitempty"`
	Reason     string          `json:"reason,omitempty"`
	OccurredAt time.Time       `json:"occurred_at"`
	RecordedAt time.Time       `json:"recorded_at"`
}

// ReceiptInput 是回执入参。
type ReceiptInput struct {
	Result            ReceiptResult `json:"result"`
	ProviderMessageID string        `json:"provider_message_id,omitempty"`
	Code              string        `json:"code,omitempty"`
	Message           string        `json:"message,omitempty"`
}

// LeaseRequest 是领取请求。
type LeaseRequest struct {
	WorkerID      string        `json:"worker_id"`
	Count         int           `json:"count"`
	LeaseDuration time.Duration `json:"lease_duration"`
}

// ReceiptAck 是回执处理结果。Duplicated=true 表示该回执此前已处理，返回的是稳定的既有结论。
type ReceiptAck struct {
	DispatchKey string         `json:"dispatch_key"`
	Attempt     int            `json:"attempt"`
	State       TaskState      `json:"state"`
	Duplicate   bool           `json:"duplicate"`
	Outcome     *ReceiptRecord `json:"outcome,omitempty"`
}

// CampaignStats 是活动统计。所有数字由任务状态单一事实源派生，重复回执/取消不会重复累加。
type CampaignStats struct {
	CampaignID      string         `json:"campaign_id"`
	Status          CampaignStatus `json:"status"`
	TemplateVersion string         `json:"template_version"`
	Total           int            `json:"total"`
	Pending         int            `json:"pending"`
	Leased          int            `json:"leased"`
	Authorized      int            `json:"authorized"`
	RetryWait       int            `json:"retry_wait"`
	Sent            int            `json:"sent"`
	Failed          int            `json:"failed"`
	Suppressed      int            `json:"suppressed"`
	Canceled        int            `json:"canceled"`
	Held            int            `json:"held"`
	// TotalAttempts 是累计租约尝试次数（每次重试 +1）。
	TotalAttempts int `json:"total_attempts"`
	Terminal      int `json:"terminal"`
	// Finished 表示所有投递项都已进入终态（活动状态可能仍为 running）。
	Finished bool `json:"finished"`
}
