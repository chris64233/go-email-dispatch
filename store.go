package emaildispatch

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"sync"
	"time"
)

// DispatchKey 由活动与收件人地址稳定派生：同一组合在任意租约/重试下幂等键不变。
func DispatchKey(campaignID, address string) string {
	sum := sha256.Sum256([]byte(campaignID + "\x00" + normalizeAddress(address)))
	return "dk_" + hex.EncodeToString(sum[:16])
}

// Store 是持久化事务边界。每个方法都必须原子地完成状态迁移与审计落库；
// 内存实现 MemoryStore 满足这一语义，SQL 实现应在单事务内完成同样的读写集合。
type Store interface {
	InsertCampaign(c *Campaign) error
	GetCampaign(id string) (*Campaign, error)
	// StartCampaign 冻结模板版本与受众，并为每个收件人创建 pending 投递项。
	StartCampaign(c *Campaign, templateVersion string, recipients []Recipient, at time.Time) error
	CancelCampaign(id string, at time.Time) (*Campaign, bool, error)

	RecordSuppression(ev *SuppressionEvent) error
	// SuppressionsRecordedSince 返回 after 之后录入、且匹配活动/地址的抑制事件（用于授权后审计）。
	SuppressionsFor(campaignID, address string) []SuppressionEvent

	// UpsertRetentionRule 幂等提交合规保留规则：相同规则号且内容一致返回既有记录（created=false）；
	// 相同规则号但范围/动作/原因/有效期变化返回 ErrConflict。
	UpsertRetentionRule(rule *RetentionRule, now time.Time) (created bool, err error)
	// ReleaseRetentionRule 解除规则（幂等：重复解除返回 changed=false）；冻结规则解除后其 held 项回到 pending。
	ReleaseRetentionRule(campaignID, ruleID string, at time.Time) (*RetentionRule, bool, error)
	// RetentionRule 读取单条规则；缺失返回 ErrNotFound。
	RetentionRule(campaignID, ruleID string, now time.Time) (*RetentionRule, error)
	// RetentionRules 列出活动的全部规则（含已解除/已失效），按创建顺序返回。
	RetentionRules(campaignID string, now time.Time) ([]RetentionRule, error)
	// RetentionBasisFor 返回 at 时刻对该收件人有效的保留依据快照（freeze 优先于 retain）；无匹配返回 nil。
	RetentionBasisFor(campaignID, address string, at time.Time) *RetentionBasis

	// LeaseTasks 原子领取至多 count 个就绪项，写入新租约与 attempt 记录。
	LeaseTasks(campaignID string, workerID string, count int, dur time.Duration, now time.Time) ([]LeasedTask, error)
	// Authorize 是持久化发送授权点：决策（无论放行/拒绝）先落库再返回。
	Authorize(key, token string, now time.Time) (*AuthDecision, error)
	// CompleteReceipt 落库回执并迁移任务状态；重复 attempt 回执返回既有稳定结论。
	CompleteReceipt(key, token string, in ReceiptInput, now time.Time) (ReceiptAck, error)

	Stats(campaignID string) (CampaignStats, error)
	GetDispatch(campaignID, key string, now time.Time) (*DispatchDetail, error)
}

// taskRow 汇总一个投递项的全部持久化状态。
type taskRow struct {
	task      Task
	recipient Recipient
	attempts  map[int]*AttemptRecord
}

type memoryStore struct {
	mu sync.Mutex

	campaigns  map[string]*Campaign
	recipients map[string]map[string]Recipient // campaignID -> address -> Recipient
	taskOrder  map[string][]string             // campaignID -> 有序 dispatchKey 列表
	tasks      map[string]*taskRow             // dispatchKey -> row

	// globalSupp: address -> 事件（全局退订/地址退信，跨活动生效）
	globalSupp map[string][]*SuppressionEvent
	// campaignSupp: campaignID -> address -> 事件
	campaignSupp map[string]map[string][]*SuppressionEvent
	// retentionRules: campaignID -> ruleID -> 规则
	retentionRules map[string]map[string]*RetentionRule
	ruleOrder      map[string][]string

	nextEventID int64
	nextAuthID  int64
}

