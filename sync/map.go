// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"sync/atomic"
)

// Map is like a Go map[interface{}]interface{} but is safe for concurrent use
// by multiple goroutines without additional locking or coordination.
// Loads, stores, and deletes run in amortized constant time.
//
// Map 类型类似于 Go 的 map[interface{}]interface{}，但可安全地供多个 goroutine 并发使用，而无需额外的锁定或协调。
// Loads, stores and deletes 方法在常数时间内运行。
//
// The Map type is specialized. Most code should use a plain Go map instead,
// with separate locking or coordination, for better type safety and to make it
// easier to maintain other invariants along with the map content.
// Map 类型是专用的。大多数代码应该使用普通的 Go map，而不是使用单独的锁定或协调
// 以获得更好的类型安全性，并使其更容易维护 map 内容以及其他不变量。
//
// The Map type is optimized for two common use cases: (1) when the entry for a given
// key is only ever written once but read many times, as in caches that only grow,
// or (2) when multiple goroutines read, write, and overwrite entries for disjoint
// sets of keys. In these two cases, use of a Map may significantly reduce lock
// contention compared to a Go map paired with a separate Mutex or RWMutex.
// Map 类型针对两种常见用例进行了优化：
// (1) 给定键的条目只写入一次，但读取多次，例如仅增长的缓存
// (2) 多个 goroutine 读取、写入和覆盖不相交键集的条目。
// 在这两种情况下，与单独的 Mutex 或 RWMutex 配对的 Go map 相比，使用 Map 可以显著减少锁争用。
//
// The zero Map is empty and ready for use. A Map must not be copied after first use.
// 零值 Map 是空的并且可以直接使用。Map 在第一次使用后不得复制。
//
// In the terminology of the Go memory model, Map arranges that a write operation
// “synchronizes before” any read operation that observes the effect of the write, where
// read and write operations are defined as follows.
// 根据 Go 内存模型的术语，Map 会使得写操作“同步早于”任何观察到该写操作效果的读操作
// 读写操作的定义如下：
// Load, LoadAndDelete, LoadOrStore, Swap, CompareAndSwap, and CompareAndDelete
// are read operations; Delete, LoadAndDelete, Store, and Swap are write operations;
// LoadOrStore is a write operation when it returns loaded set to false;
// CompareAndSwap is a write operation when it returns swapped set to true;
// and CompareAndDelete is a write operation when it returns deleted set to true.
type Map struct {
	mu Mutex

	// read contains the portion of the map's contents that are safe for
	// concurrent access (with or without mu held).
	// read 包含 map 的内容的一部分，这部分内容可以安全地并发访问（不用持有 mu）。
	//
	// The read field itself is always safe to load, but must only be stored with
	// mu held.
	// read 字段本身始终可以安全地加载，但是只能在持有mu时才能存储。
	//
	// Entries stored in read may be updated concurrently without mu, but updating
	// a previously-expunged entry requires that the entry be copied to the dirty
	// map and unexpunged with mu held.
	// 存储在read中的条目可以在没有mu的情况下并发更新
	// 但是更新先前删除的条目需要将条目复制到dirty map中，并在持有mu的情况下取消删除。
	read atomic.Pointer[readOnly]

	// dirty contains the portion of the map's contents that require mu to be
	// held. To ensure that the dirty map can be promoted to the read map quickly,
	// it also includes all of the non-expunged entries in the read map.
	// dirty 包含 map 的内容的一部分，需要持有 mu。
	// 为了确保 dirty map 可以快速提升为 read map，它还包括 read map 中所有未删除的条目。
	//
	// Expunged entries are not stored in the dirty map. An expunged entry in the
	// clean map must be unexpunged and added to the dirty map before a new value
	// can be stored to it.
	// 删除的条目不存储在dirty map 中。在read转为dirty map的时候，删除的那些不会被复制过去
	// 在将新值存储到clean map 中的条目之前，必须取消删除 clean map 中的条目并将其添加到 dirty map 中。
	//
	// If the dirty map is nil, the next write to the map will initialize it by
	// making a shallow copy of the clean map, omitting stale entries.
	// 如果dirty map为 nil，则对 map 的下一次写入将通过浅复制 clean map 来初始化它，省略陈旧的条目。
	dirty map[any]*entry

	// misses counts the number of loads since the read map was last updated that
	// needed to lock mu to determine whether the key was present.
	// misses 计数自上次更新 read map 以来的加载次数，这些加载次数是统计需要锁定mu来确定键是否存在的操作次数。
	//
	// Once enough misses have occurred to cover the cost of copying the dirty
	// map, the dirty map will be promoted to the read map (in the unamended
	// state) and the next store to the map will make a new dirty copy.
	// 一旦发生足够多的 misses 来覆盖复制 dirty map 的成本，dirty map 将被提升为 read map（在未修改的状态下），
	// 并且对 map 的下一次存储将创建一个新的 dirty map。
	misses int
}

