package emaildispatch

import (
	"strings"
	"time"
)

// 默认领取参数。
const (
	DefaultLeaseCount    = 1
	MaxLeaseCount        = 100
	DefaultLeaseDuration = 30 * time.Second
	MinLeaseDuration     = time.Second
)

const opService = "service"

// Service 是投递领域的应用服务，所有接口都经由它进入持久化事务。
type Service struct {
	store Store
	now   func() time.Time
}

// NewService 使用默认时钟构造服务。
func NewService(store Store) *Service {
	return &Service{store: store, now: time.Now}
}

// NewServiceWithClock 注入时钟，供测试精确控制租约过期、退避与事件时序。
func NewServiceWithClock(store Store, now func() time.Time) *Service {
	return &Service{store: store, now: now}
}

// CreateCampaign 创建草稿活动（模板版本与受众此时尚未确定）。
func (svc *Service) CreateCampaign(spec CampaignSpec) (*Campaign, error) {
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return nil, newError(opService, ErrValidation, "campaign name is required")
	}
	c := &Campaign{
		ID:          "cmp_" + newID(),
		Name:        name,
		Status:      CampaignDraft,
		RetryPolicy: spec.RetryPolicy.withDefaults(),
		CreatedAt:   svc.now().UTC(),
	}
	if err := svc.store.InsertCampaign(c); err != nil {
		return nil, err
	}
	return c, nil
}

// GetCampaign 返回活动（含冻结后的模板版本与受众快照）。
func (svc *Service) GetCampaign(id string) (*Campaign, error) {
	return svc.store.GetCampaign(id)
}

// StartCampaign 启动活动：模板版本在此锁定，受众集合在此冻结，投递项随后生成。
// 重复启动返回 ErrConflict；活动启动后模板与受众不可再变。
func (svc *Service) StartCampaign(id string, spec StartSpec) (*Campaign, error) {
	tv := strings.TrimSpace(spec.TemplateVersion)
	if tv == "" {
		return nil, newError(opService, ErrValidation, "template_version is required to lock at start")
	}
	if len(spec.Recipients) == 0 {
		return nil, newError(opService, ErrValidation, "at least one recipient is required")
	}

	seen := make(map[string]struct{}, len(spec.Recipients))
	rcpts := make([]Recipient, 0, len(spec.Recipients))
	for _, r := range spec.Recipients {
		addr := normalizeAddress(r.Address)
		if addr == "" || !strings.Contains(addr, "@") {
			return nil, newError(opService, ErrValidation, "invalid recipient address %q", r.Address)
		}
		if _, dup := seen[addr]; dup {
			return nil, newError(opService, ErrValidation, "duplicate recipient address %q", addr)
		}
		seen[addr] = struct{}{}
		rcpts = append(rcpts, Recipient{Address: addr, Vars: cloneStringMap(r.Vars)})
	}

	c, err := svc.store.GetCampaign(id)
	if err != nil {
		return nil, err
	}
	if c.Status != CampaignDraft {
		return nil, newError(opService, ErrConflict, "campaign %s is %s and cannot be started again", id, c.Status)
	}
	if err := svc.store.StartCampaign(c, tv, rcpts, svc.now().UTC()); err != nil {
		return nil, err
	}
	return svc.store.GetCampaign(id)
}

// Lease 领取至多 req.Count 个就绪发送项（含首次 pending、退避到期 retry_wait、失联租约）。
// 每个发送项写入新的有期限租约；同一 dispatchKey 的重复领取只会在旧租约过期后发生，并产生新 attempt。
// 当前没有就绪项时返回空切片与 nil 错误，调用方可稍后轮询。
func (svc *Service) Lease(campaignID string, req LeaseRequest) ([]LeasedTask, error) {
	if _, err := svc.store.GetCampaign(campaignID); err != nil {
		return nil, err
	}
	worker := strings.TrimSpace(req.WorkerID)
	if worker == "" {
		return nil, newError(opService, ErrValidation, "worker_id is required")
	}
	count := req.Count
	if count <= 0 {
		count = DefaultLeaseCount
	}
	if count > MaxLeaseCount {
		count = MaxLeaseCount
	}
	dur := req.LeaseDuration
	if dur <= 0 {
		dur = DefaultLeaseDuration
	}
	if dur < MinLeaseDuration {
		dur = MinLeaseDuration
	}
	return svc.store.LeaseTasks(campaignID, worker, count, dur, svc.now().UTC())
}

// Authorize 执行持久化发送授权点。工作者真正调用邮件提供商之前必须调用本方法：
//   - 决策先落库再返回，授权以前已生效的全局退订/地址退信/活动抑制/活动取消一律拦截；
//   - 重复调用返回同一持久化决策（稳定结果）；
//   - token 属于过期或被取代的租约时返回 ErrLeaseExpired / ErrStaleAttempt。
func (svc *Service) Authorize(campaignID, dispatchKey, leaseToken string) (*AuthDecision, error) {
	if err := validateKeyRef(campaignID, dispatchKey, leaseToken); err != nil {
		return nil, err
	}
	return svc.store.Authorize(dispatchKey, leaseToken, svc.now().UTC())
}

