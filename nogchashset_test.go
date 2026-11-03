package nogchashmap

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// 断言 fn 会 panic，且 panic 信息包含 want。
func assertPanic(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("期望 panic（包含 %q），但没有 panic", want)
		}
		if msg, ok := r.(string); ok {
			if !strings.Contains(msg, want) {
				t.Fatalf("panic 信息 = %q，期望包含 %q", msg, want)
			}
			return
		}
		t.Fatalf("panic 内容 = %v（%T），期望包含 %q 的字符串", r, r, want)
	}()
	fn()
}

// forceHash 替换实例的 hash 函数，用来确定性地构造「同一个分片」「hash 冲突」等场景。
func forceHash(h *HashSet, hashBytes func([]byte) uint64, hashString func(string) uint64) {
	h.hash = hashBytes
	h.hashString = hashString
}

// forceHashCollision 让所有键的 hash 都是 1，即所有键都落在同一个分片、且互相冲突。
func forceHashCollision(h *HashSet) uint64 {
	forceHash(h, func([]byte) uint64 { return 1 }, func(string) uint64 { return 1 })
	return 1 % bucketsNum
}

// 统计所有分片占用的 chunk 字节总数与 chunk 个数。
func chunkStat(h *HashSet) (bytesTotal, chunkNum int) {
	for i := range h.buckets {
		b := &h.buckets[i]
		for _, c := range b.chunks {
			bytesTotal += len(c)
		}
		chunkNum += len(b.chunks)
	}
	return
}

func bucketsOf(h *HashSet) int { return len(h.buckets) }

func TestSetHasDeleteAgainstStdMap(t *testing.T) {
	h := New()
	ref := make(map[string]bool)
	keys := make([][]byte, 0, 3000)
	// 覆盖：空键、单字节键、含 0x00 的二进制键、普通键、最长键
	keys = append(keys,
		[]byte(""),
		[]byte("a"),
		[]byte{0x00, 0x00, 0x01},
		[]byte("a\x00b"),
		bytes.Repeat([]byte("L"), MaxKeyLen),
	)
	for i := 0; i < 3000; i++ {
		keys = append(keys, []byte(fmt.Sprintf("key-%d-%s", i, strings.Repeat("x", i%64))))
	}
	for _, k := range keys {
		h.Add(k)
		ref[string(k)] = true
	}
	if h.Len() != len(ref) {
		t.Fatalf("Len() = %d，期望 %d", h.Len(), len(ref))
	}
	for _, k := range keys {
		if !h.Contain(k) {
			t.Fatalf("Contain(%q) = false，期望 true", k)
		}
		if !h.ContainString(string(k)) {
			t.Fatalf("ContainString(%q) = false，期望 true", k)
		}
	}
	// 删除一半
	for i, k := range keys {
		if i%2 == 0 {
			h.Remove(k)
			delete(ref, string(k))
		}
	}
	if h.Len() != len(ref) {
		t.Fatalf("删除后 Len() = %d，期望 %d", h.Len(), len(ref))
	}
	for i, k := range keys {
		want := i%2 == 1
		if got := h.Contain(k); got != want {
			t.Fatalf("删除后 Contain(%q) = %v，期望 %v", k, got, want)
		}
	}
	// 重复删除、删除不存在的键都不应出错
	for i, k := range keys {
		if i%2 == 0 {
			h.Remove(k)
		}
	}
	h.Remove([]byte("从来没有插入过的键"))
	if h.Len() != len(ref) {
		t.Fatalf("重复删除后 Len() = %d，期望 %d", h.Len(), len(ref))
	}
}

