package emaildispatch

import (
	"sync"
	"testing"
	"time"
)

// TestPauseLeaseAndUnauthorizedFence 需求 1：暂停后不得领取、未授权邮件被围栏；已授权邮件可完成。
func TestPauseLeaseAndUnauthorizedFence(t *testing.T) {
	svc, _ := newTestService()
	id := mustStart(t, svc, "tpl-1", "p@x.com", "l@x.com", "a@x.com")

	// 顺序领取：p@x.com（leased 未授权）。
	leased := mustLeaseOne(t, svc, id, "w-l")
	if leased.Recipient != "p@x.com" {
		t.Fatalf("unexpected lease order: %s", leased.Recipient)
	}

	paused, changed, err := svc.Pause(id)
	if err != nil || !changed || paused.Status != CampaignPaused {
		t.Fatalf("pause: %+v changed=%v err=%v", paused, changed, err)
	}
	// 重复暂停幂等：changed=false，paused_at 稳定。
	again, changed2, err := svc.Pause(id)
	if err != nil || changed2 || again.Status != CampaignPaused || !again.PausedAt.Equal(paused.PausedAt) {
		t.Fatalf("idempotent pause broken: %+v changed=%v err=%v", again, changed2, err)
	}

	// 暂停后不得领取。
	if _, err := svc.Lease(id, LeaseRequest{WorkerID: "w2", LeaseDuration: time.Minute}); !IsCode(err, ErrCampaignNotReady) {
		t.Fatalf("lease while paused must fail, got %v", err)
	}

	// 已领取未授权：授权点持久化 campaign_paused 拒绝（暂停事务已提前围栏，结果稳定）。
	dec, err := svc.Authorize(id, leased.DispatchKey, leased.LeaseToken)
	if err != nil {
		t.Fatalf("authorize fenced task should return persisted denial, got %v", err)
	}
	if dec.Granted || dec.Reason != ReasonCampaignPaused {
		t.Fatalf("expected campaign_paused denial, got %+v", dec)
	}
	// 旧工作者任何时候重试授权都拿到同一份不可变决策。
	dec2, _ := svc.Authorize(id, leased.DispatchKey, leased.LeaseToken)
	if dec2.ID != dec.ID || dec2.Granted || dec2.Reason != ReasonCampaignPaused {
		t.Fatalf("fenced decision must be stable: %+v vs %+v", dec, dec2)
	}
	// 被拒后不得发送回执。
	if _, err := svc.SubmitReceipt(id, leased.DispatchKey, leased.LeaseToken, ReceiptInput{Result: ResultSuccess}); !IsCode(err, ErrInvalidState) {
		t.Fatalf("receipt after paused denial must be rejected, got %v", err)
	}
}

// TestPauseAuthorizedInFlightCompletes 需求 1：已授权的邮件暂停期间可以完成并接收回执。
func TestPauseAuthorizedInFlightCompletes(t *testing.T) {
	svc, _ := newTestService()
	id := mustStart(t, svc, "tpl-1", "a@x.com")
	task := mustLeaseOne(t, svc, id, "w1")
	dec, err := svc.Authorize(id, task.DispatchKey, task.LeaseToken)
	if err != nil || !dec.Granted {
		t.Fatalf("authorize: %+v %v", dec, err)
	}

	if _, _, err := svc.Pause(id); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// 暂停期间已授权邮件照发，成功回执如实记录。
	ack, err := svc.SubmitReceipt(id, task.DispatchKey, task.LeaseToken, ReceiptInput{
		Result: ResultSuccess, ProviderMessageID: "pm-9",
	})
	if err != nil || ack.State != TaskSent {
		t.Fatalf("in-flight authorized mail must complete while paused: %+v err=%v", ack, err)
	}
	// 重复回执仍幂等。
	ack2, err := svc.SubmitReceipt(id, task.DispatchKey, task.LeaseToken, ReceiptInput{Result: ResultSuccess})
	if err != nil || !ack2.Duplicate || ack2.State != TaskSent {
		t.Fatalf("duplicate receipt while paused: %+v err=%v", ack2, err)
	}
}

