// Package nogchashmap 是一个低 GC 压力的哈希集合（hash set）实现：只保存键、不保存值。
//
// 设计思路与 https://github.com/yudeguang17/noGcMap 一致：把不利于 GC 的复杂容器拆成
// 「基础类型索引 + []byte 字节流 + 少量回退 map」——
//
//  1. 集合被切成 bucketsNum 个分片（bucket），每个分片一把读写锁，读写冲突被分散；
//  2. 每个分片用 map[uint64]uint 做索引，值是键在 chunk 字节流中的绝对偏移；
//  3. 键本身按 [2 字节 keyLen][key] 的格式顺序追加在若干个 chunk（[]byte）里；
//  4. hash 冲突的键放到该分片的回退 map（map[string]struct{}）中，冲突极少所以代价可忽略。
//
// 与 noGcMap 的主要差异：
//
//  1. 只有键，没有值（hashset 语义）；因此写入不再有「更新」路径，实现更简单；
//  2. 键长度必须小于 65535（即最大 65534），传入长度 >= 65535 的键会直接 panic，
//     不会像 noGcMap 那样因长度字段只有 2 字节而被静默截断；
//  3. Config 只在构造时生效并保存在实例上，两个实例可以有不同的配置，互不影响
//     （noGcMap 把配置写成包级全局变量，实例之间会互相串扰）；
//  4. 同一个键在集合内部只会存在于一处（字节流或回退 map），删除后不会残留、
//     不会出现「删掉之后还能读到旧数据」的情况；
//  5. hash 使用标准库 hash/maphash，每个实例一个随机种子，无第三方依赖，
//     并且每个实例的 seed 不同，天然抵御构造冲突键的攻击；
//  6. 每个分片第一次写入只分配一个 4KB 的小 chunk，之后的 chunk 才按配置大小分配，
//     避免小集合也占满「512 个分片 × 64KB」。
package nogchashmap

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/maphash"
	"sync"
)

const (
	// bucketsNum 为分片数量，与 noGcMap 保持一致。
	bucketsNum = 512

	// MaxKeyLen 是单个键允许的最大长度（字节）。键长度必须小于 65535。
	MaxKeyLen = 65534

	// keyLenThreshold 是键长度的非法边界：长度 >= 该值即为非法键，会直接 panic。
	keyLenThreshold = MaxKeyLen + 1

	// oneChunkSize 为 64KB，是 MultiplesOf64KForChunkSize = 1 时的 chunk 大小。
	oneChunkSize = 64 * 1024

	// defaultChunkSize 为默认 chunk 大小（64KB）。
	defaultChunkSize = oneChunkSize

	// defaultNeedDeletePercent 为默认的空间回收触发比例。
	defaultNeedDeletePercent = 50

	// firstChunkSize 是每个分片第一次写入时分配的 chunk 大小（4KB）。
	// 之后的 chunk 都按配置的 chunkSize 分配，这样小集合不会因为「每个分片都要一块 64KB」
	// 而占用过多内存。
	firstChunkSize = 4 * 1024

	// keyLenFieldSize 是键长度字段占用的字节数（大端序 uint16）。
	keyLenFieldSize = 2

	// minItemsToCompact 表示分片的「历史键总数」超过该值后才考虑回收空间，
	// 避免小分片刚删几个键就做一次整片搬迁。
	minItemsToCompact = 256
)

// Config 是构造 HashSet 时的可选配置。
type Config struct {
	// NeedDeletePercent 定义「某个分片中已被删除的键数占该分片历史键总数的百分比」
	// 达到多少时，触发一次真正的空间回收（把存活的键搬到前面、释放不再使用的 chunk）。
	// 取值必须介于 1 到 100 之间，默认 50。取值非法会 panic。
	//
	// 对应 noGcMap 的 Config.NeedDeketeKVPairPersent（该字段名有拼写错误）。
	NeedDeletePercent int

	// MultiplesOf64KForChunkSize 定义单个 chunk 的大小：该值 * 64KB。
	// 取值必须介于 1 到 1000 之间（1 => 64KB，1000 => 64MB），默认 1。取值非法会 panic。
	//
	// 对应 noGcMap 的 Config.MultiplesOf64KForchunkSize。
	MultiplesOf64KForChunkSize int
}

// HashSet 是一个低 GC 压力的哈希集合，零值不可用，请用 New 创建。
// HashSet 可以安全地被多个 goroutine 并发使用。
type HashSet struct {
	seed              maphash.Seed
	hash              func(k []byte) uint64 // 仅测试会替换它，用于强制制造 hash 冲突
	hashString        func(k string) uint64 // 同上，供 String 系列接口使用
	chunkSize         int
	needDeletePercent int
	buckets           [bucketsNum]bucket
}