func TestKeyLengthBoundaryAndPanic(t *testing.T) {
	h := New()
	h.Add([]byte("ok"))

	// 边界内：65534 字节可用
	maxKey := bytes.Repeat([]byte("M"), MaxKeyLen)
	h.Add(maxKey)
	if !h.Contain(maxKey) {
		t.Fatalf("长度 %d 的键应当可用", MaxKeyLen)
	}

	// 边界外：>= 65535 一律 panic，且不能静默截断
	tooLong := bytes.Repeat([]byte("X"), keyLenThreshold)
	assertPanic(t, "键长度必须小于 65535", func() { h.Add(tooLong) })
	assertPanic(t, "键长度必须小于 65535", func() { h.Contain(tooLong) })
	assertPanic(t, "键长度必须小于 65535", func() { h.Remove(tooLong) })
	assertPanic(t, "键长度必须小于 65535", func() { h.AddString(strings.Repeat("x", 70000)) })
	assertPanic(t, "键长度必须小于 65535", func() { h.ContainString(strings.Repeat("x", 65535)) })
	assertPanic(t, "键长度必须小于 65535", func() { h.RemoveString(strings.Repeat("x", 65535)) })

	// panic 之后实例仍然可用，且超长键不会以任何形式进入集合
	if !h.Contain([]byte("ok")) || !h.Contain(maxKey) || h.Len() != 2 {
		t.Fatalf("panic 之后集合状态异常：Len() = %d", h.Len())
	}
}

func TestStringAndBytesInterop(t *testing.T) {
	h := New()
	h.AddString("abc")
	if !h.Contain([]byte("abc")) {
		t.Fatal("AddString 写入的键应当能被 Contain([]byte) 查到")
	}
	h.Add([]byte("def"))
	if !h.ContainString("def") {
		t.Fatal("Add([]byte) 写入的键应当能被 ContainString 查到")
	}
	if h.Len() != 2 {
		t.Fatalf("Len() = %d，期望 2", h.Len())
	}
	h.RemoveString("def")
	h.Remove([]byte("abc"))
	if h.Len() != 0 || h.Contain([]byte("abc")) || h.ContainString("def") {
		t.Fatalf("跨类型删除失败：Len() = %d", h.Len())
	}
}

// 回归：hash 冲突时走回退 map，并且删除后不得再读到旧数据（noGcMap 曾出现「删除后旧值复活」）。
func TestHashCollisionOverflowAndNoResidueAfterDelete(t *testing.T) {
	h := New()
	idx := forceHashCollision(h)

	// 第一个键进字节流，第二个键因为是同一个 hash 进回退 map
	h.Add([]byte("first"))
	h.Add([]byte("second"))
	if h.Len() != 2 {
		t.Fatalf("Len() = %d，期望 2", h.Len())
	}
	b := &h.buckets[idx]
	if len(b.index) != 1 || len(b.overflow) != 1 {
		t.Fatalf("期望 1 个键在字节流、1 个键在回退 map，实际 index=%d overflow=%d", len(b.index), len(b.overflow))
	}
	if !h.Contain([]byte("first")) || !h.Contain([]byte("second")) {
		t.Fatal("冲突的两个键都应当能被查到")
	}

	// 删掉字节流里的键，冲突键不受影响
	h.Remove([]byte("first"))
	if h.Contain([]byte("first")) {
		t.Fatal("已删除的键仍被查到")
	}
	if !h.Contain([]byte("second")) {
		t.Fatal("冲突键被误删")
	}

	// 关键回归：字节流中的坑位空出来以后，再次 Add 冲突键，它不能再被写进字节流变成两份
	h.Add([]byte("second"))
	if h.Len() != 1 {
		t.Fatalf("再次 Add 冲突键后 Len() = %d，期望 1（不能存在两份）", h.Len())
	}
	if len(b.index) != 0 || len(b.overflow) != 1 {
		t.Fatalf("再次 Add 冲突键后 index=%d overflow=%d，期望 index=0 overflow=1", len(b.index), len(b.overflow))
	}

	// 删除冲突键后必须查不到（不能残留）
	h.Remove([]byte("second"))
	if h.Contain([]byte("second")) {
		t.Fatal("删除冲突键后仍能被查到（残留旧数据）")
	}
	if h.Len() != 0 {
		t.Fatalf("Len() = %d，期望 0", h.Len())
	}

	// 坑位空出后，新键应当重新进入字节流
	h.Add([]byte("third"))
	if len(b.index) != 1 || len(b.overflow) != 0 {
		t.Fatalf("新键应写入字节流，实际 index=%d overflow=%d", len(b.index), len(b.overflow))
	}
}

