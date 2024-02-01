// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package bufio implements buffered I/O. It wraps an io.Reader or io.Writer
// object, creating another object (Reader or Writer) that also implements
// the interface but provides buffering and some help for textual I/O.
package bufio

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	defaultBufSize = 4096
)

var (
	ErrInvalidUnreadByte = errors.New("bufio: invalid use of UnreadByte")
	ErrInvalidUnreadRune = errors.New("bufio: invalid use of UnreadRune")
	ErrBufferFull        = errors.New("bufio: buffer full")
	ErrNegativeCount     = errors.New("bufio: negative count")
)

// Buffered input.

// Reader implements buffering for an io.Reader object.
type Reader struct {
	buf          []byte
	rd           io.Reader // reader provided by the client
	r, w         int       // buf read and write positions
	err          error
	lastByte     int // last byte read for UnreadByte; -1 means invalid
	lastRuneSize int // size of last rune read for UnreadRune; -1 means invalid
}

const (
	minReadBufferSize        = 16
	maxConsecutiveEmptyReads = 100
)

// NewReaderSize returns a new Reader whose buffer has at least the specified
// size. If the argument io.Reader is already a Reader with large enough
// size, it returns the underlying Reader.
// NewReaderSize 如果参数io.Reader已经是具有足够大的大小的 Reader，则返回底层 Reader。
// 否则将返回一个新的 Reader，其缓冲区至少具有指定的大小max(minReadBufferSize, size)。
func NewReaderSize(rd io.Reader, size int) *Reader {
	// Is it already a Reader?
	b, ok := rd.(*Reader)
	if ok && len(b.buf) >= size {
		return b
	}
	if size < minReadBufferSize {
		size = minReadBufferSize
	}
	r := new(Reader)
	r.reset(make([]byte, size), rd)
	return r
}

// NewReader returns a new Reader whose buffer has the default size.
// NewReader 返回一个新的 Reader，其缓冲区至少具有默认大小4k。
func NewReader(rd io.Reader) *Reader {
	return NewReaderSize(rd, defaultBufSize)
}

// Size returns the size of the underlying buffer in bytes.
// Size 返回底层缓冲区的大小（以字节为单位）。
func (b *Reader) Size() int { return len(b.buf) }

// Reset discards any buffered data, resets all state, and switches
// the buffered reader to read from r.
// Reset 丢弃任何缓冲的数据，重置所有状态，并将缓冲读取器切换到从r读取。
// Calling Reset on the zero value of Reader initializes the internal buffer
// to the default size.
// 调用 Reader 的零值的 Reset 方法会将内部缓冲区初始化为默认大小。
// Calling b.Reset(b) (that is, resetting a Reader to itself) does nothing.
// 调用 b.Reset(b)（即将 Reader 重置为其自身）不执行任何操作。
func (b *Reader) Reset(r io.Reader) {
	// If a Reader r is passed to NewReader, NewReader will return r.
	// Different layers of code may do that, and then later pass r
	// to Reset. Avoid infinite recursion in that case.
	if b == r {
		return
	}
	if b.buf == nil {
		b.buf = make([]byte, defaultBufSize)
	}
	b.reset(b.buf, r)
}

func (b *Reader) reset(buf []byte, r io.Reader) {
	*b = Reader{
		buf:          buf,
		rd:           r,
		lastByte:     -1,
		lastRuneSize: -1,
	}
}

var errNegativeRead = errors.New("bufio: reader returned negative count from Read")

// fill reads a new chunk into the buffer.
// fill 从底层读取器读取数据到缓冲区。
func (b *Reader) fill() {
	// Slide existing data to beginning.
	// 将现有数据滑动到开头。
	if b.r > 0 {
		// 如果读取位置大于0，说明读取位置之前的数据已经被读取过了，需要将读取位置之前的数据移动到缓冲区的开头。
		// 这里用了copy函数将未读的部分拷贝到开头, 同时重置r,w位置
		copy(b.buf, b.buf[b.r:b.w])
		b.w -= b.r
		b.r = 0
	}

	if b.w >= len(b.buf) {
		// 写入位置大于等于缓冲区的长度，说明缓冲区已经满了，不能再往里写了，直接panic
		panic("bufio: tried to fill full buffer")
	}

	// Read new data: try a limited number of times.
	// 读取新数据：尝试有限次数100。
	for i := maxConsecutiveEmptyReads; i > 0; i-- {
		// 从底层读取器reader读取数据到缓冲区空闲位置
		n, err := b.rd.Read(b.buf[b.w:])
		if n < 0 {
			panic(errNegativeRead)
		}
		b.w += n
		if err != nil {
			b.err = err
			return
		}
		if n > 0 {
			return
		}
	}
	// 读取100次都没有读取到数据，说明底层读取器reader有问题，将err设置为io.ErrNoProgress
	b.err = io.ErrNoProgress
}