// bucket 是一个分片：一把读写锁 + 索引 + 键字节流 + 回退 map。
type bucket struct {
	lock          sync.RWMutex
	chunkSize     int                 // 与实例配置一致，创建后不再变化
	dataBeginPos  uint                // 下一个键写入的绝对偏移
	curChunkIndex int                 // 当前正在写入的 chunk 下标
	deleteItemNum int                 // 已被删除、尚未被回收的键个数
	index         map[uint64]uint     // 索引：键的 hash -> 键在字节流中的绝对偏移；一个 hash 只对应一个键
	overflow      map[string]struct{} // 回退 map：hash 冲突的键（懒创建）
	chunks        [][]byte            // 键真实的存储位置
	scratch       []byte              // 空间回收时复用的临时缓冲，最大一个 chunk
}

// keyRef 把 []byte 键与 string 键统一到同一套内部实现里，
// 这样 String 系列接口不必先把 string 复制成 []byte。
type keyRef struct {
	b     []byte
	s     string
	isStr bool
}

// Len 返回键的字节长度。
func (k keyRef) Len() int {
	if k.isStr {
		return len(k.s)
	}
	return len(k.b)
}

// equalString 判断 keyRef 与字符串 s 的内容是否相同，不产生任何分配。
func (k keyRef) equalString(s string) bool {
	if k.isStr {
		return k.s == s
	}
	if len(k.b) != len(s) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if k.b[i] != s[i] {
			return false
		}
	}
	return true
}

// New 创建一个 HashSet。config 可选，不传则使用默认配置。
// 配置取值非法时会 panic（而不是像 noGcMap 那样静默忽略）。
func New(config ...Config) *HashSet {
	h := &HashSet{
		seed:              maphash.MakeSeed(),
		chunkSize:         defaultChunkSize,
		needDeletePercent: defaultNeedDeletePercent,
	}
	if len(config) > 0 {
		if v := config[0].NeedDeletePercent; v != 0 {
			if v < 1 || v > 100 {
				panic(fmt.Sprintf("nogchashmap: 非法配置 NeedDeletePercent=%d，必须介于 1 到 100 之间", v))
			}
			h.needDeletePercent = v
		}
		if v := config[0].MultiplesOf64KForChunkSize; v != 0 {
			if v < 1 || v > 1000 {
				panic(fmt.Sprintf("nogchashmap: 非法配置 MultiplesOf64KForChunkSize=%d，必须介于 1 到 1000 之间", v))
			}
			h.chunkSize = v * oneChunkSize
		}
	}
	// maphash.Bytes(seed, b) 与 maphash.String(seed, s) 对同一段字节给出相同的 hash，
	// 因此 []byte 键与 string 键可以互相查到对方。
	h.hash = func(k []byte) uint64 { return maphash.Bytes(h.seed, k) }
	h.hashString = func(k string) uint64 { return maphash.String(h.seed, k) }
	for i := range h.buckets {
		h.buckets[i].chunkSize = h.chunkSize
		h.buckets[i].index = make(map[uint64]uint)
	}
	return h
}

// Set 把键加入集合。键已存在时不做任何事（集合没有值，无需更新）。
// 键长度必须小于 65535，否则 panic。
func (h *HashSet) Set(k []byte) {
	h.set(keyRef{b: k})
}

// SetString 是 Set 的 string 版本，键长度必须小于 65535，否则 panic。
func (h *HashSet) SetString(k string) {
	h.set(keyRef{s: k, isStr: true})
}

// Contain 判断键是否在集合中。键长度必须小于 65535，否则 panic。
func (h *HashSet) Contain(k []byte) bool {
	return h.has(keyRef{b: k})
}

// ContainString 是 Contain 的 string 版本。键长度必须小于 65535，否则 panic。
func (h *HashSet) ContainString(k string) bool {
	return h.has(keyRef{s: k, isStr: true})
}

// Delete 从集合中删除键；键不存在时不做任何事。
// 键长度必须小于 65535，否则 panic。
func (h *HashSet) Delete(k []byte) {
	h.del(keyRef{b: k})
}

// DeleteString 是 Delete 的 string 版本。键长度必须小于 65535，否则 panic。
func (h *HashSet) DeleteString(k string) {
	h.del(keyRef{s: k, isStr: true})
}

// Len 返回集合中键的个数，取值精确。
func (h *HashSet) Len() int {
	total := 0
	for i := range h.buckets {
		b := &h.buckets[i]
		b.lock.RLock()
		total += len(b.index) + len(b.overflow)
		b.lock.RUnlock()
	}
	return total
}

// —————————————————————— 内部实现 ——————————————————————

// mustValidKey 校验键长度；长度 >= 65535 的键直接 panic。
func (h *HashSet) mustValidKey(k keyRef) {
	if n := k.Len(); n >= keyLenThreshold {
		panic(fmt.Sprintf("nogchashmap: 非法键长度 %d：键长度必须小于 %d", n, keyLenThreshold))
	}
}

