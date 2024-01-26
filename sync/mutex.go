// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package sync provides basic synchronization primitives such as mutual
// exclusion locks. Other than the Once and WaitGroup types, most are intended
// for use by low-level library routines. Higher-level synchronization is
// better done via channels and communication.
//
// Values containing the types defined in this package should not be copied.
package sync

import (
	"sync/atomic"
	"unsafe"

	"internal/race"
)

// Provided by runtime via linkname.
func throw(string)
func fatal(string)

// A Mutex is a mutual exclusion lock. mutex是互斥锁
// The zero value for a Mutex is an unlocked mutex.
//
// A Mutex must not be copied after first use. mutex使用之后就不能被复制,还不如直接新建一个mutex
//
// In the terminology of the Go memory model,
// the n'th call to Unlock “synchronizes before” the m'th call to Lock
// for any n < m.
// go内存模型中,第n次调用Unlock在第m次调用Lock之前同步, 其中满足n < m
// A successful call to TryLock is equivalent to a call to Lock.
// TryLock的调用成功等价于调用Lock
// A failed call to TryLock does not establish any “synchronizes before”
// relation at all.
// Trylock的调用失败不会建立任何同步关系，仅是一种尝试获取锁的行为
type Mutex struct {
	state int32
	sema  uint32
}

// Lcoker接口定义了Lock和Unlock方法
// A Locker represents an object that can be locked and unlocked.
type Locker interface {
	Lock()
	Unlock()
}

const (
	mutexLocked      = 1 << iota // mutex is locked; 标识是否被锁，通过位运算来判断, 值为1
	mutexWoken                   // 唤醒标识，标识是否有goroutine被唤醒, 值为2
	mutexStarving                // 锁饥饿标识，标识是否处于饥饿状态的goroutine, 值为4
	mutexWaiterShift = iota      // 用于计算等待者数量的偏移量, 值为3，代表int32的前29位用于存储等待者数量
	// int32前29位用于存储等待者数量，最多可表示2^29-1个等待者；
	// 低3位用于存储其他信息，如是否被锁、是否被唤醒、是否处于饥饿状态
	// 低3位的值为：1、2、4，分别代表是否被锁、是否被唤醒、是否处于饥饿状态

	// Mutex fairness. mutex的公平性问题
	//
	// Mutex can be in 2 modes of operations: normal and starvation.
	// mutex有两种运行模式：正常模式和饥饿模式
	// In normal mode waiters are queued in FIFO order, but a woken up waiter
	// does not own the mutex and competes with new arriving goroutines over
	// the ownership. New arriving goroutines have an advantage -- they are
	// already running on CPU and there can be lots of them, so a woken up
	// waiter has good chances of losing. In such case it is queued at front
	// of the wait queue. If a waiter fails to acquire the mutex for more than 1ms,
	// it switches mutex to the starvation mode.
	//
	// 在正常模式下mutex，等待者按照FIFO顺序排队，但是被唤醒的等待者并不拥有mutex，
	// 而是与新到达的goroutine竞争mutex的所有权；新到达的goroutine有优势——它们已经在CPU上运行，并且可能有很多这样的goroutine
	// 因此，被唤醒的等待者很有可能会失败。在这种情况下，它会排在等待队列的前面。
	// 如果等待者在1ms内未能获取mutex，则将mutex切换到饥饿模式。
	//
	// In starvation mode ownership of the mutex is directly handed off from
	// the unlocking goroutine to the waiter at the front of the queue.
	// New arriving goroutines don't try to acquire the mutex even if it appears
	// to be unlocked, and don't try to spin. Instead they queue themselves at
	// the tail of the wait queue.
	//
	// 在饥饿模式下，mutex的所有权直接从解锁的goroutine移交给等待队列前面的等待者。
	// 新到达的goroutine即使看起来mutex是解锁的，也不会尝试获取mutex，也不会尝试自旋。
	// 相反，它们将自己排队到等待队列的末尾。确保那些处于饥饿模式的goroutine能够获取到mutex
	//
	// If a waiter receives ownership of the mutex and sees that either
	// (1) it is the last waiter in the queue, or (2) it waited for less than 1 ms,
	// it switches mutex back to normal operation mode.
	//
	// 如果等待者获取到mutex的所有权，并且发现以下两种情况之一，它会将mutex切换回正常模式：
	// 1. 它是等待队列中的最后一个等待者, 也就是说等待队列中只有它一个等待者
	//  防止下一次还是饥饿模式，但是等待队列为空，造成资源浪费或者不可预知的情况出现
	// 2. 它等待的时间少于1ms
	//
	// Normal mode has considerably better performance as a goroutine can acquire
	// a mutex several times in a row even if there are blocked waiters.
	// Starvation mode is important to prevent pathological cases of tail latency.
	//
	// 正常模式的性能要好得多，因为即使有阻塞的等待者，goroutine也可以连续多次获取mutex。
	// 饥饿模式非常重要，可以防止尾部延迟的病态情况。
	starvationThresholdNs = 1e6 // 1000000ns = 1ms
)

