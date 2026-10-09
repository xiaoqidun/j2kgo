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
	"io"
	"math"
)

// packetLengthTable 保存数据包长度标记的编号及输入区间
type packetLengthTable map[byte]inputRange

// packetLengthReader 按区间顺序读取数据包长度，不复制原始长度表
type packetLengthReader struct {
	ranges        []inputRange
	index         int
	position, end int64
	buffer        *packetLengthBuffer
}

// packetLengthBuffer 缓存长度表的连续字节，允许读取器副本复用
type packetLengthBuffer struct {
	data   [256]byte
	offset int64
	count  int
}

// add 检查标记编号并保存不含编号的参数区间
// 入参: segment 标记段, budget 索引预算
// 返回: error 错误信息
func (t *packetLengthTable) add(segment markerSegment, budget *layoutBudget) error {
	if len(segment.data) < 2 {
		return FormatError("packet length marker size")
	}
	index := segment.data[0]
	if _, exists := (*t)[index]; exists {
		return FormatError("duplicate packet length marker index")
	}
	if err := budget.add(1, 128); err != nil {
		return err
	}
	if *t == nil {
		*t = make(packetLengthTable)
	}
	(*t)[index] = inputRange{offset: segment.offset + 5, length: int64(len(segment.data) - 1)}
	return nil
}

// ordered 按标记编号排列数据区间，分配前检查峰值内存
// 入参: budget 索引预算
// 返回: []inputRange 有序区间, error 错误信息
func (t packetLengthTable) ordered(budget *layoutBudget) ([]inputRange, error) {
	if err := budget.add(uint64(len(t)), 16); err != nil {
		return nil, err
	}
	ranges := make([]inputRange, 0, len(t))
	for index := range 256 {
		if span, ok := t[byte(index)]; ok {
			ranges = append(ranges, span)
		}
	}
	return ranges, nil
}

// advance 跳过已读完的区间，保留当前标记内的读取边界
// 返回: bool 是否仍有数据
func (r *packetLengthReader) advance() bool {
	for r.position == r.end {
		if r.index == len(r.ranges) {
			return false
		}
		span := r.ranges[r.index]
		r.index++
		r.position, r.end = span.offset, span.offset+span.length
	}
	return true
}

// empty 判断所有区间是否均已读完
// 返回: bool 是否读完
func (r *packetLengthReader) empty() bool {
	return r.index == len(r.ranges) && r.position == r.end
}

// leadingZeros 统计当前位置起连续的零长度数据包，不改变读取位置
// 入参: ctx 上下文, source 输入源
// 返回: int64 零长度数据包数量, error 错误信息
func (r packetLengthReader) leadingZeros(ctx context.Context, source *inputSource) (int64, error) {
	count := int64(0)
	for !r.empty() {
		length, err := r.next(ctx, source)
		if err != nil {
			return 0, err
		}
		if length != 0 {
			break
		}
		count++
	}
	return count, ctx.Err()
}

// readByte 从当前区间读取字节，缓存中没有所需数据时才访问输入源
// 入参: ctx 上下文, source 输入源
// 返回: byte 字节, error 错误信息
func (r *packetLengthReader) readByte(ctx context.Context, source *inputSource) (byte, error) {
	if r.position == r.end {
		return 0, io.EOF
	}
	if r.buffer == nil {
		r.buffer = &packetLengthBuffer{}
	}
	b := r.buffer
	if r.position < b.offset || r.position-b.offset >= int64(b.count) {
		length := int(min(int64(len(b.data)), r.end-r.position))
		count, err := source.readAt(ctx, b.data[:length], r.position)
		if err != nil {
			return 0, err
		}
		b.offset, b.count = r.position, count
	}
	value := b.data[r.position-b.offset]
	r.position++
	return value, nil
}

// next 读取按七位分组编码的数据包长度，不允许跨越标记边界
// 入参: ctx 上下文, source 输入源
// 返回: int64 数据包字节数, error 错误信息
func (r *packetLengthReader) next(ctx context.Context, source *inputSource) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !r.advance() {
		return 0, io.EOF
	}
	var length int64
	for {
		value, err := r.readByte(ctx, source)
		if err == io.EOF {
			return 0, FormatError("unterminated packet length")
		}
		if err != nil {
			return 0, err
		}
		if length > (math.MaxInt64-int64(value&127))/128 {
			return 0, FormatError("packet length overflow")
		}
		length = length*128 + int64(value&127)
		if value&128 == 0 {
			return length, nil
		}
	}
}

