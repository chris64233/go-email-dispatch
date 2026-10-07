package emaildispatch

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func freezeRule(id string, addrs []string, ttl time.Duration) RetentionRuleInput {
	return RetentionRuleInput{
		RuleID: id, Action: RetentionFreeze, Scope: addrs,
		Reason: "legal hold L-1", TTL: ttl,
	}
}

func retainRule(id string, addrs []string, ttl time.Duration) RetentionRuleInput {
	return RetentionRuleInput{
		RuleID: id, Action: RetentionRetain, Scope: addrs,
		Reason: "retain evidence R-1", TTL: ttl,
	}
}

// TestRetentionRuleValidation 覆盖规则边界校验。
func TestRetentionRuleValidation(t *testing.T) {
	svc, _ := newTestService()
	id := mustStart(t, svc, "tpl-1", "a@x.com")

	cases := []struct {
		name string
		in   RetentionRuleInput
	}{
		{"missing rule id", RetentionRuleInput{Action: RetentionFreeze, Scope: []string{"a@x.com"}, Reason: "x", TTL: time.Hour}},
		{"bad action", RetentionRuleInput{RuleID: "r", Action: "explode", Scope: []string{"a@x.com"}, Reason: "x", TTL: time.Hour}},
		{"empty scope", RetentionRuleInput{RuleID: "r", Action: RetentionFreeze, Reason: "x", TTL: time.Hour}},
		{"bad address", RetentionRuleInput{RuleID: "r", Action: RetentionFreeze, Scope: []string{"not-mail"}, Reason: "x", TTL: time.Hour}},
		{"dup address", RetentionRuleInput{RuleID: "r", Action: RetentionFreeze, Scope: []string{"a@x.com", " A@X.COM "}, Reason: "x", TTL: time.Hour}},
		{"missing reason", RetentionRuleInput{RuleID: "r", Action: RetentionFreeze, Scope: []string{"a@x.com"}, TTL: time.Hour}},
		{"no validity", RetentionRuleInput{RuleID: "r", Action: RetentionFreeze, Scope: []string{"a@x.com"}, Reason: "x"}},
	}
	for _, tc := range cases {
		if _, err := svc.UpsertRetentionRule(id, tc.in); !IsCode(err, ErrValidation) {
			t.Fatalf("%s: expected validation_error, got %v", tc.name, err)
		}
	}

	past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := svc.UpsertRetentionRule(id, RetentionRuleInput{
		RuleID: "r1", Action: RetentionFreeze, Scope: []string{"a@x.com"}, Reason: "x",
		EffectiveAt: past.Add(time.Hour), ExpiresAt: past,
	}); !IsCode(err, ErrValidation) {
		t.Fatalf("expected validation for inverted window, got %v", err)
	}
	if _, err := svc.UpsertRetentionRule(id, RetentionRuleInput{
		RuleID: "r2", Action: RetentionFreeze, Scope: []string{"a@x.com"}, Reason: "x",
		TTL: time.Hour, ExpiresAt: time.Now().Add(2 * time.Hour),
	}); !IsCode(err, ErrValidation) {
		t.Fatalf("expected validation for ttl+expires, got %v", err)
	}
	if _, err := svc.UpsertRetentionRule("cmp_nope", freezeRule("r", []string{"a@x.com"}, time.Hour)); !IsCode(err, ErrNotFound) {
		t.Fatalf("expected not_found for unknown campaign, got %v", err)
	}
}

