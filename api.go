package emaildispatch

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Handler 暴露批量邮件投递的 HTTP 接口。
type Handler struct {
	svc *Service
	mux *http.ServeMux
}

// NewHTTPHandler 构造挂载好全部路由的 HTTP Handler。
func NewHTTPHandler(svc *Service) *Handler {
	h := &Handler{svc: svc, mux: http.NewServeMux()}
	h.routes()
	return h
}

// ServeHTTP 实现 http.Handler。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

/*
路由清单：

	POST   /v1/campaigns                                    创建活动（草稿）
	POST   /v1/campaigns/{id}/start                          启动：锁定模板版本、冻结受众
	GET    /v1/campaigns/{id}                                查询活动
	POST   /v1/campaigns/{id}/leases                         领取发送项（有期限租约）
	POST   /v1/campaigns/{id}/cancel                         取消活动（幂等）
	GET    /v1/campaigns/{id}/stats                          统计查询
	GET    /v1/campaigns/{id}/dispatches/{key}               投递项审计轨迹
	POST   /v1/campaigns/{id}/dispatches/{key}/authorize     持久化发送授权点
	POST   /v1/campaigns/{id}/dispatches/{key}/receipts      提交发送回执
	POST   /v1/suppressions                                  录入抑制事件
*/
func (h *Handler) routes() {
	h.mux.HandleFunc("POST /v1/campaigns", h.createCampaign)
	h.mux.HandleFunc("POST /v1/campaigns/{id}/start", h.startCampaign)
	h.mux.HandleFunc("GET /v1/campaigns/{id}", h.getCampaign)
	h.mux.HandleFunc("POST /v1/campaigns/{id}/leases", h.lease)
	h.mux.HandleFunc("POST /v1/campaigns/{id}/cancel", h.cancel)
	h.mux.HandleFunc("GET /v1/campaigns/{id}/stats", h.stats)
	h.mux.HandleFunc("GET /v1/campaigns/{id}/dispatches/{key}", h.getDispatch)
	h.mux.HandleFunc("POST /v1/campaigns/{id}/dispatches/{key}/authorize", h.authorize)
	h.mux.HandleFunc("POST /v1/campaigns/{id}/dispatches/{key}/receipts", h.receipt)
	h.mux.HandleFunc("POST /v1/suppressions", h.suppression)
}

type leaseRequestDTO struct {
	WorkerID        string `json:"worker_id"`
	Count           int    `json:"count"`
	LeaseDurationMS int64  `json:"lease_duration_ms"`
}

type receiptRequestDTO struct {
	LeaseToken        string        `json:"lease_token"`
	Result            ReceiptResult `json:"result"`
	ProviderMessageID string        `json:"provider_message_id,omitempty"`
	Code              string        `json:"code,omitempty"`
	Message           string        `json:"message,omitempty"`
}

type suppressionRequestDTO struct {
	Type         SuppressionType `json:"type"`
	Address      string          `json:"address"`
	CampaignID   string          `json:"campaign_id,omitempty"`
	Reason       string          `json:"reason,omitempty"`
	OccurredAtMS *int64          `json:"occurred_at_ms,omitempty"`
}

type cancelResponseDTO struct {
	Campaign *Campaign `json:"campaign"`
	Changed  bool      `json:"changed"`
}

