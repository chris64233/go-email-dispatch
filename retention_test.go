package emaildispatch

import (
	"bytes"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

func freezeRule(no string, addrs ...string) RetentionRuleInput {
	return RetentionRuleInput{
		RuleNo:    no,
		Action:    RetentionActionFreeze,
		Addresses: addrs,
		Reason:    "ops freeze",
		ExpiresAt: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC),
	}
}

// TestRetentionFreezeBlocksAuthorizeAndLiftRejudges 覆盖冻结拦截、解除后重判、
// 以及迟到旧工作者不得绕过当前规则。
func TestRetentionFreezeBlocksAuthorizeAndLiftRejudges(t *testing.T) {
	svc, clk := newTestService()
	id := mustStart(t, svc, "tpl-1", "a@x.com", "b@x.com")

	if _, err := svc.SubmitRetentionRule(id, freezeRule("R1", "a@x.com")); err != nil {
		t.Fatalf("submit rule: %v", err)
	}

	task := mustLeaseOne(t, svc, id, "w1") // a@x.com（按地址序）
	if task.Recipient != "a@x.com" {
		t.Fatalf("expected first task a@x.com, got %s", task.Recipient)
	}
	dec, err := svc.Authorize(id, task.DispatchKey, task.LeaseToken)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if dec.Granted || dec.Reason != ReasonRetentionFrozen || dec.Retention == nil {
		t.Fatalf("expected frozen denial with rule snapshot, got %+v", dec)
	}
	// 冻结拒绝后不得发送。
	if _, err := svc.SubmitReceipt(id, task.DispatchKey, task.LeaseToken, ReceiptInput{Result: ResultSuccess}); !IsCode(err, ErrInvalidState) {
		t.Fatalf("frozen task must reject receipt, got %v", err)
	}
	st, _ := svc.Stats(id)
	if st.Frozen != 1 {
		t.Fatalf("expected 1 frozen, got %+v", st)
	}

	// 解除冻结：旧租约已随授权落库，重新领取产生新 attempt 并重新判断。
	if _, changed, err := svc.LiftRetentionRule(id, "R1"); err != nil || !changed {
		t.Fatalf("lift: changed=%v err=%v", changed, err)
	}
	if _, changed, err := svc.LiftRetentionRule(id, "R1"); err != nil || changed {
		t.Fatalf("re-lift must be idempotent: changed=%v err=%v", changed, err)
	}
	clk.advance(2 * time.Minute) // 旧租约过期
	tasks, err := svc.Lease(id, LeaseRequest{WorkerID: "w2", Count: 1, LeaseDuration: time.Minute})
	if err != nil || len(tasks) != 1 {
		t.Fatalf("re-lease after lift: %v %v", tasks, err)
	}
	nt := tasks[0]
	if nt.Attempt != 2 || nt.RuleEpoch <= task.RuleEpoch {
		t.Fatalf("expected new attempt and advanced epoch, got %+v vs %+v", nt, task)
	}
	// 迟到的旧工作者：回执被 fencing 拒绝，授权只返回旧的持久化拒绝决定。
	if _, err := svc.SubmitReceipt(id, task.DispatchKey, task.LeaseToken, ReceiptInput{Result: ResultSuccess}); !IsCode(err, ErrStaleAttempt) {
		t.Fatalf("stale worker receipt must be rejected, got %v", err)
	}
	old, err := svc.Authorize(id, task.DispatchKey, task.LeaseToken)
	if err != nil || old.Granted || old.Reason != ReasonRetentionFrozen {
		t.Fatalf("old decision must stay immutable, got %+v %v", old, err)
	}
	dec2, err := svc.Authorize(id, nt.DispatchKey, nt.LeaseToken)
	if err != nil || !dec2.Granted {
		t.Fatalf("after lift authorize must grant, got %+v %v", dec2, err)
	}
	if _, err := svc.SubmitReceipt(id, nt.DispatchKey, nt.LeaseToken, ReceiptInput{Result: ResultSuccess}); err != nil {
		t.Fatalf("receipt after lift: %v", err)
	}
}