// NewMemoryStore 返回进程内持久化实现（串行化所有事务，适合单节点与测试）。
func NewMemoryStore() Store {
	return &memoryStore{
		campaigns:      map[string]*Campaign{},
		recipients:     map[string]map[string]Recipient{},
		taskOrder:      map[string][]string{},
		tasks:          map[string]*taskRow{},
		globalSupp:     map[string][]*SuppressionEvent{},
		campaignSupp:   map[string]map[string][]*SuppressionEvent{},
		retentionRules: map[string]map[string]*RetentionRule{},
		ruleOrder:      map[string][]string{},
		nextEventID:    1,
		nextAuthID:     1,
	}
}

const opStore = "store"

func (s *memoryStore) InsertCampaign(c *Campaign) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.campaigns[c.ID]; ok {
		return newError(opStore, ErrConflict, "campaign %s already exists", c.ID)
	}
	cp := *c
	s.campaigns[c.ID] = &cp
	return nil
}

func (s *memoryStore) GetCampaign(id string) (*Campaign, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.campaigns[id]
	if !ok {
		return nil, newError(opStore, ErrNotFound, "campaign %s not found", id)
	}
	cp := *c
	return &cp, nil
}

func (s *memoryStore) StartCampaign(c *Campaign, templateVersion string, recipients []Recipient, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cur, ok := s.campaigns[c.ID]
	if !ok {
		return newError(opStore, ErrNotFound, "campaign %s not found", c.ID)
	}
	if cur.Status != CampaignDraft {
		return newError(opStore, ErrConflict, "campaign %s is %s, only draft campaigns can start", c.ID, cur.Status)
	}

	cur.Status = CampaignRunning
	cur.TemplateVersion = templateVersion
	cur.StartedAt = at
	cur.Recipients = append([]Recipient(nil), recipients...)

	rcptMap := make(map[string]Recipient, len(recipients))
	keys := make([]string, 0, len(recipients))
	for _, r := range recipients {
		addr := normalizeAddress(r.Address)
		key := DispatchKey(c.ID, addr)
		if _, dup := rcptMap[addr]; dup {
			// 回滚状态，返回明确冲突（由 Service 校验提前拦截，这里是最后防线）。
			cur.Status = CampaignDraft
			cur.TemplateVersion = ""
			cur.StartedAt = time.Time{}
			cur.Recipients = nil
			return newError(opStore, ErrConflict, "duplicate recipient %s in frozen audience", addr)
		}
		vars := r.Vars
		rcptMap[addr] = Recipient{Address: addr, Vars: vars}
		keys = append(keys, key)
		s.tasks[key] = &taskRow{
			task: Task{
				CampaignID:  c.ID,
				Recipient:   addr,
				DispatchKey: key,
				State:       TaskPending,
				UpdatedAt:   at,
			},
			recipient: Recipient{Address: addr, Vars: vars},
			attempts:  map[int]*AttemptRecord{},
		}
	}
	s.recipients[c.ID] = rcptMap
	s.taskOrder[c.ID] = keys
	return nil
}

func (s *memoryStore) CancelCampaign(id string, at time.Time) (*Campaign, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.campaigns[id]
	if !ok {
		return nil, false, newError(opStore, ErrNotFound, "campaign %s not found", id)
	}
	// 重复取消返回稳定结果：changed=false，状态保持 canceled。
	if c.Status == CampaignCanceled {
		cp := *c
		return &cp, false, nil
	}
	c.Status = CampaignCanceled
	c.CanceledAt = at

	for _, key := range s.taskOrder[id] {
		row := s.tasks[key]
		switch row.task.State {
		case TaskPending, TaskRetryWait, TaskHeld:
			// 尚未被工作者持有的项直接进入 canceled 终态。
			row.task.State = TaskCanceled
			row.task.HeldByRule = ""
			row.task.UpdatedAt = at
		case TaskLeased, TaskAuthorized:
			// 在途项不做强制迁移：未授权者将在授权点被拒，已授权者等待回执，
			// 失联工作者则在租约过期后被重新领取并在授权点被拒。
		}
	}
	cp := *c
	return &cp, true, nil
}

