package nogchashmap

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// toSliceSet 把 []string 转成集合，方便与参考集合精确对账。
func toSliceSet(keys []string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// assertSameKeySet 断言 got 与 want 恰好是同一组键：不多、不少、也不重复。
func assertSameKeySet(t *testing.T, got []string, want map[string]bool) {
	t.Helper()
	if len(toSliceSet(got)) != len(got) {
		t.Fatalf("ToSlice 返回了重复的键")
	}
	if len(got) != len(want) {
		preview := got
		if len(preview) > 5 {
			preview = preview[:5]
		}
		t.Fatalf("ToSlice 返回 %d 个键，期望 %d 个，前几个：%v", len(got), len(want), preview)
	}
	for _, k := range got {
		if !want[k] {
			t.Fatalf("ToSlice 返回了不该存在的键 %q", k)
		}
	}
}

// 基本场景：空集合、混合 Add/AddString、空键与二进制键、删除一半之后仍精确对账。
func TestToSliceMatchesStdMap(t *testing.T) {
	h := New()
	if got := h.ToSlice(); len(got) != 0 {
		t.Fatalf("空集合 ToSlice() = %v，期望为空", got)
	}

	ref := make(map[string]bool)
	for _, k := range []string{"", "a", "\x00\x00\x01", "a\x00b"} {
		h.AddString(k)
		ref[k] = true
	}
	const total = 2000
	keys := make([]string, total)
	for i := 0; i < total; i++ {
		k := fmt.Sprintf("key-%d-%s", i, strings.Repeat("x", i%64))
		keys[i] = k
		h.Add([]byte(k))
		ref[k] = true
	}
	if h.Len() != len(ref) {
		t.Fatalf("Len() = %d，期望 %d", h.Len(), len(ref))
	}
	assertSameKeySet(t, h.ToSlice(), ref)

	// 删除一半（连同空键），再次对账
	for i, k := range keys {
		if i%2 == 0 {
			h.RemoveString(k)
			delete(ref, k)
		}
	}
	h.RemoveString("")
	delete(ref, "")
	assertSameKeySet(t, h.ToSlice(), ref)
	if len(h.ToSlice()) != h.Len() {
		t.Fatalf("ToSlice 长度 %d 与 Len() = %d 不一致", len(h.ToSlice()), h.Len())
	}

	// 连续两次导出，结果一致
	assertSameKeySet(t, h.ToSlice(), toSliceSet(h.ToSlice()))
}

// hash 冲突场景：1 个键在字节流、其余键在回退 map，删除部分键后仍精确对账。
func TestToSliceWithHashCollisionAndOverflow(t *testing.T) {
	h := New()
	forceHashCollision(h)

	const total = 100
	ref := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		k := fmt.Sprintf("collide-%d", i)
		h.AddString(k)
		ref[k] = true
	}
	assertSameKeySet(t, h.ToSlice(), ref)

	// 删除 40 个键（第一个键在字节流中，也会被删掉），其余留在回退 map 中
	for i := 0; i < total; i++ {
		if i%5 < 2 {
			k := fmt.Sprintf("collide-%d", i)
			h.RemoveString(k)
			delete(ref, k)
		}
	}
	assertSameKeySet(t, h.ToSlice(), ref)

	// 重新加回一个键：坑位空出后它会进入字节流，导出不得出现两份
	h.AddString("collide-0")
	ref["collide-0"] = true
	assertSameKeySet(t, h.ToSlice(), ref)
}

