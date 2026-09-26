package emaildispatch

import (
	"path/filepath"
	"testing"
	"time"
)

func TestFileStoreSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	store1, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	svc1, clk := testService(store1)
	seedCampaign(t, svc1, "c1", "a@x.com", "b@x.com")
	items, err := svc1.Claim("c1", "w1", 10)
	if err != nil || len(items) != 2 {
		t.Fatalf("claim = %v, %v", items, err)
	}
	if _, err := svc1.Receipt("c1", items[0].DeliveryID, items[0].LeaseToken, "rcpt-1", OutcomeSent, "250"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc1.AddSuppression(SuppressionInput{Type: SuppressionAddressBounce, Email: "b@x.com"}); err != nil {
		t.Fatal(err)
	}
	st1, _ := svc1.Stats("c1")

	// 用同一文件重新打开：状态完整恢复。
	store2, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		LeaseTTL:    time.Minute,
		MaxAttempts: 3,
		Backoff:     func(n int) time.Duration { return time.Duration(n) * time.Second },
		Now:         func() time.Time { return clk.t },
	}
	svc2 := NewService(store2, cfg)

	c, err := svc2.GetCampaign("c1")
	if err != nil {
		t.Fatal(err)
	}
	if c.TemplateVersion != 1 || len(c.Recipients) != 2 || c.Status != CampaignRunning {
		t.Fatalf("campaign not restored: %+v", c)
	}
	st2, err := svc2.Stats("c1")
	if err != nil {
		t.Fatal(err)
	}
	if st2.Sent != st1.Sent || st2.Leased != st1.Leased || st2.Total != 2 {
		t.Fatalf("stats after restart = %+v, before = %+v", st2, st1)
	}

	// 重复回执去重表同样持久化。
	r, err := svc2.Receipt("c1", items[0].DeliveryID, items[0].LeaseToken, "rcpt-1", OutcomeSent, "250")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Duplicate {
		t.Fatalf("duplicate detection lost across restart: %+v", r)
	}

	// b 在重启前已被抑制，授权点复查结果持久化。
	d, err := svc2.GetDelivery("c1", items[1].DeliveryID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != DeliveryLeased {
		// 抑制是在 b 已授权之后登记的：保持 leased，等待回执（late suppression 仅审计）。
		t.Fatalf("b status = %s, want leased", d.Status)
	}
}

func TestSweepExpiredLeasesRunningAndCancelled(t *testing.T) {
	svc, clk := testService(NewMemoryStore())
	seedCampaign(t, svc, "c1", "a@x.com", "b@x.com")
	items, _ := svc.Claim("c1", "w", 1)
	if len(items) != 1 {
		t.Fatalf("claim = %+v", items)
	}
	expiredID := items[0].DeliveryID

	clk.advance(2 * time.Minute)
	if _, err := svc.CancelCampaign("c1"); err != nil {
		t.Fatal(err)
	}
	// CancelCampaign 已立即收口过期租约；Sweep 对其幂等、无重复变更。
	changed, err := svc.SweepExpiredLeases("c1")
	if err != nil || len(changed) != 0 {
		t.Fatalf("sweep after cancel = %v, %v", changed, err)
	}
	d, _ := svc.GetDelivery("c1", expiredID)
	if d.Status != DeliveryCancelled {
		t.Fatalf("expired lease after cancel = %s, want cancelled", d.Status)
	}

	// 另一个活动：运行中过期 → 退回 pending 并可重新授权。
	seedCampaign(t, svc, "c2", "z@x.com")
	zItems, _ := svc.Claim("c2", "w", 10)
	clk.advance(2 * time.Minute)
	changed2, err := svc.SweepExpiredLeases("c2")
	if err != nil || len(changed2) != 1 {
		t.Fatalf("sweep running = %v, %v", changed2, err)
	}
	d2, _ := svc.GetDelivery("c2", zItems[0].DeliveryID)
	if d2.Status != DeliveryPending {
		t.Fatalf("swept status = %s, want pending", d2.Status)
	}
	reclaimed, err := svc.Claim("c2", "w2", 10)
	if err != nil || len(reclaimed) != 1 || reclaimed[0].Attempt != 2 {
		t.Fatalf("reclaim after sweep = %v, %v", reclaimed, err)
	}
}
