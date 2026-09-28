package emaildispatch

import (
	"net/http"
	"sync"
	"testing"
	"time"
)

// TestPauseResumeCoreSemantics 覆盖需求 1、2、3、4 的主流程：
// 暂停拦截未授权在途邮件、已授权邮件暂停期间照常完成、恢复沿用快照、
// 暂停期间新抑制在恢复后生效、幂等切换、统计区分暂停拦截/取消/实际发送。
func TestPauseResumeCoreSemantics(t *testing.T) {
	svc, clk := newTestService()
	id := mustStart(t, svc, "tpl-1", "p@x.com", "l@x.com", "a@x.com", "s@x.com")

	// l: 已领取未授权；a: 已授权在途；s: 已完成发送；p: 始终 pending。
	l := mustLeaseOne(t, svc, id, "w-l") // p@x.com（冻结顺序首个 pending 被领取）
	if l.Recipient != "p@x.com" {
		t.Fatalf("lease order: %s", l.Recipient)
	}
	// 再领取三项：l@x.com、a@x.com、s@x.com。
	batch, err := svc.Lease(id, LeaseRequest{WorkerID: "w", Count: 10, LeaseDuration: time.Minute})
	if err != nil || len(batch) != 3 {
		t.Fatalf("lease rest: %+v err=%v", batch, err)
	}
	byAddr := map[string]LeasedTask{}
	for _, tk := range batch {
		byAddr[tk.Recipient] = tk
	}
	lTask := byAddr["l@x.com"]
	aTask := byAddr["a@x.com"]
	sTask := byAddr["s@x.com"]

	if d, err := svc.Authorize(id, aTask.DispatchKey, aTask.LeaseToken); err != nil || !d.Granted {
		t.Fatalf("authorize a: %+v %v", d, err)
	}
	if d, err := svc.Authorize(id, sTask.DispatchKey, sTask.LeaseToken); err != nil || !d.Granted {
		t.Fatalf("authorize s: %+v %v", d, err)
	}
	if ack, err := svc.SubmitReceipt(id, sTask.DispatchKey, sTask.LeaseToken, ReceiptInput{Result: ResultSuccess}); err != nil || ack.State != TaskSent {
		t.Fatalf("send s: %+v %v", ack, err)
	}

	// ---- 暂停 ----
	paused, changed, err := svc.Pause(id)
	if err != nil || !changed || paused.Status != CampaignPaused || paused.PausedAt.IsZero() {
		t.Fatalf("pause: %+v changed=%v err=%v", paused, changed, err)
	}
	// 重复暂停幂等：changed=false，paused_at 稳定。
	again, changed2, err := svc.Pause(id)
	if err != nil || changed2 || !again.PausedAt.Equal(paused.PausedAt) {
		t.Fatalf("idempotent pause broken: %+v changed=%v err=%v", again, changed2, err)
	}

	// 暂停后不得新领取（即使租约过期重领也不行）。
	clk.advance(2 * time.Minute)
	if _, err := svc.Lease(id, LeaseRequest{WorkerID: "late"}); !IsCode(err, ErrCampaignNotReady) {
		t.Fatalf("lease while paused must fail, got %v", err)
	}

	// 已领取但未授权的 l / p：授权点返回持久化的暂停拒绝。
	for _, tk := range []LeasedTask{lTask, l} {
		dec, err := svc.Authorize(id, tk.DispatchKey, tk.LeaseToken)
		if err != nil {
			t.Fatalf("authorize while paused should persist denial, got %v", err)
		}
		if dec.Granted || dec.Reason != ReasonCampaignPaused {
			t.Fatalf("expected campaign_paused denial, got %+v", dec)
		}
		// 旧工作者重试授权：同一稳定决策，不允许发送。
		dec2, err := svc.Authorize(id, tk.DispatchKey, tk.LeaseToken)
		if err != nil || dec2.ID != dec.ID || dec2.Granted {
			t.Fatalf("paused denial must be stable: %+v err=%v", dec2, err)
		}
		// 被拒后尝试回执（声称已发送）必须报错：暂停拦截的邮件不可能发出。
		if _, err := svc.SubmitReceipt(id, tk.DispatchKey, tk.LeaseToken, ReceiptInput{Result: ResultSuccess}); !IsCode(err, ErrInvalidState) {
			t.Fatalf("receipt after paused denial must fail, got %v", err)
		}
	}

	// 已授权的 a：暂停期间照常完成并接收回执（成功与失败均可）。
	ack, err := svc.SubmitReceipt(id, aTask.DispatchKey, aTask.LeaseToken, ReceiptInput{Result: ResultSuccess})
	if err != nil || ack.State != TaskSent {
		t.Fatalf("granted mail must finish during pause: %+v err=%v", ack, err)
	}
	// 重复回执保持幂等。
	ack2, err := svc.SubmitReceipt(id, aTask.DispatchKey, aTask.LeaseToken, ReceiptInput{Result: ResultSuccess})
	if err != nil || !ack2.Duplicate || ack2.State != TaskSent {
		t.Fatalf("duplicate receipt during pause: %+v err=%v", ack2, err)
	}

	// 暂停期间数据仍可变化：为 l@x.com 录入全局退订，为恢复后的授权点准备。
	if _, err := svc.RecordSuppression(SuppressionInput{Type: SuppressionGlobalUnsubscribe, Address: "l@x.com", Reason: "unsubscribed while paused"}); err != nil {
		t.Fatalf("suppression during pause: %v", err)
	}

	st, _ := svc.Stats(id)
	if st.Status != CampaignPaused || st.Paused != 2 || st.PauseIntercepted != 2 ||
		st.Sent != 2 || st.Canceled != 0 || st.Authorized != 0 || st.Terminal != 2 {
		t.Fatalf("stats while paused: %+v", st)
	}

	// ---- 恢复 ----
	resumed, rchanged, err := svc.Resume(id)
	if err != nil || !rchanged || resumed.Status != CampaignRunning || resumed.ResumedAt.IsZero() {
		t.Fatalf("resume: %+v changed=%v err=%v", resumed, rchanged, err)
	}
	// 重复恢复幂等。
	if _, c3, err := svc.Resume(id); err != nil || c3 {
		t.Fatalf("idempotent resume broken: changed=%v err=%v", c3, err)
	}
	// 模板版本沿用启动快照，没有被改动。
	if resumed.TemplateVersion != "tpl-1" || len(resumed.Recipients) != 4 {
		t.Fatalf("snapshot must survive pause/resume: %+v", resumed)
	}

	// p@x.com：暂停拦截后恢复，重新领取得到新 attempt + 新 token（fencing）。
	pAgain := mustLeaseOne(t, svc, id, "w-new")
	if pAgain.Recipient != "p@x.com" || pAgain.Attempt != 2 || pAgain.LeaseToken == l.LeaseToken ||
		pAgain.TemplateVersion != "tpl-1" {
		t.Fatalf("resumed re-lease must be fresh attempt on frozen template: %+v", pAgain)
	}
	// 过期工作者拿着暂停时的旧 token 调授权：只能拿到既有暂停拒绝，永远无法再获授权。
	oldDec, err := svc.Authorize(id, l.DispatchKey, l.LeaseToken)
	if err != nil || oldDec.Granted || oldDec.Reason != ReasonCampaignPaused {
		t.Fatalf("stale worker must keep seeing paused denial: %+v err=%v", oldDec, err)
	}
	// 新 attempt 授权通过（p@x.com 无抑制）。
	if d, err := svc.Authorize(id, pAgain.DispatchKey, pAgain.LeaseToken); err != nil || !d.Granted {
		t.Fatalf("p@x.com re-authorize after resume: %+v %v", d, err)
	}

	// l@x.com：暂停期间录入的退订在恢复后的授权点生效（抑制优先于一切）。
	lAgain := mustLeaseOne(t, svc, id, "w-new2")
	if lAgain.Recipient != "l@x.com" || lAgain.Attempt != 2 {
		t.Fatalf("expected l@x.com fresh attempt 2, got %+v", lAgain)
	}
	dL, err := svc.Authorize(id, lAgain.DispatchKey, lAgain.LeaseToken)
	if err != nil || dL.Granted || dL.Reason != ReasonGlobalUnsubscribe {
		t.Fatalf("suppression recorded during pause must apply after resume: %+v err=%v", dL, err)
	}

	st2, _ := svc.Stats(id)
	// 已完成的不重发；暂停拦截计数保留为历史（2 次持久化拒绝决策），实际发送 Sent=2。
	if st2.Sent != 2 || st2.Suppressed != 1 || st2.PauseIntercepted != 2 ||
		st2.Canceled != 0 || st2.Authorized != 1 {
		t.Fatalf("stats after resume: %+v", st2)
	}

	// s@x.com 审计轨迹只有一次 attempt、一次发送，恢复没有造成重复投递。
	detail, err := svc.GetDispatch(id, sTask.DispatchKey)
	if err != nil || len(detail.Attempts) != 1 {
		t.Fatalf("sent task must not be retried after resume: %+v err=%v", detail, err)
	}
}

