package emaildispatch

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func testService(store Store) (*Service, *fakeClock) {
	clk := &fakeClock{t: time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)}
	cfg := Config{
		LeaseTTL:    time.Minute,
		MaxAttempts: 3,
		Backoff:     func(n int) time.Duration { return time.Duration(n) * time.Second },
		Now:         func() time.Time { return clk.t },
	}
	return NewService(store, cfg), clk
}

func seedCampaign(t *testing.T, svc *Service, campaignID string, emails ...string) {
	t.Helper()
	if _, err := svc.PutTemplate("tpl-1", "hello v1"); err != nil {
		t.Fatal(err)
	}
	rcpts := make([]Recipient, 0, len(emails))
	for i, e := range emails {
		rcpts = append(rcpts, Recipient{ID: fmt.Sprintf("r%d", i+1), Email: e})
	}
	if _, err := svc.CreateCampaign(campaignID, "c", "tpl-1", rcpts); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartCampaign(campaignID); err != nil {
		t.Fatal(err)
	}
}

func TestStartLocksTemplateVersionAndFreezesRecipients(t *testing.T) {
	svc, _ := testService(NewMemoryStore())

	tpl, err := svc.PutTemplate("tpl-1", "v1 body")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateCampaign("c1", "camp", "tpl-1", []Recipient{{ID: "r1", Email: "a@x.com"}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddRecipients("c1", []Recipient{{ID: "r2", Email: "b@x.com"}}); err != nil {
		t.Fatal(err)
	}

	c, err := svc.StartCampaign("c1")
	if err != nil {
		t.Fatal(err)
	}
	if c.TemplateVersion != 1 {
		t.Fatalf("locked version = %d, want 1", c.TemplateVersion)
	}
	if len(c.Recipients) != 2 {
		t.Fatalf("frozen recipients = %d, want 2", len(c.Recipients))
	}

	// 启动后：模板发布新版本、草稿接口拒绝变更，均不影响活动快照。
	if _, err := svc.PutTemplate("tpl-1", "v2 body"); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddRecipients("c1", []Recipient{{ID: "r3", Email: "c@x.com"}}); !errors.Is(err, ErrCampaignNotDraft) {
		t.Fatalf("AddRecipients after start err = %v, want ErrCampaignNotDraft", err)
	}
	c2, _ := svc.GetCampaign("c1")
	if c2.TemplateVersion != 1 || len(c2.Recipients) != 2 {
		t.Fatalf("frozen snapshot mutated: %+v", c2)
	}

	items, err := svc.Claim("c1", "w1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("claimed = %d, want 2", len(items))
	}
	for _, it := range items {
		if it.TemplateVersion != 1 {
			t.Fatalf("claimed item uses template v%d, want locked v1", it.TemplateVersion)
		}
	}
	_ = tpl
}

func TestIdempotencyKeyStable(t *testing.T) {
	k1 := IdempotencyKey("c1", "r1")
	k2 := IdempotencyKey("c1", "r1")
	k3 := IdempotencyKey("c1", "r2")
	k4 := IdempotencyKey("c2", "r1")
	if k1 != k2 || len(k1) != 64 || k1 == k3 || k1 == k4 {
		t.Fatalf("idempotency key properties violated: %q %q %q %q", k1, k2, k3, k4)
	}
}

func TestSuppressionsBeforeAuthorizationBlock(t *testing.T) {
	cases := []struct {
		name string
		in   SuppressionInput
	}{
		{"global unsubscribe", SuppressionInput{Type: SuppressionGlobalUnsubscribe, Email: "a@x.com", Reason: "user opted out"}},
		{"address bounce", SuppressionInput{Type: SuppressionAddressBounce, Email: "a@x.com", Reason: "mailbox full bounce"}},
		{"campaign suppression", SuppressionInput{Type: SuppressionCampaign, Email: "a@x.com", CampaignID: "c1", Reason: "complaint"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, clk := testService(NewMemoryStore())
			seedCampaign(t, svc, "c1", "a@x.com", "b@x.com")
			clk.advance(time.Minute)
			if _, err := svc.AddSuppression(tc.in); err != nil {
				t.Fatal(err)
			}
			items, err := svc.Claim("c1", "w1", 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 1 || items[0].Email != "b@x.com" {
				t.Fatalf("want only b@x.com claimable, got %+v", items)
			}
			d, err := svc.GetDelivery("c1", IdempotencyKey("c1", "r1"))
			if err != nil {
				t.Fatal(err)
			}
			if d.Status != DeliverySuppressed {
				t.Fatalf("status = %s, want suppressed", d.Status)
			}
			if len(d.Audit) == 0 || d.Audit[len(d.Audit)-1].Event != "suppressed" {
				t.Fatalf("missing suppression audit: %+v", d.Audit)
			}
			st, _ := svc.Stats("c1")
			if st.Suppressed != 1 || st.Leased != 1 || st.Total != 2 {
				t.Fatalf("stats = %+v", st)
			}
		})
	}
}

func TestCampaignSuppressionDoesNotLeakAcrossCampaigns(t *testing.T) {
	svc, clk := testService(NewMemoryStore())
	seedCampaign(t, svc, "c1", "a@x.com")
	// 用同一模板再建一个活动。
	if _, err := svc.CreateCampaign("c2", "camp2", "tpl-1", []Recipient{{ID: "r1", Email: "a@x.com"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartCampaign("c2"); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Second)
	if _, err := svc.AddSuppression(SuppressionInput{Type: SuppressionCampaign, Email: "a@x.com", CampaignID: "c1"}); err != nil {
		t.Fatal(err)
	}
	c1Items, err := svc.Claim("c1", "w1", 10)
	if err != nil || len(c1Items) != 0 {
		t.Fatalf("c1 claim = %v, %v", c1Items, err)
	}
	c2Items, err := svc.Claim("c2", "w1", 10)
	if err != nil || len(c2Items) != 1 {
		t.Fatalf("c2 claim = %v, %v", c2Items, err)
	}
}

func TestLateSuppressionKeepsOutcomeWithAudit(t *testing.T) {
	svc, clk := testService(NewMemoryStore())
	seedCampaign(t, svc, "c1", "a@x.com")

	items, err := svc.Claim("c1", "w1", 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim = %v, %v", items, err)
	}
	// 授权点之后抑制生效。
	clk.advance(time.Second)
	if _, err := svc.AddSuppression(SuppressionInput{Type: SuppressionGlobalUnsubscribe, Email: "a@x.com"}); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Receipt("c1", items[0].DeliveryID, items[0].LeaseToken, "rcpt-1", OutcomeSent, "250 OK")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != DeliverySent {
		t.Fatalf("receipt status = %s, want sent (late suppression must not block)", res.Status)
	}
	d, _ := svc.GetDelivery("c1", items[0].DeliveryID)
	found := false
	for _, a := range d.Audit {
		if a.Event == "late_suppression" {
			found = true
		}
	}
	if !found {
		t.Fatalf("late_suppression audit missing: %+v", d.Audit)
	}
}

func TestExpiredLeaseFenceOldWorkerReceipt(t *testing.T) {
	svc, clk := testService(NewMemoryStore())
	seedCampaign(t, svc, "c1", "a@x.com")

	first, err := svc.Claim("c1", "worker-old", 10)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim = %v, %v", first, err)
	}
	oldToken := first[0].LeaseToken

	// 租约过期后被新工作者领取，产生新的尝试与令牌。
	clk.advance(61 * time.Second)
	second, err := svc.Claim("c1", "worker-new", 10)
	if err != nil || len(second) != 1 {
		t.Fatalf("second claim = %v, %v", second, err)
	}
	if second[0].LeaseToken == oldToken || second[0].Attempt != 2 {
		t.Fatalf("reclaim did not produce a new fenced attempt: %+v", second[0])
	}

	// 旧工作者迟到的回执必须被拒绝，不得覆盖新尝试。
	if _, err := svc.Receipt("c1", first[0].DeliveryID, oldToken, "rcpt-old", OutcomeSent, "stale"); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("old worker receipt err = %v, want ErrStaleLease", err)
	}
	d, _ := svc.GetDelivery("c1", first[0].DeliveryID)
	if d.Status != DeliveryLeased || d.Lease.Token != second[0].LeaseToken {
		t.Fatalf("new attempt overwritten: status=%s", d.Status)
	}

	// 新工作者回执正常落地。
	if _, err := svc.Receipt("c1", second[0].DeliveryID, second[0].LeaseToken, "rcpt-new", OutcomeSent, "250 OK"); err != nil {
		t.Fatal(err)
	}
	d, _ = svc.GetDelivery("c1", first[0].DeliveryID)
	if d.Status != DeliverySent || d.Attempts != 2 {
		t.Fatalf("status=%s attempts=%d, want sent/2", d.Status, d.Attempts)
	}
}

func TestRetriesAndPermanentFailure(t *testing.T) {
	svc, clk := testService(NewMemoryStore())
	seedCampaign(t, svc, "c1", "a@x.com", "b@x.com")

	// a: 两次临时失败后成功。
	claim, _ := svc.Claim("c1", "w", 10)
	var a, b ClaimedItem
	for _, it := range claim {
		if it.Email == "a@x.com" {
			a = it
		} else {
			b = it
		}
	}
	r, err := svc.Receipt("c1", a.DeliveryID, a.LeaseToken, "ra1", OutcomeTransientFailure, "421 try later")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != DeliveryPending || r.NextAttemptAt == nil {
		t.Fatalf("transient receipt = %+v, want pending with next attempt", r)
	}
	// 退避窗口内不可再领取。
	again, _ := svc.Claim("c1", "w", 10)
	if len(again) != 0 {
		t.Fatalf("claimed during backoff: %+v", again)
	}
	clk.advance(2 * time.Second)
	claim2, _ := svc.Claim("c1", "w", 10)
	if len(claim2) != 1 || claim2[0].DeliveryID != a.DeliveryID || claim2[0].Attempt != 2 {
		t.Fatalf("retry claim = %+v", claim2)
	}
	if _, err := svc.Receipt("c1", a.DeliveryID, claim2[0].LeaseToken, "ra2", OutcomeSent, "250 OK"); err != nil {
		t.Fatal(err)
	}

	// b: 三次临时失败后重试耗尽，进入永久失败终态。
	// 第 1 次尝试的租约来自最初那次领取。
	r1, err := svc.Receipt("c1", b.DeliveryID, b.LeaseToken, "rb1", OutcomeTransientFailure, "421 again")
	if err != nil {
		t.Fatal(err)
	}
	if r1.Status != DeliveryPending {
		t.Fatalf("attempt 1 status = %s, want pending", r1.Status)
	}
	for i := 2; i <= 3; i++ {
		clk.advance(10 * time.Second) // 越过退避窗口
		items, _ := svc.Claim("c1", "w", 10)
		var it *ClaimedItem
		for j := range items {
			if items[j].DeliveryID == b.DeliveryID {
				it = &items[j]
			}
		}
		if it == nil {
			t.Fatalf("attempt %d: b not claimable", i)
		}
		if it.Attempt != i {
			t.Fatalf("attempt = %d, want %d", it.Attempt, i)
		}
		r, err := svc.Receipt("c1", b.DeliveryID, it.LeaseToken, fmt.Sprintf("rb%d", i), OutcomeTransientFailure, "421 again")
		if err != nil {
			t.Fatal(err)
		}
		if i < 3 && r.Status != DeliveryPending {
			t.Fatalf("attempt %d status = %s, want pending", i, r.Status)
		}
		if i == 3 && r.Status != DeliveryFailed {
			t.Fatalf("attempt %d status = %s, want failed", i, r.Status)
		}
	}
	d, _ := svc.GetDelivery("c1", b.DeliveryID)
	if d.Status != DeliveryFailed || d.Attempts != 3 {
		t.Fatalf("b = %s/%d, want failed/3", d.Status, d.Attempts)
	}
	// 终态后不再被领取。
	items, _ := svc.Claim("c1", "w", 10)
	if len(items) != 0 {
		t.Fatalf("terminal delivery claimed: %+v", items)
	}

	st, _ := svc.Stats("c1")
	if st.Sent != 1 || st.Failed != 1 || st.Attempts != 2+3 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestPermanentFailureImmediatelyTerminal(t *testing.T) {
	svc, _ := testService(NewMemoryStore())
	seedCampaign(t, svc, "c1", "a@x.com")
	items, _ := svc.Claim("c1", "w", 10)
	r, err := svc.Receipt("c1", items[0].DeliveryID, items[0].LeaseToken, "r1", OutcomePermanentFailure, "550 unknown user")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != DeliveryFailed {
		t.Fatalf("status = %s", r.Status)
	}
}

func TestDuplicateReceiptIsStableAndNotDoubleCounted(t *testing.T) {
	svc, _ := testService(NewMemoryStore())
	seedCampaign(t, svc, "c1", "a@x.com")
	items, _ := svc.Claim("c1", "w", 10)

	r1, err := svc.Receipt("c1", items[0].DeliveryID, items[0].LeaseToken, "rcpt-1", OutcomeSent, "250 OK")
	if err != nil {
		t.Fatal(err)
	}
	st1, _ := svc.Stats("c1")

	// 完全重复的回执：返回稳定结果（Duplicate=true），状态与统计不再变化。
	r2, err := svc.Receipt("c1", items[0].DeliveryID, items[0].LeaseToken, "rcpt-1", OutcomeSent, "250 OK")
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Duplicate || r2.Status != r1.Status || r2.Attempt != r1.Attempt {
		t.Fatalf("duplicate receipt = %+v, want stable copy of %+v", r2, r1)
	}
	st2, _ := svc.Stats("c1")
	if *st1 != *st2 {
		t.Fatalf("stats changed by duplicate receipt: %+v vs %+v", st1, st2)
	}

	// 同 receiptID 即使换一个 outcome 也必须返回首次的稳定结果。
	r3, err := svc.Receipt("c1", items[0].DeliveryID, items[0].LeaseToken, "rcpt-1", OutcomePermanentFailure, "550")
	if err != nil {
		t.Fatal(err)
	}
	if r3.Status != DeliverySent || !r3.Duplicate {
		t.Fatalf("conflicting duplicate not suppressed: %+v", r3)
	}
}

func TestCancelStopsAuthorizationsAndIsIdempotent(t *testing.T) {
	svc, clk := testService(NewMemoryStore())
	seedCampaign(t, svc, "c1", "a@x.com", "b@x.com")

	// a 已授权在途；b 仍 pending。
	items, _ := svc.Claim("c1", "w", 1)
	if len(items) != 1 || items[0].Email != "a@x.com" {
		t.Fatalf("claim = %+v", items)
	}

	c1, err := svc.CancelCampaign("c1")
	if err != nil {
		t.Fatal(err)
	}
	if c1.Status != CampaignCancelled {
		t.Fatalf("status = %s", c1.Status)
	}
	// 重复取消：稳定结果，不报错。
	c2, err := svc.CancelCampaign("c1")
	if err != nil || c2.Status != CampaignCancelled || c2.CancelledAt != c1.CancelledAt {
		t.Fatalf("repeated cancel unstable: %+v %v", c2, err)
	}

	// b 被收口为 cancelled，a 仍 leased 等待在途回执。
	st, _ := svc.Stats("c1")
	if st.CampaignStatus != CampaignCancelled || st.Cancelled != 1 || st.Leased != 1 {
		t.Fatalf("stats after cancel = %+v", st)
	}

	// 取消后任何领取都不得授权新邮件。
	more, err := svc.Claim("c1", "w", 10)
	if !errors.Is(err, ErrCampaignNotActive) || len(more) != 0 {
		t.Fatalf("claim after cancel = %v, %v", more, err)
	}

	// 在途回执（授权早于取消）正常落地并带解释性审计。
	if _, err := svc.Receipt("c1", items[0].DeliveryID, items[0].LeaseToken, "r1", OutcomeSent, "250"); err != nil {
		t.Fatal(err)
	}
	d, _ := svc.GetDelivery("c1", items[0].DeliveryID)
	if d.Status != DeliverySent {
		t.Fatalf("in-flight receipt after cancel: %s", d.Status)
	}

	// 租约过期后再领取：取消的活动中过期租约项收口为终态。
	clk.advance(2 * time.Minute)
	if _, err := svc.Claim("c1", "w", 10); !errors.Is(err, ErrCampaignNotActive) {
		t.Fatalf("want ErrCampaignNotActive, got %v", err)
	}
	d, _ = svc.GetDelivery("c1", items[0].DeliveryID)
	if d.Status != DeliverySent {
		t.Fatalf("sent delivery changed: %s", d.Status)
	}
}

func TestCancelReclaimExpiredLease(t *testing.T) {
	svc, clk := testService(NewMemoryStore())
	seedCampaign(t, svc, "c1", "a@x.com")
	items, _ := svc.Claim("c1", "w", 10)
	clk.advance(2 * time.Minute) // 租约过期
	if _, err := svc.CancelCampaign("c1"); err != nil {
		t.Fatal(err)
	}
	d, _ := svc.GetDelivery("c1", items[0].DeliveryID)
	if d.Status != DeliveryCancelled || d.Lease != nil {
		t.Fatalf("expired-lease delivery after cancel = %s", d.Status)
	}
	// 旧工作者回执被拒：发送项已收口为终态。
	if _, err := svc.Receipt("c1", items[0].DeliveryID, items[0].LeaseToken, "r1", OutcomeSent, "x"); !errors.Is(err, ErrDeliveryTerminal) {
		t.Fatalf("err = %v, want ErrDeliveryTerminal", err)
	}
}

func TestClaimRejectsInactiveCampaign(t *testing.T) {
	svc, _ := testService(NewMemoryStore())
	if _, err := svc.PutTemplate("t", "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateCampaign("cdraft", "c", "t", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Claim("cdraft", "w", 1); !errors.Is(err, ErrCampaignNotActive) {
		t.Fatalf("draft claim err = %v", err)
	}
	if _, err := svc.Claim("missing", "w", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing claim err = %v", err)
	}
}

func TestReceiptGuardErrors(t *testing.T) {
	svc, _ := testService(NewMemoryStore())
	seedCampaign(t, svc, "c1", "a@x.com")
	items, _ := svc.Claim("c1", "w", 10)

	if _, err := svc.Receipt("c1", items[0].DeliveryID, "wrong-token", "r", OutcomeSent, ""); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("wrong token err = %v", err)
	}
	if _, err := svc.Receipt("c1", items[0].DeliveryID, items[0].LeaseToken, "r", Outcome("bogus"), ""); !errors.Is(err, ErrInvalidOutcome) {
		t.Fatalf("bad outcome err = %v", err)
	}
	if _, err := svc.Receipt("c1", "missing-delivery", items[0].LeaseToken, "r", OutcomeSent, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing delivery err = %v", err)
	}
}