func (h *HashSet) hashOf(k keyRef) uint64 {
	if k.isStr {
		return h.hashString(k.s)
	}
	return h.hash(k.b)
}

// set 把键加入集合。
// 不变量：一个键在集合内部只会存在于一处（字典流的键或回退 map 的键，二选一）。
func (h *HashSet) set(k keyRef) {
	h.mustValidKey(k)
	hash := h.hashOf(k)
	b := &h.buckets[hash%bucketsNum]
	b.lock.Lock()
	defer b.lock.Unlock()
	// 1) 该 hash 已经被占用
	if pos, exist := b.index[hash]; exist {
		if b.keyEquals(pos, k) {
			return // 已经在集合里
		}
		b.overflowAdd(k) // 是另一个键的 hash 冲突，只能放回退 map
		return
	}
	// 2) 该 hash 空闲：先确认这个键没有历史遗留在回退 map 中（否则会成为两份），再写入字节流
	if b.overflowHas(k) {
		return
	}
	b.writeKey(hash, k)
}

// has 判断键是否存在。
func (h *HashSet) has(k keyRef) bool {
	h.mustValidKey(k)
	hash := h.hashOf(k)
	b := &h.buckets[hash%bucketsNum]
	b.lock.RLock()
	defer b.lock.RUnlock()
	if pos, exist := b.index[hash]; exist && b.keyEquals(pos, k) {
		return true
	}
	return b.overflowHas(k)
}

// del 删除键。
func (h *HashSet) del(k keyRef) {
	h.mustValidKey(k)
	hash := h.hashOf(k)
	b := &h.buckets[hash%bucketsNum]
	b.lock.Lock()
	defer b.lock.Unlock()
	if pos, exist := b.index[hash]; exist && b.keyEquals(pos, k) {
		delete(b.index, hash)
		b.deleteItemNum++
		if b.needCompact(h.needDeletePercent) {
			h.compact(b)
		}
		return
	}
	b.overflowDel(k)
}

// —————————————————————— 分片内部操作 ——————————————————————

// overflowHas 判断键是否在回退 map 中。map 查找里的 string([]byte) 转换不会分配内存。
func (b *bucket) overflowHas(k keyRef) bool {
	if k.isStr {
		_, exist := b.overflow[k.s]
		return exist
	}
	_, exist := b.overflow[string(k.b)]
	return exist
}

// overflowAdd 把键加入回退 map（懒创建）。只有 hash 冲突这条极少走的路径才可能分配内存。
func (b *bucket) overflowAdd(k keyRef) {
	if b.overflow == nil {
		b.overflow = make(map[string]struct{})
	}
	if k.isStr {
		b.overflow[k.s] = struct{}{}
		return
	}
	b.overflow[string(k.b)] = struct{}{}
}

// overflowDel 从回退 map 中删除键。回退 map 很小，直接扫描可以避免字符串转换带来的分配。
func (b *bucket) overflowDel(k keyRef) {
	for key := range b.overflow {
		if k.equalString(key) {
			delete(b.overflow, key)
			return
		}
	}
}

// keyEquals 判断位于绝对偏移 pos 的键与 k 是否相同。
// 绝对偏移 pos 的解析方式：chunk 下标 = pos / chunkSize，块内偏移 = pos % chunkSize。
func (b *bucket) keyEquals(pos uint, k keyRef) bool {
	chunkIdx := int(pos) / b.chunkSize
	off := int(pos) % b.chunkSize
	chunk := b.chunks[chunkIdx]
	keyLen := int(chunk[off])<<8 | int(chunk[off+1])
	keyBegin := off + keyLenFieldSize
	if k.isStr {
		if len(k.s) != keyLen {
			return false
		}
		for i := 0; i < keyLen; i++ {
			if chunk[keyBegin+i] != k.s[i] {
				return false
			}
		}
		return true
	}
	if len(k.b) != keyLen {
		return false
	}
	return bytes.Equal(chunk[keyBegin:keyBegin+keyLen], k.b)
}

// reservePair 保证当前 chunk 有足够空间容纳长度为 pairLen 的键值对，
// 空间不够时就启用（或追加）一个新的 chunk。
//
// 分片第一次写入时只分配一个较小的 chunk（firstChunkSize），
// 避免「一个分片只放了一两个小键也要占满 64KB」；之后新增的 chunk 按配置的 chunkSize 分配。
func (b *bucket) reservePair(pairLen int) {
	if len(b.chunks) == 0 {
		if pairLen > firstChunkSize {
			b.chunks = append(b.chunks, make([]byte, 0, b.chunkSize))
		} else {
			b.chunks = append(b.chunks, make([]byte, 0, firstChunkSize))
		}
		b.dataBeginPos = 0
		b.curChunkIndex = 0
		return
	}
	if cap(b.chunks[b.curChunkIndex])-len(b.chunks[b.curChunkIndex]) < pairLen {
		b.curChunkIndex++
		b.dataBeginPos = uint(b.curChunkIndex * b.chunkSize)
		if b.curChunkIndex >= len(b.chunks) {
			b.chunks = append(b.chunks, make([]byte, 0, b.chunkSize))
		}
	}
}

