package emaildispatch

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Config 控制租约时长、最大尝试次数与重试退避。
type Config struct {
	LeaseTTL    time.Duration // 单次领取租约时长
	MaxAttempts int           // 临时失败的最大尝试次数（含首次）
	// Backoff 给定第 n 次（从 1 开始）尝试后的退避时长。
	Backoff func(attempt int) time.Duration
	Now     func() time.Time // 可注入时钟，便于测试
}

// DefaultConfig 返回合理的默认配置。
func DefaultConfig() Config {
	return Config{
		LeaseTTL:    2 * time.Minute,
		MaxAttempts: 3,
		Backoff: func(attempt int) time.Duration {
			d := time.Duration(1<<uint(attempt-1)) * 30 * time.Second
			return d
		},
		Now: time.Now,
	}
}

// Service 是抑制感知的批量邮件投递服务。
// 所有状态流转都在 Store 的单次 Update 内原子完成，
// 因而“授权点 vs 抑制事件”的先后顺序由持久化写入顺序严格确定。
type Service struct {
	store Store
	cfg   Config
}

func NewService(store Store, cfg Config) *Service {
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = DefaultConfig().LeaseTTL
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = DefaultConfig().MaxAttempts
	}
	if cfg.Backoff == nil {
		cfg.Backoff = DefaultConfig().Backoff
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{store: store, cfg: cfg}
}

func (s *Service) now() time.Time { return s.cfg.Now().UTC() }

func newToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ---------- 模板 ----------