// TestRetentionRuleIdempotencyAndConflict 覆盖相同规则号重复提交的幂等与冲突语义。
func TestRetentionRuleIdempotencyAndConflict(t *testing.T) {
	svc, clk := newTestService()
	id := mustStart(t, svc, "tpl-1", "a@x.com", "b@x.com")

	ack, err := svc.UpsertRetentionRule(id, freezeRule("R-100", []string{"a@x.com", "b@x.com"}, time.Hour))
	if err != nil || !ack.Created {
		t.Fatalf("first submit should create: ack=%+v err=%v", ack, err)
	}
	first := ack.Rule

	// 地址规范化后语义相同（大小写/空白差异）仍是同一份内容 -> 返回原记录。
	again, err := svc.UpsertRetentionRule(id, RetentionRuleInput{
		RuleID: "R-100", Action: RetentionFreeze, Scope: []string{" A@X.COM ", "b@x.com"},
		Reason: "legal hold L-1", EffectiveAt: first.EffectiveAt, ExpiresAt: first.ExpiresAt,
	})
	if err != nil || again.Created || !again.Rule.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("identical resubmit must return original: ack=%+v err=%v", again, err)
	}

	// 范围变化 -> 冲突。
	if _, err := svc.UpsertRetentionRule(id, freezeRule("R-100", []string{"a@x.com"}, time.Hour)); !IsCode(err, ErrConflict) {
		t.Fatalf("scope change must conflict, got %v", err)
	}
	// 动作变化 -> 冲突。
	if _, err := svc.UpsertRetentionRule(id, retainRule("R-100", []string{"a@x.com", "b@x.com"}, time.Hour)); !IsCode(err, ErrConflict) {
		t.Fatalf("action change must conflict, got %v", err)
	}
	// 原因变化 -> 冲突。
	badReason := freezeRule("R-100", []string{"a@x.com", "b@x.com"}, time.Hour)
	badReason.Reason = "other reason"
	if _, err := svc.UpsertRetentionRule(id, badReason); !IsCode(err, ErrConflict) {
		t.Fatalf("reason change must conflict, got %v", err)
	}
	// 有效期变化 -> 冲突。
	changedTTL := freezeRule("R-100", []string{"a@x.com", "b@x.com"}, 2*time.Hour)
	changedTTL.EffectiveAt = first.EffectiveAt
	if _, err := svc.UpsertRetentionRule(id, changedTTL); !IsCode(err, ErrConflict) {
		t.Fatalf("validity change must conflict, got %v", err)
	}

	rules, err := svc.ListRetentionRules(id)
	if err != nil || len(rules) != 1 || rules[0].RuleID != "R-100" {
		t.Fatalf("list rules: %+v err=%v", rules, err)
	}
	if _, err := svc.GetRetentionRule(id, "missing"); !IsCode(err, ErrNotFound) {
		t.Fatalf("unknown rule should be not_found, got %v", err)
	}

	// 规则失效后状态转为 expired，仍可在历史查询中看到。
	clk.advance(61 * time.Minute)
	exp, err := svc.GetRetentionRule(id, "R-100")
	if err != nil || exp.Status != RuleExpired {
		t.Fatalf("rule should be expired: %+v err=%v", exp, err)
	}
}

// TestFreezeBlocksUnauthorizedAndReleaseRestores 覆盖冻结拦截、解除恢复与 held 统计。
func TestFreezeBlocksUnauthorizedAndReleaseRestores(t *testing.T) {
	svc, _ := newTestService()
	id := mustStart(t, svc, "tpl-1", "a@x.com", "b@x.com")

	if _, err := svc.UpsertRetentionRule(id, freezeRule("F-1", []string{"a@x.com"}, time.Hour)); err != nil {
		t.Fatalf("freeze: %v", err)
	}

	// 冻结的收件人尚未授权 -> held；批次只能领取未冻结的 b。
	tasks, err := svc.Lease(id, LeaseRequest{WorkerID: "w", Count: 10, LeaseDuration: time.Minute})
	if err != nil || len(tasks) != 1 || tasks[0].Recipient != "b@x.com" {
		t.Fatalf("only unfrozen recipient should be leased, got %+v err=%v", tasks, err)
	}
	st, _ := svc.Stats(id)
	if st.Held != 1 || st.Pending != 0 {
		t.Fatalf("expected 1 held, got %+v", st)
	}

	if _, changed, err := svc.ReleaseRetentionRule(id, "F-1"); err != nil || !changed {
		t.Fatalf("release: changed=%v err=%v", changed, err)
	}
	if _, changed, err := svc.ReleaseRetentionRule(id, "F-1"); err != nil || changed {
		t.Fatalf("duplicate release must be idempotent: changed=%v err=%v", changed, err)
	}
	task := mustLeaseOne(t, svc, id, "w2")
	if task.Recipient != "a@x.com" {
		t.Fatalf("released recipient should be leasable, got %s", task.Recipient)
	}
	dec, err := svc.Authorize(id, task.DispatchKey, task.LeaseToken)
	if err != nil || !dec.Granted {
		t.Fatalf("released recipient should authorize: %+v err=%v", dec, err)
	}
}