// readErr 返回reader的错误，并置为nil
func (b *Reader) readErr() error {
	err := b.err
	b.err = nil
	return err
}

// Peek returns the next n bytes without advancing the reader. The bytes stop
// being valid at the next read call. If Peek returns fewer than n bytes, it
// also returns an error explaining why the read is short. The error is
// ErrBufferFull if n is larger than b's buffer size.
// Peek 返回接下来的n个字节，而不推进reader的r字段(和read相比)。本次停止读的字节在下一次读取调用时有效。如果 Peek 返回少于 n 个字节，
// 它还会返回一个错误，解释为什么读取短。如果 n 大于 b 的缓冲区大小，则错误为 ErrBufferFull。
//
// Calling Peek prevents a UnreadByte or UnreadRune call from succeeding
// until the next read operation.
// 调用 Peek 会阻止 UnreadByte 或 UnreadRune 调用成功，直到下一次读取操作。
func (b *Reader) Peek(n int) ([]byte, error) {
	if n < 0 {
		return nil, ErrNegativeCount
	}

	b.lastByte = -1
	b.lastRuneSize = -1

	// 确保缓冲区有足够的数据或者在填充时遇到错误
	for b.w-b.r < n && b.w-b.r < len(b.buf) && b.err == nil {
		// 容量足够且没有错误，读取数据到缓冲区
		b.fill() // b.w-b.r < len(b.buf) => buffer is not full
	}

	if n > len(b.buf) {
		// 要读取的字节数大于缓冲区的长度,返回未读取的最大容量数据，返回错误
		return b.buf[b.r:b.w], ErrBufferFull
	}

	// 0 <= n <= len(b.buf)
	var err error
	// 如果缓冲区的可读数据小于要读取的数据，说明缓冲区的数据不够(前面fill的过程是因为读取遇到错误而提前返回)
	// 返回缓冲区的可读数据，返回错误
	if avail := b.w - b.r; avail < n {
		// not enough data in buffer
		// 缓冲区的数据不够
		n = avail
		err = b.readErr()
		// 确保err不为nil
		if err == nil {
			err = ErrBufferFull
		}
	}
	return b.buf[b.r : b.r+n], err
}

// Discard skips the next n bytes, returning the number of bytes discarded.
// Discard 跳过接下来的n个字节，返回丢弃的字节数。
//
// If Discard skips fewer than n bytes, it also returns an error.
// If 0 <= n <= b.Buffered(), Discard is guaranteed to succeed without
// reading from the underlying io.Reader.
// 如果 Discard 跳过的字节数少于 n，则它也会返回一个错误。
// 如果 0 <= n <= b.Buffered()，则 Discard 保证不成功且不会从底层io.Reader读取数据。
func (b *Reader) Discard(n int) (discarded int, err error) {
	if n < 0 {
		return 0, ErrNegativeCount
	}
	if n == 0 {
		return
	}

	b.lastByte = -1
	b.lastRuneSize = -1

	remain := n
	for {
		skip := b.Buffered()
		if skip == 0 {
			// 缓冲区没有数据，从底层读取器读取数据到缓冲区
			b.fill()
			skip = b.Buffered()
		}
		if skip > remain {
			skip = remain
		}
		b.r += skip
		remain -= skip
		if remain == 0 {
			return n, nil
		}
		if b.err != nil {
			return n - remain, b.readErr()
		}
	}
}