// readOnly is an immutable struct stored atomically in the Map.read field.
// readOnly 是一个不可变的结构，以原子方式存储在 Map.read 字段中。
type readOnly struct {
	m map[any]*entry
	// 是否有修改. 如果dirtymap包含一些键不在m中，则amended为true
	amended bool // true if the dirty map contains some key not in m.
}

// expunged is an arbitrary pointer that marks entries which have been deleted
// from the dirty map.
// expunged 是一个任意指针，用于标记已从dirty map中删除的条目。
var expunged = new(any)

// An entry is a slot in the map corresponding to a particular key.
// entry 是 map 中与特定键对应的槽。
type entry struct {
	// p points to the interface{} value stored for the entry.
	// p 指向存储在 entry 中的 interface{} 值。
	//
	// If p == nil, the entry has been deleted, and either m.dirty == nil or
	// m.dirty[key] is e.
	// 如果 p == nil，则 entry 已被删除，并且满足要么m.dirty == nil要么m.dirty[key] == e。
	//
	// If p == expunged, the entry has been deleted, m.dirty != nil, and the entry
	// is missing from m.dirty.
	// 如果 p == expunged，则 entry 已被删除，m.dirty != nil，并且 entry 从 m.dirty 中丢失。
	//
	// Otherwise, the entry is valid and recorded in m.read.m[key] and, if m.dirty
	// != nil, in m.dirty[key].
	// 否则，entry 是有效的，并记录在 m.read.m[key] 和（如果 m.dirty != nil）m.dirty[key] 中。
	//
	// An entry can be deleted by atomic replacement with nil: when m.dirty is
	// next created, it will atomically replace nil with expunged and leave
	// m.dirty[key] unset.
	// 可以通过原子替换为 nil 来删除条目：
	//    当下次创建 m.dirty 时，它将原子替换为 expunged 并将 m.dirty[key] 置为未设置。
	//
	// An entry's associated value can be updated by atomic replacement, provided
	// p != expunged. If p == expunged, an entry's associated value can be updated
	// only after first setting m.dirty[key] = e so that lookups using the dirty
	// map find the entry.
	// 如果p != expunged，可以通过原子替换来更新条目的关联值
	// 如果 p == expunged，则只能在首先设置 m.dirty[key] = e 之后才能更新条目的关联值
	// 以便使用 dirty map 进行查找。
	p atomic.Pointer[any]
}

// newEntry 分配一个新的 entry，存储传入的i值的指针，并返回指向entry值的指针。
func newEntry(i any) *entry {
	e := &entry{}
	e.p.Store(&i)
	return e
}

// loadReadOnly 从p中加载read-only部分，并返回其值。如果为nil，则新建一个。
func (m *Map) loadReadOnly() readOnly {
	if p := m.read.Load(); p != nil {
		return *p
	}
	return readOnly{}
}

