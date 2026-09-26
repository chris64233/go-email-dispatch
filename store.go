package emaildispatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// state 是落盘的完整服务状态。
type state struct {
	Seq              int                  `json:"seq"`
	Templates        map[string]*Template `json:"templates"` // 每个 id 只保留最新版本
	Campaigns        map[string]*Campaign `json:"campaigns"`
	Deliveries       map[string]*Delivery `json:"deliveries"`
	CampaignDelivery map[string][]string  `json:"campaign_deliveries"`
	Suppressions     []*Suppression       `json:"suppressions"`
	// ReceiptResults 记录已处理回执的稳定结果，用于重复回执去重。
	ReceiptResults map[string]*ReceiptResult `json:"receipt_results"`
}

func newState() *state {
	return &state{
		Templates:        map[string]*Template{},
		Campaigns:        map[string]*Campaign{},
		Deliveries:       map[string]*Delivery{},
		CampaignDelivery: map[string][]string{},
		ReceiptResults:   map[string]*ReceiptResult{},
	}
}

// Store 抽象持久化：所有读改写都必须在 Update 回调内原子完成。
type Store interface {
	Update(mut func(s *state) error) error
	Read(view func(s *state))
}

// MemoryStore 仅驻留内存的 Store，用于测试。
type MemoryStore struct {
	mu sync.RWMutex
	s  *state
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{s: newState()}
}

func (m *MemoryStore) Update(mut func(*state) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return mut(m.s)
}

func (m *MemoryStore) Read(view func(*state)) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	view(m.s)
}

// FileStore 在每次变更后把状态以 JSON 原子写入文件（临时文件 + rename）。
type FileStore struct {
	mu   sync.RWMutex
	path string
	s    *state
}

// NewFileStore 打开 path；文件不存在则初始化为空状态。
func NewFileStore(path string) (*FileStore, error) {
	f := &FileStore{path: path, s: newState()}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return f, f.persist()
		}
		return nil, err
	}
	if err := json.Unmarshal(b, f.s); err != nil {
		return nil, err
	}
	if f.s.Templates == nil {
		f.s = newState()
	}
	return f, nil
}

func (f *FileStore) Update(mut func(*state) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := mut(f.s); err != nil {
		return err
	}
	return f.persist()
}

func (f *FileStore) Read(view func(*state)) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	view(f.s)
}

// persist 调用方必须已持有写锁。
func (f *FileStore) persist() error {
	b, err := json.MarshalIndent(f.s, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(f.path)
	tmp, err := os.CreateTemp(dir, ".dispatch-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, f.path)
}