// TestFreezeOnLeasedTaskRejectedAtAuthorize 覆盖在途未授权邮件：授权点按当前规则拒绝；
// 迟到的旧工作者只能拿到稳定的拒绝结论，解除后新 attempt 才能放行。
func TestFreezeOnLeasedTaskRejectedAtAuthorize(t *testing.T) {
	svc, _ := newTestService()
	id := mustStart(t, svc, "tpl-1", "a@x.com")
	task := mustLeaseOne(t, svc, id, "w1")

	if _, err := svc.UpsertRetentionRule(id, freezeRule("F-2", []string{"a@x.com"}, time.Hour)); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	dec, err := svc.Authorize(id, task.DispatchKey, task.LeaseToken)
	if err != nil || dec.Granted || dec.Reason != ReasonComplianceHeld || dec.Basis == nil || dec.Basis.RuleID != "F-2" {
		t.Fatalf("authorize must be denied by freeze: %+v err=%v", dec, err)
	}

	// 旧工作者重试授权只能拿到同一个持久化拒绝结论。
	again, err := svc.Authorize(id, task.DispatchKey, task.LeaseToken)
	if err != nil || again.ID != dec.ID || again.Granted {
		t.Fatalf("late worker must see stable denial: %+v err=%v", again, err)
	}
	if _, err := svc.SubmitReceipt(id, task.DispatchKey, task.LeaseToken, ReceiptInput{Result: ResultSuccess}); !IsCode(err, ErrInvalidState) {
		t.Fatalf("receipt after freeze denial must be rejected, got %v", err)
	}

	// 解除后用新 attempt 重新领取，新工作者放行，旧 attempt 决定保持拒绝。
	if _, _, err := svc.ReleaseRetentionRule(id, "F-2"); err != nil {
		t.Fatalf("release: %v", err)
	}
	next := mustLeaseOne(t, svc, id, "w2")
	if next.Attempt != 2 {
		t.Fatalf("expected attempt 2, got %d", next.Attempt)
	}
	dec2, err := svc.Authorize(id, next.DispatchKey, next.LeaseToken)
	if err != nil || !dec2.Granted {
		t.Fatalf("fresh attempt should be granted: %+v err=%v", dec2, err)
	}
}

// TestAlreadyAuthorizedRetainsDecisionUnderRuleChange 覆盖已授权邮件保留原决定：
// 授权后录入的冻结/保留规则不回滚发送。
func TestAlreadyAuthorizedRetainsDecisionUnderRuleChange(t *testing.T) {
	svc, clk := newTestService()
	id := mustStart(t, svc, "tpl-1", "a@x.com")
	task := mustLeaseOne(t, svc, id, "w1")
	dec, _ := svc.Authorize(id, task.DispatchKey, task.LeaseToken)
	if !dec.Granted {
		t.Fatal("precondition: granted")
	}

	clk.advance(time.Second) // 规则确实在授权时刻之后录入
	if _, err := svc.UpsertRetentionRule(id, freezeRule("F-3", []string{"a@x.com"}, time.Hour)); err != nil {
		t.Fatalf("freeze after authorize: %v", err)
	}
	// 重复授权返回原决定（granted 不变、依据为授权时快照：nil）。
	again, err := svc.Authorize(id, task.DispatchKey, task.LeaseToken)
	if err != nil || again.ID != dec.ID || !again.Granted || again.Basis != nil {
		t.Fatalf("existing grant must be retained: %+v err=%v", again, err)
	}
	ack, err := svc.SubmitReceipt(id, task.DispatchKey, task.LeaseToken, ReceiptInput{Result: ResultSuccess})
	if err != nil || ack.State != TaskSent {
		t.Fatalf("authorized mail sends despite later freeze: %+v err=%v", ack, err)
	}
	// 回执审计解释授权后生效的规则。
	if len(ack.Outcome.Audit) == 0 || !strings.Contains(ack.Outcome.Audit[0], "F-3") {
		t.Fatalf("expected post-authorization rule audit, got %+v", ack.Outcome.Audit)
	}
	st, _ := svc.Stats(id)
	if st.Sent != 1 {
		t.Fatalf("completed send must not be rolled back, stats=%+v", st)
	}
}

