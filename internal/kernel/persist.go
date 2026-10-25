package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrSessionNotFound 表示 Store 中没有该 id 的会话。
var ErrSessionNotFound = errors.New("kernel: session not found")

// sessionIDPattern 限定会话 id，防止路径穿越与非法文件名。
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidateSessionID 校验会话 id 是否可用作存储键 / 文件名。
func ValidateSessionID(id string) error {
	if !sessionIDPattern.MatchString(id) {
		return fmt.Errorf("kernel: invalid session id %q (want %s)", id, sessionIDPattern.String())
	}
	if strings.Contains(id, "..") {
		return fmt.Errorf("kernel: invalid session id %q (contains ..)", id)
	}
	return nil
}

// SessionInfo 是会话的元信息（由 state 解析而来，便于列表/巡检）。
type SessionInfo struct {
	ID               string    `json:"id"`
	UpdatedAt        time.Time `json:"updatedAt"`
	Bytes            int       `json:"bytes"`
	Blocks           int       `json:"blocks"`
	ActiveBlocks     int       `json:"activeBlocks"`
	Refs             int       `json:"refs"`
	TokensCompressed int       `json:"tokensCompressed"`
	Compressions     int       `json:"compressionCount"`
}

// Store 是会话状态的持久化后端。
//
// 内置实现：*FileStore（JSON 文件）、*MemoryStore（进程内，适合测试）。
// SQLite / Redis / 对象存储只需实现这 4 个方法（见 docs/kernel.md 的 SQLite 示例）。
//
// 约定：state 是不透明字节（由 StateJSON 产生），实现不应修改其内容。
type Store interface {
	// Save 写入（或覆盖）一个会话，返回从 state 解析出的元信息。
	Save(ctx context.Context, id string, state []byte) (SessionInfo, error)
	// Load 读取会话状态与元信息；不存在时返回 ErrSessionNotFound。
	Load(ctx context.Context, id string) (SessionInfo, []byte, error)
	// Delete 删除会话；不存在时返回 nil（幂等）。
	Delete(ctx context.Context, id string) error
	// List 返回全部会话元信息，按 UpdatedAt 降序。
	List(ctx context.Context) ([]SessionInfo, error)
}

// stateShape 只解出生成 SessionInfo 需要的字段，避免与上游 state 结构强耦合。
type stateShape struct {
	Blocks []struct {
		BlockID          string `json:"blockId"`
		CompressedTokens int    `json:"compressedTokens"`
		Active           bool   `json:"active"`
	} `json:"blocks"`
	MessageRefs struct {
		ByRaw map[string]json.RawMessage `json:"byRaw"`
	} `json:"messageRefs"`
	Stats struct {
		TokensCompressed int `json:"tokensCompressed"`
		CompressionCount int `json:"compressionCount"`
	} `json:"stats"`
}

// SummarizeState 从 state JSON 提取元信息（state 原样保留，不会被修改）。
func SummarizeState(id string, state []byte) (SessionInfo, error) {
	info := SessionInfo{ID: id, Bytes: len(state)}
	if len(state) == 0 {
		return info, errors.New("kernel: empty state")
	}
	var shape stateShape
	if err := json.Unmarshal(state, &shape); err != nil {
		return info, fmt.Errorf("kernel: parse state: %w", err)
	}
	info.Blocks = len(shape.Blocks)
	for _, b := range shape.Blocks {
		if b.Active {
			info.ActiveBlocks++
		}
	}
	info.Refs = len(shape.MessageRefs.ByRaw)
	info.TokensCompressed = shape.Stats.TokensCompressed
	info.Compressions = shape.Stats.CompressionCount
	return info, nil
}

// Persist 导出当前 state 并写入 st。返回写入后的元信息。
func (s *Session) Persist(ctx context.Context, st Store, id string) (SessionInfo, error) {
	if err := ValidateSessionID(id); err != nil {
		return SessionInfo{}, err
	}
	state, err := s.StateJSON()
	if err != nil {
		return SessionInfo{}, err
	}
	return st.Save(ctx, id, state)
}

// Restore 从 st 载入 state 覆盖当前会话。
//
// 载入后的渲染视图是空的，但 `s.last` 不参与 state 校验：实测（`kernel 0.0.163`）
// 恢复后直接 ApplyCompression 会被接受。若你的上游版本拒绝，先调 SetMessages
// 或先跑一次 Turn 再压。
func (s *Session) Restore(ctx context.Context, st Store, id string) (SessionInfo, error) {
	info, state, err := st.Load(ctx, id)
	if err != nil {
		return SessionInfo{}, err
	}
	if err := s.LoadState(state); err != nil {
		return SessionInfo{}, err
	}
	return info, nil
}