// SubmitReceipt 提交发送回执。只有在授权点放行的当前 attempt 接受回执：
// 过期租约的旧工作者回执返回 ErrStaleAttempt，绝不覆盖新尝试；
// 重复回执返回 Duplicate=true 与首次处理时的稳定结论，统计不重复累加。
func (svc *Service) SubmitReceipt(campaignID, dispatchKey, leaseToken string, in ReceiptInput) (ReceiptAck, error) {
	if err := validateKeyRef(campaignID, dispatchKey, leaseToken); err != nil {
		return ReceiptAck{}, err
	}
	switch in.Result {
	case ResultSuccess, ResultTemporaryFailure, ResultPermanentFailure:
	default:
		return ReceiptAck{}, newError(opService, ErrInvalidReceipt,
			"result must be one of success|temporary_failure|permanent_failure, got %q", in.Result)
	}
	return svc.store.CompleteReceipt(dispatchKey, leaseToken, in, svc.now().UTC())
}

// SuppressionInput 是录入抑制事件的入参。
type SuppressionInput struct {
	Type       SuppressionType `json:"type"`
	Address    string          `json:"address"`
	CampaignID string          `json:"campaign_id,omitempty"`
	Reason     string          `json:"reason,omitempty"`
	// OccurredAt 为抑制实际生效时间；留空则取录入时间。
	// 允许回填早于当前时间的事件（如下游退信 webhook 延迟到达）。
	OccurredAt time.Time `json:"occurred_at,omitempty"`
}

// RecordSuppression 持久化抑制事件。事件对“授权点时刻”生效：
// OccurredAt 早于某次授权的尝试会在授权点被拦截，晚于授权的只进入审计轨迹。
func (svc *Service) RecordSuppression(in SuppressionInput) (*SuppressionEvent, error) {
	now := svc.now().UTC()
	ev := &SuppressionEvent{
		Type:       in.Type,
		Reason:     strings.TrimSpace(in.Reason),
		OccurredAt: in.OccurredAt.UTC(),
		RecordedAt: now,
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = now
	}

	switch in.Type {
	case SuppressionGlobalUnsubscribe, SuppressionBounce:
		ev.Address = normalizeAddress(in.Address)
		if ev.Address == "" {
			return nil, newError(opService, ErrValidation, "address is required for suppression %q", in.Type)
		}
	case SuppressionCampaign:
		ev.CampaignID = strings.TrimSpace(in.CampaignID)
		ev.Address = normalizeAddress(in.Address)
		if ev.CampaignID == "" || ev.Address == "" {
			return nil, newError(opService, ErrValidation,
				"campaign_id and address are required for campaign_suppression")
		}
		if _, err := svc.store.GetCampaign(ev.CampaignID); err != nil {
			return nil, err
		}
	default:
		return nil, newError(opService, ErrValidation,
			"type must be one of global_unsubscribe|bounce|campaign_suppression, got %q", in.Type)
	}

	if err := svc.store.RecordSuppression(ev); err != nil {
		return nil, err
	}
	return ev, nil
}

// Cancel 取消活动。取消后不再授权任何新邮件；重复取消返回稳定结果（Changed=false），幂等。
func (svc *Service) Cancel(campaignID string) (*Campaign, bool, error) {
	c, changed, err := svc.store.CancelCampaign(campaignID, svc.now().UTC())
	if err != nil {
		return nil, false, err
	}
	return c, changed, nil
}

// Stats 返回活动统计；计数全部由任务持久化状态派生，重复回执/取消不会重复累加。
func (svc *Service) Stats(campaignID string) (CampaignStats, error) {
	return svc.store.Stats(campaignID)
}

// GetDispatch 返回单个投递项的完整审计轨迹（租约尝试、授权点决策、回执与授权后审计）。
func (svc *Service) GetDispatch(campaignID, dispatchKey string) (*DispatchDetail, error) {
	return svc.store.GetDispatch(campaignID, dispatchKey)
}

func validateKeyRef(campaignID, dispatchKey, leaseToken string) error {
	if strings.TrimSpace(campaignID) == "" {
		return newError(opService, ErrValidation, "campaign_id is required")
	}
	if !strings.HasPrefix(dispatchKey, "dk_") {
		return newError(opService, ErrValidation, "invalid dispatch_key")
	}
	if !strings.HasPrefix(leaseToken, "lease_") {
		return newError(opService, ErrValidation, "invalid lease_token")
	}
	return nil
}