// PutTemplate 创建模板或发布新版本：相同 ID 时版本号自动 +1。
// 活动启动时锁定的是当时的版本号，后续新版本不影响已启动活动。
func (s *Service) PutTemplate(id, body string) (*Template, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("%w: template id is empty", ErrInvalidArgument)
	}
	var out *Template
	err := s.store.Update(func(st *state) error {
		t := &Template{ID: id, Body: body, Version: 1}
		if existing := st.Templates[id]; existing != nil {
			t.Version = existing.Version + 1
		}
		st.Templates[id] = t
		out = t
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetTemplate 查询模板（当前最新版本）。
func (s *Service) GetTemplate(id string) (*Template, error) {
	var out *Template
	s.store.Read(func(st *state) {
		if t := st.Templates[id]; t != nil {
			cp := *t
			out = &cp
		}
	})
	if out == nil {
		return nil, fmt.Errorf("%w: template %q", ErrNotFound, id)
	}
	return out, nil
}

// ---------- 活动 ----------

// CreateCampaign 创建草稿活动，须指定已存在的模板 ID。
func (s *Service) CreateCampaign(id, name, templateID string, recipients []Recipient) (*Campaign, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("%w: campaign id is empty", ErrInvalidArgument)
	}
	rcpts := dedupRecipients(recipients)
	var out *Campaign
	err := s.store.Update(func(st *state) error {
		if _, exists := st.Campaigns[id]; exists {
			return fmt.Errorf("%w: campaign %q already exists", ErrInvalidArgument, id)
		}
		if _, ok := st.Templates[templateID]; !ok {
			return fmt.Errorf("%w: template %q", ErrNotFound, templateID)
		}
		now := s.now()
		c := &Campaign{
			ID:         id,
			Name:       name,
			TemplateID: templateID,
			Status:     CampaignDraft,
			Recipients: append([]Recipient(nil), rcpts...),
			CreatedAt:  now,
		}
		st.Campaigns[id] = c
		out = c
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AddRecipients 向草稿活动追加收件人（按收件人 ID 去重）。
func (s *Service) AddRecipients(campaignID string, recipients []Recipient) error {
	return s.store.Update(func(st *state) error {
		c, err := mustCampaign(st, campaignID)
		if err != nil {
			return err
		}
		if c.Status != CampaignDraft {
			return fmt.Errorf("%w: campaign %q status %s", ErrCampaignNotDraft, campaignID, c.Status)
		}
		c.Recipients = dedupRecipients(append(c.Recipients, recipients...))
		return nil
	})
}

// StartCampaign 启动活动：锁定当前模板版本、冻结收件人集合，并为每个收件人
// 生成稳定幂等键对应的发送项。重复启动返回 ErrCampaignNotDraft。
func (s *Service) StartCampaign(campaignID string) (*Campaign, error) {
	var out *Campaign
	err := s.store.Update(func(st *state) error {
		c, err := mustCampaign(st, campaignID)
		if err != nil {
			return err
		}
		if c.Status != CampaignDraft {
			return fmt.Errorf("%w: campaign %q status %s", ErrCampaignNotDraft, campaignID, c.Status)
		}
		tpl := st.Templates[c.TemplateID]
		if tpl == nil {
			return fmt.Errorf("%w: template %q vanished", ErrNotFound, c.TemplateID)
		}
		now := s.now()
		c.TemplateVersion = tpl.Version
		c.Status = CampaignRunning
		c.StartedAt = &now

		ids := make([]string, 0, len(c.Recipients))
		for _, r := range c.Recipients {
			key := IdempotencyKey(c.ID, r.ID)
			if _, exists := st.Deliveries[key]; exists {
				continue
			}
			d := &Delivery{
				ID:          key,
				CampaignID:  c.ID,
				RecipientID: r.ID,
				Email:       r.Email,
				Status:      DeliveryPending,
				CreatedAt:   now,
				UpdatedAt:   now,
			}
			st.Deliveries[key] = d
			ids = append(ids, key)
		}
		st.CampaignDelivery[c.ID] = append(st.CampaignDelivery[c.ID], ids...)
		out = c
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetCampaign 查询活动（含冻结快照）。
func (s *Service) GetCampaign(campaignID string) (*Campaign, error) {
	var out *Campaign
	s.store.Read(func(st *state) {
		if c := st.Campaigns[campaignID]; c != nil {
			cp := *c
			cp.Recipients = append([]Recipient(nil), c.Recipients...)
			out = &cp
		}
	})
	if out == nil {
		return nil, fmt.Errorf("%w: campaign %q", ErrNotFound, campaignID)
	}
	return out, nil
}

// CancelCampaign 取消活动。取消后不再授权任何新邮件；尚未发送的发送项进入
// cancelled 终态。重复取消是幂等操作：返回当前状态且不产生任何状态变更。
func (s *Service) CancelCampaign(campaignID string) (*Campaign, error) {
	var out *Campaign
	err := s.store.Update(func(st *state) error {
		c, err := mustCampaign(st, campaignID)
		if err != nil {
			return err
		}
		now := s.now()
		if c.Status == CampaignCancelled {
			out = c // 幂等：重复取消直接返回稳定结果
			return nil
		}
		c.Status = CampaignCancelled
		c.CancelledAt = &now
		for _, id := range st.CampaignDelivery[c.ID] {
			d := st.Deliveries[id]
			// 终态发送项保持不变，统计因此不会被重复改动。
			if d.Status.Terminal() {
				continue
			}
			switch {
			case d.Status == DeliveryPending:
				// 尚未授权：直接取消。
				d.Status = DeliveryCancelled
				d.NextAttemptAt = time.Time{}
				d.UpdatedAt = now
				d.Audit = append(d.Audit, AuditEntry{At: now, Event: "cancelled", Detail: "campaign cancelled before send authorization"})
			case d.Status == DeliveryLeased && (d.Lease == nil || !d.Lease.ExpiresAt.After(now)):
				// 租约已过期：不会再有有效回执，直接收口为终态。
				d.Status = DeliveryCancelled
				d.Lease = nil
				d.UpdatedAt = now
				d.Audit = append(d.Audit, AuditEntry{At: now, Event: "cancelled", Detail: "campaign cancelled after lease expired"})
			default:
				// 已授权且租约有效：不再授权新邮件，但允许在途回执落地。
				d.Audit = append(d.Audit, AuditEntry{At: now, Event: "cancel_requested", Detail: "campaign cancelled while delivery in flight"})
			}
		}
		out = c
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SweepExpiredLeases 处理已过期但尚未收口的租约：
// 运行中的活动把发送项退回 pending，等待下次领取重新授权；
// 已取消的活动把它们收口为 cancelled 终态。
// 返回发生状态变更的发送项 ID 列表。可由定时器周期调用。
func (s *Service) SweepExpiredLeases(campaignID string) ([]string, error) {
	var changed []string
	err := s.store.Update(func(st *state) error {
		c, err := mustCampaign(st, campaignID)
		if err != nil {
			return err
		}
		now := s.now()
		for _, id := range st.CampaignDelivery[c.ID] {
			d := st.Deliveries[id]
			if d.Status != DeliveryLeased || d.Lease == nil || d.Lease.ExpiresAt.After(now) {
				continue
			}
			d.Lease = nil
			d.UpdatedAt = now
			if c.Status == CampaignCancelled {
				d.Status = DeliveryCancelled
				d.Audit = append(d.Audit, AuditEntry{At: now, Event: "cancelled", Detail: "lease expired after campaign cancellation"})
			} else {
				d.Status = DeliveryPending
				d.Audit = append(d.Audit, AuditEntry{At: now, Event: "lease_expired", Detail: "lease expired without receipt; awaiting re-claim"})
			}
			changed = append(changed, id)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return changed, nil
}

// ---------- 抑制事件 ----------

// SuppressionInput 登记抑制事件的参数。EffectiveAt 为零值时取当前时刻。
type SuppressionInput struct {
	Type        SuppressionType
	Email       string
	CampaignID  string // 仅 SuppressionCampaign 需要
	Reason      string
	EffectiveAt time.Time
}

// AddSuppression 持久化一条抑制事件。
// 它与“领取授权”“发送回执”在同一个串行写入点排序，因此授权点前后的
// 抑制归属是确定的：授权之前生效的拦截投递，授权之后的只写审计。
func (s *Service) AddSuppression(in SuppressionInput) (*Suppression, error) {
	if in.Type != SuppressionGlobalUnsubscribe && in.Type != SuppressionAddressBounce && in.Type != SuppressionCampaign {
		return nil, fmt.Errorf("%w: suppression type %q", ErrInvalidArgument, in.Type)
	}
	if strings.TrimSpace(in.Email) == "" {
		return nil, fmt.Errorf("%w: email is empty", ErrInvalidArgument)
	}
	if in.Type == SuppressionCampaign && strings.TrimSpace(in.CampaignID) == "" {
		return nil, fmt.Errorf("%w: campaign suppression requires campaign id", ErrInvalidArgument)
	}
	var out *Suppression
	err := s.store.Update(func(st *state) error {
		now := s.now()
		eff := in.EffectiveAt.UTC()
		if eff.IsZero() {
			eff = now
		}
		st.Seq++
		sup := &Suppression{
			ID:          fmt.Sprintf("sup-%d-%s", st.Seq, newToken()[:8]),
			Type:        in.Type,
			Email:       normalizeEmail(in.Email),
			CampaignID:  in.CampaignID,
			Reason:      in.Reason,
			EffectiveAt: eff,
			RecordedAt:  now,
		}
		st.Suppressions = append(st.Suppressions, sup)
		out = sup
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// suppressionBlocks 判断在 at 时刻，抑制列表是否拦截该发送项。
func suppressionBlocks(st *state, d *Delivery, at time.Time) *Suppression {
	for i := len(st.Suppressions) - 1; i >= 0; i-- {
		sup := st.Suppressions[i]
		if sup.EffectiveAt.After(at) {
			continue // 授权时刻尚未生效
		}
		if sup.Email != d.Email {
			continue
		}
		switch sup.Type {
		case SuppressionGlobalUnsubscribe, SuppressionAddressBounce:
			return sup
		case SuppressionCampaign:
			if sup.CampaignID == d.CampaignID {
				return sup
			}
		}
	}
	return nil
}

// lateSuppressions 返回授权点之后才生效的相关抑制（仅用于审计解释）。
func lateSuppressions(st *state, d *Delivery) []*Suppression {
	var out []*Suppression
	for _, sup := range st.Suppressions {
		if sup.Email != d.Email {
			continue
		}
		if !sup.EffectiveAt.After(d.Authorization.AuthorizedAt) {
			continue
		}
		if sup.Type == SuppressionCampaign && sup.CampaignID != d.CampaignID {
			continue
		}
		out = append(out, sup)
	}
	return out
}

// ---------- 领取 / 授权 ----------

// Claim 从活动中领取至多 maxItems 个可发送项。
// 每一次领取都会落一个持久化的“发送授权点”：授权之前已生效的全局退订、
// 地址退信、活动级抑制直接把发送项判为 suppressed 终态；活动已取消则判为
// cancelled。过期租约会被重新领取并产生新的栅栏令牌，旧工作者的回执随之失效。
func (s *Service) Claim(campaignID, workerID string, maxItems int) ([]ClaimedItem, error) {
	if strings.TrimSpace(workerID) == "" {
		return nil, fmt.Errorf("%w: worker id is empty", ErrInvalidArgument)
	}
	if maxItems <= 0 {
		maxItems = 1
	}
	var out []ClaimedItem
	err := s.store.Update(func(st *state) error {
		c, err := mustCampaign(st, campaignID)
		if err != nil {
			return err
		}
		if c.Status != CampaignRunning {
			return fmt.Errorf("%w: campaign %q status %s", ErrCampaignNotActive, campaignID, c.Status)
		}
		now := s.now()
		for _, id := range st.CampaignDelivery[c.ID] {
			if len(out) >= maxItems {
				break
			}
			d := st.Deliveries[id]
			reclaim := false
			switch d.Status {
			case DeliveryPending:
				if !d.NextAttemptAt.IsZero() && d.NextAttemptAt.After(now) {
					continue // 退避窗口未到
				}
			case DeliveryLeased:
				if d.Lease == nil || d.Lease.ExpiresAt.After(now) {
					continue // 租约仍有效
				}
				reclaim = true // 租约过期：旧尝试作废
			default:
				continue
			}
			// 授权前最后一次抑制复查。
			if sup := suppressionBlocks(st, d, now); sup != nil {
				d.Status = DeliverySuppressed
				d.Lease = nil
				d.NextAttemptAt = time.Time{}
				d.UpdatedAt = now
				d.FailReason = sup.Reason
				d.Audit = append(d.Audit, AuditEntry{
					At: now, Event: "suppressed",
					Detail: fmt.Sprintf("blocked before authorization by %s %s: %s", sup.Type, sup.ID, sup.Reason),
				})
				continue
			}
			// 持久化发送授权点。
			d.Attempts++
			d.Status = DeliveryLeased
			d.Authorization = &Authorization{
				Attempt:         d.Attempts,
				AuthorizedAt:    now,
				TemplateVersion: c.TemplateVersion,
			}
			d.Lease = &Lease{Token: newToken(), WorkerID: workerID, ExpiresAt: now.Add(s.cfg.LeaseTTL)}
			d.NextAttemptAt = time.Time{}
			d.UpdatedAt = now
			if reclaim {
				d.Audit = append(d.Audit, AuditEntry{
					At: now, Event: "lease_expired_reclaimed",
					Detail: fmt.Sprintf("previous attempt %d lease expired; new worker %s", d.Attempts-1, workerID),
				})
			}
			d.Audit = append(d.Audit, AuditEntry{
				At: now, Event: "authorized",
				Detail: fmt.Sprintf("attempt %d authorized for worker %s, template v%d", d.Attempts, workerID, c.TemplateVersion),
			})
			out = append(out, ClaimedItem{
				DeliveryID:      d.ID,
				IdempotencyKey:  d.ID,
				Email:           d.Email,
				TemplateID:      c.TemplateID,
				TemplateVersion: c.TemplateVersion,
				Attempt:         d.Attempts,
				LeaseToken:      d.Lease.Token,
				LeaseExpiresAt:  d.Lease.ExpiresAt,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---------- 回执 ----------

// Receipt 工作者对一次已授权发送的回执。
//
// 租约令牌是栅栏：过期租约对应的旧工作者回执返回 ErrStaleLease，
// 绝不覆盖新尝试的状态。相同 receiptID 的重复回执返回与首次处理
// 完全一致的稳定结果（Duplicate=true），统计不会被重复累加。
func (s *Service) Receipt(campaignID, deliveryID, leaseToken, receiptID string, outcome Outcome, detail string) (*ReceiptResult, error) {
	if outcome != OutcomeSent && outcome != OutcomeTransientFailure && outcome != OutcomePermanentFailure {
		return nil, fmt.Errorf("%w: outcome %q", ErrInvalidOutcome, outcome)
	}
	if strings.TrimSpace(leaseToken) == "" || strings.TrimSpace(receiptID) == "" {
		return nil, fmt.Errorf("%w: lease token and receipt id are required", ErrInvalidArgument)
	}
	var out *ReceiptResult
	err := s.store.Update(func(st *state) error {
		c, err := mustCampaign(st, campaignID)
		if err != nil {
			return err
		}
		d, ok := st.Deliveries[deliveryID]
		if !ok || d.CampaignID != campaignID {
			return fmt.Errorf("%w: delivery %q", ErrNotFound, deliveryID)
		}
		// 重复回执：返回首次处理的稳定结果，不再产生任何状态变更。
		if r, dup := st.ReceiptResults[receiptID]; dup {
			cp := *r
			cp.Duplicate = true
			out = &cp
			return nil
		}
		now := s.now()
		// 栅栏校验：只有持有当前有效租约的工作者能写入结果。
		if d.Status.Terminal() {
			return fmt.Errorf("%w: delivery %q status %s", ErrDeliveryTerminal, deliveryID, d.Status)
		}
		if d.Lease == nil || d.Status != DeliveryLeased {
			return fmt.Errorf("%w: delivery %q status %s", ErrDeliveryNotLeased, deliveryID, d.Status)
		}
		if d.Lease.Token != leaseToken {
			return fmt.Errorf("%w: delivery %q lease %q", ErrStaleLease, deliveryID, leaseToken)
		}
		if !d.Lease.ExpiresAt.After(now) {
			// 租约已到期：即使令牌曾经有效，旧工作者也不得再写入。
			return fmt.Errorf("%w: delivery %q lease expired at %s", ErrStaleLease, deliveryID, d.Lease.ExpiresAt.Format(time.RFC3339))
		}
		d.LastReceiptID = receiptID
		d.LastOutcome = outcome
		d.UpdatedAt = now

		res := &ReceiptResult{DeliveryID: d.ID, Attempt: d.Attempts}

		switch outcome {
		case OutcomeSent:
			d.Status = DeliverySent
			d.FailReason = ""
			d.Lease = nil
			d.Audit = append(d.Audit, AuditEntry{At: now, Event: "sent", Detail: detail})
			res.Status = DeliverySent
		case OutcomePermanentFailure:
			d.Status = DeliveryFailed
			d.FailReason = detail
			d.Lease = nil
			d.Audit = append(d.Audit, AuditEntry{At: now, Event: "permanent_failure", Detail: detail})
			res.Status = DeliveryFailed
		case OutcomeTransientFailure:
			if d.Attempts >= s.cfg.MaxAttempts {
				d.Status = DeliveryFailed
				d.FailReason = fmt.Sprintf("retries exhausted after %d attempts: %s", d.Attempts, detail)
				d.Lease = nil
				d.Audit = append(d.Audit, AuditEntry{At: now, Event: "retries_exhausted", Detail: d.FailReason})
				res.Status = DeliveryFailed
			} else {
				next := now.Add(s.cfg.Backoff(d.Attempts))
				d.Status = DeliveryPending
				d.Lease = nil
				d.NextAttemptAt = next
				d.Audit = append(d.Audit, AuditEntry{
					At: now, Event: "transient_failure",
					Detail: fmt.Sprintf("%s; retry scheduled at %s", detail, next.Format(time.RFC3339)),
				})
				res.Status = DeliveryPending
				res.NextAttemptAt = &next
			}
		}

		// 授权点之后生效的抑制不改变本次结果（邮件可能已经发出），
		// 但保留可解释的审计痕迹。
		for _, sup := range lateSuppressions(st, d) {
			d.Audit = append(d.Audit, AuditEntry{
				At: now, Event: "late_suppression",
				Detail: fmt.Sprintf("suppression %s (%s) became effective after authorization at %s; receipt outcome retained",
					sup.ID, sup.Type, d.Authorization.AuthorizedAt.Format(time.RFC3339)),
			})
		}
		// 授权之后活动才被取消：本次发送在授权点是合法的，结果照常落地，仅补审计。
		if c.Status == CampaignCancelled && c.CancelledAt != nil && d.Authorization != nil &&
			c.CancelledAt.After(d.Authorization.AuthorizedAt) && outcome == OutcomeSent {
			d.Audit = append(d.Audit, AuditEntry{
				At: now, Event: "sent_after_cancel",
				Detail: fmt.Sprintf("send was authorized at %s before campaign cancellation at %s",
					d.Authorization.AuthorizedAt.Format(time.RFC3339), c.CancelledAt.Format(time.RFC3339)),
			})
		}

		st.ReceiptResults[receiptID] = res
		cp := *res
		out = &cp
		_ = c
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---------- 查询 ----------

// GetDelivery 查询单个发送项（返回副本）。
func (s *Service) GetDelivery(campaignID, deliveryID string) (*Delivery, error) {
	var out *Delivery
	s.store.Read(func(st *state) {
		if d := st.Deliveries[deliveryID]; d != nil && d.CampaignID == campaignID {
			cp := *d
			out = &cp
		}
	})
	if out == nil {
		return nil, fmt.Errorf("%w: delivery %q", ErrNotFound, deliveryID)
	}
	return out, nil
}

// Stats 由发送项当前状态实时推导统计，重复回执、重复取消都不会重复计数。
func (s *Service) Stats(campaignID string) (*Stats, error) {
	out := &Stats{}
	found := false
	s.store.Read(func(st *state) {
		c, ok := st.Campaigns[campaignID]
		if !ok {
			return
		}
		found = true
		out.CampaignStatus = c.Status
		for _, id := range st.CampaignDelivery[campaignID] {
			d := st.Deliveries[id]
			out.Total++
			out.Attempts += d.Attempts
			switch d.Status {
			case DeliveryPending:
				out.Pending++
			case DeliveryLeased:
				out.Leased++
			case DeliverySent:
				out.Sent++
			case DeliveryFailed:
				out.Failed++
			case DeliverySuppressed:
				out.Suppressed++
			case DeliveryCancelled:
				out.Cancelled++
			}
		}
	})
	if !found {
		return nil, fmt.Errorf("%w: campaign %q", ErrNotFound, campaignID)
	}
	return out, nil
}

// ---------- 内部辅助 ----------

func mustCampaign(st *state, id string) (*Campaign, error) {
	c, ok := st.Campaigns[id]
	if !ok {
		return nil, fmt.Errorf("%w: campaign %q", ErrNotFound, id)
	}
	return c, nil
}

func dedupRecipients(in []Recipient) []Recipient {
	seen := map[string]bool{}
	out := make([]Recipient, 0, len(in))
	for _, r := range in {
		if strings.TrimSpace(r.ID) == "" {
			continue
		}
		if seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		r.Email = normalizeEmail(r.Email)
		out = append(out, r)
	}
	return out
}

func normalizeEmail(e string) string {
	return strings.ToLower(strings.TrimSpace(e))
}