func (s *memoryStore) RecordSuppression(ev *SuppressionEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ev.Address != "" {
		ev.Address = normalizeAddress(ev.Address)
	}
	ev.ID = s.nextEventID
	s.nextEventID++

	switch ev.Type {
	case SuppressionGlobalUnsubscribe, SuppressionBounce:
		s.globalSupp[ev.Address] = append(s.globalSupp[ev.Address], cloneEventPtr(ev))
	case SuppressionCampaign:
		m := s.campaignSupp[ev.CampaignID]
		if m == nil {
			m = map[string][]*SuppressionEvent{}
			s.campaignSupp[ev.CampaignID] = m
		}
		m[ev.Address] = append(m[ev.Address], cloneEventPtr(ev))
	default:
		return newError(opStore, ErrValidation, "unknown suppression type %q", ev.Type)
	}
	return nil
}

// effectiveSuppressions 返回 at 时刻已生效的匹配抑制（OccurredAt <= at），按生效时间排序。
// 调用方须持有 s.mu。
func (s *memoryStore) effectiveSuppressions(campaignID, address string, at time.Time) []SuppressionEvent {
	var out []SuppressionEvent
	for _, p := range s.globalSupp[address] {
		if !p.OccurredAt.After(at) {
			out = append(out, *p)
		}
	}
	for _, p := range s.campaignSupp[campaignID][address] {
		if !p.OccurredAt.After(at) {
			out = append(out, *p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OccurredAt.Before(out[j].OccurredAt) })
	return out
}

func (s *memoryStore) SuppressionsFor(campaignID, address string) []SuppressionEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	address = normalizeAddress(address)
	var out []SuppressionEvent
	for _, p := range s.globalSupp[address] {
		out = append(out, *p)
	}
	for _, p := range s.campaignSupp[campaignID][address] {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].OccurredAt.Equal(out[j].OccurredAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].OccurredAt.Before(out[j].OccurredAt)
	})
	return out
}

// normalizeRuleStatus 将已过有效期但尚未解除的 active 规则惰性标记为 expired。
// 冻结规则失效时，其持有的 held 项回到 pending（已完成发送不受影响）。
// 调用方须持有 s.mu。
func (s *memoryStore) reapExpiredRulesLocked(now time.Time) {
	for campaignID, byID := range s.retentionRules {
		for id, rule := range byID {
			if rule.Status == RuleActive && !rule.ExpiresAt.After(now) {
				rule.Status = RuleExpired
				if rule.Action == RetentionFreeze {
					s.releaseHeldTasksLocked(campaignID, id, now)
				}
			}
		}
	}
}

// releaseHeldTasksLocked 把指定冻结规则持有的未授权项放回 pending。调用方须持有 s.mu。
func (s *memoryStore) releaseHeldTasksLocked(campaignID, ruleID string, at time.Time) {
	for _, key := range s.taskOrder[campaignID] {
		row := s.tasks[key]
		if row.task.State == TaskHeld && row.task.HeldByRule == ruleID {
			row.task.State = TaskPending
			row.task.HeldByRule = ""
			row.task.LeaseToken = ""
			row.task.LeasedBy = ""
			row.task.LeaseExpiresAt = time.Time{}
			row.task.UpdatedAt = at
		}
	}
}

// applyFreezeLocked 使冻结规则立即生效：尚未授权的 pending/retry_wait 项进入 held；
// 已 leased 但未授权的项不在此迁移：它们在授权点被拒并转入 held。调用方须持有 s.mu。
func (s *memoryStore) applyFreezeLocked(campaignID string, rule *RetentionRule, at time.Time) {
	for _, key := range s.taskOrder[campaignID] {
		row := s.tasks[key]
		if !rule.Matches(row.task.Recipient) {
			continue
		}
		switch row.task.State {
		case TaskPending, TaskRetryWait:
			row.task.State = TaskHeld
			row.task.HeldByRule = rule.RuleID
			row.task.NextAttemptAt = time.Time{}
			row.task.UpdatedAt = at
		}
	}
}

