package emaildispatch

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// CampaignStatus 表示活动生命周期状态。
type CampaignStatus string

const (
	CampaignDraft     CampaignStatus = "draft"
	CampaignRunning   CampaignStatus = "running"
	CampaignCancelled CampaignStatus = "cancelled"
)

// DeliveryStatus 表示单个发送项的状态。
// sent / failed / suppressed / cancelled 均为终态。
type DeliveryStatus string

const (
	DeliveryPending    DeliveryStatus = "pending"
	DeliveryLeased     DeliveryStatus = "leased"
	DeliverySent       DeliveryStatus = "sent"
	DeliveryFailed     DeliveryStatus = "failed" // 永久失败终态
	DeliverySuppressed DeliveryStatus = "suppressed"
	DeliveryCancelled  DeliveryStatus = "cancelled"
)

// Terminal 报告状态是否为终态。
func (s DeliveryStatus) Terminal() bool {
	switch s {
	case DeliverySent, DeliveryFailed, DeliverySuppressed, DeliveryCancelled:
		return true
	}
	return false
}

// SuppressionType 抑制规则类型。
type SuppressionType string

const (
	// SuppressionGlobalUnsubscribe 全局退订：对该邮箱的所有投递均被拦截。
	SuppressionGlobalUnsubscribe SuppressionType = "global_unsubscribe"
	// SuppressionAddressBounce 地址退信：该邮箱被判定不可达。
	SuppressionAddressBounce SuppressionType = "address_bounce"
	// SuppressionCampaign 活动级抑制：仅拦截指定活动对该邮箱的投递。
	SuppressionCampaign SuppressionType = "campaign"
)

// Outcome 是工作者回执的发送结果。
type Outcome string

const (
	OutcomeSent             Outcome = "sent"
	OutcomeTransientFailure Outcome = "transient_failure"
	OutcomePermanentFailure Outcome = "permanent_failure"
)

// Recipient 目标收件人。
type Recipient struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// Template 邮件模板，Version 单调递增。
type Template struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
	Body    string `json:"body"`
}

// Campaign 批量投递活动。模板版本与收件人集合在启动时冻结。
type Campaign struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	TemplateID      string         `json:"template_id"`
	TemplateVersion int            `json:"template_version"` // 启动时锁定，草稿期为 0
	Status          CampaignStatus `json:"status"`
	Recipients      []Recipient    `json:"recipients"` // 启动时冻结
	CreatedAt       time.Time      `json:"created_at"`
	StartedAt       *time.Time     `json:"started_at,omitempty"`
	CancelledAt     *time.Time     `json:"cancelled_at,omitempty"`
}

// Lease 有期限的发送项租约，Token 作为栅栏令牌防止旧工作者回执覆盖新尝试。
type Lease struct {
	Token     string    `json:"token"`
	WorkerID  string    `json:"worker_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Authorization 持久化的“发送授权点”：授权时刻之前的抑制必须拦截投递，
// 授权之后到达的抑制不拦截，但会写入审计信息。
type Authorization struct {
	Attempt         int       `json:"attempt"`
	AuthorizedAt    time.Time `json:"authorized_at"`
	TemplateVersion int       `json:"template_version"`
}

// AuditEntry 发送项级别的审计记录。
type AuditEntry struct {
	At     time.Time `json:"at"`
	Event  string    `json:"event"`
	Detail string    `json:"detail"`
}

// Delivery 发送项。ID 即稳定幂等键，由活动与收件人组合派生。
type Delivery struct {
	ID            string         `json:"id"` // 幂等键：sha256(campaignID, recipientID)
	CampaignID    string         `json:"campaign_id"`
	RecipientID   string         `json:"recipient_id"`
	Email         string         `json:"email"`
	Status        DeliveryStatus `json:"status"`
	Attempts      int            `json:"attempts"`
	Lease         *Lease         `json:"lease,omitempty"`
	Authorization *Authorization `json:"authorization,omitempty"`
	NextAttemptAt time.Time      `json:"next_attempt_at"`
	LastReceiptID string         `json:"last_receipt_id,omitempty"`
	LastOutcome   Outcome        `json:"last_outcome,omitempty"`
	FailReason    string         `json:"fail_reason,omitempty"`
	Audit         []AuditEntry   `json:"audit,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

// Suppression 一条抑制事件。
type Suppression struct {
	ID          string          `json:"id"`
	Type        SuppressionType `json:"type"`
	Email       string          `json:"email"`
	CampaignID  string          `json:"campaign_id,omitempty"` // 仅活动级抑制使用
	Reason      string          `json:"reason"`
	EffectiveAt time.Time       `json:"effective_at"`
	RecordedAt  time.Time       `json:"recorded_at"`
}

// ClaimedItem 领取成功后返回给工作者的发送项视图。
type ClaimedItem struct {
	DeliveryID      string    `json:"delivery_id"`
	IdempotencyKey  string    `json:"idempotency_key"`
	Email           string    `json:"email"`
	TemplateID      string    `json:"template_id"`
	TemplateVersion int       `json:"template_version"`
	Attempt         int       `json:"attempt"`
	LeaseToken      string    `json:"lease_token"`
	LeaseExpiresAt  time.Time `json:"lease_expires_at"`
}

// ReceiptResult 回执处理结果；重复回执返回与首次一致的结果且 Duplicate 为 true。
type ReceiptResult struct {
	DeliveryID    string         `json:"delivery_id"`
	Status        DeliveryStatus `json:"status"`
	Attempt       int            `json:"attempt"`
	Duplicate     bool           `json:"duplicate"`
	NextAttemptAt *time.Time     `json:"next_attempt_at,omitempty"`
}

// Stats 活动统计，由发送项状态实时推导，杜绝重复累加。
type Stats struct {
	CampaignStatus CampaignStatus `json:"campaign_status"`
	Total          int            `json:"total"`
	Pending        int            `json:"pending"`
	Leased         int            `json:"leased"`
	Sent           int            `json:"sent"`
	Failed         int            `json:"failed"`
	Suppressed     int            `json:"suppressed"`
	Cancelled      int            `json:"cancelled"`
	Attempts       int            `json:"attempts"`
}

// IdempotencyKey 由活动与收件人组合生成稳定投递幂等键。
func IdempotencyKey(campaignID, recipientID string) string {
	sum := sha256.Sum256([]byte(campaignID + "\x00" + recipientID))
	return hex.EncodeToString(sum[:])
}