func (h *Handler) createCampaign(w http.ResponseWriter, r *http.Request) {
	var spec CampaignSpec
	if err := decodeJSON(r, &spec); err != nil {
		writeDomainError(w, err)
		return
	}
	c, err := h.svc.CreateCampaign(spec)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (h *Handler) startCampaign(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var spec StartSpec
	if err := decodeJSON(r, &spec); err != nil {
		writeDomainError(w, err)
		return
	}
	c, err := h.svc.StartCampaign(id, spec)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) getCampaign(w http.ResponseWriter, r *http.Request) {
	c, err := h.svc.GetCampaign(r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) lease(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var dto leaseRequestDTO
	if err := decodeJSON(r, &dto); err != nil {
		writeDomainError(w, err)
		return
	}
	tasks, err := h.svc.Lease(id, LeaseRequest{
		WorkerID:      dto.WorkerID,
		Count:         dto.Count,
		LeaseDuration: time.Duration(dto.LeaseDurationMS) * time.Millisecond,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	if tasks == nil {
		tasks = []LeasedTask{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	c, changed, err := h.svc.Cancel(r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cancelResponseDTO{Campaign: c, Changed: changed})
}

func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	st, err := h.svc.Stats(r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (h *Handler) getDispatch(w http.ResponseWriter, r *http.Request) {
	d, err := h.svc.GetDispatch(r.PathValue("id"), r.PathValue("key"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

type authorizeRequestDTO struct {
	LeaseToken string `json:"lease_token"`
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request) {
	id, key := r.PathValue("id"), r.PathValue("key")
	var dto authorizeRequestDTO
	if err := decodeJSON(r, &dto); err != nil {
		writeDomainError(w, err)
		return
	}
	dec, err := h.svc.Authorize(id, key, dto.LeaseToken)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dec)
}

func (h *Handler) receipt(w http.ResponseWriter, r *http.Request) {
	id, key := r.PathValue("id"), r.PathValue("key")
	var dto receiptRequestDTO
	if err := decodeJSON(r, &dto); err != nil {
		writeDomainError(w, err)
		return
	}
	token := leaseTokenFromRequest(r, dto.LeaseToken)
	ack, err := h.svc.SubmitReceipt(id, key, token, ReceiptInput{
		Result:            dto.Result,
		ProviderMessageID: dto.ProviderMessageID,
		Code:              dto.Code,
		Message:           dto.Message,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ack)
}

// leaseTokenFromRequest 依次取查询参数、X-Lease-Token 头、请求体 lease_token 字段。
func leaseTokenFromRequest(r *http.Request, bodyToken string) string {
	if t := strings.TrimSpace(r.URL.Query().Get("lease_token")); t != "" {
		return t
	}
	if t := strings.TrimSpace(r.Header.Get("X-Lease-Token")); t != "" {
		return t
	}
	return bodyToken
}

func (h *Handler) suppression(w http.ResponseWriter, r *http.Request) {
	var dto suppressionRequestDTO
	if err := decodeJSON(r, &dto); err != nil {
		writeDomainError(w, err)
		return
	}
	in := SuppressionInput{
		Type:       dto.Type,
		Address:    dto.Address,
		CampaignID: dto.CampaignID,
		Reason:     dto.Reason,
	}
	if dto.OccurredAtMS != nil {
		in.OccurredAt = time.UnixMilli(*dto.OccurredAtMS).UTC()
	}
	ev, err := h.svc.RecordSuppression(in)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ev)
}

// ---- HTTP 辅助 ----

func decodeJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return newError("http", ErrValidation, "empty request body")
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return newError("http", ErrValidation, "invalid JSON body: %v", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type errorBody struct {
	Error struct {
		Code    ErrorCode `json:"code"`
		Message string    `json:"message"`
	} `json:"error"`
}

func writeDomainError(w http.ResponseWriter, err error) {
	body := errorBody{}
	de := AsError(err)
	if de == nil {
		body.Error.Code = ErrorCode("internal_error")
		body.Error.Message = "internal error"
		writeJSON(w, http.StatusInternalServerError, body)
		return
	}
	body.Error.Code = de.Code
	body.Error.Message = de.Message
	writeJSON(w, httpStatusFor(de.Code), body)
}

func httpStatusFor(code ErrorCode) int {
	switch code {
	case ErrValidation, ErrInvalidReceipt:
		return http.StatusBadRequest
	case ErrNotFound:
		return http.StatusNotFound
	case ErrConflict, ErrInvalidLease, ErrLeaseExpired, ErrStaleAttempt,
		ErrCampaignNotReady, ErrInvalidState:
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}