func cloneRule(r *RetentionRule) *RetentionRule {
	cp := *r
	cp.Scope = append([]string(nil), r.Scope...)
	return &cp
}

func sameRuleContent(a, b *RetentionRule) bool {
	if a.Action != b.Action || a.Reason != b.Reason ||
		!a.EffectiveAt.Equal(b.EffectiveAt) || !a.ExpiresAt.Equal(b.ExpiresAt) ||
		len(a.Scope) != len(b.Scope) {
		return false
	}
	seen := make(map[string]struct{}, len(a.Scope))
	for _, addr := range a.Scope {
		seen[addr] = struct{}{}
	}
	for _, addr := range b.Scope {
		if _, ok := seen[addr]; !ok {
			return false
		}
	}
	return true
}

func (s *memoryStore) UpsertRetentionRule(rule *RetentionRule, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.reapExpiredRulesLocked(now)

	byID := s.retentionRules[rule.CampaignID]
	if existing, ok := byID[rule.RuleID]; ok {
		if !sameRuleContent(existing, rule) {
			return false, newError(opStore, ErrConflict,
				"retention rule %s already exists with different scope or validity", rule.RuleID)
		}
		*rule = *cloneRule(existing)
		return false, nil
	}

	stored := cloneRule(rule)
	stored.Status = RuleActive
	stored.CreatedAt = now
	if byID == nil {
		byID = map[string]*RetentionRule{}
		s.retentionRules[rule.CampaignID] = byID
	}
	byID[rule.RuleID] = stored
	s.ruleOrder[rule.CampaignID] = append(s.ruleOrder[rule.CampaignID], rule.RuleID)

	// 立即生效的冻结规则马上持有未授权项；EffectiveAt 在未来的规则由授权点/领取时再裁决。
	if stored.Action == RetentionFreeze && stored.EffectiveAtTime(now) {
		s.applyFreezeLocked(rule.CampaignID, stored, now)
	}
	*rule = *stored
	return true, nil
}

func (s *memoryStore) ReleaseRetentionRule(campaignID, ruleID string, at time.Time) (*RetentionRule, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.reapExpiredRulesLocked(at)

	rule, ok := s.retentionRules[campaignID][ruleID]
	if !ok {
		return nil, false, newError(opStore, ErrNotFound, "retention rule %s not found in campaign %s", ruleID, campaignID)
	}
	if rule.Status != RuleActive {
		return cloneRule(rule), false, nil
	}
	rule.Status = RuleReleased
	rule.ReleasedAt = at
	if rule.Action == RetentionFreeze {
		s.releaseHeldTasksLocked(campaignID, ruleID, at)
	}
	return cloneRule(rule), true, nil
}

func (s *memoryStore) RetentionRule(campaignID, ruleID string, now time.Time) (*RetentionRule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapExpiredRulesLocked(now)
	rule, ok := s.retentionRules[campaignID][ruleID]
	if !ok {
		return nil, newError(opStore, ErrNotFound, "retention rule %s not found in campaign %s", ruleID, campaignID)
	}
	return cloneRule(rule), nil
}

func (s *memoryStore) RetentionRules(campaignID string, now time.Time) ([]RetentionRule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapExpiredRulesLocked(now)
	ids := s.ruleOrder[campaignID]
	out := make([]RetentionRule, 0, len(ids))
	for _, id := range ids {
		if rule := s.retentionRules[campaignID][id]; rule != nil {
			out = append(out, *cloneRule(rule))
		}
	}
	return out, nil
}

// effectiveRetentionBasisLocked 返回 at 时刻命中地址的有效依据：freeze 优先，其次 retain。调用方须持有 s.mu。
func (s *memoryStore) effectiveRetentionBasisLocked(campaignID, address string, at time.Time) *RetentionBasis {
	var retainHit *RetentionRule
	for _, id := range s.ruleOrder[campaignID] {
		rule := s.retentionRules[campaignID][id]
		if rule == nil || !rule.EffectiveAtTime(at) || !rule.Matches(address) {
			continue
		}
		if rule.Action == RetentionFreeze {
			return &RetentionBasis{RuleID: rule.RuleID, Action: rule.Action, Reason: rule.Reason}
		}
		if retainHit == nil {
			retainHit = rule
		}
	}
	if retainHit != nil {
		return &RetentionBasis{RuleID: retainHit.RuleID, Action: retainHit.Action, Reason: retainHit.Reason}
	}
	return nil
}

