package emaildispatch

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer() (*httptest.Server, *Service, *fakeClock) {
	svc, clk := newTestService()
	h := NewHTTPHandler(svc)
	return httptest.NewServer(h), svc, clk
}

func doJSON(t *testing.T, srv *httptest.Server, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s: %v", resp.Status, err)
	}
	return resp.StatusCode, out
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected object, got %T: %v", v, v)
	}
	return m
}

// TestAPIEndToEnd 走通 创建→启动→抑制→领取→授权→回执→统计→取消 的完整 HTTP 流程。
func TestAPIEndToEnd(t *testing.T) {
	srv, _, _ := newTestServer()
	defer srv.Close()

	// 创建活动。
	status, body := doJSON(t, srv, http.MethodPost, "/v1/campaigns", map[string]any{
		"name": "welcome flow",
		"retry_policy": map[string]any{
			"max_attempts":       2,
			"initial_backoff_ms": 1000,
			"max_backoff_ms":     5000,
			"multiplier":         2,
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("create status %d body %v", status, body)
	}
	campaignID, _ := body["id"].(string)
	if !strings.HasPrefix(campaignID, "cmp_") {
		t.Fatalf("bad campaign id: %v", body)
	}

	// 非法入参 → 400 + 领域错误码。
	status, body = doJSON(t, srv, http.MethodPost, "/v1/campaigns", map[string]any{})
	if status != http.StatusBadRequest || asMap(t, body["error"])["code"] != string(ErrValidation) {
		t.Fatalf("expected 400 validation_error, got %d %v", status, body)
	}

	// 启动：锁定模板版本、冻结受众。
	status, body = doJSON(t, srv, http.MethodPost, "/v1/campaigns/"+campaignID+"/start", map[string]any{
		"template_version": "tpl-42",
		"recipients": []map[string]any{
			{"address": "a@x.com"},
			{"address": "off@x.com"},
		},
	})
	if status != http.StatusOK || body["template_version"] != "tpl-42" {
		t.Fatalf("start failed: %d %v", status, body)
	}

	// 录入全局退订（off@x.com）。
	status, body = doJSON(t, srv, http.MethodPost, "/v1/suppressions", map[string]any{
		"type": "global_unsubscribe", "address": "OFF@X.COM", "reason": "link",
	})
	if status != http.StatusCreated || body["address"] != "off@x.com" {
		t.Fatalf("suppression failed: %d %v", status, body)
	}

	// 批量领取。
	status, body = doJSON(t, srv, http.MethodPost, "/v1/campaigns/"+campaignID+"/leases", map[string]any{
		"worker_id": "worker-1", "count": 10, "lease_duration_ms": 60000,
	})
	if status != http.StatusOK {
		t.Fatalf("lease failed: %d %v", status, body)
	}
	tasksRaw, _ := body["tasks"].([]any)
	if len(tasksRaw) != 2 {
		t.Fatalf("expected 2 leased tasks, got %v", body)
	}

	dispatch := func(recipient string) map[string]any {
		for _, item := range tasksRaw {
			m := asMap(t, item)
			if m["recipient"] == recipient {
				return m
			}
		}
		t.Fatalf("recipient %s not in leases %v", recipient, tasksRaw)
		return nil
	}

	// a@x.com 授权放行 → 成功回执。
	aTask := dispatch("a@x.com")
	keyA, _ := aTask["dispatch_key"].(string)
	tokenA, _ := aTask["lease_token"].(string)
	status, body = doJSON(t, srv, http.MethodPost,
		"/v1/campaigns/"+campaignID+"/dispatches/"+keyA+"/authorize",
		map[string]any{"lease_token": tokenA})
	if status != http.StatusOK || body["granted"] != true {
		t.Fatalf("authorize a: %d %v", status, body)
	}
	status, body = doJSON(t, srv, http.MethodPost,
		"/v1/campaigns/"+campaignID+"/dispatches/"+keyA+"/receipts",
		map[string]any{"lease_token": tokenA, "result": "success", "provider_message_id": "pm-1"})
	if status != http.StatusOK || body["state"] != string(TaskSent) {
		t.Fatalf("receipt a: %d %v", status, body)
	}
	// 重复回执：200 + duplicate=true，统计不重复累加。
	status, body = doJSON(t, srv, http.MethodPost,
		"/v1/campaigns/"+campaignID+"/dispatches/"+keyA+"/receipts",
		map[string]any{"lease_token": tokenA, "result": "success"})
	if status != http.StatusOK || body["duplicate"] != true {
		t.Fatalf("duplicate receipt: %d %v", status, body)
	}

	// off@x.com 授权点被拦截。
	offTask := dispatch("off@x.com")
	keyOff, _ := offTask["dispatch_key"].(string)
	tokenOff, _ := offTask["lease_token"].(string)
	status, body = doJSON(t, srv, http.MethodPost,
		"/v1/campaigns/"+campaignID+"/dispatches/"+keyOff+"/authorize",
		map[string]any{"lease_token": tokenOff})
	if status != http.StatusOK || body["granted"] != false || body["reason"] != string(ReasonGlobalUnsubscribe) {
		t.Fatalf("authorize off should deny: %d %v", status, body)
	}

	// 统计。
	status, body = doJSON(t, srv, http.MethodGet, "/v1/campaigns/"+campaignID+"/stats", nil)
	if status != http.StatusOK || body["sent"] != float64(1) || body["suppressed"] != float64(1) ||
		body["total"] != float64(2) || body["total_attempts"] != float64(2) {
		t.Fatalf("stats mismatch: %d %v", status, body)
	}

	// 取消（幂等两次）。
	status, body = doJSON(t, srv, http.MethodPost, "/v1/campaigns/"+campaignID+"/cancel", map[string]any{})
	if status != http.StatusOK || body["changed"] != true {
		t.Fatalf("first cancel: %d %v", status, body)
	}
	status, body = doJSON(t, srv, http.MethodPost, "/v1/campaigns/"+campaignID+"/cancel", map[string]any{})
	if status != http.StatusOK || body["changed"] != false {
		t.Fatalf("second cancel must be stable: %d %v", status, body)
	}

	// 审计轨迹可查。
	status, body = doJSON(t, srv, http.MethodGet,
		"/v1/campaigns/"+campaignID+"/dispatches/"+keyA, nil)
	if status != http.StatusOK {
		t.Fatalf("get dispatch: %d %v", status, body)
	}
	attempts, _ := body["attempts"].([]any)
	if len(attempts) != 1 {
		t.Fatalf("expected 1 attempt in audit, got %v", body)
	}

	// 不存在的活动 → 404。
	status, _ = doJSON(t, srv, http.MethodGet, "/v1/campaigns/cmp_nonexistent/stats", nil)
	if status != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", status)
	}
}

// TestAPIPauseResume 暂停/恢复接口的 HTTP 语义：幂等 changed、领取 409、围栏拒绝、恢复后重新发送。
func TestAPIPauseResume(t *testing.T) {
	srv, svc, _ := newTestServer()
	defer srv.Close()

	id := mustStart(t, svc, "tpl-1", "a@x.com")
	task := mustLeaseOne(t, svc, id, "w1")

	// 暂停：200 changed=true，活动 paused。
	status, body := doJSON(t, srv, http.MethodPost, "/v1/campaigns/"+id+"/pause", map[string]any{})
	if status != http.StatusOK || body["changed"] != true || asMap(t, body["campaign"])["status"] != string(CampaignPaused) {
		t.Fatalf("pause: %d %v", status, body)
	}
	// 重复暂停：changed=false。
	status, body = doJSON(t, srv, http.MethodPost, "/v1/campaigns/"+id+"/pause", map[string]any{})
	if status != http.StatusOK || body["changed"] != false {
		t.Fatalf("idempotent pause: %d %v", status, body)
	}
	// 暂停期间领取：409 campaign_not_running。
	status, body = doJSON(t, srv, http.MethodPost, "/v1/campaigns/"+id+"/leases",
		map[string]any{"worker_id": "w2"})
	if status != http.StatusConflict || asMap(t, body["error"])["code"] != string(ErrCampaignNotReady) {
		t.Fatalf("lease while paused: %d %v", status, body)
	}
	// 围栏任务授权：200 granted=false reason=campaign_paused。
	status, body = doJSON(t, srv, http.MethodPost,
		"/v1/campaigns/"+id+"/dispatches/"+task.DispatchKey+"/authorize",
		map[string]any{"lease_token": task.LeaseToken})
	if status != http.StatusOK || body["granted"] != false || body["reason"] != string(ReasonCampaignPaused) {
		t.Fatalf("fenced authorize: %d %v", status, body)
	}

	// 恢复后旧 token 仍被围栏，新租约可完成发送。
	status, _ = doJSON(t, srv, http.MethodPost, "/v1/campaigns/"+id+"/resume", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("resume: %d", status)
	}
	status, body = doJSON(t, srv, http.MethodPost, "/v1/campaigns/"+id+"/leases",
		map[string]any{"worker_id": "w2", "lease_duration_ms": 60000})
	if status != http.StatusOK {
		t.Fatalf("lease after resume: %d %v", status, body)
	}
	tasks, _ := body["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task after resume, got %v", body)
	}
	fresh := asMap(t, tasks[0])
	if fresh["attempt"] != float64(2) || fresh["template_version"] != "tpl-1" {
		t.Fatalf("resumed lease should be attempt 2 with frozen template: %v", fresh)
	}
	key, token := fresh["dispatch_key"].(string), fresh["lease_token"].(string)
	status, body = doJSON(t, srv, http.MethodPost,
		"/v1/campaigns/"+id+"/dispatches/"+key+"/authorize",
		map[string]any{"lease_token": token})
	if status != http.StatusOK || body["granted"] != true {
		t.Fatalf("authorize after resume: %d %v", status, body)
	}
	status, body = doJSON(t, srv, http.MethodPost,
		"/v1/campaigns/"+id+"/dispatches/"+key+"/receipts",
		map[string]any{"lease_token": token, "result": "success"})
	if status != http.StatusOK || body["state"] != string(TaskSent) {
		t.Fatalf("receipt after resume: %d %v", status, body)
	}

	// 统计中区分暂停拦截与实际发送。
	status, body = doJSON(t, srv, http.MethodGet, "/v1/campaigns/"+id+"/stats", nil)
	if status != http.StatusOK || body["sent"] != float64(1) || body["paused_intercepted"] != float64(1) ||
		body["canceled"] != float64(0) {
		t.Fatalf("stats: %d %v", status, body)
	}
}
func TestAPIStaleLeaseConflict(t *testing.T) {
	srv, svc, clk := newTestServer()
	defer srv.Close()

	id := mustStart(t, svc, "tpl-1", "a@x.com")
	first := mustLeaseOne(t, svc, id, "slow")
	clk.advance(time.Minute + time.Second)
	second := mustLeaseOne(t, svc, id, "fast")
	if second.Attempt != 2 {
		t.Fatalf("expected attempt 2, got %d", second.Attempt)
	}

	status, body := doJSON(t, srv, http.MethodPost,
		"/v1/campaigns/"+id+"/dispatches/"+first.DispatchKey+"/receipts",
		map[string]any{"lease_token": first.LeaseToken, "result": "success"})
	if status != http.StatusConflict || asMap(t, body["error"])["code"] != string(ErrStaleAttempt) {
		t.Fatalf("expected 409 stale_attempt, got %d %v", status, body)
	}

	// 未启动活动领取 → 409 campaign_not_running。
	draft, err := svc.CreateCampaign(CampaignSpec{Name: "d"})
	if err != nil {
		t.Fatal(err)
	}
	status, body = doJSON(t, srv, http.MethodPost, "/v1/campaigns/"+draft.ID+"/leases",
		map[string]any{"worker_id": "w"})
	if status != http.StatusConflict || asMap(t, body["error"])["code"] != string(ErrCampaignNotReady) {
		t.Fatalf("expected 409 campaign_not_running, got %d %v", status, body)
	}
}