// Read reads data into p.
// Read 将数据读入p。
// It returns the number of bytes read into p.
// 返回读入p的字节数。
// The bytes are taken from at most one Read on the underlying Reader,
// hence n may be less than len(p).
// 这些被读取字节最多来自底层Reader上的一个Read，因此n可能小于len(p)。
// To read exactly len(p) bytes, use io.ReadFull(b, p).
// 要读取确切的len(p)字节，请使用io.ReadFull(b, p)。
// If the underlying Reader can return a non-zero count with io.EOF,
// then this Read method can do so as well; see the [io.Reader] docs.
// 如果底层Reader可以返回非零计数和io.EOF，则此Read方法也可以这样做；请参阅[io.Reader]文档。
func (b *Reader) Read(p []byte) (n int, err error) {
	n = len(p)
	if n == 0 {
		if b.Buffered() > 0 {
			return 0, nil
		}
		// 如果底层缓冲器没有数据
		return 0, b.readErr()
	}
	if b.r == b.w {
		// 如果读取位置等于写入位置，说明缓冲区没有数据，从底层读取器读取数据到缓冲区
		if b.err != nil {
			// 如果底层读取器有错误，返回0和错误
			return 0, b.readErr()
		}
		if len(p) >= len(b.buf) {
			// Large read, empty buffer.
			// Read directly into p to avoid copy.
			// 如果要读取的字节数大于等于缓冲区的长度，直接从底层读取器读取数据到p
			n, b.err = b.rd.Read(p)
			if n < 0 {
				panic(errNegativeRead)
			}
			if n > 0 {
				b.lastByte = int(p[n-1])
				b.lastRuneSize = -1
			}
			return n, b.readErr()
		}
		// One read.
		// Do not use b.fill, which will loop.
		b.r = 0
		b.w = 0
		// 为什么这里读取数据到b.buf，而不是直接读取到p？
		// 因为如果读取到p，那么p的数据会被覆盖，如果读取到b.buf，那么p的数据就不会被覆盖了
		// 因为buf容量足够本次读取的长度，所以先将buf的容量填满
		n, b.err = b.rd.Read(b.buf)
		if n < 0 {
			panic(errNegativeRead)
		}
		if n == 0 {
			return 0, b.readErr()
		}
		b.w += n
	}

	// copy as much as we can
	// Note: if the slice panics here, it is probably because
	// the underlying reader returned a bad count. See issue 49795.
	// 尽可能将缓冲区的数据拷贝到p
	// 注意：如果切片在这里发生panic，可能是因为底层读取器返回了错误的计数。请参阅issue 49795。
	n = copy(p, b.buf[b.r:b.w])
	b.r += n
	b.lastByte = int(b.buf[b.r-1])
	b.lastRuneSize = -1
	return n, nil
}

// ReadByte reads and returns a single byte.
// If no byte is available, returns an error.
// ReadByte 读取并返回一个单字节。
// 如果没有字节可用，则返回错误。
func (b *Reader) ReadByte() (byte, error) {
	b.lastRuneSize = -1
	for b.r == b.w {
		if b.err != nil {
			// 如果原先底层读取器有错误，返回0和错误
			return 0, b.readErr()
		}
		// buffer 为空，填充
		b.fill() // buffer is empty
	}
	c := b.buf[b.r]
	b.r++
	b.lastByte = int(c)
	return c, nil
}

// UnreadByte unreads the last byte. Only the most recently read byte can be unread.
//
// UnreadByte returns an error if the most recent method called on the
// Reader was not a read operation. Notably, Peek, Discard, and WriteTo are not
// considered read operations.
// UnreadByte 取消读取最后一个字节。但是只能取消读取最近读取的字节。
// 如果最近在Reader上调用的方法不是读取操作，则 UnreadByte 返回错误。
// 特别是，Peek，Discard和WriteTo不被视为读取操作。
func (b *Reader) UnreadByte() error {
	// 每次非读取操作，lastByte都会被置为-1
	// 或者没有读取过数据，lastByte也会被置为-1
	if b.lastByte < 0 || b.r == 0 && b.w > 0 {
		return ErrInvalidUnreadByte
	}
	// b.r > 0 || b.w == 0
	// 这里b.w不用考虑都可以吧，因为b.r > 0 说明条件判断是false了
	if b.r > 0 {
		b.r--
	} else {
		// b.r == 0 && b.w == 0
		b.w = 1
	}
	// 将最后依次读取的字节填入缓冲区最后一次读取的位置
	b.buf[b.r] = byte(b.lastByte)
	// 重置状态
	b.lastByte = -1
	b.lastRuneSize = -1
	return nil
}

// ReadRune reads a single UTF-8 encoded Unicode character and returns the
// rune and its size in bytes. If the encoded rune is invalid, it consumes one byte
// and returns unicode.ReplacementChar (U+FFFD) with a size of 1.
// ReadRune 读取一个UTF-8编码的Unicode字符，并返回rune及其字节大小。
// 如果编码的rune无效，则消耗一个字节并返回unicode.ReplacementChar（U+FFFD），大小为1。
func (b *Reader) ReadRune() (r rune, size int, err error) {
	for b.r+utf8.UTFMax > b.w && !utf8.FullRune(b.buf[b.r:b.w]) && b.err == nil && b.w-b.r < len(b.buf) {
		// 所需读取的数据不够就填充
		b.fill() // b.w-b.r < len(buf) => buffer is not full
	}
	b.lastRuneSize = -1
	if b.r == b.w {
		return 0, 0, b.readErr()
	}
	r, size = rune(b.buf[b.r]), 1
	if r >= utf8.RuneSelf {
		// 多字节rune，即UTF-8编码的Unicode字符
		r, size = utf8.DecodeRune(b.buf[b.r:b.w])
	}
	b.r += size
	b.lastByte = int(b.buf[b.r-1])
	b.lastRuneSize = size
	return r, size, nil
}