// NewSessionFrom 创建会话并从 st 恢复指定 id 的状态。
func (k *Kernel) NewSessionFrom(ctx context.Context, st Store, id string, cfg Config) (*Session, error) {
	sess, err := k.NewSession(cfg)
	if err != nil {
		return nil, err
	}
	if _, err := sess.Restore(ctx, st, id); err != nil {
		_ = sess.Close()
		return nil, err
	}
	return sess, nil
}

// FileStore 把每个会话写到 dir/<id>.json。
//
// 单文件存放 envelope（meta + state）：写入走「临时文件 + fsync + rename」，
// 崩溃时不会留下半截文件；陈旧临时文件由 CleanupTemp 清理。
type FileStore struct {
	dir string

	mu sync.Mutex // 进程内串行化（跨进程由 rename 的原子性保证）
}

const fileStoreVersion = 1

// sessionEnvelope 是磁盘上的文件格式。
type sessionEnvelope struct {
	Version   int             `json:"version"`
	ID        string          `json:"id"`
	UpdatedAt time.Time       `json:"updatedAt"`
	Meta      SessionInfo     `json:"meta"`
	State     json.RawMessage `json:"state"`
}

// NewFileStore 创建（或复用）一个文件存储目录。
func NewFileStore(dir string) (*FileStore, error) {
	if dir == "" {
		return nil, errors.New("kernel: empty store dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("kernel: create store dir: %w", err)
	}
	return &FileStore{dir: dir}, nil
}

// Dir 返回存储目录。
func (f *FileStore) Dir() string { return f.dir }

func (f *FileStore) path(id string) string {
	return filepath.Join(f.dir, id+".json")
}

// Save 原子写入一个会话。
func (f *FileStore) Save(ctx context.Context, id string, state []byte) (SessionInfo, error) {
	if err := ctxErr(ctx); err != nil {
		return SessionInfo{}, err
	}
	if err := ValidateSessionID(id); err != nil {
		return SessionInfo{}, err
	}
	info, err := SummarizeState(id, state)
	if err != nil {
		return SessionInfo{}, err
	}
	info.UpdatedAt = time.Now().UTC()

	env := sessionEnvelope{
		Version:   fileStoreVersion,
		ID:        id,
		UpdatedAt: info.UpdatedAt,
		Meta:      info,
		State:     json.RawMessage(state),
	}
	blob, err := json.Marshal(&env)
	if err != nil {
		return SessionInfo{}, fmt.Errorf("kernel: marshal envelope: %w", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if err := writeFileAtomic(f.path(id), blob); err != nil {
		return SessionInfo{}, err
	}
	return info, nil
}

// Load 读取一个会话。
func (f *FileStore) Load(ctx context.Context, id string) (SessionInfo, []byte, error) {
	if err := ctxErr(ctx); err != nil {
		return SessionInfo{}, nil, err
	}
	if err := ValidateSessionID(id); err != nil {
		return SessionInfo{}, nil, err
	}
	f.mu.Lock()
	blob, err := os.ReadFile(f.path(id))
	f.mu.Unlock()
	if errors.Is(err, os.ErrNotExist) {
		return SessionInfo{}, nil, fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	if err != nil {
		return SessionInfo{}, nil, fmt.Errorf("kernel: read session %s: %w", id, err)
	}

	var env sessionEnvelope
	if err := json.Unmarshal(blob, &env); err != nil || env.Version == 0 || len(env.State) == 0 {
		// 向前兼容：文件里直接放的是裸 state。
		info, serr := SummarizeState(id, blob)
		if serr != nil {
			if err != nil {
				return SessionInfo{}, nil, fmt.Errorf("kernel: parse session %s: %w", id, err)
			}
			return SessionInfo{}, nil, serr
		}
		return info, blob, nil
	}
	info := env.Meta
	if info.ID == "" {
		info.ID = id
	}
	if info.UpdatedAt.IsZero() {
		info.UpdatedAt = env.UpdatedAt
	}
	if info.Bytes == 0 {
		info.Bytes = len(env.State)
	}
	return info, env.State, nil
}

// Delete 删除一个会话（不存在时返回 nil）。
func (f *FileStore) Delete(ctx context.Context, id string) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := ValidateSessionID(id); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.Remove(f.path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("kernel: delete session %s: %w", id, err)
	}
	return nil
}

// List 列出全部会话（UpdatedAt 降序）。
func (f *FileStore) List(ctx context.Context) ([]SessionInfo, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return nil, fmt.Errorf("kernel: list store: %w", err)
	}
	out := make([]SessionInfo, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.Contains(name, ".tmp-") {
			continue
		}
		blob, err := os.ReadFile(filepath.Join(f.dir, name))
		if err != nil {
			continue // 并发删除，跳过
		}
		var env sessionEnvelope
		info := SessionInfo{ID: strings.TrimSuffix(name, ".json")}
		if err := json.Unmarshal(blob, &env); err == nil && env.Version != 0 {
			info = env.Meta
			if info.ID == "" {
				info.ID = strings.TrimSuffix(name, ".json")
			}
			if info.UpdatedAt.IsZero() {
				info.UpdatedAt = env.UpdatedAt
			}
		} else if s, serr := SummarizeState(info.ID, blob); serr == nil {
			info = s
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// CleanupTemp 删除超过 olderThan 的残留临时文件（进程被杀留下的）。
func (f *FileStore) CleanupTemp(olderThan time.Duration) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return 0, fmt.Errorf("kernel: list store: %w", err)
	}
	cutoff := time.Now().Add(-olderThan)
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.Contains(e.Name(), ".tmp-") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(f.dir, e.Name())); err == nil {
			n++
		}
	}
	return n, nil
}

// MemoryStore 是进程内 Store，适合测试与单机无盘场景。
type MemoryStore struct {
	mu      sync.RWMutex
	entries map[string]memoryEntry
}

type memoryEntry struct {
	info  SessionInfo
	state []byte
}

// NewMemoryStore 创建一个空的内存 Store。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{entries: map[string]memoryEntry{}}
}

