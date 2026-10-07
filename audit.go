package emaildispatch

import (
	"fmt"
	"log"
	"strings"
	"sync"
)

// AuditLogger 记录合规相关的操作审计。实现方不得在日志中输出完整收件地址，
// 统一使用 MaskAddress 脱敏，避免敏感地址进入普通日志。
type AuditLogger interface {
	Logf(format string, args ...any)
}

// MaskAddress 将邮箱地址脱敏为可安全写日志的形式：
// "alice@example.com" -> "a****@example.com"；非法输入整体打码。
func MaskAddress(addr string) string {
	addr = normalizeAddress(addr)
	at := strings.IndexByte(addr, '@')
	if at <= 0 {
		if addr == "" {
			return ""
		}
		return "****"
	}
	return addr[:1] + "****" + addr[at:]
}

// stdLogger 用标准库 log 输出审计行（默认实现）。
type stdLogger struct{ l *log.Logger }

func (s stdLogger) Logf(format string, args ...any) {
	s.l.Printf(format, args...)
}

// NewStdAuditLogger 返回基于标准库 logger 的审计日志。
func NewStdAuditLogger(l *log.Logger) AuditLogger {
	if l == nil {
		l = log.Default()
	}
	return stdLogger{l: l}
}

// memoryAuditLogger 是供测试/调用方使用的进程内捕获型审计日志。
type memoryAuditLogger struct {
	mu    sync.Mutex
	lines []string
}

// NewMemoryAuditLogger 返回内存审计日志，可用于断言与排查。
func NewMemoryAuditLogger() interface {
	AuditLogger
	Lines() []string
} {
	return &memoryAuditLogger{}
}

func (m *memoryAuditLogger) Logf(format string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lines = append(m.lines, fmt.Sprintf(format, args...))
}

func (m *memoryAuditLogger) Lines() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.lines...)
}