// UnreadRune unreads the last rune. If the most recent method called on
// the Reader was not a ReadRune, UnreadRune returns an error. (In this
// regard it is stricter than UnreadByte, which will unread the last byte
// from any read operation.)
// UnreadRune 取消读取最后一个rune。如果最近在Reader上调用的方法不是ReadRune，则UnreadRune返回错误。
// （在这方面，它比UnreadByte更严格，后者将从任何读取操作中取消读取最后一个字节）
func (b *Reader) UnreadRune() error {
	if b.lastRuneSize < 0 || b.r < b.lastRuneSize {
		// lastRuneSize < 0 说明没有读取过rune数据
		// b.r < b.lastRuneSize 说明读取位置小于rune的大小，说明读取位置之前没有读取过rune数据;
		// 或者说当前的数据读取位置不支持unread一个rune
		return ErrInvalidUnreadRune
	}
	// b.r 位置往前移动lastRuneSize个位置
	b.r -= b.lastRuneSize
	b.lastByte = -1
	b.lastRuneSize = -1
	return nil
}

// Buffered returns the number of bytes that can be read from the current buffer.
// Buffered 返回可以从当前缓冲区读取的字节数。
func (b *Reader) Buffered() int { return b.w - b.r }

// ReadSlice reads until the first occurrence of delim in the input,
// returning a slice pointing at the bytes in the buffer.
// ReadSlice 读取输入中第一个delim出现的位置，返回一个指向缓冲区中字节的切片。
// The bytes stop being valid at the next read.
// 停止读取的字节在下一次读取时将不再有效。
// If ReadSlice encounters an error before finding a delimiter,
// it returns all the data in the buffer and the error itself (often io.EOF).
// 如果ReadSlice在找到分隔符之前遇到错误，则返回缓冲区中的所有数据和错误本身（通常为io.EOF）。
// ReadSlice fails with error ErrBufferFull if the buffer fills without a delim.
// 如果已经填满的缓冲区没有分隔符，则ReadSlice失败并出现错误ErrBufferFull。
// Because the data returned from ReadSlice will be overwritten
// by the next I/O operation, most clients should use
// ReadBytes or ReadString instead.
// 由于从ReadSlice返回的数据将被下一次I/O操作覆盖，因此大多数客户端应改用ReadBytes或ReadString。
// ReadSlice returns err != nil if and only if line does not end in delim.
// 如果line不以delim结尾，则ReadSlice返回err！= nil。
func (b *Reader) ReadSlice(delim byte) (line []byte, err error) {
	s := 0 // search start index
	for {
		// Search buffer.
		if i := bytes.IndexByte(b.buf[b.r+s:b.w], delim); i >= 0 {
			// 找到了delim
			i += s
			// 返回的slice包含delim
			line = b.buf[b.r : b.r+i+1]
			b.r += i + 1
			break
		}

		// Pending error?
		if b.err != nil {
			// 有错误，返回缓冲区中的所有数据和错误本身
			line = b.buf[b.r:b.w]
			b.r = b.w
			err = b.readErr()
			break
		}

		// Buffer full?
		if b.Buffered() >= len(b.buf) {
			// b.w - b.r >= len(b.buf) => buffer is full
			b.r = b.w
			line = b.buf
			err = ErrBufferFull
			break
		}

		// 不能rescan之前扫描过的区域
		s = b.w - b.r // do not rescan area we scanned before

		b.fill() // buffer is not full
	}

	// Handle last byte, if any.
	// 处理最后一个字节，如果有的话
	if i := len(line) - 1; i >= 0 {
		b.lastByte = int(line[i])
		b.lastRuneSize = -1
	}

	return
}