func (m *MemoryStore) Save(ctx context.Context, id string, state []byte) (SessionInfo, error) {
	if err := ctxErr(ctx); err != nil {
		return SessionInfo{}, err
	}
	if err := ValidateSessionID(id); err != nil {
		return SessionInfo{}, err
	}
	info, err := SummarizeState(id, state)
	if err != nil {
		return SessionInfo{}, err
	}
	info.UpdatedAt = time.Now().UTC()
	cp := append([]byte(nil), state...)
	m.mu.Lock()
	m.entries[id] = memoryEntry{info: info, state: cp}
	m.mu.Unlock()
	return info, nil
}

func (m *MemoryStore) Load(ctx context.Context, id string) (SessionInfo, []byte, error) {
	if err := ctxErr(ctx); err != nil {
		return SessionInfo{}, nil, err
	}
	if err := ValidateSessionID(id); err != nil {
		return SessionInfo{}, nil, err
	}
	m.mu.RLock()
	e, ok := m.entries[id]
	m.mu.RUnlock()
	if !ok {
		return SessionInfo{}, nil, fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	return e.info, append([]byte(nil), e.state...), nil
}

func (m *MemoryStore) Delete(ctx context.Context, id string) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.entries, id)
	m.mu.Unlock()
	return nil
}

func (m *MemoryStore) List(ctx context.Context) ([]SessionInfo, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	m.mu.RLock()
	out := make([]SessionInfo, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, e.info)
	}
	m.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// writeFileAtomic 写临时文件 → fsync → rename。rename 在 Windows 上可能因并发
// 打开而被拒，故带短重试。
func writeFileAtomic(dst string, data []byte) error {
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, filepath.Base(dst)+".tmp-*")
	if err != nil {
		return fmt.Errorf("kernel: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后是空操作

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("kernel: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("kernel: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("kernel: close temp file: %w", err)
	}

	var rerr error
	for i := 0; i < 5; i++ {
		if rerr = os.Rename(tmpName, dst); rerr == nil {
			syncDir(dir)
			return nil
		}
		time.Sleep(time.Duration(10*(i+1)) * time.Millisecond)
	}
	return fmt.Errorf("kernel: rename temp file: %w", rerr)
}

// syncDir 尽力 fsync 目录（Windows 不支持，忽略错误）。
func syncDir(dir string) {
	f, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = f.Sync()
	_ = f.Close()
}

func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