// TestResumePreservesRecipientVars 恢复后领取仍下发冻结的收件人变量快照。
func TestResumePreservesRecipientVars(t *testing.T) {
	svc, _ := newTestService()
	c, err := svc.CreateCampaign(CampaignSpec{Name: "vars"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartCampaign(c.ID, StartSpec{
		TemplateVersion: "tpl-7",
		Recipients:      []Recipient{{Address: "r@x.com", Vars: map[string]string{"name": "Ren", "coupon": "SPRING"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Pause(c.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Resume(c.ID); err != nil {
		t.Fatal(err)
	}
	second := mustLeaseOne(t, svc, c.ID, "w2")
	if second.TemplateVersion != "tpl-7" || second.Vars["name"] != "Ren" || second.Vars["coupon"] != "SPRING" {
		t.Fatalf("resumed lease lost frozen snapshot: %+v", second)
	}
}

// TestPauseResumeStateGuards 非法状态切换返回 conflict；暂停态可直接取消。
func TestPauseResumeStateGuards(t *testing.T) {
	svc, _ := newTestService()

	draft, err := svc.CreateCampaign(CampaignSpec{Name: "d"})
	if err != nil {
		t.Fatal(err)
	}
	// draft 不能暂停/恢复。
	if _, _, err := svc.Pause(draft.ID); !IsCode(err, ErrConflict) {
		t.Fatalf("pause draft: %v", err)
	}
	if _, _, err := svc.Resume(draft.ID); !IsCode(err, ErrConflict) {
		t.Fatalf("resume draft: %v", err)
	}
	// 不存在的活动。
	if _, _, err := svc.Pause("cmp_nope"); !IsCode(err, ErrNotFound) {
		t.Fatalf("pause missing: %v", err)
	}

	id := mustStart(t, svc, "tpl-1", "p@x.com", "l@x.com")
	tk := mustLeaseOne(t, svc, id, "w1")
	if _, _, err := svc.Pause(id); err != nil {
		t.Fatal(err)
	}
	// 暂停态取消：暂停拦截任务进入 canceled（主动取消），恢复被拒绝。
	cancelled, changed, err := svc.Cancel(id)
	if err != nil || !changed || cancelled.Status != CampaignCanceled {
		t.Fatalf("cancel from paused: %+v %v", cancelled, err)
	}
	if _, _, err := svc.Resume(id); !IsCode(err, ErrConflict) {
		t.Fatalf("resume canceled must conflict, got %v", err)
	}
	if _, _, err := svc.Pause(id); !IsCode(err, ErrConflict) {
		t.Fatalf("pause canceled must conflict, got %v", err)
	}
	// 暂停拦截历史保留，但当前状态计入主动取消。
	st, _ := svc.Stats(id)
	if st.Canceled != 2 || st.Paused != 0 || st.PauseIntercepted != 1 || st.Sent != 0 {
		t.Fatalf("stats after cancel-from-paused: %+v", st)
	}
	// 被暂停拦截的旧 token 现在拿到的是取消结论还是暂停结论？
	// 决策在暂停事务已持久化且不可变，旧工作者永远看到同一结论（campaign_paused），
	// 任务终态为 canceled，不会重发。
	dec, err := svc.Authorize(id, tk.DispatchKey, tk.LeaseToken)
	if err != nil || dec.Granted || dec.Reason != ReasonCampaignPaused {
		t.Fatalf("persisted pause decision stays immutable after cancel: %+v err=%v", dec, err)
	}
}

// TestConcurrentPauseLeaseAuthorize 覆盖需求 2：暂停、领取、授权并发时，
// 每封邮件只有一个一致结果——授权要么先于暂停发生（可完成发送），
// 要么被持久化暂停拦截；不存在“暂停后新授权”。
func TestConcurrentPauseLeaseAuthorize(t *testing.T) {
	svc, _ := newTestService()
	const n = 64
	addrs := make([]string, 0, n)
	for i := 0; i < n; i++ {
		addrs = append(addrs, "u"+itoa(i)+"@x.com")
	}
	id := mustStart(t, svc, "tpl-1", addrs...)

	var mu sync.Mutex
	var granted, denied int
	var barrier sync.WaitGroup
	barrier.Add(1)
	var wg sync.WaitGroup

	// 多个工作者并发领取并立刻授权。
	const workers = 8
	wg.Add(workers + 1)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			barrier.Wait()
			tasks, err := svc.Lease(id, LeaseRequest{WorkerID: "w", Count: n, LeaseDuration: time.Minute})
			if err != nil {
				return // 暂停已落地，领取被拒：合法结果。
			}
			for _, tk := range tasks {
				dec, err := svc.Authorize(id, tk.DispatchKey, tk.LeaseToken)
				if err != nil {
					t.Errorf("authorize should always resolve for current lease, got %v", err)
					continue
				}
				mu.Lock()
				if dec.Granted {
					granted++
				} else if dec.Reason == ReasonCampaignPaused {
					denied++
				} else {
					t.Errorf("unexpected decision during pause race: %+v", dec)
				}
				mu.Unlock()
			}
		}()
	}
	go func() {
		defer wg.Done()
		barrier.Wait()
		_, _, _ = svc.Pause(id)
	}()
	barrier.Done()
	wg.Wait()

	st, _ := svc.Stats(id)
	// 每个被领取的任务恰好一个结论：granted（authorized）或 denied（paused），
	// 计数与持久化状态严格一致，且暂停后绝不可能出现新授权。
	if st.Authorized != granted {
		t.Fatalf("granted count mismatch: decisions=%d stats.authorized=%d", granted, st.Authorized)
	}
	if st.Paused != denied || st.PauseIntercepted != denied {
		t.Fatalf("denied count mismatch: decisions=%d stats.paused=%d intercepted=%d",
			denied, st.Paused, st.PauseIntercepted)
	}
	if st.Leased != 0 {
		t.Fatalf("no task may remain leased after pause settles: %+v", st)
	}
	if st.Pending+st.Authorized+st.Paused != st.Total || st.Pending+granted+denied != n {
		t.Fatalf("partition mismatch: %+v granted=%d denied=%d", st, granted, denied)
	}

	// 暂停态下，已授权邮件仍可完成；暂停拦截邮件不可发送。
	leased, _ := svc.Lease(id, LeaseRequest{WorkerID: "noop"})
	if len(leased) != 0 {
		t.Fatalf("must not lease while paused")
	}

	// 恢复后：未发送的任务（暂停拦截的 + 从未被领取的 pending）都可重新领取；
	// 曾被暂停拦截的任务以新 attempt 运行，旧 token 永远拿不到授权，无重复发送。
	if _, _, err := svc.Resume(id); err != nil {
		t.Fatal(err)
	}
	rel, err := svc.Lease(id, LeaseRequest{WorkerID: "w2", Count: n, LeaseDuration: time.Minute})
	if err != nil || len(rel) != n-granted {
		t.Fatalf("all non-sent tasks should be re-leasable: got %d want %d (granted=%d) err=%v",
			len(rel), n-granted, granted, err)
	}
	var freshAttempts int
	for _, tk := range rel {
		if tk.Attempt >= 2 {
			freshAttempts++
		}
	}
	if freshAttempts != denied {
		t.Fatalf("each denied task must come back on a new attempt: fresh=%d denied=%d", freshAttempts, denied)
	}
	st2, _ := svc.Stats(id)
	if st2.Sent != 0 || st2.PauseIntercepted != denied {
		t.Fatalf("intercept history retained, nothing sent yet: %+v", st2)
	}
}

// TestPauseAuthorizedTemporaryFailureResumesRetry 暂停期间已授权邮件收到临时失败回执：
// 进入 retry_wait；恢复后退避到期以新 attempt 重试成功，全程只有这一封邮件被投递尝试。
func TestPauseAuthorizedTemporaryFailureResumesRetry(t *testing.T) {
	svc, clk := newTestService()
	c, err := svc.CreateCampaign(CampaignSpec{
		Name:        "rt",
		RetryPolicy: RetryPolicy{MaxAttempts: 3, InitialBackoff: 10 * time.Second, MaxBackoff: time.Minute, Multiplier: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartCampaign(c.ID, StartSpec{
		TemplateVersion: "tpl-1",
		Recipients:      []Recipient{{Address: "r@x.com"}},
	}); err != nil {
		t.Fatal(err)
	}
	tk := mustLeaseOne(t, svc, c.ID, "w1")
	if d, _ := svc.Authorize(c.ID, tk.DispatchKey, tk.LeaseToken); !d.Granted {
		t.Fatal("grant")
	}
	if _, _, err := svc.Pause(c.ID); err != nil {
		t.Fatal(err)
	}
	// 暂停期间：已授权邮件如实接收临时失败回执 → retry_wait。
	ack, err := svc.SubmitReceipt(c.ID, tk.DispatchKey, tk.LeaseToken, ReceiptInput{
		Result: ResultTemporaryFailure, Code: "421",
	})
	if err != nil || ack.State != TaskRetryWait {
		t.Fatalf("temp failure during pause: %+v err=%v", ack, err)
	}
	// 退避未到期，恢复后也不能立即领取。
	if _, _, err := svc.Resume(c.ID); err != nil {
		t.Fatal(err)
	}
	if tasks, _ := svc.Lease(c.ID, LeaseRequest{WorkerID: "w2"}); len(tasks) != 0 {
		t.Fatalf("backoff must still be respected after resume: %+v", tasks)
	}
	clk.advance(11 * time.Second)
	tk2 := mustLeaseOne(t, svc, c.ID, "w2")
	if tk2.Attempt != 2 {
		t.Fatalf("retry must be attempt 2, got %d", tk2.Attempt)
	}
	// 旧工作者用暂停期间的旧 token 重查授权：只拿到不可变的旧决策，不会产生新授权；
	// 其迟到回执在回执点被 fencing 拒绝，绝不覆盖新 attempt。
	old, err := svc.Authorize(c.ID, tk.DispatchKey, tk.LeaseToken)
	if err != nil || !old.Granted {
		t.Fatalf("old token should return its immutable grant decision, got %+v err=%v", old, err)
	}
	if _, err := svc.SubmitReceipt(c.ID, tk.DispatchKey, tk.LeaseToken, ReceiptInput{Result: ResultSuccess}); !IsCode(err, ErrStaleAttempt) {
		t.Fatalf("stale receipt must be fenced off, got %v", err)
	}
	if d, _ := svc.Authorize(c.ID, tk2.DispatchKey, tk2.LeaseToken); !d.Granted {
		t.Fatal("grant retry")
	}
	ack2, err := svc.SubmitReceipt(c.ID, tk2.DispatchKey, tk2.LeaseToken, ReceiptInput{Result: ResultSuccess})
	if err != nil || ack2.State != TaskSent {
		t.Fatalf("retry success: %+v err=%v", ack2, err)
	}
	st, _ := svc.Stats(c.ID)
	if st.Sent != 1 || st.PauseIntercepted != 0 || st.TotalAttempts != 2 {
		t.Fatalf("stats: %+v", st)
	}
}

// TestRepeatedPauseAccumulatesIntercepts 同一封在途邮件两次被暂停拦截：
// 每次暂停各落一条持久化拒绝决策，pause_intercepted 累计为 2；恢复只重发未完成的一次流程。
func TestRepeatedPauseAccumulatesIntercepts(t *testing.T) {
	svc, _ := newTestService()
	id := mustStart(t, svc, "tpl-1", "x@x.com")

	for i := 1; i <= 2; i++ {
		tk := mustLeaseOne(t, svc, id, "w")
		if _, _, err := svc.Pause(id); err != nil {
			t.Fatalf("pause %d: %v", i, err)
		}
		dec, err := svc.Authorize(id, tk.DispatchKey, tk.LeaseToken)
		if err != nil || dec.Granted || dec.Reason != ReasonCampaignPaused {
			t.Fatalf("pause %d denial: %+v err=%v", i, dec, err)
		}
		if i == 2 {
			break
		}
		if _, _, err := svc.Resume(id); err != nil {
			t.Fatalf("resume %d: %v", i, err)
		}
	}
	st, _ := svc.Stats(id)
	if st.Paused != 1 || st.PauseIntercepted != 2 || st.Sent != 0 {
		t.Fatalf("repeated pause stats: %+v", st)
	}
	// 最终恢复并完成：仅一次实际发送。
	if _, _, err := svc.Resume(id); err != nil {
		t.Fatal(err)
	}
	tk := mustLeaseOne(t, svc, id, "w-fin")
	if tk.Attempt != 3 {
		t.Fatalf("expected attempt 3, got %d", tk.Attempt)
	}
	if d, _ := svc.Authorize(id, tk.DispatchKey, tk.LeaseToken); !d.Granted {
		t.Fatal("final grant")
	}
	if ack, err := svc.SubmitReceipt(id, tk.DispatchKey, tk.LeaseToken, ReceiptInput{Result: ResultSuccess}); err != nil || ack.State != TaskSent {
		t.Fatalf("final send: %+v err=%v", ack, err)
	}
	st2, _ := svc.Stats(id)
	if st2.Sent != 1 || st2.PauseIntercepted != 2 {
		t.Fatalf("final stats: %+v", st2)
	}
}

// TestAPIPauseResume 走通暂停/恢复 HTTP 接口与统计字段。
func TestAPIPauseResume(t *testing.T) {
	srv, _, _ := newTestServer()
	defer srv.Close()

	status, body := doJSON(t, srv, http.MethodPost, "/v1/campaigns", map[string]any{"name": "p"})
	if status != http.StatusCreated {
		t.Fatalf("create: %d %v", status, body)
	}
	id, _ := body["id"].(string)
	if status, body := doJSON(t, srv, http.MethodPost, "/v1/campaigns/"+id+"/start", map[string]any{
		"template_version": "tpl-1",
		"recipients":       []map[string]any{{"address": "a@x.com"}, {"address": "b@x.com"}},
	}); status != http.StatusOK {
		t.Fatalf("start: %d %v", status, body)
	}
	// 领取一个（暂停拦截目标）。
	if status, body := doJSON(t, srv, http.MethodPost, "/v1/campaigns/"+id+"/leases", map[string]any{
		"worker_id": "w1", "lease_duration_ms": 60000,
	}); status != http.StatusOK {
		t.Fatalf("lease: %d %v", status, body)
	}

	status, body = doJSON(t, srv, http.MethodPost, "/v1/campaigns/"+id+"/pause", map[string]any{})
	if status != http.StatusOK ||
		asMap(t, body["campaign"])["status"] != string(CampaignPaused) || body["changed"] != true {
		t.Fatalf("pause: %d %v", status, body)
	}
	status, body = doJSON(t, srv, http.MethodPost, "/v1/campaigns/"+id+"/pause", map[string]any{})
	if status != http.StatusOK || body["changed"] != false {
		t.Fatalf("idempotent pause: %d %v", status, body)
	}
	status, body = doJSON(t, srv, http.MethodGet, "/v1/campaigns/"+id+"/stats", nil)
	if status != http.StatusOK || body["pause_intercepted"] != float64(1) || body["paused"] != float64(1) {
		t.Fatalf("paused stats: %d %v", status, body)
	}
	// 暂停期间领取 → 409 campaign_not_running。
	status, body = doJSON(t, srv, http.MethodPost, "/v1/campaigns/"+id+"/leases", map[string]any{"worker_id": "w2"})
	if status != http.StatusConflict || asMap(t, body["error"])["code"] != string(ErrCampaignNotReady) {
		t.Fatalf("lease while paused: %d %v", status, body)
	}
	status, body = doJSON(t, srv, http.MethodPost, "/v1/campaigns/"+id+"/resume", map[string]any{})
	if status != http.StatusOK ||
		asMap(t, body["campaign"])["status"] != string(CampaignRunning) || body["changed"] != true {
		t.Fatalf("resume: %d %v", status, body)
	}
	status, _ = doJSON(t, srv, http.MethodPost, "/v1/campaigns/"+id+"/resume", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("idempotent resume status: %d", status)
	}

	// 非法切换：draft 暂停 → 409 conflict。
	_, db := doJSON(t, srv, http.MethodPost, "/v1/campaigns", map[string]any{"name": "d"})
	draftID, _ := db["id"].(string)
	status, body = doJSON(t, srv, http.MethodPost, "/v1/campaigns/"+draftID+"/pause", map[string]any{})
	if status != http.StatusConflict || asMap(t, body["error"])["code"] != string(ErrConflict) {
		t.Fatalf("pause draft: %d %v", status, body)
	}
}