// ReadLine is a low-level line-reading primitive. Most callers should use
// ReadBytes('\n') or ReadString('\n') instead or use a Scanner.
// ReadLIne 是一个底层的读取行的原语。
// 大多数调用者应该使用ReadBytes('\n')或ReadString('\n')，或者使用Scanner。
//
// ReadLine tries to return a single line, not including the end-of-line bytes.
// If the line was too long for the buffer then isPrefix is set and the
// beginning of the line is returned. The rest of the line will be returned
// from future calls. isPrefix will be false when returning the last fragment
// of the line. The returned buffer is only valid until the next call to
// ReadLine. ReadLine either returns a non-nil line or it returns an error,
// never both.
// ReadLine 尝试返回单行，不包括行尾字节。
// 如果行对于缓冲区太长，则设置isPrefix并返回行的开头。行的其余部分将从未来的调用中返回。
// 返回最后一个片段时，isPrefix将为false。返回的缓冲区仅在下一次调用ReadLine之前有效。
// ReadLine要么返回非nil行，要么返回错误，永远不会同时返回两者。
//
// The text returned from ReadLine does not include the line end ("\r\n" or "\n").
// No indication or error is given if the input ends without a final line end.
// Calling UnreadByte after ReadLine will always unread the last byte read
// (possibly a character belonging to the line end) even if that byte is not
// part of the line returned by ReadLine.
// ReadLine 返回的文本不包括行尾（"\r\n"或"\n"）。
// 如果输入在没有最终行尾的情况下结束，则不会给出指示或错误。
// 在ReadLine之后调用UnreadByte将始终取消读取最后一个字节（可能是属于行尾的字符）
// 即使该字节不是ReadLine返回的行的一部分。
// 这里描述了一个特殊情况，在readline之后调用unreadbyte会取消读取最后一个字节
// 但是这个字节不是readline返回的行的一部分
func (b *Reader) ReadLine() (line []byte, isPrefix bool, err error) {
	line, err = b.ReadSlice('\n')
	if err == ErrBufferFull {
		// 如果缓冲区满了，说明行太长了
		// Handle the case where "\r\n" straddles the buffer.
		// 处理"\r\n"跨越缓冲区的情况
		if len(line) > 0 && line[len(line)-1] == '\r' {
			// Put the '\r' back on buf and drop it from line.
			// Let the next call to ReadLine check for "\r\n".
			if b.r == 0 {
				// b.r为0，说明缓冲区已经满了，但是没有读取过数据，直接panic
				// should be unreachable
				panic("bufio: tried to rewind past start of buffer")
			}
			// 将'\r'放回buf并从line中删除它。
			// 让下一次调用ReadLine检查"\r\n"。
			b.r--
			line = line[:len(line)-1]
		}
		return line, true, nil
	}

	if len(line) == 0 {
		if err != nil {
			line = nil
		}
		return
	}
	err = nil

	if line[len(line)-1] == '\n' {
		drop := 1
		if len(line) > 1 && line[len(line)-2] == '\r' {
			drop = 2
		}
		// 这里为什么不用更新b.r的值呢？
		// 这里说的是函数提及的特殊情况
		line = line[:len(line)-drop]
	}
	return
}

// collectFragments reads until the first occurrence of delim in the input. It
// returns (slice of full buffers, remaining bytes before delim, total number
// of bytes in the combined first two elements, error).
// collectFragments 读取输入中第一个delim出现的位置。
// 它返回（完整缓冲区的切片，delim之前的剩余字节，组合的前两个元素中的总字节数，错误）。
// The complete result is equal to
// `bytes.Join(append(fullBuffers, finalFragment), nil)`, which has a
// length of `totalLen`. The result is structured in this way to allow callers
// to minimize allocations and copies.
// 完整结果等于`bytes.Join(append(fullBuffers, finalFragment), nil)`，其长度为`totalLen`。
// 结果以这种方式结构化，以允许调用者最小化分配和复制。
func (b *Reader) collectFragments(delim byte) (fullBuffers [][]byte, finalFragment []byte, totalLen int, err error) {
	var frag []byte
	// Use ReadSlice to look for delim, accumulating full buffers.
	for {
		var e error
		frag, e = b.ReadSlice(delim)
		if e == nil { // got final fragment
			break
		}
		if e != ErrBufferFull { // unexpected error
			err = e
			break
		}

		// Make a copy of the buffer.
		buf := bytes.Clone(frag)
		fullBuffers = append(fullBuffers, buf)
		totalLen += len(buf)
	}

	totalLen += len(frag)
	return fullBuffers, frag, totalLen, err
}

// ReadBytes reads until the first occurrence of delim in the input,
// returning a slice containing the data up to and including the delimiter.
// ReadBytes 读取输入中第一个delim出现的位置，返回一个包含数据的切片，直到包含分隔符为止。
// If ReadBytes encounters an error before finding a delimiter,
// it returns the data read before the error and the error itself (often io.EOF).
// 如果ReadBytes在找到分隔符之前遇到错误，则返回错误之前读取的数据和错误本身（通常为io.EOF）。
// ReadBytes returns err != nil if and only if the returned data does not end in
// delim.
// 如果返回的数据不以delim结尾，则ReadBytes返回err!=nil。
// For simple uses, a Scanner may be more convenient.
// 对于简单的用法，Scanner可能更方便。
func (b *Reader) ReadBytes(delim byte) ([]byte, error) {
	full, frag, n, err := b.collectFragments(delim)
	// Allocate new buffer to hold the full pieces and the fragment.
	buf := make([]byte, n)
	n = 0
	// Copy full pieces and fragment in.
	for i := range full {
		n += copy(buf[n:], full[i])
	}
	copy(buf[n:], frag)
	return buf, err
}