// Load returns the value stored in the map for a key, or nil if no
// value is present.
// The ok result indicates whether value was found in the map.
// Load 返回 map 中存储的键值，如果没有值，则返回 nil。
// ok 结果指示是否在 map 中找到了值
func (m *Map) Load(key any) (value any, ok bool) {
	read := m.loadReadOnly()
	e, ok := read.m[key]
	// 如果没有找到值并且read.amended为true(dirty map中可能有)，则尝试从 dirty map 中加载值。
	if !ok && read.amended {
		m.mu.Lock()
		// Avoid reporting a spurious miss if m.dirty got promoted while we were
		// blocked on m.mu. (If further loads of the same key will not miss, it's
		// not worth copying the dirty map for this key.)
		// 如果在我们在 m.mu 上阻塞时 m.dirty 被提升，则避免报告虚假的 miss。
		// （如果进一步加载相同的键不会 miss，则不值得为该键复制 dirty map。）
		read = m.loadReadOnly()
		e, ok = read.m[key]
		if !ok && read.amended {
			e, ok = m.dirty[key]
			// Regardless of whether the entry was present, record a miss: this key
			// will take the slow path until the dirty map is promoted to the read
			// map.
			// 无论条目是否存在，都记录一个 miss：直到 dirty map 提升为 read map，否则该键将采用慢路径
			m.missLocked()
		}
		m.mu.Unlock()
	}
	if !ok {
		return nil, false
	}
	return e.load()
}

func (e *entry) load() (value any, ok bool) {
	p := e.p.Load()
	// nil 或者 expunged 都表示没有值
	if p == nil || p == expunged {
		return nil, false
	}
	return *p, true
}

// Store sets the value for a key.
func (m *Map) Store(key, value any) {
	_, _ = m.Swap(key, value)
}

// tryCompareAndSwap compare the entry with the given old value and swaps
// it with a new value if the entry is equal to the old value, and the entry
// has not been expunged.
// tryCompareAndSwap 将给定的旧值和entry进行比较，并在entry等于旧值且entry未被删除时将其与新值交换。
//
// If the entry is expunged, tryCompareAndSwap returns false and leaves
// the entry unchanged.
// 如果 entry 被删除，则 tryCompareAndSwap 返回 false 并保持 entry 不变。
func (e *entry) tryCompareAndSwap(old, new any) bool {
	p := e.p.Load()
	if p == nil || p == expunged || *p != old {
		return false
	}

	// Copy the interface after the first load to make this method more amenable
	// to escape analysis: if the comparison fails from the start, we shouldn't
	// bother heap-allocating an interface value to store.
	// 在第一次加载后复制接口，使该方法更易于逃逸分析：
	// 如果比较从一开始就失败，则不应该打扰堆分配接口值来存储。
	nc := new
	for {
		if e.p.CompareAndSwap(p, &nc) {
			return true
		}
		p = e.p.Load()
		if p == nil || p == expunged || *p != old {
			return false
		}
	}
}

// unexpungeLocked ensures that the entry is not marked as expunged.
//
// If the entry was previously expunged, it must be added to the dirty map
// before m.mu is unlocked.
func (e *entry) unexpungeLocked() (wasExpunged bool) {
	return e.p.CompareAndSwap(expunged, nil)
}

// swapLocked unconditionally swaps a value into the entry.
//
// The entry must be known not to be expunged.
// swapLocked 无条件地将值交换到条目中。
// 必须知道该条目没有被删除。
func (e *entry) swapLocked(i *any) *any {
	return e.p.Swap(i)
}

