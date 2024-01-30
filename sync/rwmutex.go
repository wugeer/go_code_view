// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"sync/atomic"
	"unsafe"

	"internal/race"
)

// There is a modified copy of this file in runtime/rwmutex.go.
// If you make any changes here, see if you should make them there.

// A RWMutex is a reader/writer mutual exclusion lock.
// The lock can be held by an arbitrary number of readers or a single writer.
// The zero value for a RWMutex is an unlocked mutex.
//
// RWMutex 是读写互斥锁。这个锁可以被任意数量的reader或单个writer持有。RWMutex 的零值是一个未加锁的互斥锁。
//
// A RWMutex must not be copied after first use.
// RWMutex 在第一次使用后不能被复制。
//
// If a goroutine holds a RWMutex for reading and another goroutine might
// call Lock, no goroutine should expect to be able to acquire a read lock
// until the initial read lock is released. In particular, this prohibits
// recursive read locking. This is to ensure that the lock eventually becomes
// available; a blocked Lock call excludes new readers from acquiring the
// lock.
// 如果一个goroutine持有一个RWMutex进行读操作，而另一个goroutine可能会调用Lock，
// 那么没有goroutine应该期望能够在原先读锁被释放之前获得读锁(后面的写锁堵塞后续的读锁)。
// 特别是，这禁止了递归读锁定。这是为了确保锁最终可用；阻塞的Lock调用会排除新的读者获取锁。
//
// In the terminology of the Go memory model,
// the n'th call to Unlock “synchronizes before” the m'th call to Lock
// for any n < m, just as for Mutex.
// 在Go内存模型的术语中，对于任何n < m，第n次调用Unlock在第m次调用Lock之前同步，就像Mutex一样。
// For any call to RLock, there exists an n such that
// the n'th call to Unlock “synchronizes before” that call to RLock,
// and the corresponding call to RUnlock “synchronizes before”
// the n+1'th call to Lock.
// 对于任何对RLock的调用，都存在一个n，使得第n次调用Unlock在该对RLock的调用之前同步，
// 并且相应的对RUnlock的调用在第n+1次调用Lock之前同步。
type RWMutex struct {
	// 多个writer竞争锁
	w Mutex // held if there are pending writers
	// 等待reader完成的信号量
	writerSem uint32 // semaphore for writers to wait for completing readers
	// 等待writer完成的信号量
	readerSem uint32 // semaphore for readers to wait for completing writers
	// 正在运行reader的数量
	readerCount atomic.Int32 // number of pending readers
	// 等待完成reader的数量
	readerWait atomic.Int32 // number of departing readers
}

const rwmutexMaxReaders = 1 << 30

// Happens-before relationships are indicated to the race detector via:
// happens-before关系通过以下方式指示给race检测器：
// - Unlock  -> Lock:  readerSem  unlock happens before reader next lock
// - Unlock  -> RLock: readerSem unlock happens before reader next Rlock
// - RUnlock -> Lock:  writerSem RUnlock happens before writer next lock

//
// The methods below temporarily disable handling of race synchronization
// events in order to provide the more precise model above to the race
// detector.
// 下面的方法暂时禁用race同步事件的处理，以便向race检测器提供上面更精确的模型。
//
// For example, atomic.AddInt32 in RLock should not appear to provide
// acquire-release semantics, which would incorrectly synchronize racing
// readers, thus potentially missing races.
// 例如，RLock中的atomic.AddInt32不应该提供acquire-release语义，这将错误地同步竞争读取器，从而可能会丢失竞争。