// TestRetainRuleAttachesImmutableBasis 覆盖保留依据随新邮件生成、授权时快照且不可变。
func TestRetainRuleAttachesImmutableBasis(t *testing.T) {
	svc, _ := newTestService()
	id := mustStart(t, svc, "tpl-1", "a@x.com", "b@x.com")
	if _, err := svc.UpsertRetentionRule(id, retainRule("R-1", []string{"a@x.com"}, 2*time.Hour)); err != nil {
		t.Fatalf("retain: %v", err)
	}

	task := mustLeaseOne(t, svc, id, "w")
	if task.Recipient != "a@x.com" || task.Basis == nil || task.Basis.RuleID != "R-1" || task.Basis.Action != RetentionRetain {
		t.Fatalf("new mail must carry basis: %+v", task)
	}
	dec, _ := svc.Authorize(id, task.DispatchKey, task.LeaseToken)
	if !dec.Granted || dec.Basis == nil || dec.Basis.RuleID != "R-1" {
		t.Fatalf("granted decision must snapshot basis: %+v", dec)
	}
	if _, _, err := svc.ReleaseRetentionRule(id, "R-1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	ack, err := svc.SubmitReceipt(id, task.DispatchKey, task.LeaseToken, ReceiptInput{Result: ResultSuccess})
	if err != nil || ack.State != TaskSent {
		t.Fatalf("authorized mail should still send: %+v err=%v", ack, err)
	}

	taskB := mustLeaseOne(t, svc, id, "w")
	if taskB.Recipient != "b@x.com" || taskB.Basis != nil {
		t.Fatalf("unmatched recipient carries no basis: %+v", taskB)
	}
}

// TestLeaseBatchesNeverMixBases 覆盖新旧决定不能混在同一批次：单次领取只含同一依据。
func TestLeaseBatchesNeverMixBases(t *testing.T) {
	svc, _ := newTestService()
	id := mustStart(t, svc, "tpl-1", "a@x.com", "b@x.com", "c@x.com", "d@x.com")
	if _, err := svc.UpsertRetentionRule(id, retainRule("R-9", []string{"c@x.com", "d@x.com"}, time.Hour)); err != nil {
		t.Fatalf("retain: %v", err)
	}

	batch1, err := svc.Lease(id, LeaseRequest{WorkerID: "w", Count: 10, LeaseDuration: time.Minute})
	if err != nil || len(batch1) != 2 {
		t.Fatalf("batch1 should stop at first basis change: %d err=%v", len(batch1), err)
	}
	for _, tk := range batch1 {
		if tk.Basis != nil {
			t.Fatalf("batch1 must be the no-basis cohort, got %+v", tk)
		}
	}
	batch2, err := svc.Lease(id, LeaseRequest{WorkerID: "w", Count: 10, LeaseDuration: time.Minute})
	if err != nil || len(batch2) != 2 {
		t.Fatalf("batch2 should contain only basis cohort: %d err=%v", len(batch2), err)
	}
	for _, tk := range batch2 {
		if tk.Basis == nil || tk.Basis.RuleID != "R-9" {
			t.Fatalf("batch2 must share the same basis, got %+v", tk)
		}
	}
}

// TestFreezeExpiryReleasesHeldAndKeepsHistory 覆盖规则到期：held 项恢复且不影响历史。
func TestFreezeExpiryReleasesHeldAndHistory(t *testing.T) {
	svc, clk := newTestService()
	id := mustStart(t, svc, "tpl-1", "a@x.com", "b@x.com")

	// a 先完成发送（规则生效之前），不应被之后失效的规则影响。
	taskA := mustLeaseOne(t, svc, id, "w")
	if dec, err := svc.Authorize(id, taskA.DispatchKey, taskA.LeaseToken); err != nil || !dec.Granted {
		t.Fatalf("authorize a: %v", err)
	}
	if _, err := svc.SubmitReceipt(id, taskA.DispatchKey, taskA.LeaseToken, ReceiptInput{Result: ResultSuccess}); err != nil {
		t.Fatalf("receipt a: %v", err)
	}

	if _, err := svc.UpsertRetentionRule(id, freezeRule("F-TTL", []string{"b@x.com"}, time.Minute)); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if tasks, _ := svc.Lease(id, LeaseRequest{WorkerID: "w", Count: 10, LeaseDuration: time.Minute}); len(tasks) != 0 {
		t.Fatalf("frozen b must not lease, got %d", len(tasks))
	}

	clk.advance(61 * time.Second)
	taskB := mustLeaseOne(t, svc, id, "w2")
	if taskB.Recipient != "b@x.com" {
		t.Fatalf("b should be releasable after expiry, got %s", taskB.Recipient)
	}
	st, _ := svc.Stats(id)
	if st.Held != 0 || st.Sent != 1 {
		t.Fatalf("expiry must release held and keep completed send: %+v", st)
	}
	rules, _ := svc.ListRetentionRules(id)
	if len(rules) != 1 || rules[0].Status != RuleExpired {
		t.Fatalf("expired rule remains visible in history: %+v", rules)
	}
}

// TestDispatchDetailShowsRuleAndTemplate 覆盖审计查询：活动、收件人、当前规则、模板版本、保留原因。
func TestDispatchDetailShowsRuleAndTemplate(t *testing.T) {
	svc, _ := newTestService()
	id := mustStart(t, svc, "tpl-77", "a@x.com")
	if _, err := svc.UpsertRetentionRule(id, retainRule("R-VIEW", []string{"a@x.com"}, time.Hour)); err != nil {
		t.Fatalf("retain: %v", err)
	}
	task := mustLeaseOne(t, svc, id, "w")
	dec, _ := svc.Authorize(id, task.DispatchKey, task.LeaseToken)

	detail, err := svc.GetDispatch(id, task.DispatchKey)
	if err != nil {
		t.Fatalf("get dispatch: %v", err)
	}
	if detail.CampaignID != id || detail.TemplateVersion != "tpl-77" || detail.Recipient.Address != "a@x.com" {
		t.Fatalf("detail missing campaign/template/recipient: %+v", detail)
	}
	if detail.CurrentBasis == nil || detail.CurrentBasis.RuleID != "R-VIEW" || detail.CurrentBasis.Reason != "retain evidence R-1" {
		t.Fatalf("detail must show current rule and reason: %+v", detail.CurrentBasis)
	}
	if len(detail.Attempts) != 1 || detail.Attempts[0].Decision == nil || detail.Attempts[0].Decision.Basis == nil {
		t.Fatalf("attempt decision must carry frozen basis: %+v", detail.Attempts)
	}
	if detail.Attempts[0].Decision.ID != dec.ID {
		t.Fatal("decision id mismatch")
	}
}

// TestFreezeReleaseAndAuthorizeRace 覆盖冻结/解除/授权并发：同一收件人最终只有一个明确结果。
func TestFreezeReleaseAndAuthorizeRace(t *testing.T) {
	svc, _ := newTestService()
	id := mustStart(t, svc, "tpl-1", "a@x.com")

	var wg sync.WaitGroup
	run := func(fn func()) {
		wg.Add(1)
		go func() { defer wg.Done(); fn() }()
	}
	rule := func() {
		_, _ = svc.UpsertRetentionRule(id, freezeRule("F-RACE", []string{"a@x.com"}, time.Hour))
	}
	release := func() { _, _, _ = svc.ReleaseRetentionRule(id, "F-RACE") }
	leaseAuth := func(out chan<- bool) {
		tasks, err := svc.Lease(id, LeaseRequest{WorkerID: "w", Count: 1, LeaseDuration: time.Hour})
		if err != nil || len(tasks) == 0 {
			return
		}
		dec, err := svc.Authorize(id, tasks[0].DispatchKey, tasks[0].LeaseToken)
		if err == nil && dec.Granted {
			out <- true
		}
	}
	granted := make(chan bool, 8)
	for i := 0; i < 32; i++ {
		run(rule)
		run(release)
		run(func() { leaseAuth(granted) })
		run(rule)
	}
	wg.Wait()
	close(granted)

	// 无论竞态如何，最终一致：解除后该收件人可以完成且仅完成一次发送。
	var task LeasedTask
	for {
		tasks, err := svc.Lease(id, LeaseRequest{WorkerID: "closer", Count: 1, LeaseDuration: time.Hour})
		if err != nil || len(tasks) == 0 {
			break
		}
		task = tasks[0]
		dec, err := svc.Authorize(id, task.DispatchKey, task.LeaseToken)
		if err == nil && dec.Granted {
			if _, err := svc.SubmitReceipt(id, task.DispatchKey, task.LeaseToken, ReceiptInput{Result: ResultSuccess}); err != nil {
				t.Fatalf("receipt: %v", err)
			}
		}
	}
	st, _ := svc.Stats(id)
	if st.Sent > 1 {
		t.Fatalf("recipient must have at most one explicit result, sent=%d", st.Sent)
	}
}

// TestAuditLoggerMasksAddresses 覆盖敏感地址不出现在普通日志中。
func TestAuditLoggerMasksAddresses(t *testing.T) {
	if got := MaskAddress("Alice@Example.COM"); got != "a****@example.com" {
		t.Fatalf("mask = %q", got)
	}
	if got := MaskAddress("garbage"); got != "****" {
		t.Fatalf("mask invalid = %q", got)
	}

	ml := NewMemoryAuditLogger()
	svc, _ := newTestService()
	svc.WithAuditLogger(ml)
	id := mustStart(t, svc, "tpl-1", "secret@example.com")
	if _, err := svc.UpsertRetentionRule(id, freezeRule("F-LOG", []string{"secret@example.com"}, time.Hour)); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if _, _, err := svc.ReleaseRetentionRule(id, "F-LOG"); err != nil {
		t.Fatalf("release: %v", err)
	}
	for _, line := range ml.Lines() {
		if strings.Contains(line, "secret@example.com") {
			t.Fatalf("sensitive address leaked into audit log: %q", line)
		}
		if !strings.Contains(line, "s****@example.com") && strings.Contains(line, "scope=") {
			t.Fatalf("audit line should contain masked address: %q", line)
		}
	}
}

// TestFreezeCancelsWithCampaign 覆盖活动取消时 held 项一并终止。
func TestFreezeCancelsWithCampaign(t *testing.T) {
	svc, _ := newTestService()
	id := mustStart(t, svc, "tpl-1", "a@x.com")
	if _, err := svc.UpsertRetentionRule(id, freezeRule("F-C", []string{"a@x.com"}, time.Hour)); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if _, _, err := svc.Cancel(id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	st, _ := svc.Stats(id)
	if st.Held != 0 || st.Canceled != 1 {
		t.Fatalf("held task should be canceled with campaign: %+v", st)
	}
}