// LoadOrStore returns the existing value for the key if present.
// Otherwise, it stores and returns the given value.
// The loaded result is true if the value was loaded, false if stored.
// LoadOrStore 返回键的现有值（如果存在）。否则，它存储并返回给定的值。
// 如果已经有了值，则 loaded 结果为 true；如果存储了值，则为 false。
func (m *Map) LoadOrStore(key, value any) (actual any, loaded bool) {
	// Avoid locking if it's a clean hit.
	read := m.loadReadOnly()
	if e, ok := read.m[key]; ok {
		// 读取到了
		actual, loaded, ok := e.tryLoadOrStore(value)
		if ok {
			return actual, loaded
		}
	}

	m.mu.Lock()
	read = m.loadReadOnly()
	if e, ok := read.m[key]; ok {
		// 确保没有删除标记
		if e.unexpungeLocked() {
			// 原先是删除的，需要加到dirty map中
			m.dirty[key] = e
		}
		actual, loaded, _ = e.tryLoadOrStore(value)
	} else if e, ok := m.dirty[key]; ok {
		actual, loaded, _ = e.tryLoadOrStore(value)
		m.missLocked()
	} else {
		// dirty map和read map都没有
		if !read.amended {
			// We're adding the first new key to the dirty map.
			// Make sure it is allocated and mark the read-only map as incomplete.
			// 我们正在将第一个新键添加到 dirty map 中。
			// 确保它已分配，并将只读 map 标记为不完整。
			m.dirtyLocked()
			m.read.Store(&readOnly{m: read.m, amended: true})
		}
		m.dirty[key] = newEntry(value)
		actual, loaded = value, false
	}
	m.mu.Unlock()

	return actual, loaded
}

// tryLoadOrStore atomically loads or stores a value if the entry is not
// expunged.
// tryLoadOrStore 原子地加载或存储值（如果条目未被删除）。
//
// If the entry is expunged, tryLoadOrStore leaves the entry unchanged and
// returns with ok==false.
func (e *entry) tryLoadOrStore(i any) (actual any, loaded, ok bool) {
	p := e.p.Load()
	if p == expunged {
		return nil, false, false
	}
	if p != nil {
		return *p, true, true
	}

	// Copy the interface after the first load to make this method more amenable
	// to escape analysis: if we hit the "load" path or the entry is expunged, we
	// shouldn't bother heap-allocating.
	ic := i
	for {
		// p 可能值有三种情况：nil, expunged, *any
		// 如果是nil, 则将i存储到p中
		if e.p.CompareAndSwap(nil, &ic) {
			return i, false, true
		}
		p = e.p.Load()
		// 如果是expunged, 则返回false
		if p == expunged {
			return nil, false, false
		}
		// 如果是*any, 则返回true和对应的值
		if p != nil {
			return *p, true, true
		}
	}
}

// LoadAndDelete deletes the value for a key, returning the previous value if any.
// The loaded result reports whether the key was present.
// LoadAndDelete 如果有, 删除键的值并返回先前的值。
// loaded 结果报告键是否存在。
func (m *Map) LoadAndDelete(key any) (value any, loaded bool) {
	read := m.loadReadOnly()
	e, ok := read.m[key]
	// 如果没有找到值并且read.amended为true(dirty map中可能有)，则尝试从 dirty map 中加载值。
	if !ok && read.amended {
		m.mu.Lock()
		read = m.loadReadOnly()
		e, ok = read.m[key]
		if !ok && read.amended {
			e, ok = m.dirty[key]
			delete(m.dirty, key)
			// Regardless of whether the entry was present, record a miss: this key
			// will take the slow path until the dirty map is promoted to the read
			// map.
			m.missLocked()
		}
		m.mu.Unlock()
	}
	if ok {
		return e.delete()
	}
	return nil, false
}

// Delete deletes the value for a key.
func (m *Map) Delete(key any) {
	m.LoadAndDelete(key)
}

func (e *entry) delete() (value any, ok bool) {
	for {
		p := e.p.Load()
		if p == nil || p == expunged {
			return nil, false
		}
		if e.p.CompareAndSwap(p, nil) {
			return *p, true
		}
	}
}

// trySwap swaps a value if the entry has not been expunged.
//
// If the entry is expunged, trySwap returns false and leaves the entry
// unchanged.
func (e *entry) trySwap(i *any) (*any, bool) {
	for {
		p := e.p.Load()
		if p == expunged {
			return nil, false
		}
		if e.p.CompareAndSwap(p, i) {
			return p, true
		}
	}
}