// writeKey 把键追加到字节流末尾，并登记索引。
//
// 键长度 < 65535 => 键值对长度 <= 2 + 65534 = 65536 <= 最小的 chunkSize(64KB)，
// 因此任何一个新 chunk 都一定能放得下一整个键，不存在 noGcMap 里「超长键值对塞不下」的情况。
func (b *bucket) writeKey(hash uint64, k keyRef) {
	pairLen := keyLenFieldSize + k.Len()
	b.reservePair(pairLen)
	b.index[hash] = b.dataBeginPos
	chunk := b.chunks[b.curChunkIndex]
	var lenBuf [keyLenFieldSize]byte
	binary.BigEndian.PutUint16(lenBuf[:], uint16(k.Len()))
	chunk = append(chunk, lenBuf[:]...)
	if k.isStr {
		chunk = append(chunk, k.s...)
	} else {
		chunk = append(chunk, k.b...)
	}
	b.chunks[b.curChunkIndex] = chunk
	b.dataBeginPos += uint(pairLen)
}

// needCompact 判断该分片是否需要回收空间。调用方必须已经持有写锁。
func (b *bucket) needCompact(percent int) bool {
	total := b.deleteItemNum + len(b.index) + len(b.overflow)
	if total < minItemsToCompact {
		return false
	}
	return b.deleteItemNum*100 >= percent*total
}

// —————————————————————— 空间回收 ——————————————————————

// compact 把分片中存活的键往前搬，回收被删除键占用的空间。调用方必须已持有该分片的写锁。
//
// 做法：逐个 chunk 复制到临时缓冲（b.scratch）后清空该 chunk，再按键在字节流中的先后顺序
// 把存活的键重新追加到从 0 号开始的 chunk 中，并同步更新索引。
// 判断一个键是否存活的方式：它的 hash 在 index 中的偏移是否正好等于它在字节流中的当前位置
// （被删除的键在 Delete 时就把索引摘掉了）。
func (h *HashSet) compact(b *bucket) {
	if len(b.chunks) == 0 {
		b.deleteItemNum = 0
		return
	}
	scratch := b.scratch
	dstChunkIdx := 0
	dstOff := 0
	wroteAny := false
	for i := 0; i < len(b.chunks); i++ {
		src := b.chunks[i]
		if len(src) == 0 {
			continue
		}
		scratch = append(scratch[:0], src...)
		b.chunks[i] = b.chunks[i][:0] // 源数据已复制到 scratch，可以安全清空
		for pos := 0; pos < len(src); {
			keyLen := int(scratch[pos])<<8 | int(scratch[pos+1])
			pairLen := keyLenFieldSize + keyLen
			oldPos := uint(i*b.chunkSize + pos)
			hash := h.hash(scratch[pos+keyLenFieldSize : pos+pairLen])
			if livePos, alive := b.index[hash]; alive && livePos == oldPos {
				// 用 cap-len 判断目标 chunk 剩余空间（目标 chunk 可能是首次分配的小 chunk）
				if cap(b.chunks[dstChunkIdx])-len(b.chunks[dstChunkIdx]) < pairLen {
					dstChunkIdx++
					dstOff = 0
					if dstChunkIdx >= len(b.chunks) {
						b.chunks = append(b.chunks, make([]byte, 0, b.chunkSize))
					}
				}
				b.chunks[dstChunkIdx] = append(b.chunks[dstChunkIdx], scratch[pos:pos+pairLen]...)
				b.index[hash] = uint(dstChunkIdx*b.chunkSize + dstOff)
				dstOff += pairLen
				wroteAny = true
			}
			pos += pairLen
		}
	}
	if !wroteAny {
		// 该分片已经没有存活键，chunk 全部释放
		b.chunks = nil
		b.dataBeginPos = 0
		b.curChunkIndex = 0
	} else {
		usedChunks := dstChunkIdx + 1 // dstOff > 0 时（wroteAny 为真），最后写入的 chunk 下标是 dstChunkIdx
		for j := usedChunks; j < len(b.chunks); j++ {
			b.chunks[j] = nil
		}
		b.chunks = b.chunks[:usedChunks]
		b.curChunkIndex = dstChunkIdx
		b.dataBeginPos = uint(dstChunkIdx*b.chunkSize + dstOff)
	}
	b.deleteItemNum = 0
	b.scratch = scratch[:0]
}