// TestResumeReusesSnapshotAndRejectsStaleWorker 需求 2、3：恢复沿用冻结收件人与模板；
// 被围栏的任务以新 attempt 重新领取，过期工作者无法再取得授权；终态不重复发送。
func TestResumeReusesSnapshotAndRejectsStaleWorker(t *testing.T) {
	svc, clk := newTestService()
	c, err := svc.CreateCampaign(CampaignSpec{Name: "freeze"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartCampaign(c.ID, StartSpec{
		TemplateVersion: "tpl-snap-1",
		Recipients:      []Recipient{{Address: " L@X.com ", Vars: map[string]string{"name": "Leo"}}},
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	id := c.ID

	old := mustLeaseOne(t, svc, id, "stale-worker") // l@x.com, attempt 1
	if _, _, err := svc.Pause(id); err != nil {
		t.Fatalf("pause: %v", err)
	}

	resumed, changed, err := svc.Resume(id)
	if err != nil || !changed || resumed.Status != CampaignRunning {
		t.Fatalf("resume: %+v changed=%v err=%v", resumed, changed, err)
	}
	// 重复恢复幂等。
	if _, changed2, err := svc.Resume(id); err != nil || changed2 {
		t.Fatalf("idempotent resume broken: changed=%v err=%v", changed2, err)
	}

	// 恢复后可重新领取；沿用冻结模板版本与变量快照，attempt 递增、token 更新。
	fresh := mustLeaseOne(t, svc, id, "new-worker")
	if fresh.Recipient != "l@x.com" {
		t.Fatalf("resume must keep original recipient, got %s", fresh.Recipient)
	}
	if fresh.TemplateVersion != "tpl-snap-1" {
		t.Fatalf("resume must reuse frozen template snapshot, got %q", fresh.TemplateVersion)
	}
	if fresh.Vars["name"] != "Leo" || fresh.Attempt != 2 || fresh.LeaseToken == old.LeaseToken {
		t.Fatalf("resumed lease must be a fresh attempt with frozen vars: %+v", fresh)
	}

	// 过期工作者用旧 token 授权：只拿到暂停时持久化的拒绝决策，无法取得新授权。
	stale, err := svc.Authorize(id, old.DispatchKey, old.LeaseToken)
	if err != nil {
		t.Fatalf("stale authorize should return stable denial, got %v", err)
	}
	if stale.Granted || stale.Reason != ReasonCampaignPaused || stale.ID == 0 {
		t.Fatalf("stale worker must never be granted, got %+v", stale)
	}
	// 旧 token 回执被 fencing 拒绝。
	if _, err := svc.SubmitReceipt(id, old.DispatchKey, old.LeaseToken, ReceiptInput{Result: ResultSuccess}); !IsCode(err, ErrStaleAttempt) {
		t.Fatalf("stale receipt must be rejected, got %v", err)
	}

	// 新 attempt 正常授权 → 成功，只发送一次。
	d, _ := svc.Authorize(id, fresh.DispatchKey, fresh.LeaseToken)
	if !d.Granted {
		t.Fatalf("fresh attempt should be granted after resume, got %+v", d)
	}
	ack, err := svc.SubmitReceipt(id, fresh.DispatchKey, fresh.LeaseToken, ReceiptInput{Result: ResultSuccess})
	if err != nil || ack.State != TaskSent {
		t.Fatalf("resumed delivery: %+v err=%v", ack, err)
	}

	// 租约到期后旧工作者拿到的仍是同一份不可变拒绝决策，不能翻案。
	clk.advance(2 * time.Minute)
	staleAfterExpiry, err := svc.Authorize(id, old.DispatchKey, old.LeaseToken)
	if err != nil || staleAfterExpiry.ID != stale.ID {
		t.Fatalf("persisted denial must remain stable after lease expiry: %+v err=%v", staleAfterExpiry, err)
	}

	detail, err := svc.GetDispatch(id, fresh.DispatchKey)
	if err != nil || len(detail.Attempts) != 2 {
		t.Fatalf("audit must retain fenced attempt 1 and completed attempt 2: %+v err=%v", detail, err)
	}
	if detail.Attempts[0].Decision == nil || detail.Attempts[0].Decision.Reason != ReasonCampaignPaused {
		t.Fatalf("attempt 1 must carry the pause denial: %+v", detail.Attempts[0])
	}
	if detail.Attempts[1].Outcome == nil || detail.Attempts[1].Outcome.Result != ResultSuccess {
		t.Fatalf("attempt 2 must carry success: %+v", detail.Attempts[1])
	}
}

// TestResumeTerminalTasksNotRedelivered 需求 3：暂停期间完成（sent）与取消（canceled）的邮件恢复后不再发送。
func TestResumeTerminalTasksNotRedelivered(t *testing.T) {
	svc, _ := newTestService()
	id := mustStart(t, svc, "tpl-1", "sent@x.com", "hold@x.com")

	// sent@x.com：授权后暂停，暂停期间回执成功。
	sentTask := mustLeaseOne(t, svc, id, "w1")
	if _, err := svc.Authorize(id, sentTask.DispatchKey, sentTask.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Pause(id); err != nil {
		t.Fatal(err)
	}
	if ack, err := svc.SubmitReceipt(id, sentTask.DispatchKey, sentTask.LeaseToken, ReceiptInput{Result: ResultSuccess}); err != nil || ack.State != TaskSent {
		t.Fatalf("complete while paused: %+v %v", ack, err)
	}

	if _, _, err := svc.Resume(id); err != nil {
		t.Fatalf("resume: %v", err)
	}
	// 恢复后只能领取 hold@x.com；sent@x.com 绝不重复。
	tasks, err := svc.Lease(id, LeaseRequest{WorkerID: "w2", Count: 10, LeaseDuration: time.Minute})
	if err != nil || len(tasks) != 1 || tasks[0].Recipient != "hold@x.com" {
		t.Fatalf("only held task may be redelivered, got %+v err=%v", tasks, err)
	}
	st, _ := svc.Stats(id)
	if st.Sent != 1 {
		t.Fatalf("sent must remain exactly 1: %+v", st)
	}

	// 暂停后取消：被围栏任务进入 canceled；再次暂停/恢复均不能让它复活。
	id2 := mustStart(t, svc, "tpl-1", "c@x.com")
	c2 := mustLeaseOne(t, svc, id2, "w")
	if _, _, err := svc.Pause(id2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Cancel(id2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Resume(id2); !IsCode(err, ErrConflict) {
		t.Fatalf("resume after cancel must conflict, got %v", err)
	}
	if _, err := svc.Authorize(id2, c2.DispatchKey, c2.LeaseToken); err != nil {
		t.Fatalf("existing denial stays readable: %v", err)
	}
	st2, _ := svc.Stats(id2)
	if st2.Canceled != 1 || st2.Sent != 0 {
		t.Fatalf("canceled-after-pause stats: %+v", st2)
	}
}

// TestSuppressionDuringPauseAppliesAfterResume 需求 2：暂停期间新生效的退订/退信恢复后仍在授权点拦截。
func TestSuppressionDuringPauseAppliesAfterResume(t *testing.T) {
	svc, _ := newTestService()
	id := mustStart(t, svc, "tpl-1", "off@x.com", "bounce@x.com", "ok@x.com")

	// 三封全部领取后暂停（均被围栏）。
	batch, err := svc.Lease(id, LeaseRequest{WorkerID: "w", Count: 10, LeaseDuration: time.Minute})
	if err != nil || len(batch) != 3 {
		t.Fatalf("lease batch: %d err=%v", len(batch), err)
	}
	if _, _, err := svc.Pause(id); err != nil {
		t.Fatal(err)
	}

	// 暂停期间到达的退订与退信。
	if _, err := svc.RecordSuppression(SuppressionInput{Type: SuppressionGlobalUnsubscribe, Address: "off@x.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RecordSuppression(SuppressionInput{Type: SuppressionBounce, Address: "bounce@x.com"}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := svc.Resume(id); err != nil {
		t.Fatal(err)
	}
	again, err := svc.Lease(id, LeaseRequest{WorkerID: "w2", Count: 10, LeaseDuration: time.Minute})
	if err != nil || len(again) != 3 {
		t.Fatalf("re-lease all paused: %d err=%v", len(again), err)
	}
	byAddr := map[string]LeasedTask{}
	for _, l := range again {
		byAddr[l.Recipient] = l
	}

	off := byAddr["off@x.com"]
	d1, _ := svc.Authorize(id, off.DispatchKey, off.LeaseToken)
	if d1.Granted || d1.Reason != ReasonGlobalUnsubscribe {
		t.Fatalf("unsubscribe during pause must block after resume, got %+v", d1)
	}
	bnc := byAddr["bounce@x.com"]
	d2, _ := svc.Authorize(id, bnc.DispatchKey, bnc.LeaseToken)
	if d2.Granted || d2.Reason != ReasonBounce {
		t.Fatalf("bounce during pause must block after resume, got %+v", d2)
	}
	clean := byAddr["ok@x.com"]
	d3, _ := svc.Authorize(id, clean.DispatchKey, clean.LeaseToken)
	if !d3.Granted {
		t.Fatalf("clean recipient should send with frozen snapshot, got %+v", d3)
	}
	ack, err := svc.SubmitReceipt(id, clean.DispatchKey, clean.LeaseToken, ReceiptInput{Result: ResultSuccess})
	if err != nil || ack.State != TaskSent {
		t.Fatalf("clean delivery: %+v err=%v", ack, err)
	}
}

// TestPauseResumeStats 需求 4：统计区分暂停拦截、主动取消与实际发送。
func TestPauseResumeStats(t *testing.T) {
	svc, _ := newTestService()
	id := mustStart(t, svc, "tpl-1", "i@x.com", "s@x.com", "c@x.com", "p@x.com")

	// i: 领取后暂停→围栏→（本轮取消）；s: 授权后暂停，暂停期间成功；
	// c: 领取未授权→围栏→取消；p: 纯 pending 经历暂停后取消。
	i := mustLeaseOne(t, svc, id, "w-i") // i@x.com
	rest, err := svc.Lease(id, LeaseRequest{WorkerID: "w-rest", Count: 2, LeaseDuration: time.Minute})
	if err != nil || len(rest) != 2 {
		t.Fatalf("lease s,c: %+v err=%v", rest, err)
	}
	sTask, cTask := rest[0], rest[1] // s@x.com, c@x.com
	if sTask.Recipient != "s@x.com" || cTask.Recipient != "c@x.com" {
		t.Fatalf("unexpected order: %s %s", sTask.Recipient, cTask.Recipient)
	}
	if _, err := svc.Authorize(id, sTask.DispatchKey, sTask.LeaseToken); err != nil {
		t.Fatal(err)
	}

	if _, _, err := svc.Pause(id); err != nil {
		t.Fatal(err)
	}
	st, _ := svc.Stats(id)
	// i/c/p 进入 paused；s 已授权保持 authorized 等待回执；其中 i、c 计暂停拦截。
	if st.Status != CampaignPaused || st.Paused != 3 || st.Authorized != 1 || st.PausedIntercepted != 2 {
		t.Fatalf("stats while paused: %+v", st)
	}

	// s 暂停期间完成。
	if ack, err := svc.SubmitReceipt(id, sTask.DispatchKey, sTask.LeaseToken, ReceiptInput{Result: ResultSuccess}); err != nil || ack.State != TaskSent {
		t.Fatalf("send while paused: %+v %v", ack, err)
	}
	// 暂停期间取消：c 被围栏后 canceled；其余暂停项一并 canceled。
	if _, _, err := svc.Cancel(id); err != nil {
		t.Fatal(err)
	}
	st, _ = svc.Stats(id)
	if st.Sent != 1 || st.Canceled != 3 || st.PausedIntercepted != 2 {
		t.Fatalf("stats after cancel: %+v", st)
	}

	// i 的旧授权决策仍是暂停拒绝；c 同理（暂停拦截与最终取消分别计数，互不覆盖）。
	di, _ := svc.Authorize(id, i.DispatchKey, i.LeaseToken)
	if di.Granted || di.Reason != ReasonCampaignPaused {
		t.Fatalf("intercepted attempt keeps paused denial: %+v", di)
	}
	dc, _ := svc.Authorize(id, cTask.DispatchKey, cTask.LeaseToken)
	if dc.Granted || dc.Reason != ReasonCampaignPaused {
		t.Fatalf("canceled-after-pause attempt keeps original denial: %+v", dc)
	}
}

// TestPauseResumeCycleInterceptCounter 多轮暂停/恢复：拦截计数按轮累加，发送只发生一次。
func TestPauseResumeCycleInterceptCounter(t *testing.T) {
	svc, _ := newTestService()
	id := mustStart(t, svc, "tpl-1", "x@x.com")

	for round := 1; round <= 2; round++ {
		task := mustLeaseOne(t, svc, id, "w")
		if _, _, err := svc.Pause(id); err != nil {
			t.Fatalf("pause round %d: %v", round, err)
		}
		dec, err := svc.Authorize(id, task.DispatchKey, task.LeaseToken)
		if err != nil || dec.Granted {
			t.Fatalf("round %d denial: %+v %v", round, dec, err)
		}
		if _, _, err := svc.Resume(id); err != nil {
			t.Fatalf("resume round %d: %v", round, err)
		}
	}
	st, _ := svc.Stats(id)
	if st.PausedIntercepted != 2 || st.Sent != 0 || st.TotalAttempts != 2 {
		t.Fatalf("cycle stats: %+v", st)
	}

	// 第三轮真正发送。
	last := mustLeaseOne(t, svc, id, "w-final")
	if last.Attempt != 3 {
		t.Fatalf("expected attempt 3, got %d", last.Attempt)
	}
	if d, _ := svc.Authorize(id, last.DispatchKey, last.LeaseToken); !d.Granted {
		t.Fatalf("final attempt must grant: %+v", d)
	}
	if ack, err := svc.SubmitReceipt(id, last.DispatchKey, last.LeaseToken, ReceiptInput{Result: ResultSuccess}); err != nil || ack.State != TaskSent {
		t.Fatalf("final send: %+v %v", ack, err)
	}
	st, _ = svc.Stats(id)
	if st.Sent != 1 || st.PausedIntercepted != 2 || st.TotalAttempts != 3 {
		t.Fatalf("final stats: %+v", st)
	}
}

// TestPauseResumeStateGuards 非法状态迁移。
func TestPauseResumeStateGuards(t *testing.T) {
	svc, _ := newTestService()
	c, err := svc.CreateCampaign(CampaignSpec{Name: "d"})
	if err != nil {
		t.Fatal(err)
	}
	// draft 不可暂停。
	if _, _, err := svc.Pause(c.ID); !IsCode(err, ErrConflict) {
		t.Fatalf("pause draft must conflict, got %v", err)
	}
	id := mustStart(t, svc, "tpl-1", "a@x.com")
	// running 上恢复为幂等 no-op（changed=false）。
	if _, changed, err := svc.Resume(id); err != nil || changed {
		t.Fatalf("resume running must be idempotent no-op, got changed=%v err=%v", changed, err)
	}
	// draft 不可恢复。
	if _, _, err := svc.Resume(c.ID); !IsCode(err, ErrConflict) {
		t.Fatalf("resume draft must conflict, got %v", err)
	}
	if _, _, err := svc.Pause(id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Pause("cmp_missing"); !IsCode(err, ErrNotFound) {
		t.Fatalf("pause missing: %v", err)
	}
	if _, _, err := svc.Resume("cmp_missing"); !IsCode(err, ErrNotFound) {
		t.Fatalf("resume missing: %v", err)
	}
}

// TestConcurrentPauseResumeLease 需求 2：暂停、恢复、领取并发时每封邮件只有一个一致结果，
// 暂停窗口内不可能出现新授权；恢复后所有未完成项最终仍可被处理一次。
func TestConcurrentPauseResumeLease(t *testing.T) {
	svc, _ := newTestService()
	const n = 20
	addrs := make([]string, n)
	for i := range addrs {
		addrs[i] = "r" + itoa(i) + "@x.com"
	}
	id := mustStart(t, svc, "tpl-1", addrs...)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 一个工作者循环领取 + 授权 + 成功回执；所有授权调用要么成功要么拿到持久化拒绝，绝不报错外的不一致。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			tasks, err := svc.Lease(id, LeaseRequest{WorkerID: "w", Count: 5, LeaseDuration: time.Minute})
			if err != nil {
				if IsCode(err, ErrCampaignNotReady) {
					time.Sleep(time.Millisecond)
					continue
				}
				t.Errorf("lease: %v", err)
				return
			}
			if len(tasks) == 0 {
				time.Sleep(time.Millisecond)
				continue
			}
			for _, task := range tasks {
				dec, err := svc.Authorize(id, task.DispatchKey, task.LeaseToken)
				if err != nil {
					t.Errorf("authorize: %v", err)
					continue
				}
				if !dec.Granted {
					// 被暂停/取消围栏是合法一致结果；不得再发。
					if _, err := svc.SubmitReceipt(id, task.DispatchKey, task.LeaseToken, ReceiptInput{Result: ResultSuccess}); !IsCode(err, ErrInvalidState) {
						t.Errorf("denied task must reject receipt, got %v", err)
					}
					continue
				}
				if _, err := svc.SubmitReceipt(id, task.DispatchKey, task.LeaseToken, ReceiptInput{Result: ResultSuccess}); err != nil {
					t.Errorf("receipt: %v", err)
				}
			}
		}
	}()

	// 反复暂停/恢复若干轮。
	for i := 0; i < 30; i++ {
		if _, _, err := svc.Pause(id); err != nil && !IsCode(err, ErrConflict) {
			t.Fatalf("pause: %v", err)
		}
		if _, _, err := svc.Resume(id); err != nil && !IsCode(err, ErrConflict) {
			t.Fatalf("resume: %v", err)
		}
	}
	close(stop)
	wg.Wait()

	// 最终恢复，排空所有未完成项。
	if _, _, err := svc.Resume(id); err != nil {
		t.Fatalf("final resume: %v", err)
	}
	for {
		tasks, _ := svc.Lease(id, LeaseRequest{WorkerID: "drain", Count: n, LeaseDuration: time.Minute})
		if len(tasks) == 0 {
			break
		}
		for _, task := range tasks {
			dec, err := svc.Authorize(id, task.DispatchKey, task.LeaseToken)
			if err != nil {
				t.Fatalf("drain authorize: %v", err)
			}
			if !dec.Granted {
				t.Fatalf("running campaign with no suppressions must grant, got %+v", dec)
			}
			if _, err := svc.SubmitReceipt(id, task.DispatchKey, task.LeaseToken, ReceiptInput{Result: ResultSuccess}); err != nil {
				t.Fatalf("drain receipt: %v", err)
			}
		}
	}

	st, _ := svc.Stats(id)
	if st.Sent != n {
		t.Fatalf("every recipient must be sent exactly once, stats: %+v", st)
	}
	if st.Canceled+st.Suppressed+st.Failed != 0 {
		t.Fatalf("unexpected terminal outcomes: %+v", st)
	}
	// 审计逐封核对：至多一个 attempt 有成功 outcome，被围栏的 attempt 全部带 paused 拒绝决策。
	for _, addr := range addrs {
		key := DispatchKey(id, addr)
		detail, err := svc.GetDispatch(id, key)
		if err != nil {
			t.Fatalf("dispatch %s: %v", addr, err)
		}
		var successes int
		for _, a := range detail.Attempts {
			if a.Outcome != nil && a.Outcome.Result == ResultSuccess {
				successes++
			}
			if a.Decision != nil && a.Decision.Granted && a.Outcome == nil {
				t.Fatalf("granted attempt without outcome for %s", addr)
			}
		}
		if successes != 1 {
			t.Fatalf("%s must have exactly one success outcome, got %d", addr, successes)
		}
	}
}