func (s *memoryStore) RetentionBasisFor(campaignID, address string, at time.Time) *RetentionBasis {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapExpiredRulesLocked(at)
	return s.effectiveRetentionBasisLocked(campaignID, normalizeAddress(address), at)
}

// readyForLease 报告任务是否可被领取。调用方须持有 s.mu。
func readyForLease(row *taskRow, now time.Time) bool {
	switch row.task.State {
	case TaskPending:
		return true
	case TaskHeld:
		// held 只能由解除/失效迁移回 pending，不直接领取。
		return false
	case TaskRetryWait:
		return !row.task.NextAttemptAt.After(now)
	case TaskLeased, TaskAuthorized:
		// 工作者失联：租约到期、且当前 attempt 尚无回执结论。
		cur := row.attempts[row.task.Attempt]
		return row.task.LeaseExpiresAt.Before(now) && cur != nil && cur.Outcome == nil
	default:
		return false
	}
}

func (s *memoryStore) LeaseTasks(campaignID, workerID string, count int, dur time.Duration, now time.Time) ([]LeasedTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.campaigns[campaignID]
	if !ok {
		return nil, newError(opStore, ErrNotFound, "campaign %s not found", campaignID)
	}
	if c.Status != CampaignRunning {
		return nil, newError(opStore, ErrCampaignNotReady, "campaign %s is %s, lease requires running", campaignID, c.Status)
	}

	// 先惰性处理过期规则（可能释放 held 项），再按依据对就绪项分批。
	s.reapExpiredRulesLocked(now)

	out := make([]LeasedTask, 0, count)
	var batchBasis *RetentionBasis // 本批次统一依据；nil 表示“无依据”批次
	batchDecided := false
	for _, key := range s.taskOrder[campaignID] {
		if len(out) >= count {
			break
		}
		row := s.tasks[key]
		if !readyForLease(row, now) {
			continue
		}
		// 规则生效后新生成的邮件必须带上对应依据；新旧决定不能混在同一批次：
		// 单次领取只返回同一依据指纹的任务，依据变化即结束本批次（剩余任务由后续批次处理）。
		basis := s.effectiveRetentionBasisLocked(campaignID, row.recipient.Address, now)
		if basis != nil && basis.Action == RetentionFreeze {
			continue // 仍在冻结期：该任务不可领取
		}
		if !batchDecided {
			batchBasis = basis
			batchDecided = true
		} else if basisFingerprint(batchBasis) != basisFingerprint(basis) {
			break
		}
		row.task.Attempt++
		n := row.task.Attempt
		token := newToken()
		expires := now.Add(dur)
		row.task.State = TaskLeased
		row.task.LeaseToken = token
		row.task.LeasedBy = workerID
		row.task.LeaseExpiresAt = expires
		row.task.NextAttemptAt = time.Time{}
		row.task.AuthID = 0
		row.task.AuthorizedAt = time.Time{}
		row.task.HeldByRule = ""
		row.task.UpdatedAt = now
		row.attempts[n] = &AttemptRecord{
			Number:         n,
			WorkerID:       workerID,
			Token:          token,
			LeasedAt:       now,
			LeaseExpiresAt: expires,
		}
		out = append(out, LeasedTask{
			DispatchKey:     key,
			CampaignID:      campaignID,
			Recipient:       row.recipient.Address,
			TemplateVersion: c.TemplateVersion, // 冻结版本随租约下发
			Vars:            cloneStringMap(row.recipient.Vars),
			Attempt:         n,
			LeaseToken:      token,
			LeaseExpiresAt:  expires,
			Basis:           cloneBasis(batchBasis),
		})
	}
	return out, nil
}

func basisFingerprint(b *RetentionBasis) string {
	if b == nil {
		return ""
	}
	return b.RuleID + "|" + string(b.Action)
}