func TestConfigBelongsToInstanceNotGlobal(t *testing.T) {
	h1 := New(Config{MultiplesOf64KForChunkSize: 1, NeedDeletePercent: 50})
	h2 := New(Config{MultiplesOf64KForChunkSize: 2, NeedDeletePercent: 10})

	if h1.chunkSize != 64*1024 {
		t.Fatalf("h1.chunkSize = %d，期望 65536", h1.chunkSize)
	}
	if h2.chunkSize != 2*64*1024 {
		t.Fatalf("h2.chunkSize = %d，期望 131072", h2.chunkSize)
	}
	// 建第二个实例之后，第一个实例的配置与行为都必须保持不变
	h1.Add([]byte("a"))
	h2.Add([]byte("b"))
	for i := range h1.buckets {
		if h1.buckets[i].chunkSize != 64*1024 {
			t.Fatalf("h1 的分片 chunkSize 被其它实例改成了 %d", h1.buckets[i].chunkSize)
		}
	}
	// 64KB 分片：65534 字节的键（键值对 65536 字节）应当独占一个 chunk
	h1.Add(bytes.Repeat([]byte("K"), MaxKeyLen))
	if !h1.Contain(bytes.Repeat([]byte("K"), MaxKeyLen)) {
		t.Fatal("h1 在 64KB 分片下读不回最长键")
	}
	if h1.Len() != 2 || h2.Len() != 1 {
		t.Fatalf("两个实例互相影响了：h1.Len()=%d h2.Len()=%d", h1.Len(), h2.Len())
	}

	// 非法配置直接 panic，不能静默忽略
	assertPanic(t, "非法配置 NeedDeletePercent", func() { New(Config{NeedDeletePercent: 101}) })
	assertPanic(t, "非法配置 NeedDeletePercent", func() { New(Config{NeedDeletePercent: -1}) })
	assertPanic(t, "非法配置 MultiplesOf64KForChunkSize", func() { New(Config{MultiplesOf64KForChunkSize: 1001}) })
	assertPanic(t, "非法配置 MultiplesOf64KForChunkSize", func() { New(Config{MultiplesOf64KForChunkSize: -2}) })
}

// 最长键正好填满一个 chunk 时，下一个键必须开新 chunk 且能正确读回。
func TestMaxKeyExactlyFillsChunk(t *testing.T) {
	h := New()
	// 让两个键 hash 不同（不冲突），但都落在同一个分片 1 上
	forceHash(h,
		func(k []byte) uint64 {
			if k[0] == 'A' {
				return 1
			}
			return 1 + bucketsNum // 1+512=513，513%512 == 1，同一个分片
		},
		func(string) uint64 { return 1 },
	)
	idx := uint64(1) % bucketsNum
	k1 := bytes.Repeat([]byte("A"), MaxKeyLen)
	k2 := bytes.Repeat([]byte("B"), MaxKeyLen)
	h.Add(k1)
	h.Add(k2)
	if h.Len() != 2 {
		t.Fatalf("Len() = %d，期望 2", h.Len())
	}
	b := &h.buckets[idx]
	if len(b.chunks) != 2 {
		t.Fatalf("期望 2 个 chunk（每个键各占一个），实际 %d 个", len(b.chunks))
	}
	for i, c := range b.chunks {
		if len(c) != 64*1024 {
			t.Fatalf("chunk[%d] 长度 = %d，期望 65536", i, len(c))
		}
	}
	if !h.Contain(k1) || !h.Contain(k2) {
		t.Fatal("正好填满 chunk 的键读不回")
	}
	h.Remove(k1)
	if h.Contain(k1) || !h.Contain(k2) {
		t.Fatal("删除第一个键后状态不正确")
	}
}