// 空间回收之后：存活键被搬动过位置（含跨 chunk 搬动），ToSlice 仍必须精确。
func TestToSliceAfterCompact(t *testing.T) {
	// 所有键落进同一个分片且 hash 互不相同，删除比例超过阈值后可确定性地触发空间回收
	h := New(Config{NeedDeletePercent: 10})
	keyIdx := func(k []byte) int {
		v := 0
		for _, c := range k[2:] { // 键格式为 "k-%05d"，跳过前缀 "k-"
			v = v*10 + int(c-'0')
		}
		return v
	}
	forceHash(h,
		func(k []byte) uint64 { return uint64(keyIdx(k))*bucketsNum + 1 },
		func(s string) uint64 { return uint64(keyIdx([]byte(s)))*bucketsNum + 1 },
	)

	const total = 600
	keys := make([]string, total)
	for i := range keys {
		keys[i] = fmt.Sprintf("k-%05d", i)
		h.AddString(keys[i])
	}
	// 约 600 个键 × 9 字节，先填满首个 4KB 小 chunk，再进入完整 chunk，形成多 chunk 布局
	beforeBytes, beforeChunks := chunkStat(h)

	ref := make(map[string]bool)
	for i, k := range keys {
		if i%6 == 0 {
			ref[k] = true
			continue
		}
		h.RemoveString(k)
	}
	afterBytes, afterChunks := chunkStat(h)
	if afterBytes >= beforeBytes || afterChunks > beforeChunks {
		t.Fatalf("期望触发空间回收：bytes %d -> %d，chunks %d -> %d", beforeBytes, afterBytes, beforeChunks, afterChunks)
	}
	if h.Len() != len(ref) {
		t.Fatalf("回收后 Len() = %d，期望 %d", h.Len(), len(ref))
	}
	assertSameKeySet(t, h.ToSlice(), ref)

	// 回收之后继续写入（新键落在搬动后的位置上），再次对账
	for i := 0; i < 50; i++ {
		k := fmt.Sprintf("k-%05d", total+i)
		h.AddString(k)
		ref[k] = true
	}
	assertSameKeySet(t, h.ToSlice(), ref)
}

// 回归：ToSlice 不能依赖「用 h.hash 重算字节流 hash」来判断存活。
// 写入时用 hashString、重算时用 hash（或写入之后才替换 hash 函数）都会让重算结果与索引对不上，
// 旧实现会因此漏掉键；正确实现应当直接遍历索引，不重算任何 hash。
func TestToSliceNotDependOnHashRecompute(t *testing.T) {
	h := New()
	// 两个 hash 函数返回不同的值，模拟互相不一致的实现
	forceHash(h,
		func([]byte) uint64 { return 7 },
		func(string) uint64 { return 9 },
	)
	h.AddString("via-string")  // 写入时用 hashString -> 9
	h.Add([]byte("via-bytes")) // 写入时用 hash -> 7
	assertSameKeySet(t, h.ToSlice(), map[string]bool{"via-string": true, "via-bytes": true})

	// 写入之后才替换 hash 函数，ToSlice 也不应受影响
	h2 := New()
	h2.AddString("a")
	h2.AddString("b")
	forceHash(h2,
		func([]byte) uint64 { return 123 },
		func(string) uint64 { return 456 },
	)
	assertSameKeySet(t, h2.ToSlice(), map[string]bool{"a": true, "b": true})
}

// 快照独立性：导出之后再增删键，不影响已导出的内容。
func TestToSliceSnapshotIsIndependent(t *testing.T) {
	h := New()
	h.AddString("keep")
	h.AddString("drop")
	snap := h.ToSlice()
	h.RemoveString("drop")
	h.AddString("later")
	assertSameKeySet(t, snap, map[string]bool{"keep": true, "drop": true})
	assertSameKeySet(t, h.ToSlice(), map[string]bool{"keep": true, "later": true})
}

// 并发：写删进行中反复导出快照，不得 panic、不得返回格式非法的键；
// 静止后快照必须与 Contain / Len 完全一致。配合 -race 运行可同时验证锁的正确性。
func TestToSliceConcurrent(t *testing.T) {
	h := New()
	const goroutines = 8
	const keySpace = 400
	const opsPerGoroutine = 4000

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				k := fmt.Sprintf("g%d-k%d", g, i%keySpace)
				switch i % 3 {
				case 0:
					h.AddString(k)
				case 1:
					h.ContainString(k)
				case 2:
					h.RemoveString(k)
				}
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for n := 0; n < 100; n++ {
			for _, k := range h.ToSlice() {
				if !strings.HasPrefix(k, "g") || !strings.Contains(k, "-k") {
					panic("ToSlice 读到格式非法的键：" + k)
				}
			}
		}
	}()
	wg.Wait()

	snap := h.ToSlice()
	if len(snap) != h.Len() {
		t.Fatalf("静止后 ToSlice 长度 %d 与 Len() = %d 不一致", len(snap), h.Len())
	}
	if len(toSliceSet(snap)) != len(snap) {
		t.Fatalf("静止后 ToSlice 返回了重复的键")
	}
	for _, k := range snap {
		if !h.ContainString(k) {
			t.Fatalf("ToSlice 返回的键 %q 不在集合中", k)
		}
	}
}