// TestRetentionRuleBoundaries 覆盖有效期边界：生效前/失效后不拦截，
// 规则失效后不影响已完成发送。
func TestRetentionRuleBoundaries(t *testing.T) {
	svc, clk := newTestService()
	id := mustStart(t, svc, "tpl-1", "a@x.com", "b@x.com", "c@x.com")

	in := freezeRule("R1", "a@x.com", "b@x.com", "c@x.com")
	in.EffectiveAt = clk.now().Add(time.Hour) // 尚未生效
	in.ExpiresAt = clk.now().Add(2 * time.Hour)
	if _, err := svc.SubmitRetentionRule(id, in); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// 生效前：放行。
	t1 := mustLeaseOne(t, svc, id, "w1")
	if d, err := svc.Authorize(id, t1.DispatchKey, t1.LeaseToken); err != nil || !d.Granted {
		t.Fatalf("before effective must grant, got %+v %v", d, err)
	}
	if _, err := svc.SubmitReceipt(id, t1.DispatchKey, t1.LeaseToken, ReceiptInput{Result: ResultSuccess}); err != nil {
		t.Fatalf("receipt: %v", err)
	}

	// 生效后：冻结。
	clk.advance(time.Hour)
	t2 := mustLeaseOne(t, svc, id, "w1")
	if d, _ := svc.Authorize(id, t2.DispatchKey, t2.LeaseToken); d.Granted || d.Reason != ReasonRetentionFrozen {
		t.Fatalf("within validity must freeze, got %+v", d)
	}

	// 失效后：重判放行；已完成的发送（t1）保持 sent 不受影响。
	clk.advance(time.Hour)
	tasks, err := svc.Lease(id, LeaseRequest{WorkerID: "w2", Count: 2, LeaseDuration: time.Minute})
	if err != nil || len(tasks) != 2 {
		t.Fatalf("lease after expiry: %v %v", tasks, err)
	}
	for _, tk := range tasks {
		if d, err := svc.Authorize(id, tk.DispatchKey, tk.LeaseToken); err != nil || !d.Granted {
			t.Fatalf("after expiry must grant, got %+v %v", d, err)
		}
	}
	st, _ := svc.Stats(id)
	if st.Sent != 1 || st.Frozen != 0 {
		t.Fatalf("expired rule must not affect finished sends: %+v", st)
	}
}

// TestRetentionRuleIdempotency 覆盖相同规则号重复提交返回原记录、
// 范围或有效期变化返回冲突。
func TestRetentionRuleIdempotency(t *testing.T) {
	svc, _ := newTestService()
	id := mustStart(t, svc, "tpl-1", "a@x.com")

	ack1, err := svc.SubmitRetentionRule(id, freezeRule("R1", "a@x.com"))
	if err != nil || ack1.Duplicate {
		t.Fatalf("first submit: %+v %v", ack1, err)
	}
	ack2, err := svc.SubmitRetentionRule(id, freezeRule("R1", "a@x.com"))
	if err != nil || !ack2.Duplicate {
		t.Fatalf("duplicate submit must return original, got %+v %v", ack2, err)
	}
	if ack2.Rule.ID != ack1.Rule.ID || !ack2.Rule.CreatedAt.Equal(ack1.Rule.CreatedAt) {
		t.Fatalf("duplicate must be the original record: %+v vs %+v", ack2.Rule, ack1.Rule)
	}

	// 范围变化 -> 冲突。
	bad := freezeRule("R1", "a@x.com", "b@x.com")
	if _, err := svc.SubmitRetentionRule(id, bad); !IsCode(err, ErrConflict) {
		t.Fatalf("scope change must conflict, got %v", err)
	}
	// 有效期变化 -> 冲突。
	bad2 := freezeRule("R1", "a@x.com")
	bad2.ExpiresAt = bad2.ExpiresAt.Add(time.Hour)
	if _, err := svc.SubmitRetentionRule(id, bad2); !IsCode(err, ErrConflict) {
		t.Fatalf("validity change must conflict, got %v", err)
	}
	// 不同规则号正常落库。
	if _, err := svc.SubmitRetentionRule(id, freezeRule("R2", "a@x.com")); err != nil {
		t.Fatalf("distinct rule_no must succeed: %v", err)
	}
	// 校验：缺有效期、缺原因。
	if _, err := svc.SubmitRetentionRule(id, RetentionRuleInput{RuleNo: "R3", Action: RetentionActionFreeze, Reason: "x", Addresses: []string{"a@x.com"}}); !IsCode(err, ErrValidation) {
		t.Fatalf("missing expires_at must fail validation, got %v", err)
	}
	if _, err := svc.SubmitRetentionRule(id, RetentionRuleInput{RuleNo: "R3", Action: RetentionActionFreeze, ExpiresAt: time.Now().Add(time.Hour), Addresses: []string{"a@x.com"}}); !IsCode(err, ErrValidation) {
		t.Fatalf("missing reason must fail validation, got %v", err)
	}
}