// RLock locks rw for reading.
// RLock 锁定rw进行读取。
//
// It should not be used for recursive read locking; a blocked Lock
// call excludes new readers from acquiring the lock. See the
// documentation on the RWMutex type.
// 它不应该用于递归读锁定；阻塞的Lock调用会排除新的读者获取锁。请参阅RWMutex类型的文档。
func (rw *RWMutex) RLock() {
	if race.Enabled {
		_ = rw.w.state
		race.Disable()
	}
	if rw.readerCount.Add(1) < 0 {
		// 如果有writer在等待，那么readerCount值为负数
		// A writer is pending, wait for it.
		runtime_SemacquireRWMutexR(&rw.readerSem, false, 0)
	}
	if race.Enabled {
		race.Enable()
		race.Acquire(unsafe.Pointer(&rw.readerSem))
	}
}

// TryRLock tries to lock rw for reading and reports whether it succeeded.
// TryRLock 尝试锁定rw进行读取，并报告是否成功。
//
// Note that while correct uses of TryRLock do exist, they are rare,
// and use of TryRLock is often a sign of a deeper problem
// in a particular use of mutexes.
// 请注意，尽管存在TryRLock的正确用法，但它们很少见
// 并且TryRLock的使用通常是互斥锁特定用法中更深层次问题的标志。
func (rw *RWMutex) TryRLock() bool {
	if race.Enabled {
		_ = rw.w.state
		race.Disable()
	}
	for {
		c := rw.readerCount.Load()
		if c < 0 {
			// 有writer在等待
			if race.Enabled {
				race.Enable()
			}
			return false
		}
		// CAS 操作，如果readerCount值为c，那么将readerCount值加1
		if rw.readerCount.CompareAndSwap(c, c+1) {
			if race.Enabled {
				race.Enable()
				race.Acquire(unsafe.Pointer(&rw.readerSem))
			}
			return true
		}
	}
}

// RUnlock undoes a single RLock call;
// it does not affect other simultaneous readers.
// It is a run-time error if rw is not locked for reading
// on entry to RUnlock.
// RUnlock 撤消单个RLock调用；它不会影响其他同时reader。
// 如果rw在进入RUnlock时没有被读取锁定，则为运行时错误。
func (rw *RWMutex) RUnlock() {
	if race.Enabled {
		_ = rw.w.state
		race.ReleaseMerge(unsafe.Pointer(&rw.writerSem))
		race.Disable()
	}
	if r := rw.readerCount.Add(-1); r < 0 {
		// Outlined slow-path to allow the fast-path to be inlined
		// 区分快慢路径，以便快路径可以内联
		rw.rUnlockSlow(r)
	}
	if race.Enabled {
		race.Enable()
	}
}

func (rw *RWMutex) rUnlockSlow(r int32) {
	// 临界条件检查: readerCount值为0，或者readerCount值为-rwmutexMaxReaders
	if r+1 == 0 || r+1 == -rwmutexMaxReaders {
		race.Enable()
		fatal("sync: RUnlock of unlocked RWMutex")
	}
	// A writer is pending.
	if rw.readerWait.Add(-1) == 0 {
		// The last reader unblocks the writer.
		// 最后一个reader解除writer的阻塞
		runtime_Semrelease(&rw.writerSem, false, 1)
	}
}

// Lock locks rw for writing.
// If the lock is already locked for reading or writing,
// Lock blocks until the lock is available.
// Lock 锁定rw进行写入。
// 如果锁已经被锁定进行读取或写入，则Lock阻塞，直到锁可用。
func (rw *RWMutex) Lock() {
	if race.Enabled {
		_ = rw.w.state
		race.Disable()
	}
	// First, resolve competition with other writers.
	// 首先，解决与其他writer的竞争。
	rw.w.Lock()
	// 通知reader有writer在等待
	// Announce to readers there is a pending writer.
	r := rw.readerCount.Add(-rwmutexMaxReaders) + rwmutexMaxReaders
	// Wait for active readers.
	// 等待活动的reader
	if r != 0 && rw.readerWait.Add(r) != 0 {
		runtime_SemacquireRWMutex(&rw.writerSem, false, 0)
	}
	if race.Enabled {
		race.Enable()
		race.Acquire(unsafe.Pointer(&rw.readerSem))
		race.Acquire(unsafe.Pointer(&rw.writerSem))
	}
}

