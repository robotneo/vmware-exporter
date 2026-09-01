package vmwareCollectors

import (
	"sync"
	"testing"
	"time"
)

func TestVersionSetAddDeduplicates(t *testing.T) {
	s := newVersionSet()

	if !s.Add("igbn", "1.4.11") {
		t.Fatal("first Add() = false, want true for a previously unseen pair")
	}

	if s.Add("igbn", "1.4.11") {
		t.Fatal("second Add() with the same pair = true, want false")
	}

	// 同一 key 的不同 value 应当算首次出现。
	if !s.Add("igbn", "1.5.0") {
		t.Fatal("Add() with a new version under an existing key = false, want true")
	}

	// 不同 key 之间互不影响。
	if !s.Add("ixgben", "1.4.11") {
		t.Fatal("Add() with a new key = false, want true")
	}

	if s.Add("ixgben", "1.4.11") {
		t.Fatal("repeated Add() under a new key = true, want false")
	}
}

// TestVersionSetAddIsAtomic 验证 P2-6 的核心修复。
//
// 原实现把 `if _, exists := m[k]` 的读放在锁外、只把写放在锁内，
// 是非原子的 check-then-act：多个 goroutine 可以同时通过同一个 exists
// 判断，于是每个都认为自己是首次出现，产出重复的时间序列。
//
// 现在读写在同一把锁内，无论多少 goroutine 竞争同一对 (key, value)，
// 有且只有一个能拿到 true。
//
// 注意：单靠并发压测抓不稳这个 bug —— 第一个 goroutine 拿到锁后立刻写入，
// 其余即便读在锁外也大概率已读到新值，竞态窗口极窄。真正可靠的检测手段是
// go test -race（见下方 barrier 版本以及 CI 中的 -race 配置）。
// 这里用 barrier 强制所有 goroutine 在同一时刻进入 Add，最大化碰撞概率。
func TestVersionSetAddIsAtomic(t *testing.T) {
	const goroutines = 256

	s := newVersionSet()

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		wins  int
		ready sync.WaitGroup
		start = make(chan struct{})
	)

	wg.Add(goroutines)
	ready.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()

			// 先声明就位，再统一放行，确保所有 goroutine 都已调度起来。
			ready.Done()
			<-start

			if s.Add("igbn", "1.4.11") {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}

	ready.Wait()
	close(start)
	wg.Wait()

	if wins != 1 {
		t.Fatalf("Add() returned true %d times for the same pair, want exactly 1; the check-then-act is not atomic", wins)
	}
}

// TestVersionSetAddHoldsLockDuringRead 是对 Add 原子性的结构性验证，
// 不依赖调度时序，因此比并发压测可靠。
//
// 做法：从另一个 goroutine 抢占锁并持有一段时间，此时主 goroutine 调用 Add。
// 如果 Add 的读发生在锁外（原来的 bug），它会立刻读到 map 并推进；
// 如果读写都在锁内（正确实现），它必须阻塞到锁释放为止。
// 通过测量 Add 的耗时即可区分两种实现。
func TestVersionSetAddHoldsLockDuringRead(t *testing.T) {
	const hold = 150 * time.Millisecond

	s := newVersionSet()

	// 预置一条记录，让 Add 走「key 已存在」那条分支 —— 原 bug 正在此处。
	s.Add("igbn", "1.4.11")

	locked := make(chan struct{})
	released := make(chan struct{})

	go func() {
		s.mu.Lock()
		close(locked)
		time.Sleep(hold)
		s.mu.Unlock()
		close(released)
	}()

	<-locked

	begin := time.Now()
	s.Add("igbn", "1.5.0")
	elapsed := time.Since(begin)

	<-released

	// 留一半余量吸收调度抖动。
	if elapsed < hold/2 {
		t.Fatalf("Add() returned after %v while the mutex was held for %v; the read must happen inside the lock, otherwise the check-then-act is not atomic",
			elapsed, hold)
	}
}

// TestVersionSetConcurrentDistinctKeys 确认并发写入不同 key 不会丢数据。
// map 并发写在 Go 里会直接 fatal，因此这个测试也能兜住锁被误删的情况。
func TestVersionSetConcurrentDistinctKeys(t *testing.T) {
	const goroutines = 64

	s := newVersionSet()

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)

	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()

			// 每个 goroutine 用独立的 key，全部都应算首次出现。
			if s.Add(string(rune('a'+i%26))+string(rune('0'+i/26)), "1.0") {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(i)
	}

	wg.Wait()

	if wins != goroutines {
		t.Fatalf("Add() returned true %d times for %d distinct keys, want %d", wins, goroutines, goroutines)
	}
}