func cloneBasis(b *RetentionBasis) *RetentionBasis {
	if b == nil {
		return nil
	}
	cp := *b
	return &cp
}

func reasonFor(ev SuppressionEvent) string {
	switch ev.Type {
	case SuppressionGlobalUnsubscribe:
		return ReasonGlobalUnsubscribe
	case SuppressionBounce:
		return ReasonBounce
	case SuppressionCampaign:
		return ReasonCampaignSuppressed
	}
	return string(ev.Type)
}

func (s *memoryStore) Authorize(key, token string, now time.Time) (*AuthDecision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	row, err := s.rowByKey(key)
	if err != nil {
		return nil, err
	}
	t := &row.task

	// 定位 token 对应的 attempt：只接受当前租约的持有者。
	cur, ok := row.attempts[t.Attempt]
	if !ok || cur.Token != token {
		old := s.findAttemptByToken(row, token)
		if old == nil {
			return nil, newError(opStore, ErrInvalidLease, "no lease matches token for %s", key)
		}
		// 决策已落库即不可变：无论租约是否过期、是否被新 attempt 取代，都返回稳定结论。
		if old.Decision != nil {
			cp := *old.Decision
			return &cp, nil
		}
		if !old.LeaseExpiresAt.After(now) {
			return nil, newError(opStore, ErrLeaseExpired, "lease for attempt %d expired at %s", old.Number, old.LeaseExpiresAt.Format(time.RFC3339))
		}
		return nil, newError(opStore, ErrStaleAttempt, "token belongs to superseded attempt %d, current attempt is %d", old.Number, t.Attempt)
	}
	// 即使是当前 attempt，租约到期也不得再授权（所有权已失效，等待重新领取）。
	// 但已持久化的决策优先返回：保证响应丢失后的重试拿到稳定结论。
	if cur.Decision != nil {
		cp := *cur.Decision
		return &cp, nil
	}
	if !cur.LeaseExpiresAt.After(now) {
		return nil, newError(opStore, ErrLeaseExpired, "lease for attempt %d expired at %s", cur.Number, cur.LeaseExpiresAt.Format(time.RFC3339))
	}
	if t.State != TaskLeased {
		return nil, newError(opStore, ErrInvalidState, "task %s is %s, authorize requires leased", key, t.State)
	}

	dec := &AuthDecision{ID: s.nextAuthID, Attempt: cur.Number, At: now}
	s.nextAuthID++

	campaign := s.campaigns[t.CampaignID]
	if campaign == nil || campaign.Status != CampaignRunning {
		// 取消后不得授权新邮件：决策拒绝并持久化，任务进入 canceled 终态。
		dec.Granted = false
		dec.Reason = ReasonCampaignCanceled
		cur.Decision = dec
		t.State = TaskCanceled
		t.UpdatedAt = now
		cp := *dec
		return &cp, nil
	}

	// 合规保留点：尚未授权的邮件必须按当前规则重新判断，迟到的旧工作者不能绕过当前规则。
	// 规则依据在授权时刻快照进决策；决策落库后规则再变化（解除/失效/改规则）不影响既有结论。
	s.reapExpiredRulesLocked(now)
	if basis := s.effectiveRetentionBasisLocked(t.CampaignID, t.Recipient, now); basis != nil {
		dec.Basis = cloneBasis(basis)
		if basis.Action == RetentionFreeze {
			dec.Granted = false
			dec.Reason = ReasonComplianceHeld
			cur.Decision = dec
			t.State = TaskHeld
			t.HeldByRule = basis.RuleID
			t.FailureReason = dec.Reason
			t.LeaseToken = ""
			t.LeasedBy = ""
			t.LeaseExpiresAt = time.Time{}
			t.UpdatedAt = now
			cp := *dec
			return &cp, nil
		}
	}

	// 授权点权威复查：全局退订 / 地址退信 / 活动级抑制，全部以持久化事件时序为准。
	if hits := s.effectiveSuppressions(t.CampaignID, t.Recipient, now); len(hits) > 0 {
		dec.Granted = false
		dec.Reason = reasonFor(hits[0])
		dec.Suppressions = hits
		cur.Decision = dec
		t.State = TaskSuppressed
		t.FailureReason = dec.Reason
		t.UpdatedAt = now
		cp := *dec
		return &cp, nil
	}

	dec.Granted = true
	cur.Decision = dec
	t.State = TaskAuthorized
	t.AuthID = dec.ID
	t.AuthorizedAt = now
	t.UpdatedAt = now
	cp := *dec
	return &cp, nil
}

