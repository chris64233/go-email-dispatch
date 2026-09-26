package emaildispatch

import "errors"

// 服务暴露的全部错误均为可比较的哨兵错误，调用方可用 errors.Is 判定。
var (
	// ErrNotFound 活动、发送项或模板不存在。
	ErrNotFound = errors.New("emaildispatch: not found")
	// ErrCampaignNotDraft 操作要求活动处于草稿态（如添加收件人、启动）。
	ErrCampaignNotDraft = errors.New("emaildispatch: campaign is not in draft state")
	// ErrCampaignNotActive 操作要求活动处于运行态（如领取发送项）。
	ErrCampaignNotActive = errors.New("emaildispatch: campaign is not running")
	// ErrStaleLease 回执携带的租约令牌与当前租约不匹配（租约已过期并被他人领取）。
	ErrStaleLease = errors.New("emaildispatch: stale lease token")
	// ErrDeliveryNotLeased 回执针对的发送项当前未处于租出状态。
	ErrDeliveryNotLeased = errors.New("emaildispatch: delivery is not leased")
	// ErrDeliveryTerminal 发送项已进入终态，且回执不是已处理回执的重复。
	ErrDeliveryTerminal = errors.New("emaildispatch: delivery is in terminal state")
	// ErrInvalidOutcome 回执结果取值非法。
	ErrInvalidOutcome = errors.New("emaildispatch: invalid receipt outcome")
	// ErrInvalidArgument 请求参数非法。
	ErrInvalidArgument = errors.New("emaildispatch: invalid argument")
)