// Swap swaps the value for a key and returns the previous value if any.
// The loaded result reports whether the key was present.
// Swap 交换键的值，并返回先前的值（如果有）。
// loaded 结果报告键是否存在。
func (m *Map) Swap(key, value any) (previous any, loaded bool) {
	read := m.loadReadOnly()
	if e, ok := read.m[key]; ok {
		if v, ok := e.trySwap(&value); ok {
			if v == nil {
				return nil, false
			}
			return *v, true
		}
	}

	m.mu.Lock()
	read = m.loadReadOnly()
	if e, ok := read.m[key]; ok {
		// 确保没有删除标记
		if e.unexpungeLocked() {
			// 原先是删除的，需要加到dirty map中
			// The entry was previously expunged, which implies that there is a
			// non-nil dirty map and this entry is not in it.
			// 条目先前已被删除，这意味着有一个非nil的dirty map，并且该条目不在其中。
			m.dirty[key] = e
		}
		if v := e.swapLocked(&value); v != nil {
			loaded = true
			previous = *v
		}
	} else if e, ok := m.dirty[key]; ok {
		if v := e.swapLocked(&value); v != nil {
			loaded = true
			previous = *v
		}
	} else {
		if !read.amended {
			// dirty map和read map都没有
			// We're adding the first new key to the dirty map.
			// Make sure it is allocated and mark the read-only map as incomplete.
			m.dirtyLocked()
			m.read.Store(&readOnly{m: read.m, amended: true})
		}
		m.dirty[key] = newEntry(value)
	}
	m.mu.Unlock()
	return previous, loaded
}