// ReadString reads until the first occurrence of delim in the input,
// returning a string containing the data up to and including the delimiter.
// ReadString 读取输入中第一个delim出现的位置，返回一个包含数据的字符串，直到包含分隔符为止。
// If ReadString encounters an error before finding a delimiter,
// it returns the data read before the error and the error itself (often io.EOF).
// 如果ReadString在找到分隔符之前遇到错误，则返回错误之前读取的数据和错误本身（通常为io.EOF）。
// ReadString returns err != nil if and only if the returned data does not end in
// delim.
// 如果返回的数据不以delim结尾，则ReadString返回err!=nil。
// For simple uses, a Scanner may be more convenient.
// 对于简单的用法，Scanner可能更方便。
func (b *Reader) ReadString(delim byte) (string, error) {
	full, frag, n, err := b.collectFragments(delim)
	// Allocate new buffer to hold the full pieces and the fragment.
	var buf strings.Builder
	buf.Grow(n)
	// Copy full pieces and fragment in.
	for _, fb := range full {
		buf.Write(fb)
	}
	buf.Write(frag)
	return buf.String(), err
}

// WriteTo implements io.WriterTo.
// This may make multiple calls to the Read method of the underlying Reader.
// If the underlying reader supports the WriteTo method,
// this calls the underlying WriteTo without buffering.
// WriteTo 实现了io.WriterTo。
// 这可能会多次调用底层Reader的Read方法。
// 如果底层读取器支持WriteTo方法，则调用底层WriteTo而不缓冲。
func (b *Reader) WriteTo(w io.Writer) (n int64, err error) {
	b.lastByte = -1
	b.lastRuneSize = -1

	n, err = b.writeBuf(w)
	if err != nil {
		return
	}

	if r, ok := b.rd.(io.WriterTo); ok {
		m, err := r.WriteTo(w)
		n += m
		return n, err
	}

	if w, ok := w.(io.ReaderFrom); ok {
		m, err := w.ReadFrom(b.rd)
		n += m
		return n, err
	}

	if b.w-b.r < len(b.buf) {
		// buffer 没满
		b.fill() // buffer not full
	}

	for b.r < b.w {
		// b.r < b.w => buffer is not empty
		m, err := b.writeBuf(w)
		n += m
		if err != nil {
			return n, err
		}
		b.fill() // buffer is empty
	}

	if b.err == io.EOF {
		b.err = nil
	}

	return n, b.readErr()
}

var errNegativeWrite = errors.New("bufio: writer returned negative count from Write")

// writeBuf writes the Reader's buffer to the writer.
// writeBuf 将Reader的缓冲区写入writer。
func (b *Reader) writeBuf(w io.Writer) (int64, error) {
	n, err := w.Write(b.buf[b.r:b.w])
	if n < 0 {
		panic(errNegativeWrite)
	}
	b.r += n
	return int64(n), err
}

// buffered output

// Writer implements buffering for an io.Writer object.
// Writer实现了io.Writer对象的缓冲。
// If an error occurs writing to a Writer, no more data will be
// accepted and all subsequent writes, and Flush, will return the error.
// 如果在写入Writer时发生错误，则不会接受更多数据，并且所有后续写入和Flush都将返回错误。
// After all data has been written, the client should call the
// Flush method to guarantee all data has been forwarded to
// the underlying io.Writer.
// 写入所有数据后，客户端应调用Flush方法，以确保所有数据都已转发到底层io.Writer。
type Writer struct {
	err error
	buf []byte
	n   int
	wr  io.Writer
}

// NewWriterSize returns a new Writer whose buffer has at least the specified
// size. If the argument io.Writer is already a Writer with large enough
// size, it returns the underlying Writer.
// NewWriterSize 返回一个新的Writer，其缓冲区至少具有指定的大小。
// 如果参数io.Writer已经是具有足够大的大小的Writer，则返回底层Writer。
func NewWriterSize(w io.Writer, size int) *Writer {
	// Is it already a Writer?
	b, ok := w.(*Writer)
	if ok && len(b.buf) >= size {
		// 如果w是Writer类型，且缓冲区的容量足够，直接返回w
		return b
	}
	if size <= 0 {
		size = defaultBufSize
	}
	return &Writer{
		buf: make([]byte, size),
		wr:  w,
	}
}