// Lock locks m.
// If the lock is already in use, the calling goroutine
// blocks until the mutex is available.
// Lock加锁，如果mutex已经被锁定，调用goroutine会阻塞直到mutex可用
func (m *Mutex) Lock() {
	// Fast path: grab unlocked mutex.
	// 快速路径：获取未锁定的mutex
	// 如果mutex未被锁定，通过CAS操作将mutex锁定，然后返回
	if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) {
		// 如果race检测开启，调用race.Acquire获取锁的指针
		if race.Enabled {
			race.Acquire(unsafe.Pointer(m))
		}
		return
	}
	// Slow path (outlined so that the fast path can be inlined)
	// 慢速路径（慢速路径被提取出来，以便快速路径可以内联）
	m.lockSlow()
}

// TryLock tries to lock m and reports whether it succeeded.
// TryLock尝试获取mutex的锁，如果mutex已经被锁定，则返回false，否则返回true
//
// Note that while correct uses of TryLock do exist, they are rare,
// and use of TryLock is often a sign of a deeper problem
// in a particular use of mutexes.
// 注意：虽然TryLock的正确使用确实存在，但是它们很少见
// 并且TryLock的使用通常是使用mutex的特定用法中存在更深层次问题的标志。
func (m *Mutex) TryLock() bool {
	old := m.state
	// 如果mutex已经被锁定，或者mutex处于饥饿状态，则返回false
	// Lock标志位或者饥饿标志位被设置，表示mutex已经被锁定
	if old&(mutexLocked|mutexStarving) != 0 {
		return false
	}

	// There may be a goroutine waiting for the mutex, but we are
	// running now and can try to grab the mutex before that
	// goroutine wakes up.
	// 可能有一个goroutine正在等待mutex，但是我们现在正在运行，可以在该goroutine唤醒之前尝试获取mutex
	// 这里考虑的是并发操作这个mutex时，仅有一个会成功，其余的会失败
	if !atomic.CompareAndSwapInt32(&m.state, old, old|mutexLocked) {
		return false
	}

	// 如果race检测开启，调用race.Acquire获取锁的指针
	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
	return true
}

