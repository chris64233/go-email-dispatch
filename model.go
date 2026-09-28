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
	// CampaignPaused 已暂停：不再领取/授权新邮件；已授权邮件仍可完成并接收回执，可恢复。
	CampaignPaused CampaignStatus = "paused"
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
	TaskPaused     TaskState = "paused"     // 活动暂停：已领取但未获授权，在当前 attempt 的授权点被持久化拦截
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
	// PausedAt / ResumedAt 记录最近一次暂停/恢复时刻；同一活动可反复暂停与恢复。
	PausedAt   time.Time `json:"paused_at,omitempty"`
	ResumedAt  time.Time `json:"resumed_at,omitempty"`
	CanceledAt time.Time `json:"canceled_at,omitempty"`
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
}

// 授权拒绝原因码。
const (
	ReasonCampaignCanceled   = "campaign_canceled"
	ReasonCampaignPaused     = "campaign_paused"
	ReasonCampaignSuppressed = "campaign_suppressed"
	ReasonGlobalUnsubscribe  = "global_unsubscribe"
	ReasonBounce             = "bounce"
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
	Paused          int            `json:"paused"`
	Sent            int            `json:"sent"`
	Failed          int            `json:"failed"`
	Suppressed      int            `json:"suppressed"`
	Canceled        int            `json:"canceled"`
	// TotalAttempts 是累计租约尝试次数（每次重试 +1）。
	TotalAttempts int `json:"total_attempts"`
	// PauseIntercepted 是累计暂停拦截次数：暂停时刻在授权点被持久化拒绝的领取中任务数，
	// 每次暂停累加一次/任务（由授权决策派生，暂停-恢复-再暂停会增长）。
	PauseIntercepted int `json:"pause_intercepted"`
	Terminal         int `json:"terminal"`
	// Finished 表示所有投递项都已进入终态（活动状态可能仍为 running）。
	Finished bool `json:"finished"`
}