// NewWriter returns a new Writer whose buffer has the default size.
// If the argument io.Writer is already a Writer with large enough buffer size,
// it returns the underlying Writer.
// NewWriter 返回一个新的Writer，其缓冲区具有默认大小。
// 如果参数io.Writer已经是具有足够大缓冲区的Writer，则返回底层Writer。
func NewWriter(w io.Writer) *Writer {
	return NewWriterSize(w, defaultBufSize)
}

// Size returns the size of the underlying buffer in bytes.
// Size 返回底层缓冲区的大小（以字节为单位）。
func (b *Writer) Size() int { return len(b.buf) }

// Reset discards any unflushed buffered data, clears any error, and
// resets b to write its output to w.
// Reset 丢弃任何未刷新的缓冲数据，清除任何错误，并将b重置为将其输出写入w。
// Calling Reset on the zero value of Writer initializes the internal buffer
// to the default size.
// 在Writer的零值上调用Reset会将内部缓冲区初始化为默认大小。
// Calling w.Reset(w) (that is, resetting a Writer to itself) does nothing.
// 调用w.Reset(w)（即将Writer重置为自身）不会执行任何操作。
func (b *Writer) Reset(w io.Writer) {
	// If a Writer w is passed to NewWriter, NewWriter will return w.
	// Different layers of code may do that, and then later pass w
	// to Reset. Avoid infinite recursion in that case.
	if b == w {
		return
	}
	if b.buf == nil {
		b.buf = make([]byte, defaultBufSize)
	}
	b.err = nil
	b.n = 0
	b.wr = w
}

// Flush writes any buffered data to the underlying io.Writer.
// Flush 将任何缓冲数据写入底层io.Writer。
func (b *Writer) Flush() error {
	if b.err != nil {
		return b.err
	}
	if b.n == 0 {
		return nil
	}
	n, err := b.wr.Write(b.buf[0:b.n])
	if n < b.n && err == nil {
		err = io.ErrShortWrite
	}
	if err != nil {
		if n > 0 && n < b.n {
			copy(b.buf[0:b.n-n], b.buf[n:b.n])
		}
		b.n -= n
		b.err = err
		return err
	}
	b.n = 0
	return nil
}

// Available returns how many bytes are unused in the buffer.
// Available 返回缓冲区中未使用的字节数。
func (b *Writer) Available() int { return len(b.buf) - b.n }

// AvailableBuffer returns an empty buffer with b.Available() capacity.
// This buffer is intended to be appended to and
// passed to an immediately succeeding Write call.
// The buffer is only valid until the next write operation on b.
// AvailableBuffer 返回一个空缓冲区，其容量为b.Available()。
// 该缓冲区旨在附加到并传递给紧接的Write调用。
// 该缓冲区仅在b上的下一次写操作之前有效。
func (b *Writer) AvailableBuffer() []byte {
	return b.buf[b.n:][:0]
}

// Buffered returns the number of bytes that have been written into the current buffer.
// Buffered 返回已写入当前缓冲区的字节数。
func (b *Writer) Buffered() int { return b.n }

// Write writes the contents of p into the buffer.
// It returns the number of bytes written.
// If nn < len(p), it also returns an error explaining
// why the write is short.
// Write 将p的内容写入缓冲区。
// 它返回写入的字节数。
// 如果nn < len(p)，它还会返回一个错误，解释为什么写入不足。
func (b *Writer) Write(p []byte) (nn int, err error) {
	for len(p) > b.Available() && b.err == nil {
		var n int
		if b.Buffered() == 0 {
			// Large write, empty buffer.
			// Write directly from p to avoid copy.
			n, b.err = b.wr.Write(p)
		} else {
			n = copy(b.buf[b.n:], p)
			b.n += n
			b.Flush()
		}
		nn += n
		p = p[n:]
	}
	if b.err != nil {
		return nn, b.err
	}
	n := copy(b.buf[b.n:], p)
	b.n += n
	nn += n
	return nn, nil
}

// WriteByte writes a single byte.
// WriteByte 写入一个单字节。
func (b *Writer) WriteByte(c byte) error {
	if b.err != nil {
		return b.err
	}
	if b.Available() <= 0 && b.Flush() != nil {
		return b.err
	}
	b.buf[b.n] = c
	b.n++
	return nil
}