// validatePacketLengthRange 检查长度表中各整数的完整性及取值范围
// 入参: ctx 上下文, source 输入源, span 长度列表区间, buffer 复用缓存
// 返回: error 错误信息
func validatePacketLengthRange(ctx context.Context, source *inputSource, span inputRange, buffer *packetLengthBuffer) error {
	r := packetLengthReader{position: span.offset, end: span.offset + span.length, buffer: buffer}
	for !r.empty() {
		if _, err := r.next(ctx, source); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// indexPacketLengths 校验PLM和PLT长度表，并建立可重复读取的区间索引
// 入参: ctx 上下文, source 输入源, budget 索引预算
// 返回: error 错误信息
func (s *streamIndex) indexPacketLengths(ctx context.Context, source *inputSource, budget *layoutBudget) error {
	present := len(s.packetLengths) != 0
	for _, part := range s.parts {
		present = present || len(part.lengths) != 0
	}
	if !present {
		return ctx.Err()
	}
	if err := budget.add(1, 512); err != nil {
		return err
	}
	defer func() { budget.used -= 512 }()
	buffer := &packetLengthBuffer{}
	if len(s.packetLengths) != 0 {
		if err := s.indexMainPacketLengths(ctx, source, budget, buffer); err != nil {
			return err
		}
	}
	for _, part := range s.parts {
		if len(part.lengths) == 0 {
			continue
		}
		ranges, err := part.lengths.ordered(budget)
		if err != nil {
			return err
		}
		for _, span := range ranges {
			if err := validatePacketLengthRange(ctx, source, span, buffer); err != nil {
				return err
			}
		}
		part.plt = ranges
		budget.used -= uint64(len(part.lengths)) * 128
		part.lengths = nil
	}
	return ctx.Err()
}

// indexMainPacketLengths 按瓦片分段顺序拆分PLM，每组包长编码最多255字节
// 入参: ctx 上下文, source 输入源, budget 索引预算, buffer 复用缓存
// 返回: error 错误信息
func (s *streamIndex) indexMainPacketLengths(ctx context.Context, source *inputSource, budget *layoutBudget, buffer *packetLengthBuffer) error {
	ranges, err := s.packetLengths.ordered(budget)
	if err != nil {
		return err
	}
	defer func() { budget.used -= uint64(len(ranges)) * 16 }()
	r := packetLengthReader{ranges: ranges, buffer: buffer}
	for _, part := range s.parts {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !r.advance() {
			return FormatError("missing PLM tile-part")
		}
		count, err := r.readByte(ctx, source)
		if err != nil {
			return err
		}
		remaining := int64(count)
		if remaining > 0 && r.position == r.end {
			return FormatError("PLM marker ends before first packet length")
		}
		part.plm = []inputRange{}
		for remaining > 0 {
			if !r.advance() {
				return FormatError("truncated PLM tile-part")
			}
			length := min(remaining, r.end-r.position)
			span := inputRange{offset: r.position, length: length}
			if err := validatePacketLengthRange(ctx, source, span, buffer); err != nil {
				return err
			}
			part.plm, err = appendBudgeted(part.plm, []inputRange{span}, budget, 16)
			if err != nil {
				return err
			}
			remaining -= length
			r.position += length
		}
	}
	if r.advance() {
		return FormatError("extra PLM tile-part")
	}
	budget.used -= uint64(len(s.packetLengths)) * 128
	s.packetLengths = nil
	return ctx.Err()
}

// mergePacketLengths 合并前后PLT长度表，重叠的数据包长度须一致
// 入参: ctx 上下文, source 输入源, previous 前一长度表的未读部分, current 当前长度表
// 返回: packetLengthReader 合并后的读取器, error 错误信息
func mergePacketLengths(ctx context.Context, source *inputSource, previous, current packetLengthReader) (packetLengthReader, error) {
	a, b := previous, current
	for !a.empty() && !b.empty() {
		x, err := a.next(ctx, source)
		if err != nil {
			return packetLengthReader{}, err
		}
		y, err := b.next(ctx, source)
		if err != nil {
			return packetLengthReader{}, err
		}
		if x != y {
			return packetLengthReader{}, FormatError("overlapping PLT lengths disagree")
		}
	}
	if a.empty() {
		return current, ctx.Err()
	}
	return previous, ctx.Err()
}

// checkPacketLengths 核对数据包长度，尾包截断时允许长度表保留原始长度
// 入参: ctx 上下文, source 输入源, main PLM读取器, tile PLT读取器, required 是否必须有PLM条目, actual 实际包长, declared 根据包头计算的包长，负值表示包头被截断, optionalSOP 是否允许长度表计入已截去的SOP
// 返回: error 错误信息
func checkPacketLengths(ctx context.Context, source *inputSource, main, tile *packetLengthReader, required bool, actual, declared int64, optionalSOP bool) error {
	for i, r := range []*packetLengthReader{main, tile} {
		if r.empty() && !(i == 0 && required) {
			continue
		}
		length, err := r.next(ctx, source)
		if err == io.EOF {
			return FormatError("missing PLM packet length")
		}
		if err != nil {
			return err
		}
		if length != actual && !(declared >= actual && length == declared) && !(declared < 0 && length >= actual) && !(optionalSOP && declared <= math.MaxInt64-6 && length == declared+6) {
			return FormatError("packet length disagrees with packet header")
		}
	}
	return ctx.Err()
}