func (s *memoryStore) CompleteReceipt(key, token string, in ReceiptInput, now time.Time) (ReceiptAck, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	row, err := s.rowByKey(key)
	if err != nil {
		return ReceiptAck{}, err
	}
	t := &row.task

	rec := s.findAttemptByToken(row, token)
	if rec == nil {
		return ReceiptAck{}, newError(opStore, ErrInvalidLease, "no lease matches token for %s", key)
	}
	if rec.Number < t.Attempt {
		// 过期租约的旧工作者回执：明确拒绝，绝不覆盖新尝试。
		return ReceiptAck{}, newError(opStore, ErrStaleAttempt,
			"receipt for attempt %d rejected, task is on attempt %d", rec.Number, t.Attempt)
	}
	switch in.Result {
	case ResultSuccess, ResultTemporaryFailure, ResultPermanentFailure:
	default:
		return ReceiptAck{}, newError(opStore, ErrInvalidReceipt, "unknown receipt result %q", in.Result)
	}
	if rec.Outcome != nil {
		// 重复回执：返回稳定的既有结论，统计不会重复累加。
		return ReceiptAck{
			DispatchKey: key,
			Attempt:     rec.Number,
			State:       t.State,
			Duplicate:   true,
			Outcome:     cloneReceiptPtr(rec.Outcome),
		}, nil
	}
	if rec.Decision == nil || !rec.Decision.Granted {
		return ReceiptAck{}, newError(opStore, ErrInvalidState,
			"attempt %d was not granted at the authorization point; sending prohibited", rec.Number)
	}

	out := &ReceiptRecord{
		Attempt:           rec.Number,
		Result:            in.Result,
		ProviderMessageID: in.ProviderMessageID,
		Code:              in.Code,
		Message:           in.Message,
		ReceivedAt:        now,
	}

	// 授权点之后录入/生效的抑制不能回滚投递决策，但必须留下解释性审计轨迹。
	for _, ev := range s.effectiveSuppressions(t.CampaignID, t.Recipient, now) {
		if ev.RecordedAt.After(rec.Decision.At) {
			out.Audit = append(out.Audit, formatPostAuthAudit(ev, rec.Decision.At))
		}
	}
	// 授权后生效/变更的合规规则同样不能回滚已落库的发送决定，仅留下解释性审计。
	s.reapExpiredRulesLocked(now)
	for _, ruleID := range s.ruleOrder[t.CampaignID] {
		rule := s.retentionRules[t.CampaignID][ruleID]
		if rule == nil || !rule.Matches(t.Recipient) || !rule.EffectiveAtTime(now) {
			continue
		}
		if rule.CreatedAt.After(rec.Decision.At) {
			out.Audit = append(out.Audit, formatPostAuthRuleAudit(*rule, rec.Decision.At))
		}
	}
	rec.Outcome = out

	policy := s.campaigns[t.CampaignID].RetryPolicy.withDefaults()
	switch in.Result {
	case ResultSuccess:
		t.State = TaskSent
		t.FailureReason = ""
	case ResultPermanentFailure:
		t.State = TaskFailed
		t.FailureReason = failureSummary(in)
	case ResultTemporaryFailure:
		if rec.Number >= policy.MaxAttempts {
			t.State = TaskFailed
			t.FailureReason = "retry budget exhausted after " + itoa(rec.Number) + " attempts: " + failureSummary(in)
		} else {
			t.State = TaskRetryWait
			t.NextAttemptAt = now.Add(policy.Backoff(rec.Number))
			t.FailureReason = failureSummary(in)
		}
	}
	t.UpdatedAt = now

	return ReceiptAck{
		DispatchKey: key,
		Attempt:     rec.Number,
		State:       t.State,
		Duplicate:   false,
		Outcome:     cloneReceiptPtr(out),
	}, nil
}

