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
	TaskSent       TaskState = "sent"       // 终态：成功
	TaskFailed     TaskState = "failed"     // 终态：永久失败或重试耗尽
	TaskSuppressed TaskState = "suppressed" // 终态：授权点被抑制拦截
	TaskCanceled   TaskState = "canceled"   // 终态：活动取消，未再投递
	// TaskFrozen 非终态：授权点被合规保留规则（freeze）拦截；
	// 规则解除或失效后可重新领取并在授权点重新判断。
	TaskFrozen TaskState = "frozen"
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
	FailureReason  string    `json:"failure_reason,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
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
	// RuleEpoch 是领取时刻的合规规则纪元：同一批次只包含同一纪元的任务，
	// 规则变更（新增/解除）会推进纪元，已授权与待重判的邮件不会混批。
	RuleEpoch int64 `json:"rule_epoch"`
}

// RetentionAction 是合规保留规则的动作类型。
type RetentionAction string

const (
	// RetentionActionFreeze 冻结：命中收件人在授权点被拒（可解除后重判）。
	RetentionActionFreeze RetentionAction = "freeze"
	// RetentionActionRetain 保留：命中邮件的授权决策必须携带不可变发送依据。
	RetentionActionRetain RetentionAction = "retain"
)

// RetentionRule 是合规保留规则：按活动、收件人范围、原因与有效期保存。
// RuleNo 是调用方提供的规则号，作为活动内幂等键。
type RetentionRule struct {
	ID            int64           `json:"id"`
	RuleNo        string          `json:"rule_no"`
	CampaignID    string          `json:"campaign_id"`
	Action        RetentionAction `json:"action"`
	AllRecipients bool            `json:"all_recipients"`
	Addresses     []string        `json:"addresses,omitempty"`
	Reason        string          `json:"reason"`
	EffectiveAt   time.Time       `json:"effective_at"`
	ExpiresAt     time.Time       `json:"expires_at"`
	LiftedAt      time.Time       `json:"lifted_at,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
}

// Active 报告规则在 at 时刻是否生效（含边界：EffectiveAt <= at < ExpiresAt，且未解除）。
func (r *RetentionRule) Active(at time.Time) bool {
	return !r.EffectiveAt.After(at) && r.ExpiresAt.After(at) && r.LiftedAt.IsZero()
}

// Covers 报告规则是否覆盖指定（已规范化）地址。
func (r *RetentionRule) Covers(address string) bool {
	if r.AllRecipients {
		return true
	}
	for _, a := range r.Addresses {
		if a == address {
			return true
		}
	}
	return false
}

// samePayload 报告两条规则除规则号外的业务载荷是否一致（幂等判定用）。
func (r *RetentionRule) samePayload(o *RetentionRule) bool {
	if r.Action != o.Action || r.AllRecipients != o.AllRecipients ||
		r.Reason != o.Reason || !r.EffectiveAt.Equal(o.EffectiveAt) || !r.ExpiresAt.Equal(o.ExpiresAt) {
		return false
	}
	if len(r.Addresses) != len(o.Addresses) {
		return false
	}
	for i := range r.Addresses {
		if r.Addresses[i] != o.Addresses[i] {
			return false
		}
	}
	return true
}

// RetentionBasis 是随授权决策持久化的不可变发送依据快照。
type RetentionBasis struct {
	RuleNo          string          `json:"rule_no"`
	Action          RetentionAction `json:"action"`
	Reason          string          `json:"reason"`
	TemplateVersion string          `json:"template_version"`
	DecidedAt       time.Time       `json:"decided_at"`
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
	// RuleEpoch 是决策时的合规规则纪元。
	RuleEpoch int64 `json:"rule_epoch"`
	// Basis 是命中 retain 规则时随决策落库的不可变发送依据（含模板版本与保留原因）。
	Basis []RetentionBasis `json:"basis,omitempty"`
	// Retention 是命中 freeze 规则被拒时的规则快照。
	Retention *RetentionBasis `json:"retention,omitempty"`
}

// 授权拒绝原因码。
const (
	ReasonCampaignCanceled   = "campaign_canceled"
	ReasonCampaignSuppressed = "campaign_suppressed"
	ReasonGlobalUnsubscribe  = "global_unsubscribe"
	ReasonBounce             = "bounce"
	ReasonRetentionFrozen    = "retention_frozen"
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
	Task      Task             `json:"task"`
	Recipient Recipient        `json:"recipient"`
	Attempts  []*AttemptRecord `json:"attempts"`
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

// RetentionRuleInput 是提交合规保留规则的入参。
type RetentionRuleInput struct {
	RuleNo        string          `json:"rule_no"`
	Action        RetentionAction `json:"action"`
	AllRecipients bool            `json:"all_recipients,omitempty"`
	Addresses     []string        `json:"addresses,omitempty"`
	Reason        string          `json:"reason"`
	// EffectiveAt 生效时刻；留空取提交时刻。
	EffectiveAt time.Time `json:"effective_at,omitempty"`
	// ExpiresAt 失效时刻（必填，开区间边界）。
	ExpiresAt time.Time `json:"expires_at"`
}

// RetentionRuleAck 是规则提交结果。Duplicate=true 表示同规则号同载荷的重复提交，
// 返回的是首次落库的原记录。
type RetentionRuleAck struct {
	Rule      *RetentionRule `json:"rule"`
	Duplicate bool           `json:"duplicate"`
}

// RetentionRecipientView 是单个收件人的合规视图：
// 当前命中规则、采用的模板版本与保留/冻结原因。
type RetentionRecipientView struct {
	Address         string           `json:"address"`
	TemplateVersion string           `json:"template_version"`
	Frozen          bool             `json:"frozen"`
	Rules           []RetentionBasis `json:"rules,omitempty"`
}

// RetentionReport 是活动级合规保留查询结果。
type RetentionReport struct {
	CampaignID      string                   `json:"campaign_id"`
	Status          CampaignStatus           `json:"status"`
	TemplateVersion string                   `json:"template_version"`
	RuleEpoch       int64                    `json:"rule_epoch"`
	Rules           []RetentionRule          `json:"rules"`
	Recipients      []RetentionRecipientView `json:"recipients"`
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
	// Frozen 当前被合规保留规则冻结、等待重判的投递项数（非终态）。
	Frozen int `json:"frozen"`
	// TotalAttempts 是累计租约尝试次数（每次重试 +1）。
	TotalAttempts int `json:"total_attempts"`
	Terminal      int `json:"terminal"`
	// Finished 表示所有投递项都已进入终态（活动状态可能仍为 running）。
	Finished bool `json:"finished"`
}