// TryLock tries to lock rw for writing and reports whether it succeeded.
//
// Note that while correct uses of TryLock do exist, they are rare,
// and use of TryLock is often a sign of a deeper problem
// in a particular use of mutexes.
// TryLock 尝试锁定rw进行写入，并报告是否成功。
// 请注意，尽管存在TryLock的正确用法，但它们很少见,并且TryLock的使用通常是互斥锁特定用法中更深层次问题的标志。
func (rw *RWMutex) TryLock() bool {
	if race.Enabled {
		_ = rw.w.state
		race.Disable()
	}
	if !rw.w.TryLock() {
		// writer竞争锁失败
		if race.Enabled {
			race.Enable()
		}
		return false
	}
	if !rw.readerCount.CompareAndSwap(0, -rwmutexMaxReaders) {
		// readerCount值不为0
		rw.w.Unlock()
		if race.Enabled {
			race.Enable()
		}
		return false
	}
	if race.Enabled {
		race.Enable()
		race.Acquire(unsafe.Pointer(&rw.readerSem))
		race.Acquire(unsafe.Pointer(&rw.writerSem))
	}
	return true
}

// Unlock unlocks rw for writing. It is a run-time error if rw is
// not locked for writing on entry to Unlock.
// Unlock 解锁rw进行写入。如果rw在进入Unlock时没有被写入锁定，则为运行时错误。
//
// As with Mutexes, a locked RWMutex is not associated with a particular
// goroutine. One goroutine may RLock (Lock) a RWMutex and then
// arrange for another goroutine to RUnlock (Unlock) it.
// 与Mutexes一样，锁定的RWMutex与特定的goroutine无关。一个goroutine可以RLock（Lock）一个RWMutex，
// 然后安排另一个goroutine RUnlock（Unlock）它。
func (rw *RWMutex) Unlock() {
	if race.Enabled {
		_ = rw.w.state
		race.Release(unsafe.Pointer(&rw.readerSem))
		race.Disable()
	}

	// Announce to readers there is no active writer.
	// 通知reader已经没有活动的writer
	r := rw.readerCount.Add(rwmutexMaxReaders)
	// 临界条件检查: 如果没有先调用lock，则抛出panic
	if r >= rwmutexMaxReaders {
		race.Enable()
		fatal("sync: Unlock of unlocked RWMutex")
	}
	// Unblock blocked readers, if any.
	// 如果有reader在等待，那么解除reader的阻塞
	for i := 0; i < int(r); i++ {
		runtime_Semrelease(&rw.readerSem, false, 0)
	}
	// Allow other writers to proceed.
	// 允许其他writer继续竞争锁
	rw.w.Unlock()
	if race.Enabled {
		race.Enable()
	}
}

// syscall_hasWaitingReaders reports whether any goroutine is waiting
// to acquire a read lock on rw. This exists because syscall.ForkLock
// is an RWMutex, and we can't change that without breaking compatibility.
// We don't need or want RWMutex semantics for ForkLock, and we use
// this private API to avoid having to change the type of ForkLock.
// For more details see the syscall package.
//
//go:linkname syscall_hasWaitingReaders syscall.hasWaitingReaders
func syscall_hasWaitingReaders(rw *RWMutex) bool {
	r := rw.readerCount.Load()
	return r < 0 && r+rwmutexMaxReaders > 0
}

// RLocker returns a Locker interface that implements
// the Lock and Unlock methods by calling rw.RLock and rw.RUnlock.
// RLocker 返回一个Locker接口，该接口通过调用rw.RLock和rw.RUnlock实现Lock和Unlock方法。
func (rw *RWMutex) RLocker() Locker {
	return (*rlocker)(rw)
}

type rlocker RWMutex

func (r *rlocker) Lock()   { (*RWMutex)(r).RLock() }
func (r *rlocker) Unlock() { (*RWMutex)(r).RUnlock() }