// TestRetentionRetainBasisAndReport 覆盖 retain 依据随授权落库、
// 已授权邮件保留原决定，以及历史查询视图。
func TestRetentionRetainBasisAndReport(t *testing.T) {
	svc, _ := newTestService()
	id := mustStart(t, svc, "tpl-9", "a@x.com", "b@x.com")

	// 先授权 a（无规则），再落 retain 规则：a 保留原决定，b 重判并携带依据。
	ta := mustLeaseOne(t, svc, id, "w1")
	da, err := svc.Authorize(id, ta.DispatchKey, ta.LeaseToken)
	if err != nil || !da.Granted || len(da.Basis) != 0 {
		t.Fatalf("pre-rule grant: %+v %v", da, err)
	}

	rule := RetentionRuleInput{
		RuleNo:        "KEEP-1",
		Action:        RetentionActionRetain,
		AllRecipients: true,
		Reason:        "legal hold 2026",
		ExpiresAt:     time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC),
	}
	if _, err := svc.SubmitRetentionRule(id, rule); err != nil {
		t.Fatalf("submit retain: %v", err)
	}

	tb := mustLeaseOne(t, svc, id, "w1")
	db, err := svc.Authorize(id, tb.DispatchKey, tb.LeaseToken)
	if err != nil || !db.Granted {
		t.Fatalf("retain must still grant, got %+v %v", db, err)
	}
	if len(db.Basis) != 1 || db.Basis[0].RuleNo != "KEEP-1" ||
		db.Basis[0].Reason != "legal hold 2026" || db.Basis[0].TemplateVersion != "tpl-9" {
		t.Fatalf("basis must carry rule_no/reason/template version, got %+v", db.Basis)
	}

	// 历史查询：活动、收件人、当前规则、模板版本、保留原因。
	rep, err := svc.RetentionReport(id)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if rep.TemplateVersion != "tpl-9" || len(rep.Rules) != 1 || len(rep.Recipients) != 2 {
		t.Fatalf("report incomplete: %+v", rep)
	}
	for _, v := range rep.Recipients {
		if v.TemplateVersion != "tpl-9" || len(v.Rules) != 1 || v.Rules[0].Reason != "legal hold 2026" {
			t.Fatalf("recipient view missing rule/reason: %+v", v)
		}
	}

	// 投递项审计中也能看到持久化依据；已授权的 a 决策不变。
	detail, err := svc.GetDispatch(id, tb.DispatchKey)
	if err != nil || detail.Attempts[0].Decision == nil || len(detail.Attempts[0].Decision.Basis) != 1 {
		t.Fatalf("dispatch detail must persist basis: %+v %v", detail, err)
	}
	da2, err := svc.Authorize(id, ta.DispatchKey, ta.LeaseToken)
	if err != nil || da2.ID != da.ID || len(da2.Basis) != 0 {
		t.Fatalf("authorized mail keeps original decision: %+v %v", da2, err)
	}
}

// TestConcurrentLiftAndAuthorize 解除冻结与发送授权并发：
// 同一收件人只留下一个明确结果，不存在中间态。
func TestConcurrentLiftAndAuthorize(t *testing.T) {
	svc, clk := newTestService()
	id := mustStart(t, svc, "tpl-1", "a@x.com")
	if _, err := svc.SubmitRetentionRule(id, freezeRule("R1", "a@x.com")); err != nil {
		t.Fatalf("submit: %v", err)
	}
	task := mustLeaseOne(t, svc, id, "w1")

	var wg sync.WaitGroup
	var granted, frozen int64
	var mu sync.Mutex
	wg.Add(2)
	go func() { defer wg.Done(); _, _, _ = svc.LiftRetentionRule(id, "R1") }()
	go func() {
		defer wg.Done()
		d, err := svc.Authorize(id, task.DispatchKey, task.LeaseToken)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case err != nil:
			t.Errorf("authorize: %v", err)
		case d.Granted:
			granted++
		default:
			frozen++
		}
	}()
	wg.Wait()
	if granted+frozen != 1 {
		t.Fatalf("exactly one decision expected, got granted=%d frozen=%d", granted, frozen)
	}

	// 无论并发结果如何，重判路径必须收敛到解除后的规则：frozen 可重新领取并放行。
	clk.advance(2 * time.Minute)
	if granted == 1 {
		// 授权放行的邮件可正常完成发送。
		if _, err := svc.SubmitReceipt(id, task.DispatchKey, task.LeaseToken, ReceiptInput{Result: ResultSuccess}); err != nil {
			t.Fatalf("granted task must accept receipt, got %v", err)
		}
		return
	}
	tasks, _ := svc.Lease(id, LeaseRequest{WorkerID: "w2", Count: 1, LeaseDuration: time.Minute})
	if len(tasks) != 1 {
		t.Fatalf("frozen task must be re-leasable after lift, got %+v", tasks)
	}
	if d, err := svc.Authorize(id, tasks[0].DispatchKey, tasks[0].LeaseToken); err != nil || !d.Granted {
		t.Fatalf("post-lift re-judge must grant, got %+v %v", d, err)
	}
}

// TestRetentionLogsMaskAddresses 普通日志中不得出现完整敏感地址。
func TestRetentionLogsMaskAddresses(t *testing.T) {
	svc, _ := newTestService()
	id := mustStart(t, svc, "tpl-1", "alice.secret@example.com")

	var buf bytes.Buffer
	svc.SetLogger(log.New(&buf, "", 0))

	if _, err := svc.SubmitRetentionRule(id, freezeRule("R1", "alice.secret@example.com")); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, _, err := svc.LiftRetentionRule(id, "R1"); err != nil {
		t.Fatalf("lift: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "alice.secret@example.com") {
		t.Fatalf("sensitive address leaked into logs: %q", out)
	}
	if !strings.Contains(out, "a***@example.com") {
		t.Fatalf("expected masked address in logs, got %q", out)
	}
	if got := MaskAddress("Alice.Secret@Example.com"); got != "a***@example.com" {
		t.Fatalf("MaskAddress = %q", got)
	}
}
