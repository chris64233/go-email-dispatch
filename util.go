package emaildispatch

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
)

// normalizeAddress 将收件地址规范化为比较键：去首尾空白并小写（邮箱本地名除外的域部分）。
// 这里采用保守策略：整体 trim + 小写，足够覆盖测试与大多数提供商行为。
func normalizeAddress(addr string) string {
	return strings.ToLower(strings.TrimSpace(addr))
}

// newToken 生成不可猜测的租约令牌。
func newToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read 在主流平台不会失败；失败时直接 panic（进程级环境异常）。
		panic("emaildispatch: cannot read random bytes: " + err.Error())
	}
	return "lease_" + hex.EncodeToString(b[:])
}

// newID 生成活动等实体的短随机标识。
func newID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("emaildispatch: cannot read random bytes: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

func itoa(n int) string     { return strconv.Itoa(n) }
func itoa64(n int64) string { return strconv.FormatInt(n, 10) }