// CompareAndSwap swaps the old and new values for key
// if the value stored in the map is equal to old.
// The old value must be of a comparable type.
// CompareAndSwap 交换键的旧值和新值，如果 map 中存储的值等于 old。
// 旧值必须是可比较的类型。
func (m *Map) CompareAndSwap(key, old, new any) bool {
	read := m.loadReadOnly()
	if e, ok := read.m[key]; ok {
		// read map 中有这个键
		return e.tryCompareAndSwap(old, new)
	} else if !read.amended {
		// dirty map 中没有多余的键, 不用再找了
		return false // No existing value for key.
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	read = m.loadReadOnly()
	swapped := false
	if e, ok := read.m[key]; ok {
		swapped = e.tryCompareAndSwap(old, new)
	} else if e, ok := m.dirty[key]; ok {
		swapped = e.tryCompareAndSwap(old, new)
		// We needed to lock mu in order to load the entry for key,
		// and the operation didn't change the set of keys in the map
		// (so it would be made more efficient by promoting the dirty
		// map to read-only).
		// Count it as a miss so that we will eventually switch to the
		// more efficient steady state.
		// 我们需要锁定mu以便加载键的条目，并且该操作不会更改map中的键集（因此将dirty map提升为read map将更有效）
		// 将其视为miss，以便我们最终切换到更有效的稳定状态。
		m.missLocked()
	}
	return swapped
}

// CompareAndDelete deletes the entry for key if its value is equal to old.
// The old value must be of a comparable type.
// CompareAndDelete 如果键的值等于 old，则删除键的条目。可以理解为确认你要删除的值是不是你想要删除的
//
// If there is no current value for key in the map, CompareAndDelete
// returns false (even if the old value is the nil interface value).
// 如果 map 中没有键的当前值，则 CompareAndDelete 返回 false（即使旧值是 nil interface{} 值）。
func (m *Map) CompareAndDelete(key, old any) (deleted bool) {
	read := m.loadReadOnly()
	e, ok := read.m[key]
	if !ok && read.amended {
		// read map 中没有这个键, 但是dirty map中可能有
		m.mu.Lock()
		read = m.loadReadOnly()
		e, ok = read.m[key]
		if !ok && read.amended {
			e, ok = m.dirty[key]
			// Don't delete key from m.dirty: we still need to do the “compare” part
			// of the operation. The entry will eventually be expunged when the
			// dirty map is promoted to the read map.
			// 不要从 m.dirty 中删除键：我们仍然需要执行操作的“比较”部分。
			// 当 dirty map 提升为 read map 时，该条目最终将被删除。
			//
			// Regardless of whether the entry was present, record a miss: this key
			// will take the slow path until the dirty map is promoted to the read
			// map.
			// 无论条目是否存在，都记录一个 miss：直到 dirty map 提升为 read map，否则该键将采用慢路径。
			m.missLocked()
		}
		m.mu.Unlock()
	}
	for ok {
		p := e.p.Load()
		if p == nil || p == expunged || *p != old {
			return false
		}
		// 删除即为将p置为nil
		if e.p.CompareAndSwap(p, nil) {
			return true
		}
	}
	return false
}

// Range calls f sequentially for each key and value present in the map.
// If f returns false, range stops the iteration.
// Range 依次为 map 中存在的每个键和值调用 f。
//
// Range does not necessarily correspond to any consistent snapshot of the Map's
// contents: no key will be visited more than once, but if the value for any key
// is stored or deleted concurrently (including by f), Range may reflect any
// mapping for that key from any point during the Range call. Range does not
// block other methods on the receiver; even f itself may call any method on m.
// Range 不一定对应于 map 内容的任何一致快照：不会访问任何键超过一次，
// 但是如果任何键的值同时存储或删除（包括 f），则 Range 可以反映 Range 调用期间该键的任何点的任何映射。
// Range 不会阻止接收器上的其他方法；即使 f 本身也可以调用 m 上的任何方法。
//
// Range may be O(N) with the number of elements in the map even if f returns
// false after a constant number of calls.
// Range 可能是 O(N)，其中 N 是 map 中的元素数量，即使 f 在恒定数量的调用后返回 false。
func (m *Map) Range(f func(key, value any) bool) {
	// We need to be able to iterate over all of the keys that were already
	// present at the start of the call to Range.
	// 我们需要能够迭代调用 Range 时已经存在的所有键。
	// If read.amended is false, then read.m satisfies that property without
	// requiring us to hold m.mu for a long time.
	// 如果 read.amended 为 false，则 read.m 满足该属性，而无需我们长时间持有m.mu(因为不需要访问dirty map)
	read := m.loadReadOnly()
	if read.amended {
		// m.dirty contains keys not in read.m. Fortunately, Range is already O(N)
		// (assuming the caller does not break out early), so a call to Range
		// amortizes an entire copy of the map: we can promote the dirty copy
		// immediately!
		// m.dirty 包含不在 read.m 中的键。幸运的是，Range 已经是 O(N)（假设调用者没有提前退出），
		// 因此对 Range 的调用会分摊整个 map 的复制：我们可以立即提升 dirty map 的副本！
		m.mu.Lock()
		read = m.loadReadOnly()
		if read.amended {
			read = readOnly{m: m.dirty}
			m.read.Store(&read)
			m.dirty = nil
			m.misses = 0
		}
		m.mu.Unlock()
	}

	for k, e := range read.m {
		v, ok := e.load()
		if !ok {
			continue
		}
		if !f(k, v) {
			break
		}
	}
}

func (m *Map) missLocked() {
	m.misses++
	if m.misses < len(m.dirty) {
		return
	}
	m.read.Store(&readOnly{m: m.dirty})
	m.dirty = nil
	m.misses = 0
}

// dirtyLocked 将 m.dirty 初始化为 m.read 的副本, 标记删除的忽略。
func (m *Map) dirtyLocked() {
	if m.dirty != nil {
		return
	}

	read := m.loadReadOnly()
	m.dirty = make(map[any]*entry, len(read.m))
	for k, e := range read.m {
		if !e.tryExpungeLocked() {
			m.dirty[k] = e
		}
	}
}

// tryExpungeLocked 将条目标记为删除，以便在下次加载时将其复制到 dirty map 中。
func (e *entry) tryExpungeLocked() (isExpunged bool) {
	p := e.p.Load()
	// 如果是nil, 则标记为删除
	// 否则判断是否是expunged, 如果是则返回true
	for p == nil {
		if e.p.CompareAndSwap(nil, expunged) {
			return true
		}
		p = e.p.Load()
	}
	return p == expunged
}
