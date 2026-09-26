package emaildispatch

import (
	"sync"
	"testing"
	"time"
)

// fakeClock 是可手动推进的测试时钟。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}

func newTestService() (*Service, *fakeClock) {
	clk := newFakeClock()
	return NewServiceWithClock(NewMemoryStore(), clk.now), clk
}

func mustStart(t *testing.T, svc *Service, tv string, addrs ...string) string {
	t.Helper()
	c, err := svc.CreateCampaign(CampaignSpec{Name: "camp-" + tv})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	rcpts := make([]Recipient, 0, len(addrs))
	for _, a := range addrs {
		rcpts = append(rcpts, Recipient{Address: a})
	}
	if _, err := svc.StartCampaign(c.ID, StartSpec{TemplateVersion: tv, Recipients: rcpts}); err != nil {
		t.Fatalf("start campaign: %v", err)
	}
	return c.ID
}

func mustLeaseOne(t *testing.T, svc *Service, campaignID, worker string) LeasedTask {
	t.Helper()
	tasks, err := svc.Lease(campaignID, LeaseRequest{WorkerID: worker, LeaseDuration: time.Minute})
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 leased task, got %d", len(tasks))
	}
	return tasks[0]
}

// TestCreateStartLocksTemplateAndFreezesAudience 覆盖需求 1、2：启动时锁定模板版本、冻结受众。
func TestCreateStartLocksTemplateAndFreezesAudience(t *testing.T) {
	svc, _ := newTestService()

	c, err := svc.CreateCampaign(CampaignSpec{Name: "welcome"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if c.Status != CampaignDraft || c.TemplateVersion != "" {
		t.Fatalf("new campaign should be draft without template, got %+v", c)
	}

	// 未提供模板版本或受众不得启动。
	if _, err := svc.StartCampaign(c.ID, StartSpec{Recipients: []Recipient{{Address: "a@x.com"}}}); !IsCode(err, ErrValidation) {
		t.Fatalf("expected validation error for missing template, got %v", err)
	}
	if _, err := svc.StartCampaign(c.ID, StartSpec{TemplateVersion: "tpl-1"}); !IsCode(err, ErrValidation) {
		t.Fatalf("expected validation error for empty audience, got %v", err)
	}
	// 重复地址不得冻结。
	_, err = svc.StartCampaign(c.ID, StartSpec{
		TemplateVersion: "tpl-1",
		Recipients:      []Recipient{{Address: "a@x.com"}, {Address: "A@X.COM "}},
	})
	if !IsCode(err, ErrValidation) {
		t.Fatalf("expected duplicate validation error, got %v", err)
	}

	started, err := svc.StartCampaign(c.ID, StartSpec{
		TemplateVersion: "tpl-9",
		Recipients:      []Recipient{{Address: "a@x.com"}, {Address: "b@x.com", Vars: map[string]string{"name": "Bo"}}},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if started.Status != CampaignRunning || started.TemplateVersion != "tpl-9" || len(started.Recipients) != 2 {
		t.Fatalf("unexpected started campaign: %+v", started)
	}

	// 重复启动冲突。
	if _, err := svc.StartCampaign(c.ID, StartSpec{TemplateVersion: "tpl-2", Recipients: []Recipient{{Address: "z@x.com"}}}); !IsCode(err, ErrConflict) {
		t.Fatalf("expected conflict on restart, got %v", err)
	}
}

// TestDispatchKeyStable 覆盖需求 3：活动 × 收件人派生稳定投递幂等键。
func TestDispatchKeyStable(t *testing.T) {
	k1 := DispatchKey("cmp_1", "User@Example.com")
	k2 := DispatchKey("cmp_1", " user@example.com ")
	if k1 != k2 {
		t.Fatalf("dispatch key not stable under normalization: %s vs %s", k1, k2)
	}
	if k3 := DispatchKey("cmp_2", "user@example.com"); k3 == k1 {
		t.Fatal("different campaign must yield different key")
	}
	if k4 := DispatchKey("cmp_1", "other@example.com"); k4 == k1 {
		t.Fatal("different recipient must yield different key")
	}
}

// TestHappyPathAuthorizeReceiptStats 覆盖领取→授权→回执→统计主流程。
func TestHappyPathAuthorizeReceiptStats(t *testing.T) {
	svc, _ := newTestService()
	id := mustStart(t, svc, "tpl-1", "a@x.com", "b@x.com")

	st, _ := svc.Stats(id)
	if st.Total != 2 || st.Pending != 2 || st.TemplateVersion != "tpl-1" {
		t.Fatalf("unexpected initial stats: %+v", st)
	}

	task := mustLeaseOne(t, svc, id, "w1")
	if task.TemplateVersion != "tpl-1" || task.Attempt != 1 || task.LeaseToken == "" {
		t.Fatalf("unexpected lease: %+v", task)
	}

	// 未授权直接回执必须拒绝。
	if _, err := svc.SubmitReceipt(id, task.DispatchKey, task.LeaseToken, ReceiptInput{Result: ResultSuccess}); !IsCode(err, ErrInvalidState) {
		t.Fatalf("expected invalid_state before authorization, got %v", err)
	}

	dec, err := svc.Authorize(id, task.DispatchKey, task.LeaseToken)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if !dec.Granted || dec.ID == 0 {
		t.Fatalf("expected persisted grant, got %+v", dec)
	}

	// 重复授权返回同一持久化决策。
	dec2, err := svc.Authorize(id, task.DispatchKey, task.LeaseToken)
	if err != nil {
		t.Fatalf("re-authorize: %v", err)
	}
	if dec2.ID != dec.ID {
		t.Fatalf("authorization decision must be stable: %d vs %d", dec2.ID, dec.ID)
	}

	ack, err := svc.SubmitReceipt(id, task.DispatchKey, task.LeaseToken, ReceiptInput{
		Result:            ResultSuccess,
		ProviderMessageID: "msg-123",
	})
	if err != nil || ack.State != TaskSent || ack.Duplicate {
		t.Fatalf("unexpected receipt ack: %+v err=%v", ack, err)
	}

	// 重复回执：稳定结果 + Duplicate，统计不重复累加。
	ack2, err := svc.SubmitReceipt(id, task.DispatchKey, task.LeaseToken, ReceiptInput{Result: ResultSuccess})
	if err != nil || !ack2.Duplicate || ack2.State != TaskSent {
		t.Fatalf("expected duplicate stable ack, got %+v err=%v", ack2, err)
	}
	if ack2.Outcome.ProviderMessageID != "msg-123" {
		t.Fatalf("duplicate receipt must echo original outcome, got %+v", ack2.Outcome)
	}

	st, _ = svc.Stats(id)
	if st.Sent != 1 || st.Pending != 1 || st.TotalAttempts != 1 || st.Finished {
		t.Fatalf("stats after one send: %+v", st)
	}

	// 错误 token / 不存在对象。
	if _, err := svc.Authorize(id, task.DispatchKey, "lease_deadbeef"); !IsCode(err, ErrInvalidLease) {
		t.Fatalf("expected invalid lease, got %v", err)
	}
	if _, err := svc.Stats("cmp_nope"); !IsCode(err, ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}

// TestLeaseExpiryFencing 覆盖需求 3：过期租约可被新工作者接管，旧回执不得覆盖新尝试。
func TestLeaseExpiryFencing(t *testing.T) {
	svc, clk := newTestService()
	id := mustStart(t, svc, "tpl-1", "a@x.com")

	first := mustLeaseOne(t, svc, id, "slow-worker")
	firstToken := first.LeaseToken

	// 租约未过期时不可重复领取。
	if tasks, _ := svc.Lease(id, LeaseRequest{WorkerID: "w2", LeaseDuration: time.Minute}); len(tasks) != 0 {
		t.Fatalf("task must not be leasable before expiry, got %d", len(tasks))
	}

	clk.advance(61 * time.Second)

	// 旧工作者授权必须被拒（租约过期）。
	if _, err := svc.Authorize(id, first.DispatchKey, firstToken); !IsCode(err, ErrLeaseExpired) {
		t.Fatalf("expected lease_expired on late authorize, got %v", err)
	}

	second := mustLeaseOne(t, svc, id, "new-worker")
	if second.Attempt != 2 || second.LeaseToken == firstToken {
		t.Fatalf("expected fresh attempt 2 with new token, got %+v", second)
	}

	// 旧工作者带着结果回来：必须被 fencing 拒绝，不能覆盖 attempt 2。
	_, err := svc.SubmitReceipt(id, first.DispatchKey, firstToken, ReceiptInput{Result: ResultSuccess})
	if !IsCode(err, ErrStaleAttempt) {
		t.Fatalf("expected stale_attempt for old worker receipt, got %v", err)
	}

	dec, _ := svc.Authorize(id, second.DispatchKey, second.LeaseToken)
	if !dec.Granted {
		t.Fatalf("new attempt should authorize")
	}
	ack, err := svc.SubmitReceipt(id, second.DispatchKey, second.LeaseToken, ReceiptInput{Result: ResultSuccess})
	if err != nil || ack.State != TaskSent || ack.Attempt != 2 {
		t.Fatalf("new worker receipt should win: ack=%+v err=%v", ack, err)
	}

	st, _ := svc.Stats(id)
	if st.Sent != 1 || st.TotalAttempts != 2 {
		t.Fatalf("stats after fencing: %+v", st)
	}

	// 审计轨迹保留两次 attempt。
	detail, err := svc.GetDispatch(id, second.DispatchKey)
	if err != nil || len(detail.Attempts) != 2 {
		t.Fatalf("expected 2 attempt records, got %+v err=%v", detail, err)
	}
	if detail.Attempts[0].Outcome != nil {
		t.Fatal("rejected stale attempt must not carry an outcome")
	}
}

// TestLateReceiptOnCurrentAttempt 授权后迟到但尚未被接管的回执仍被接受，避免提供商侧重复发信。
func TestLateReceiptOnCurrentAttempt(t *testing.T) {
	svc, clk := newTestService()
	id := mustStart(t, svc, "tpl-1", "a@x.com")
	task := mustLeaseOne(t, svc, id, "w1")
	dec, _ := svc.Authorize(id, task.DispatchKey, task.LeaseToken)
	if !dec.Granted {
		t.Fatal("should grant")
	}
	clk.advance(2 * time.Minute) // 租约过期，但没有新工作者接管
	ack, err := svc.SubmitReceipt(id, task.DispatchKey, task.LeaseToken, ReceiptInput{Result: ResultSuccess})
	if err != nil || ack.State != TaskSent {
		t.Fatalf("late receipt on sole attempt should be accepted: %+v err=%v", ack, err)
	}
}

// TestSuppressionsBlockAtAuthorization 覆盖需求 2、4：授权以前生效的抑制必须拦截投递。
func TestSuppressionsBlockAtAuthorization(t *testing.T) {
	svc, _ := newTestService()
	id := mustStart(t, svc, "tpl-1", "global@x.com", "bounce@x.com", "camp@x.com", "ok@x.com")

	if _, err := svc.RecordSuppression(SuppressionInput{Type: SuppressionGlobalUnsubscribe, Address: "global@x.com", Reason: "user clicked unsubscribe"}); err != nil {
		t.Fatalf("record global: %v", err)
	}
	if _, err := svc.RecordSuppression(SuppressionInput{Type: SuppressionBounce, Address: "bounce@x.com"}); err != nil {
		t.Fatalf("record bounce: %v", err)
	}
	if _, err := svc.RecordSuppression(SuppressionInput{Type: SuppressionCampaign, CampaignID: id, Address: "camp@x.com"}); err != nil {
		t.Fatalf("record campaign suppression: %v", err)
	}

	// 按冻结顺序一次批量领取，再按地址逐个验证授权点拦截。
	leased, err := svc.Lease(id, LeaseRequest{WorkerID: "w", Count: 10, LeaseDuration: time.Minute})
	if err != nil || len(leased) != 4 {
		t.Fatalf("lease all: %d err=%v", len(leased), err)
	}
	byAddr := map[string]LeasedTask{}
	for _, l := range leased {
		byAddr[l.Recipient] = l
	}

	blocked := map[string]string{
		"global@x.com": ReasonGlobalUnsubscribe,
		"bounce@x.com": ReasonBounce,
		"camp@x.com":   ReasonCampaignSuppressed,
	}
	for addr, reason := range blocked {
		task := byAddr[addr]
		dec, err := svc.Authorize(id, task.DispatchKey, task.LeaseToken)
		if err != nil {
			t.Fatalf("authorize %s: %v", addr, err)
		}
		if dec.Granted || dec.Reason != reason || len(dec.Suppressions) == 0 {
			t.Fatalf("%s should be blocked with %s, got %+v", addr, reason, dec)
		}
		if _, err := svc.SubmitReceipt(id, task.DispatchKey, task.LeaseToken, ReceiptInput{Result: ResultSuccess}); !IsCode(err, ErrInvalidState) {
			t.Fatalf("sending after denial must be rejected, got %v", err)
		}
	}

	// 未被抑制的地址正常放行。
	ok := byAddr["ok@x.com"]
	dec, _ := svc.Authorize(id, ok.DispatchKey, ok.LeaseToken)
	if !dec.Granted {
		t.Fatalf("clean address should be granted, got %+v", dec)
	}

	st, _ := svc.Stats(id)
	if st.Suppressed != 3 || st.Authorized != 1 || st.Terminal != 3 {
		t.Fatalf("stats after blocking: %+v", st)
	}
}

// TestSuppressionRaceAroundAuthorization 覆盖需求 4：授权与抑制同时发生时以持久化授权点为界。
func TestSuppressionRaceAroundAuthorization(t *testing.T) {
	svc, clk := newTestService()
	id := mustStart(t, svc, "tpl-1", "race@x.com")
	task := mustLeaseOne(t, svc, id, "w1")

	// 1) 授权点之前：拦截。
	_, err := svc.RecordSuppression(SuppressionInput{Type: SuppressionGlobalUnsubscribe, Address: "race@x.com"})
	if err != nil {
		t.Fatalf("suppression: %v", err)
	}
	dec, _ := svc.Authorize(id, task.DispatchKey, task.LeaseToken)
	if dec.Granted {
		t.Fatal("must block when suppression effective before authorization")
	}

	// 2) 新活动/新租约：授权之后才录入的抑制不得回滚结果，但必须留下审计。
	id2 := mustStart(t, svc, "tpl-1", "race2@x.com")
	t2 := mustLeaseOne(t, svc, id2, "w1")
	d2, err := svc.Authorize(id2, t2.DispatchKey, t2.LeaseToken)
	if err != nil || !d2.Granted {
		t.Fatalf("grant before suppression: %+v %v", d2, err)
	}
	authAt := d2.At
	clk.advance(time.Second)
	if _, err := svc.RecordSuppression(SuppressionInput{Type: SuppressionGlobalUnsubscribe, Address: "race2@x.com"}); err != nil {
		t.Fatalf("post-auth suppression: %v", err)
	}

	ack, err := svc.SubmitReceipt(id2, t2.DispatchKey, t2.LeaseToken, ReceiptInput{Result: ResultSuccess})
	if err != nil || ack.State != TaskSent {
		t.Fatalf("post-auth suppression must not change delivery result: %+v %v", ack, err)
	}
	if len(ack.Outcome.Audit) == 0 {
		t.Fatal("post-auth suppression must be captured in receipt audit")
	}

	detail, _ := svc.GetDispatch(id2, t2.DispatchKey)
	var recorded *ReceiptRecord
	for _, a := range detail.Attempts {
		if a.Outcome != nil {
			recorded = a.Outcome
		}
	}
	if recorded == nil || len(recorded.Audit) == 0 {
		t.Fatalf("audit trail missing: %+v", detail)
	}
	if !recorded.ReceivedAt.After(authAt) {
		t.Fatal("time ordering setup wrong")
	}
}

// TestBackdatedSuppression 回填（OccurredAt 早于录入）的抑制：以生效时间在授权点判定。
func TestBackdatedSuppression(t *testing.T) {
	svc, clk := newTestService()
	id := mustStart(t, svc, "tpl-1", "late@x.com")
	task := mustLeaseOne(t, svc, id, "w1")
	dec, _ := svc.Authorize(id, task.DispatchKey, task.LeaseToken)
	if !dec.Granted {
		t.Fatal("should grant before event exists")
	}
	clk.advance(2 * time.Second)
	// Webhook 延迟到达：生效时间在授权之前，但录入在授权之后 → 只审计、不回滚。
	past := dec.At.Add(-time.Hour)
	ev, err := svc.RecordSuppression(SuppressionInput{
		Type: SuppressionBounce, Address: "late@x.com", OccurredAt: past,
	})
	if err != nil || !ev.OccurredAt.Equal(past) {
		t.Fatalf("backdated event: %+v err=%v", ev, err)
	}
	ack, err := svc.SubmitReceipt(id, task.DispatchKey, task.LeaseToken, ReceiptInput{Result: ResultSuccess})
	if err != nil || ack.State != TaskSent || len(ack.Outcome.Audit) != 1 {
		t.Fatalf("backdated-but-recorded-late event must be audit-only: %+v err=%v", ack, err)
	}
}

// TestRetryPolicyBackoffAndTerminal 覆盖需求 5：临时失败按退避重试，永久失败终态，预算耗尽终态。
func TestRetryPolicyBackoffAndTerminal(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 3, InitialBackoff: 10 * time.Second, MaxBackoff: time.Minute, Multiplier: 2}.withDefaults()
	if got := p.Backoff(1); got != 10*time.Second {
		t.Fatalf("backoff 1 = %v", got)
	}
	if got := p.Backoff(2); got != 20*time.Second {
		t.Fatalf("backoff 2 = %v", got)
	}
	if got := p.Backoff(5); got != time.Minute {
		t.Fatalf("backoff cap = %v", got)
	}

	svc, clk := newTestService()
	created, err := svc.CreateCampaign(CampaignSpec{
		Name: "retry-camp",
		RetryPolicy: RetryPolicy{
			MaxAttempts:    3,
			InitialBackoff: 10 * time.Second,
			MaxBackoff:     time.Minute,
			Multiplier:     2,
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := created.ID
	if _, err := svc.StartCampaign(id, StartSpec{
		TemplateVersion: "tpl-1",
		Recipients:      []Recipient{{Address: "retry@x.com"}, {Address: "perm@x.com"}},
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	// 临时失败：进入 retry_wait，退避到期前不可领取。
	r1 := mustLeaseOne(t, svc, id, "w1")
	if r1.Recipient != "retry@x.com" {
		t.Fatalf("order: %s", r1.Recipient)
	}
	d, _ := svc.Authorize(id, r1.DispatchKey, r1.LeaseToken)
	if !d.Granted {
		t.Fatal("grant")
	}
	ack, err := svc.SubmitReceipt(id, r1.DispatchKey, r1.LeaseToken, ReceiptInput{
		Result: ResultTemporaryFailure, Code: "421", Message: "try later",
	})
	if err != nil || ack.State != TaskRetryWait {
		t.Fatalf("expected retry_wait, got %+v err=%v", ack, err)
	}
	// 退避期间只有 perm 可领取；领取后复用该租约完成它的终态测试。
	leased, err := svc.Lease(id, LeaseRequest{WorkerID: "w-perm", Count: 10, LeaseDuration: 10 * time.Minute})
	if err != nil || len(leased) != 1 || leased[0].Recipient != "perm@x.com" {
		t.Fatalf("only perm@x.com should be leasable during backoff, got %+v err=%v", leased, err)
	}
	perm := leased[0]

	clk.advance(9 * time.Second)
	if tasks, _ := svc.Lease(id, LeaseRequest{WorkerID: "w2"}); len(tasks) != 0 {
		t.Fatalf("retry item must wait full backoff, got %+v", tasks)
	}
	clk.advance(2 * time.Second)
	r2 := mustLeaseOne(t, svc, id, "w2")
	if r2.Recipient != "retry@x.com" || r2.Attempt != 2 {
		t.Fatalf("expected retry@ attempt 2, got %+v", r2)
	}

	// 永久失败：直接终态。
	dp, _ := svc.Authorize(id, perm.DispatchKey, perm.LeaseToken)
	if !dp.Granted {
		t.Fatal("grant perm")
	}
	ackP, err := svc.SubmitReceipt(id, perm.DispatchKey, perm.LeaseToken, ReceiptInput{
		Result: ResultPermanentFailure, Code: "550", Message: "mailbox not found",
	})
	if err != nil || ackP.State != TaskFailed {
		t.Fatalf("expected failed terminal, got %+v err=%v", ackP, err)
	}

	// 重试预算耗尽：临时失败也进入 failed 终态。
	d2, _ := svc.Authorize(id, r2.DispatchKey, r2.LeaseToken)
	if !d2.Granted {
		t.Fatal("grant 2")
	}
	if _, err := svc.SubmitReceipt(id, r2.DispatchKey, r2.LeaseToken, ReceiptInput{Result: ResultTemporaryFailure}); err != nil {
		t.Fatalf("temp fail 2: %v", err)
	}
	clk.advance(30 * time.Second)
	r3 := mustLeaseOne(t, svc, id, "w4")
	if r3.Attempt != 3 {
		t.Fatalf("expected attempt 3, got %d", r3.Attempt)
	}
	d3, _ := svc.Authorize(id, r3.DispatchKey, r3.LeaseToken)
	if !d3.Granted {
		t.Fatal("grant 3")
	}
	ack3, err := svc.SubmitReceipt(id, r3.DispatchKey, r3.LeaseToken, ReceiptInput{Result: ResultTemporaryFailure, Message: "still down"})
	if err != nil || ack3.State != TaskFailed {
		t.Fatalf("retry exhaustion must be terminal failed, got %+v err=%v", ack3, err)
	}

	st, _ := svc.Stats(id)
	if st.Failed != 2 || st.TotalAttempts != 4 || !st.Finished {
		t.Fatalf("final stats: %+v", st)
	}
}

// TestInvalidReceiptResult 非法回执结论。
func TestInvalidReceiptResult(t *testing.T) {
	svc, _ := newTestService()
	id := mustStart(t, svc, "tpl-1", "a@x.com")
	task := mustLeaseOne(t, svc, id, "w1")
	if _, err := svc.SubmitReceipt(id, task.DispatchKey, task.LeaseToken, ReceiptInput{Result: "bogus"}); !IsCode(err, ErrInvalidReceipt) {
		t.Fatalf("expected invalid_receipt, got %v", err)
	}
}

// TestCancelSemantics 覆盖需求 5：取消后不再授权；重复取消稳定；统计不重复累加。
func TestCancelSemantics(t *testing.T) {
	svc, clk := newTestService()
	id := mustStart(t, svc, "tpl-1", "p@x.com", "l@x.com", "a@x.com")

	// p: 纯 pending；l: 已领取未授权；a: 已授权在途。
	l := mustLeaseOne(t, svc, id, "w-l")
	if l.Recipient != "p@x.com" {
		t.Fatalf("order %s", l.Recipient)
	}
	cancelled, changed, err := svc.Cancel(id)
	if err != nil || !changed || cancelled.Status != CampaignCanceled {
		t.Fatalf("first cancel: %+v changed=%v err=%v", cancelled, changed, err)
	}
	// 重复取消幂等：changed=false，campaign.canceled_at 稳定。
	again, changed2, err := svc.Cancel(id)
	if err != nil || changed2 || again.Status != CampaignCanceled || !again.CanceledAt.Equal(cancelled.CanceledAt) {
		t.Fatalf("idempotent cancel broken: %+v changed=%v err=%v", again, changed2, err)
	}

	// 取消后不能再领取。
	if _, err := svc.Lease(id, LeaseRequest{WorkerID: "w"}); !IsCode(err, ErrCampaignNotReady) {
		t.Fatalf("lease after cancel must fail, got %v", err)
	}
	// 已领取未授权的在途项：授权点拒绝，原因 campaign_canceled。
	dec, err := svc.Authorize(id, l.DispatchKey, l.LeaseToken)
	if err != nil {
		t.Fatalf("authorize after cancel should persist denial, got err %v", err)
	}
	if dec.Granted || dec.Reason != ReasonCampaignCanceled {
		t.Fatalf("expected canceled denial, got %+v", dec)
	}

	// 已授权在途的成功回执仍被如实记录（邮件可能已发出）。
	// 重新构造该场景：新活动先授权再取消。
	id2 := mustStart(t, svc, "tpl-1", "a2@x.com")
	a := mustLeaseOne(t, svc, id2, "w-a")
	da, _ := svc.Authorize(id2, a.DispatchKey, a.LeaseToken)
	if !da.Granted {
		t.Fatal("grant")
	}
	if _, _, err := svc.Cancel(id2); err != nil {
		t.Fatalf("cancel 2: %v", err)
	}
	ack, err := svc.SubmitReceipt(id2, a.DispatchKey, a.LeaseToken, ReceiptInput{Result: ResultSuccess})
	if err != nil || ack.State != TaskSent {
		t.Fatalf("in-flight granted receipt after cancel should record truth: %+v err=%v", ack, err)
	}
	// 新租约尝试在授权点被拒。
	clk.advance(2 * time.Minute)
	if _, err := svc.Lease(id2, LeaseRequest{WorkerID: "w-x"}); !IsCode(err, ErrCampaignNotReady) {
		t.Fatalf("no new leases after cancel, got %v", err)
	}

	st, _ := svc.Stats(id)
	if st.Canceled != 3 || st.Sent != 0 || st.Status != CampaignCanceled {
		t.Fatalf("canceled stats: %+v", st)
	}
	// 再查一次统计，数字稳定。
	st2, _ := svc.Stats(id)
	if st2 != st {
		t.Fatalf("stats must be stable across reads: %+v vs %+v", st2, st)
	}
}

// TestSuppressionValidation 抑制事件入参校验。
func TestSuppressionValidation(t *testing.T) {
	svc, _ := newTestService()
	if _, err := svc.RecordSuppression(SuppressionInput{Type: SuppressionGlobalUnsubscribe}); !IsCode(err, ErrValidation) {
		t.Fatalf("global without address: %v", err)
	}
	if _, err := svc.RecordSuppression(SuppressionInput{Type: SuppressionCampaign, Address: "a@x.com"}); !IsCode(err, ErrValidation) {
		t.Fatalf("campaign event without campaign: %v", err)
	}
	if _, err := svc.RecordSuppression(SuppressionInput{Type: "unknown", Address: "a@x.com"}); !IsCode(err, ErrValidation) {
		t.Fatalf("unknown type: %v", err)
	}
	if _, err := svc.RecordSuppression(SuppressionInput{Type: SuppressionCampaign, CampaignID: "cmp_nope", Address: "a@x.com"}); !IsCode(err, ErrNotFound) {
		t.Fatalf("missing campaign: %v", err)
	}
}

// TestLeaseOrderingAndBatch 领取顺序与批量上限。
func TestLeaseOrderingAndBatch(t *testing.T) {
	svc, _ := newTestService()
	id := mustStart(t, svc, "tpl-1", "a@x.com", "b@x.com", "c@x.com")
	tasks, err := svc.Lease(id, LeaseRequest{WorkerID: "w", Count: 10})
	if err != nil || len(tasks) != 3 {
		t.Fatalf("lease batch: %d err=%v", len(tasks), err)
	}
	if tasks[0].Recipient != "a@x.com" || tasks[2].Recipient != "c@x.com" {
		t.Fatalf("lease should follow frozen order: %+v", tasks)
	}
	if more, _ := svc.Lease(id, LeaseRequest{WorkerID: "w2"}); len(more) != 0 {
		t.Fatalf("nothing else ready, got %+v", more)
	}
}