func TestCompactReclaimsSpace(t *testing.T) {
	h := New()
	const total = 200000
	keys := make([][]byte, total)
	for i := 0; i < total; i++ {
		keys[i] = []byte(fmt.Sprintf("k-%d-%s", i, strings.Repeat("y", i%48)))
		h.Add(keys[i])
	}
	if h.Len() != total {
		t.Fatalf("Len() = %d，期望 %d", h.Len(), total)
	}
	beforeBytes, beforeChunks := chunkStat(h)

	// 每个分片留下 1 个键，其余全部删除；删除比例超过 50% 会触发空间回收
	keep := make([][]byte, 0, bucketsOf(h))
	for i := 0; i < total; i++ {
		if i%bucketsOf(h) == 0 {
			keep = append(keep, keys[i])
			continue
		}
		h.Remove(keys[i])
	}
	afterBytes, afterChunks := chunkStat(h)
	if h.Len() != len(keep) {
		t.Fatalf("回收后 Len() = %d，期望 %d", h.Len(), len(keep))
	}
	for _, k := range keep {
		if !h.Contain(k) {
			t.Fatalf("回收后幸存键 %q 丢失", k)
		}
	}
	if afterBytes >= beforeBytes {
		t.Fatalf("回收后占用没有下降：before=%d after=%d", beforeBytes, afterBytes)
	}
	if afterChunks > beforeChunks {
		t.Fatalf("回收后 chunk 数量变多了：before=%d after=%d", beforeChunks, afterChunks)
	}
	// 回收后还能继续正常写入
	for i := 0; i < 1000; i++ {
		h.Add([]byte(fmt.Sprintf("after-%d", i)))
	}
	if h.Len() != len(keep)+1000 {
		t.Fatalf("回收后继续写入异常：Len() = %d", h.Len())
	}
	for i := 0; i < 1000; i++ {
		if !h.Contain([]byte(fmt.Sprintf("after-%d", i))) {
			t.Fatalf("回收后写入的键 %d 读不回", i)
		}
	}
}

// 并发混合读写删。每个 goroutine 只操作自己的键空间，因此可以中途与参考集合对账。
func TestConcurrentMixedOpsAgainstStdMap(t *testing.T) {
	h := New()
	const keySpace = 2000
	const suffixes = 32
	const goroutines = 8
	const opsPerGoroutine = 20000

	keyOf := func(g, i int) string {
		return fmt.Sprintf("g%d-k%d-%s", g, i%keySpace, strings.Repeat("z", i%suffixes))
	}

	ref := make(map[string]bool)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				k := keyOf(g, i)
				switch i % 3 {
				case 0:
					h.AddString(k)
					mu.Lock()
					ref[k] = true
					mu.Unlock()
				case 1:
					if h.ContainString(k) {
						mu.Lock()
						exists := ref[k]
						mu.Unlock()
						if !exists {
							panic("Contain 返回 true 但该键从未被加入过")
						}
					}
				case 2:
					h.RemoveString(k)
					mu.Lock()
					delete(ref, k)
					mu.Unlock()
				}
			}
		}(g)
	}
	wg.Wait()

	mu.Lock()
	wantLen := len(ref)
	mu.Unlock()
	if h.Len() != wantLen {
		t.Fatalf("并发操作后 Len() = %d，期望 %d", h.Len(), wantLen)
	}
	for g := 0; g < goroutines; g++ {
		for i := 0; i < keySpace; i++ {
			for s := 0; s < suffixes; s++ {
				k := fmt.Sprintf("g%d-k%d-%s", g, i, strings.Repeat("z", s))
				mu.Lock()
				want := ref[k]
				mu.Unlock()
				if got := h.ContainString(k); got != want {
					t.Fatalf("并发操作后 ContainString(%q) = %v，期望 %v", k, got, want)
				}
			}
		}
	}
}