func (m *Mutex) lockSlow() {
	var waitStartTime int64
	starving := false
	awoke := false
	iter := 0
	old := m.state
	for {
		// Don't spin in starvation mode, ownership is handed off to waiters
		// so we won't be able to acquire the mutex anyway.
		// 不能在饥饿模式下自旋，因为所有权被移交给等待者，因此我们无法获取mutex
		// 因此这里的判断是要满足锁已经被持有，且不能处于饥饿模式，同时可以自旋
		if old&(mutexLocked|mutexStarving) == mutexLocked && runtime_canSpin(iter) {
			// Active spinning makes sense.
			// Try to set mutexWoken flag to inform Unlock
			// to not wake other blocked goroutines.
			// 主动自旋是有意义的。尝试设置mutexWoken标志，以通知Unlock不要唤醒其他阻塞的goroutine
			// awoke没有被设置，且没有被唤醒(mutexWoken为0)，且等待者数量不为0
			// 且CAS(为了原子性的将state的mutexWoken位置为1)成功，则设置awoke为true
			if !awoke && old&mutexWoken == 0 && old>>mutexWaiterShift != 0 &&
				atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) {
				awoke = true
			}
			// 自旋一次
			runtime_doSpin()
			iter++
			// 重新获取state的值
			old = m.state
			continue
		}
		new := old
		// Don't try to acquire starving mutex, new arriving goroutines must queue.
		// 处于饥饿模式的mutex不能被获取，新到达的goroutine必须排队
		if old&mutexStarving == 0 {
			new |= mutexLocked
		}
		// 有锁或者处于饥饿模式则将等待者数量+1
		if old&(mutexLocked|mutexStarving) != 0 {
			new += 1 << mutexWaiterShift
		}
		// The current goroutine switches mutex to starvation mode.
		// But if the mutex is currently unlocked, don't do the switch.
		// Unlock expects that starving mutex has waiters, which will not
		// be true in this case.
		// 当前goroutine将mutex切换到饥饿模式。但是如果mutex当前未锁定，则不要切换。
		// Unlock期望饥饿模式的mutex有等待者，但在这种情况下不会成立。
		if starving && old&mutexLocked != 0 {
			// 如果处于饥饿模式，且mutex已经被锁定，则将mutex的状态设置为饥饿模式
			new |= mutexStarving
		}
		// 如果设置过mutexWoken
		if awoke {
			// The goroutine has been woken from sleep,
			// so we need to reset the flag in either case.
			// goroutine已经从睡眠中唤醒，因此我们需要在任何情况下都重置标志。
			// 这里二次校验了statue的mutexWoken位是否被设置，如果没有设置,但是awoke标识已经设置过了，则抛出异常
			if new&mutexWoken == 0 {
				throw("sync: inconsistent mutex state")
			}
			// 重置mutexWoken标识
			new &^= mutexWoken
		}
		// 这里相当于将所有waiter唤醒，然后竞争mutext?
		if atomic.CompareAndSwapInt32(&m.state, old, new) {
			// 非锁、非饥饿模式，直接返回
			if old&(mutexLocked|mutexStarving) == 0 {
				break // locked the mutex with CAS
			}
			// If we were already waiting before, queue at the front of the queue.
			// 如果我们之前已经在等待了，则排在队列的前面
			queueLifo := waitStartTime != 0
			if waitStartTime == 0 {
				waitStartTime = runtime_nanotime()
			}
			runtime_SemacquireMutex(&m.sema, queueLifo, 1)
			starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs
			old = m.state
			if old&mutexStarving != 0 {
				// If this goroutine was woken and mutex is in starvation mode,
				// ownership was handed off to us but mutex is in somewhat
				// inconsistent state: mutexLocked is not set and we are still
				// accounted as waiter. Fix that.
				// 如果此goroutine被唤醒并且mutex处于饥饿模式，则所有权已经移交给我们，但是mutex处于不一致的状态：
				// mutexLocked未设置，我们仍然被视为等待者。修复这个问题。
				// waiter个数为0，且有锁或者被唤醒，视为异常情况
				if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
					throw("sync: inconsistent mutex state")
				}
				// delta值为-7, 1-1<<3 = 1-8 = -7
				delta := int32(mutexLocked - 1<<mutexWaiterShift)
				// 如果不处于饥饿模式，或者等待者数量为1，则退出饥饿模式
				if !starving || old>>mutexWaiterShift == 1 {
					// Exit starvation mode.
					// Critical to do it here and consider wait time.
					// Starvation mode is so inefficient, that two goroutines
					// can go lock-step infinitely once they switch mutex
					// to starvation mode.
					// 退出饥饿模式。在这里考虑等待时间非常重要。
					// 饥饿模式非常低效，两个goroutine一旦将mutex切换到饥饿模式，它们就可以无限期地进行锁步进。
					delta -= mutexStarving
				}
				// 这里将delta值加到state上，将mutex的状态设置为正常模式
				atomic.AddInt32(&m.state, delta)
				break
			}
			awoke = true
			iter = 0
		} else {
			// CAS失败，重新获取state的值
			old = m.state
		}
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
}