func (s *memoryStore) Stats(campaignID string) (CampaignStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.campaigns[campaignID]
	if !ok {
		return CampaignStats{}, newError(opStore, ErrNotFound, "campaign %s not found", campaignID)
	}
	st := CampaignStats{
		CampaignID:      campaignID,
		Status:          c.Status,
		TemplateVersion: c.TemplateVersion,
	}
	for _, key := range s.taskOrder[campaignID] {
		row := s.tasks[key]
		st.Total++
		st.TotalAttempts += len(row.attempts)
		switch row.task.State {
		case TaskPending:
			st.Pending++
		case TaskLeased:
			st.Leased++
		case TaskAuthorized:
			st.Authorized++
		case TaskRetryWait:
			st.RetryWait++
		case TaskSent:
			st.Sent++
		case TaskFailed:
			st.Failed++
		case TaskSuppressed:
			st.Suppressed++
		case TaskCanceled:
			st.Canceled++
		case TaskHeld:
			st.Held++
		}
		if row.task.State.IsTerminal() {
			st.Terminal++
		}
	}
	st.Finished = st.Total > 0 && st.Terminal == st.Total
	return st, nil
}

func (s *memoryStore) GetDispatch(campaignID, key string, now time.Time) (*DispatchDetail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	row, ok := s.tasks[key]
	if !ok || row.task.CampaignID != campaignID {
		return nil, newError(opStore, ErrNotFound, "dispatch %s not found in campaign %s", key, campaignID)
	}
	campaign := s.campaigns[campaignID]
	d := &DispatchDetail{
		Task:            row.task,
		CampaignID:      campaignID,
		Recipient:       row.recipient,
		TemplateVersion: campaign.TemplateVersion,
	}
	s.reapExpiredRulesLocked(now)
	d.CurrentBasis = s.effectiveRetentionBasisLocked(campaignID, row.task.Recipient, now)
	nums := make([]int, 0, len(row.attempts))
	for n := range row.attempts {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	for _, n := range nums {
		a := *row.attempts[n]
		d.Attempts = append(d.Attempts, &a)
	}
	return d, nil
}

// ---- 内部辅助（调用方须持锁）----

func (s *memoryStore) rowByKey(key string) (*taskRow, error) {
	row, ok := s.tasks[key]
	if !ok {
		return nil, newError(opStore, ErrNotFound, "dispatch %s not found", key)
	}
	return row, nil
}

func (s *memoryStore) findAttemptByToken(row *taskRow, token string) *AttemptRecord {
	for _, a := range row.attempts {
		if a.Token == token {
			return a
		}
	}
	return nil
}

func cloneEventPtr(p *SuppressionEvent) *SuppressionEvent {
	cp := *p
	return &cp
}

func cloneReceiptPtr(p *ReceiptRecord) *ReceiptRecord {
	if p == nil {
		return nil
	}
	cp := *p
	if p.Audit != nil {
		cp.Audit = append([]string(nil), p.Audit...)
	}
	return &cp
}

func cloneStringMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func failureSummary(in ReceiptInput) string {
	s := string(in.Result)
	if in.Code != "" {
		s += ": " + in.Code
	}
	if in.Message != "" {
		if in.Code != "" {
			s += " " + in.Message
		} else {
			s += ": " + in.Message
		}
	}
	return s
}

func formatPostAuthAudit(ev SuppressionEvent, authAt time.Time) string {
	return "suppression " + string(ev.Type) +
		" (id=" + itoa64(ev.ID) + ") recorded after authorization at " +
		authAt.Format(time.RFC3339Nano) +
		"; occurred_at=" + ev.OccurredAt.Format(time.RFC3339Nano) +
		"; send decision retained, message may already be delivered; do not treat as delivery failure"
}

func formatPostAuthRuleAudit(rule RetentionRule, authAt time.Time) string {
	return "retention rule " + rule.RuleID + " (" + string(rule.Action) +
		") effective after authorization at " + authAt.Format(time.RFC3339Nano) +
		"; send decision retained, message may already be delivered; do not treat as delivery failure"
}
