// Copyright 2026 肖其顿 (XIAO QI DUN)
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package j2kgo

import (
	"context"
	"fmt"
	"io"
)

// inputChunkSize 为顺序输入分配的缓存块大小，单位为字节
const inputChunkSize = 32 << 10

// inputSource 提供随机读取，顺序输入使用分块缓存
type inputSource struct {
	r         io.ReaderAt
	base      int64
	size      int64
	chunks    [][]byte
	chunkSize int
}

// contextReaderAt 为随机读取提供上下文取消检查
type contextReaderAt struct {
	ctx    context.Context
	source *inputSource
}

// memory 计算输入缓存及切片描述信息占用的内存
// 返回: uint64 内存字节数
func (s *inputSource) memory() uint64 {
	used := uint64(cap(s.chunks)) * 24
	for _, chunk := range s.chunks {
		used += uint64(cap(chunk))
	}
	return used
}

// newInputSource 创建输入源，不关闭调用方的输入流
// 输入支持随机读取且能确定长度时直接使用，查询后恢复读取位置；否则缓存顺序输入
// 入参: ctx 上下文, r 输入流, limits 资源限制
// 返回: *inputSource 输入源, error 错误信息
func newInputSource(ctx context.Context, r io.Reader, limits Limits) (*inputSource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil {
		return nil, fmt.Errorf("j2kgo: nil reader")
	}
	limits = limits.normalized()
	if random, ok := r.(interface {
		io.ReaderAt
		io.Seeker
	}); ok {
		base, err := random.Seek(0, io.SeekCurrent)
		if err != nil {
			return bufferInput(ctx, r, limits)
		}
		end, endErr := random.Seek(0, io.SeekEnd)
		_, restoreErr := random.Seek(base, io.SeekStart)
		if restoreErr != nil {
			return nil, restoreErr
		}
		if endErr != nil {
			return bufferInput(ctx, r, limits)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if base < 0 || end < base {
			return nil, FormatError("input extent")
		}
		if uint64(end-base) > limits.MaxInputBytes {
			return nil, &LimitError{Resource: "input", Limit: limits.MaxInputBytes, Required: uint64(end - base)}
		}
		return &inputSource{r: random, base: base, size: end - base}, nil
	}
	return bufferInput(ctx, r, limits)
}

// bufferInput 按固定块缓存顺序输入，避免扩容时复制全部已读内容
// 入参: ctx 上下文, r 输入流, limits 资源限制
// 返回: *inputSource 输入缓存, error 错误信息
func bufferInput(ctx context.Context, r io.Reader, limits Limits) (*inputSource, error) {
	source := &inputSource{chunkSize: int(min(inputChunkSize, limits.MaxInputBytes, limits.MaxMemoryBytes))}
	budget := &layoutBudget{limit: limits.MaxMemoryBytes}
	for {
		var first [1]byte
		if err := readFullContext(ctx, r, first[:]); err != nil {
			if err == io.EOF {
				return source, nil
			}
			return nil, err
		}
		if _, err := checkTotal("input", uint64(source.size), 1, limits.MaxInputBytes); err != nil {
			return nil, err
		}
		length := min(uint64(source.chunkSize), limits.MaxInputBytes-uint64(source.size))
		if err := budget.add(length, 1); err != nil {
			return nil, err
		}
		block := make([]byte, int(length))
		block[0] = first[0]
		n, err := readIntoContext(ctx, r, block[1:])
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return nil, err
		}
		block = block[:n+1]
		var allocationErr error
		source.chunks, allocationErr = appendBudgeted(source.chunks, [][]byte{block}, budget, 24)
		if allocationErr != nil {
			return nil, allocationErr
		}
		source.size += int64(len(block))
		if err != nil {
			return source, nil
		}
	}
}

// reader 返回绑定单次操作上下文的顺序读取器
// 入参: ctx 上下文
// 返回: io.Reader 读取器
func (s *inputSource) reader(ctx context.Context) io.Reader {
	return io.NewSectionReader(contextReaderAt{ctx: ctx, source: s}, 0, s.size)
}

// readAt 读取指定区间，不越过输入边界
// 入参: ctx 上下文, data 目标缓冲, offset 相对输入起点的字节偏移
// 返回: int 已读字节数, error 错误信息
func (s *inputSource) readAt(ctx context.Context, data []byte, offset int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if offset < 0 {
		return 0, fmt.Errorf("j2kgo: negative input offset")
	}
	if len(data) == 0 {
		return 0, nil
	}
	if offset >= s.size {
		return 0, io.EOF
	}
	length := min(int64(len(data)), s.size-offset)
	if s.r != nil {
		n, err := s.r.ReadAt(data[:length], s.base+offset)
		if ctx.Err() != nil {
			return n, ctx.Err()
		}
		if err == nil && n != len(data) {
			err = io.EOF
		}
		return n, err
	}
	n := 0
	for int64(n) < length {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		position := offset + int64(n)
		chunk := s.chunks[position/int64(s.chunkSize)]
		n += copy(data[n:length], chunk[position%int64(s.chunkSize):])
	}
	if n != len(data) {
		return n, io.EOF
	}
	return n, nil
}

// ReadAt 使用绑定的上下文读取指定位置的数据
// 入参: data 目标缓冲, offset 相对输入起点的字节偏移
// 返回: int 已读字节数, error 错误信息
func (r contextReaderAt) ReadAt(data []byte, offset int64) (int, error) {
	return r.source.readAt(r.ctx, data, offset)
}

// readFullContext 读满指定缓冲区，返回取消或读取错误
// 入参: ctx 上下文, r 输入流, data 目标缓冲
// 返回: error 错误信息
func readFullContext(ctx context.Context, r io.Reader, data []byte) error {
	_, err := readIntoContext(ctx, r, data)
	return err
}

// readIntoContext 读满目标缓冲区，连续空读达到上限时返回错误
// 入参: ctx 上下文, r 输入流, data 目标缓冲
// 返回: int 已读字节数, error 错误信息
func readIntoContext(ctx context.Context, r io.Reader, data []byte) (int, error) {
	read, empty := 0, 0
	for read < len(data) {
		if err := ctx.Err(); err != nil {
			return read, err
		}
		n, err := r.Read(data[read:])
		if n < 0 || n > len(data)-read {
			return read, fmt.Errorf("j2kgo: invalid reader count")
		}
		read += n
		if read == len(data) {
			return read, ctx.Err()
		}
		if err != nil {
			if err == io.EOF && read > 0 {
				err = io.ErrUnexpectedEOF
			}
			return read, err
		}
		if n == 0 {
			empty++
			if empty == 100 {
				return read, io.ErrNoProgress
			}
		} else {
			empty = 0
		}
	}
	return read, ctx.Err()
}
