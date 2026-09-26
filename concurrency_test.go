package emaildispatch

import (
	"sync"
	"testing"
	"time"
)

// TestConcurrentDuplicateReceipts 多个工作者并发提交同一 attempt 的回执，
// 只有一个生效，统计恰好累加一次。
func TestConcurrentDuplicateReceipts(t *testing.T) {
	svc, _ := newTestService()
	id := mustStart(t, svc, "tpl-1", "a@x.com")
	task := mustLeaseOne(t, svc, id, "w1")
	if d, err := svc.Authorize(id, task.DispatchKey, task.LeaseToken); err != nil || !d.Granted {
		t.Fatalf("authorize: %+v %v", d, err)
	}

	const n = 32
	var wg sync.WaitGroup
	var success, duplicate, rejected int64
	var mu sync.Mutex
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			ack, err := svc.SubmitReceipt(id, task.DispatchKey, task.LeaseToken, ReceiptInput{Result: ResultSuccess})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				rejected++
			case ack.Duplicate:
				duplicate++
			default:
				success++
			}
		}()
	}
	wg.Wait()

	if success != 1 || duplicate != n-1 || rejected != 0 {
		t.Fatalf("expected exactly 1 first receipt and %d duplicates, got success=%d duplicate=%d rejected=%d",
			n-1, success, duplicate, rejected)
	}
	st, _ := svc.Stats(id)
	if st.Sent != 1 || st.TotalAttempts != 1 || st.Terminal != 1 {
		t.Fatalf("stats must count once: %+v", st)
	}
}

// TestConcurrentCancelAndLease 并发取消与领取：取消后绝不可能新授权邮件。
func TestConcurrentCancelAndLease(t *testing.T) {
	svc, _ := newTestService()
	id := mustStart(t, svc, "tpl-1", "a@x.com", "b@x.com", "c@x.com")

	var leased []LeasedTask
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _, _ = svc.Cancel(id) }()
	go func() {
		defer wg.Done()
		got, _ := svc.Lease(id, LeaseRequest{WorkerID: "w", Count: 10, LeaseDuration: time.Minute})
		leased = got
	}()
	wg.Wait()

	// 取消之后不能再领取。
	if _, err := svc.Lease(id, LeaseRequest{WorkerID: "w2", Count: 10, LeaseDuration: time.Minute}); !IsCode(err, ErrCampaignNotReady) {
		t.Fatalf("lease after cancel must fail with campaign_not_running, got %v", err)
	}

	// 若取消前有租约侥幸发出，其授权点必须拒绝（reason=campaign_canceled）。
	for _, task := range leased {
		dec, err := svc.Authorize(id, task.DispatchKey, task.LeaseToken)
		if err != nil {
			t.Fatalf("authorize should persist denial, got %v", err)
		}
		if dec.Granted || dec.Reason != ReasonCampaignCanceled {
			t.Fatalf("no grants allowed after cancel: %+v", dec)
		}
	}

	st, _ := svc.Stats(id)
	if st.Status != CampaignCanceled || st.Sent != 0 || st.Authorized != 0 {
		t.Fatalf("post-cancel stats: %+v", st)
	}
	if st.Pending+st.Canceled != st.Total {
		t.Fatalf("every task must be pending or canceled, stats: %+v", st)
	}
}