// Unlock unlocks m.
// It is a run-time error if m is not locked on entry to Unlock.
//
// unlock解锁mutex，如果mutex未被锁定，则抛出异常
//
// A locked Mutex is not associated with a particular goroutine.
// It is allowed for one goroutine to lock a Mutex and then
// arrange for another goroutine to unlock it.
//
// 锁定的mutex不与特定的goroutine关联。允许一个goroutine锁定mutex，然后安排另一个goroutine解锁它。
func (m *Mutex) Unlock() {
	if race.Enabled {
		_ = m.state
		// 释放锁的指针
		race.Release(unsafe.Pointer(m))
	}

	// Fast path: drop lock bit.
	// 快速路径：释放锁位
	new := atomic.AddInt32(&m.state, -mutexLocked)
	if new != 0 {
		// Outlined slow path to allow inlining the fast path.
		// To hide unlockSlow during tracing we skip one extra frame when tracing GoUnblock.
		// 提取出慢速路径，以便内联快速路径。为了在跟踪时隐藏unlockSlow，我们在跟踪GoUnblock时跳过一个额外的帧。
		m.unlockSlow(new)
	}
}

func (m *Mutex) unlockSlow(new int32) {
	// 如果mutex未被锁定，则抛出异常
	// 这里相当于(m.state - mutexLocked + mutexLocked) & mutexLocked == 0 说明无锁
	if (new+mutexLocked)&mutexLocked == 0 {
		fatal("sync: unlock of unlocked mutex")
	}
	// 如果mutex处于正常模式
	if new&mutexStarving == 0 {
		old := new
		for {
			// If there are no waiters or a goroutine has already
			// been woken or grabbed the lock, no need to wake anyone.
			// In starvation mode ownership is directly handed off from unlocking
			// goroutine to the next waiter. We are not part of this chain,
			// since we did not observe mutexStarving when we unlocked the mutex above.
			// So get off the way.
			// 如果没有等待者，或者goroutine已经被唤醒或者获取了锁，则无需唤醒任何人
			// 在饥饿模式下，所有权直接从解锁goroutine移交给下一个等待者。这里的流程不是这个链条的一部分，
			// 因为我们在上面解锁mutex时没有观察到mutexStarving。所以不走这个分支。
			// 这里会不会有这样一种异常情况，waiter个数为0，但是mutexWoken或者mutexStarving不全为0
			if old>>mutexWaiterShift == 0 || old&(mutexLocked|mutexWoken|mutexStarving) != 0 {
				return
			}
			// Grab the right to wake someone.
			// 获取唤醒某人的权利
			new = (old - 1<<mutexWaiterShift) | mutexWoken
			if atomic.CompareAndSwapInt32(&m.state, old, new) {
				// 唤醒一个等待者
				runtime_Semrelease(&m.sema, false, 1)
				return
			}
			old = m.state
		}
	} else {
		// Starving mode: handoff mutex ownership to the next waiter, and yield
		// our time slice so that the next waiter can start to run immediately.
		// Note: mutexLocked is not set, the waiter will set it after wakeup.
		// But mutex is still considered locked if mutexStarving is set,
		// so new coming goroutines won't acquire it.
		// 饥饿模式：将mutex所有权移交给下一个等待者，并放弃我们的时间片，以便下一个等待者可以立即开始运行。
		// 注意：mutexLocked未设置，等待者将在唤醒后设置它。
		// 但是如果设置了mutexStarving，则仍然认为mutex被锁定，因此新到达的goroutine不会获取它。
		runtime_Semrelease(&m.sema, true, 1)
	}
}
