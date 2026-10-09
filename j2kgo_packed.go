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
	"encoding/binary"
	"io"
	"math"
)

// packedHeaders 按标记顺序读取集中包头，不复制压缩数据
type packedHeaders struct {
	ctx       context.Context
	source    *inputSource
	ranges    []inputRange
	index     int
	remaining int64
	current   packetByteReader
}

// orderedPacked 检查并按序排列集中包头标记
// 入参: packed 标记索引与区间, budget 元数据预算
// 返回: []inputRange 有序区间, error 错误信息
func orderedPacked(packed map[byte]inputRange, budget *layoutBudget) ([]inputRange, error) {
	for i := range len(packed) {
		if _, ok := packed[byte(i)]; !ok {
			return nil, FormatError("packed header marker sequence")
		}
	}
	if err := budget.add(uint64(len(packed)), 16); err != nil {
		return nil, err
	}
	result := make([]inputRange, len(packed))
	for i := range result {
		result[i] = packed[byte(i)]
	}
	return result, nil
}

// append 追加包头区间并预留索引内存
// 入参: ranges 包头区间, budget 元数据预算
// 返回: error 错误信息
func (r *packedHeaders) append(ranges []inputRange, budget *layoutBudget) error {
	var err error
	r.ranges, err = appendBudgeted(r.ranges, ranges, budget, 16)
	if err != nil {
		return err
	}
	for _, part := range ranges {
		r.remaining += part.length
	}
	return nil
}

// beginPacket 定位下一个集中存储的包头，单个包头不得跨越标记边界
// 返回: error 错误信息
func (r *packedHeaders) beginPacket() error {
	for r.current.position == r.current.end {
		if r.index == len(r.ranges) {
			return io.EOF
		}
		part := r.ranges[r.index]
		r.index++
		r.current = packetByteReader{ctx: r.ctx, source: r.source, position: part.offset, end: part.offset + part.length}
	}
	return nil
}

// ReadByte 顺序读取当前标记内的包头字节
// 返回: byte 字节, error 错误信息
func (r *packedHeaders) ReadByte() (byte, error) {
	if r.current.position == r.current.end {
		return 0, io.ErrUnexpectedEOF
	}
	value, err := r.current.ReadByte()
	if err == nil {
		r.remaining--
	}
	return value, err
}

// emptyPrefix 在指定数量内统计连续空包，不改变读取位置
// 入参: eph 是否包含EPH, limit 最多统计的空包数量
// 返回: int64 空包数量, error 错误信息
func (r packedHeaders) emptyPrefix(eph bool, limit int64) (int64, error) {
	count := int64(0)
	size := int64(1)
	if eph {
		size += 2
	}
	for count < limit && r.remaining >= size {
		if err := r.beginPacket(); err != nil {
			return 0, err
		}
		if r.current.end-r.current.position < size {
			break
		}
		value, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		if value&128 != 0 {
			break
		}
		if eph {
			first, err := r.ReadByte()
			if err != nil {
				return 0, err
			}
			second, err := r.ReadByte()
			if err != nil {
				return 0, err
			}
			if first != 255 || second != 0x92 {
				break
			}
		}
		count++
	}
	return count, r.ctx.Err()
}

// trailingEmptyPackets 根据集中包头和后续长度表确定分段末尾的零包体数据包数量
// 入参: lengths 未读长度表, future 后续瓦片分段, eph 是否包含EPH, budget 索引内存预算, probe 非空包头的预读回调，可为nil
// 返回: int64 零包体数据包数量, error 错误信息
func (r packedHeaders) trailingEmptyPackets(lengths packetLengthReader, future []*tilePart, eph bool, budget *layoutBudget, probe func(packedHeaders, []*tilePart, int64) (int64, error)) (int64, error) {
	count, err := lengths.leadingZeros(r.ctx, r.source)
	if err != nil || count == 0 {
		return count, err
	}
	boundary, shared := -1, int64(0)
	for i, part := range future {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		if len(part.plt) > 0 {
			next := packetLengthReader{ranges: part.plt, buffer: lengths.buffer}
			shared, err = next.leadingZeros(r.ctx, r.source)
			if err != nil {
				return 0, err
			}
			boundary = i
			break
		}
	}
	if boundary < 0 || shared == 0 {
		return count, r.ctx.Err()
	}
	target := count + min(shared, math.MaxInt64-count)
	empty, err := r.emptyPrefix(eph, target)
	if err != nil {
		return 0, err
	}
	size := int64(1)
	if eph {
		size += 2
	}
	complete := empty*size == r.remaining
	for _, part := range future[:boundary+1] {
		if empty == target || !complete {
			break
		}
		if len(part.packed) > 0 {
			ranges, err := orderedPacked(part.packed, budget)
			if err != nil {
				return 0, err
			}
			next := packedHeaders{ctx: r.ctx, source: r.source, ranges: ranges}
			for _, span := range ranges {
				next.remaining += span.length
			}
			additional, err := next.emptyPrefix(eph, target-empty)
			budget.used -= uint64(len(ranges)) * 16
			if err != nil {
				return 0, err
			}
			empty += additional
			complete = additional*size == next.remaining
		}
	}
	if empty < target && !complete && probe != nil {
		empty, err = probe(r, future[:boundary+1], target)
		if err != nil {
			return 0, err
		}
	}
	return min(count, max(0, empty-shared)), r.ctx.Err()
}

// indexPackedHeaders 按码流中的瓦片分段顺序拆分PPM包头
// 入参: ctx 上下文, source 输入源, budget 元数据预算
// 返回: error 错误信息
func (s *streamIndex) indexPackedHeaders(ctx context.Context, source *inputSource, budget *layoutBudget) error {
	if len(s.main.packed) == 0 {
		return nil
	}
	ranges, err := orderedPacked(s.main.packed, budget)
	if err != nil {
		return err
	}
	defer func() { budget.used -= uint64(len(ranges)) * 16 }()
	index, offset := 0, int64(0)
	for _, part := range s.parts {
		for index < len(ranges) && offset == ranges[index].length {
			index++
			offset = 0
		}
		if index == len(ranges) || ranges[index].length-offset < 4 {
			return FormatError("PPM tile-part length")
		}
		var data [4]byte
		if _, err := source.readAt(ctx, data[:], ranges[index].offset+offset); err != nil {
			return err
		}
		offset += 4
		remaining := int64(binary.BigEndian.Uint32(data[:]))
		for remaining > 0 {
			if index == len(ranges) {
				return FormatError("truncated PPM packet headers")
			}
			length := min(remaining, ranges[index].length-offset)
			if length > 0 {
				part.headers, err = appendBudgeted(part.headers, []inputRange{{offset: ranges[index].offset + offset, length: length}}, budget, 16)
				if err != nil {
					return err
				}
				offset += length
				remaining -= length
			}
			if offset == ranges[index].length {
				index++
				offset = 0
			}
		}
	}
	if index < len(ranges) && !(index == len(ranges)-1 && offset == ranges[index].length) {
		return FormatError("unconsumed PPM packet headers")
	}
	return nil
}
