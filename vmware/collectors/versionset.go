package vmwareCollectors

import "sync"

// versionSet 是「driver → 已见版本集合」的并发安全去重器。
//
// 替代原先的 map + Mutex 写法。那种写法有两个问题：
//
//  1. check-then-act 非原子：`if _, exists := m[k]; exists` 的读发生在锁外，
//     只有 append/赋值在锁内。两个 goroutine 可以同时通过同一个 exists 判断，
//     然后各自 append，导致重复版本或丢失写入。
//  2. 保护对象是函数内新建的局部 map，而唯一的并发点（内层遍历 nic 的
//     goroutine）当时已被注释改回串行，锁实际上从未被争用。
//
// 现在读写都在同一把锁内完成，Add 是原子的 test-and-set，
// 内层并发因此可以安全恢复。
type versionSet struct {
	mu   sync.Mutex
	seen map[string]map[string]struct{}
}

func newVersionSet() *versionSet {
	return &versionSet{seen: make(map[string]map[string]struct{})}
}

// Add 原子地记录 (key, value)。
// 返回 true 表示这是首次出现，调用方应当产出一条指标；
// 返回 false 表示已记录过，应当跳过，避免重复时间序列。
func (s *versionSet) Add(key, value string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	values, ok := s.seen[key]
	if !ok {
		s.seen[key] = map[string]struct{}{value: {}}
		return true
	}

	if _, exists := values[value]; exists {
		return false
	}

	values[value] = struct{}{}

	return true
}
