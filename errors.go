package emaildispatch

import (
	"errors"
	"fmt"
)

// ErrorCode 用稳定字符串标识错误类别，便于调用方分支处理与 HTTP 映射。
type ErrorCode string

const (
	ErrValidation       ErrorCode = "validation_error"     // 入参不合法
	ErrNotFound         ErrorCode = "not_found"            // 活动/投递项不存在
	ErrConflict         ErrorCode = "conflict"             // 状态冲突（如重复启动）
	ErrInvalidLease     ErrorCode = "invalid_lease"        // 租约不存在或 token 不匹配
	ErrLeaseExpired     ErrorCode = "lease_expired"        // 租约已过期，旧工作者的操作被拒绝
	ErrStaleAttempt     ErrorCode = "stale_attempt"        // 回执对应的尝试已被新尝试取代（fencing）
	ErrNoDispatchable   ErrorCode = "no_dispatchable"      // 当前没有可领取的发送项（可安全重试）
	ErrCampaignNotReady ErrorCode = "campaign_not_running" // 活动未处于 running
	ErrInvalidReceipt   ErrorCode = "invalid_receipt"      // 回执结论不合法
	ErrInvalidState     ErrorCode = "invalid_state"        // 投递项当前状态不允许该操作
)

// Error 是本服务所有领域错误的统一类型。
type Error struct {
	Code    ErrorCode
	Message string
	// Op 描述出错的操作，便于定位。
	Op string
}

func (e *Error) Error() string {
	if e.Op != "" {
		return fmt.Sprintf("emaildispatch: %s: %s", e.Op, e.Message)
	}
	return "emaildispatch: " + e.Message
}

func newError(op string, code ErrorCode, format string, args ...any) error {
	return &Error{Code: code, Op: op, Message: fmt.Sprintf(format, args...)}
}

// AsError 从任意 error 中提取领域 *Error，未命中时返回 nil。
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}

// IsCode 报告 err 是否为本服务指定类别的领域错误。
func IsCode(err error, code ErrorCode) bool {
	if e := AsError(err); e != nil {
		return e.Code == code
	}
	return false
}