// WriteRune writes a single Unicode code point, returning
// the number of bytes written and any error.
// WriteRune 写入单个Unicode代码点，返回写入的字节数和任何错误。
func (b *Writer) WriteRune(r rune) (size int, err error) {
	// Compare as uint32 to correctly handle negative runes.
	if uint32(r) < utf8.RuneSelf {
		err = b.WriteByte(byte(r))
		if err != nil {
			return 0, err
		}
		return 1, nil
	}
	if b.err != nil {
		return 0, b.err
	}
	n := b.Available()
	// 确认有足够的空间
	if n < utf8.UTFMax {
		if b.Flush(); b.err != nil {
			return 0, b.err
		}
		n = b.Available()
		if n < utf8.UTFMax {
			// Can only happen if buffer is silly small.
			return b.WriteString(string(r))
		}
	}
	size = utf8.EncodeRune(b.buf[b.n:], r)
	b.n += size
	return size, nil
}

// WriteString writes a string.
// It returns the number of bytes written.
// If the count is less than len(s), it also returns an error explaining
// why the write is short.
// WriteString 写入字符串。 它返回写入的字节数。 如果计数小于len(s)，它还会返回一个错误，解释为什么写入不足。
func (b *Writer) WriteString(s string) (int, error) {
	var sw io.StringWriter
	tryStringWriter := true

	nn := 0
	for len(s) > b.Available() && b.err == nil {
		// 如果缓冲区的容量不够，就先将缓冲区的数据写入底层，然后再写入s
		var n int
		if b.Buffered() == 0 && sw == nil && tryStringWriter {
			// Check at most once whether b.wr is a StringWriter.
			// 仅最多检查一次b.wr是否为StringWriter。
			sw, tryStringWriter = b.wr.(io.StringWriter)
		}
		if b.Buffered() == 0 && tryStringWriter {
			// Large write, empty buffer, and the underlying writer supports
			// WriteString: forward the write to the underlying StringWriter.
			// This avoids an extra copy.
			// 大写，空缓冲区，底层写入器支持WriteString：将写入转发到底层StringWriter。
			// 这避免了额外的复制。
			n, b.err = sw.WriteString(s)
		} else {
			n = copy(b.buf[b.n:], s)
			b.n += n
			b.Flush()
		}
		nn += n
		s = s[n:]
	}
	if b.err != nil {
		return nn, b.err
	}
	n := copy(b.buf[b.n:], s)
	b.n += n
	nn += n
	return nn, nil
}

// ReadFrom implements io.ReaderFrom. If the underlying writer
// supports the ReadFrom method, this calls the underlying ReadFrom.
// If there is buffered data and an underlying ReadFrom, this fills
// the buffer and writes it before calling ReadFrom.
// ReadFrom 实现了io.ReaderFrom。 如果底层写入器支持ReadFrom方法，则调用底层ReadFrom。
// 如果有缓冲数据和底层ReadFrom，则在调用ReadFrom之前填充缓冲区并将其写入。
func (b *Writer) ReadFrom(r io.Reader) (n int64, err error) {
	if b.err != nil {
		return 0, b.err
	}
	readerFrom, readerFromOK := b.wr.(io.ReaderFrom)
	var m int
	for {
		if b.Available() == 0 {
			if err1 := b.Flush(); err1 != nil {
				return n, err1
			}
		}
		if readerFromOK && b.Buffered() == 0 {
			// 如果底层写入器支持ReadFrom方法，并且缓冲区为空，直接调用底层的ReadFrom方法
			nn, err := readerFrom.ReadFrom(r)
			b.err = err
			n += nn
			return n, err
		}
		nr := 0
		for nr < maxConsecutiveEmptyReads {
			m, err = r.Read(b.buf[b.n:])
			// 如果读取到数据，就跳出循环
			if m != 0 || err != nil {
				break
			}
			nr++
		}
		if nr == maxConsecutiveEmptyReads {
			return n, io.ErrNoProgress
		}
		b.n += m
		n += int64(m)
		if err != nil {
			break
		}
	}
	if err == io.EOF {
		// If we filled the buffer exactly, flush preemptively.
		// 如果我们刚好填满了缓冲区，请预先刷新。
		if b.Available() == 0 {
			err = b.Flush()
		} else {
			err = nil
		}
	}
	return n, err
}

// buffered input and output

// ReadWriter stores pointers to a Reader and a Writer.
// It implements io.ReadWriter.
type ReadWriter struct {
	*Reader
	*Writer
}

// NewReadWriter allocates a new ReadWriter that dispatches to r and w.
// NewReadWriter 分配一个新的ReadWriter，该ReadWriter分派给r和w。
func NewReadWriter(r *Reader, w *Writer) *ReadWriter {
	return &ReadWriter{r, w}
}
