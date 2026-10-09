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
	"image"
	"math"
	"math/bits"
)

// inputRange 记录输入数据的起始偏移和长度
type inputRange struct {
	offset int64
	length int64
}

// packetSegment 保存跨质量层的同一编码段
type packetSegment struct {
	passes   int
	retained int
	parts    []inputRange
}

// packetBlock 保存码块的数据包状态及输入位置
type packetBlock struct {
	bounds     image.Rectangle
	bitPlanes  int
	lengthBits int
	passes     int
	segments   []packetSegment
	discard    bool
	budget     *layoutBudget
}

// packetBand 保存区域内单个子带的码块及标签树
type packetBand struct {
	orientation bandOrientation
	bounds      image.Rectangle
	maxPlanes   int
	blocks      []packetBlock
	inclusion   *tagTree
	zeroPlanes  *tagTree
}

// packetContribution 描述当前数据包中一个编码段的新增数据
type packetContribution struct {
	block   *packetBlock
	segment int
	length  uint64
}

// readPacketHeader 解析数据包头，码字仍留在输入源中
// 入参: ctx 上下文, r 位流, bands 子带, layer 质量层, style 码块方式
// 返回: []packetContribution 各编码段的新增数据描述, error 错误信息
func readPacketHeader(ctx context.Context, r *packetBits, bands []packetBand, layer int, style CodeBlockStyle) ([]packetContribution, error) {
	if layer < 0 || layer >= 65535 {
		return nil, FormatError("packet layer index")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	present, err := r.bit()
	if err != nil {
		return nil, err
	}
	var result []packetContribution
	success := false
	defer func() {
		if !success {
			releasePacketContributions(result)
		}
	}()
	if present != 0 {
		for i := range bands {
			band := &bands[i]
			for j := range band.blocks {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				block := &band.blocks[j]
				included := false
				if block.passes == 0 {
					included, err = band.inclusion.decode(r, j, uint32(layer+1))
				} else {
					var value uint8
					value, err = r.bit()
					included = value != 0
				}
				if err != nil {
					return nil, err
				}
				if !included {
					continue
				}
				if block.passes == 0 {
					if err := readBlockPlanes(r, band, j); err != nil {
						return nil, err
					}
				}
				parts, err := readBlockContribution(ctx, r, block, style)
				if err != nil {
					return nil, err
				}
				result, err = appendBudgeted(result, parts, block.budget, 24)
				releasePacketContributions(parts)
				if err != nil {
					return nil, err
				}
			}
		}
	}
	if err := r.align(); err != nil {
		return nil, err
	}
	success = true
	return result, nil
}

// readBlockPlanes 读取首次出现的码块位平面数
// 入参: r 位流, band 子带, index 码块序号
// 返回: error 错误信息
func readBlockPlanes(r *packetBits, band *packetBand, index int) error {
	if band.maxPlanes < 1 || band.maxPlanes > 292 {
		return UnsupportedError("code-block coefficient precision")
	}
	for missing := 0; missing < band.maxPlanes; missing++ {
		known, err := band.zeroPlanes.decode(r, index, uint32(missing+1))
		if err != nil {
			return err
		}
		if known {
			band.blocks[index].bitPlanes = band.maxPlanes - missing
			band.blocks[index].lengthBits = 3
			return nil
		}
	}
	return FormatError("code-block has no coded bitplanes")
}

// readBlockContribution 读取码块新增的编码遍数及各编码段的数据长度
// 入参: ctx 上下文, r 位流, block 码块, style 码块方式
// 返回: []packetContribution 各编码段的新增数据描述, error 错误信息
func readBlockContribution(ctx context.Context, r *packetBits, block *packetBlock, style CodeBlockStyle) ([]packetContribution, error) {
	count, err := r.readPassCount()
	if err != nil {
		return nil, err
	}
	total := 3*block.bitPlanes - 2
	if count > total-block.passes {
		return nil, FormatError("packet coding passes exceed bitplanes")
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		v, err := r.bit()
		if err != nil {
			return nil, err
		}
		if v == 0 {
			break
		}
		block.lengthBits++
		if block.lengthBits > math.MaxInt-8 {
			return nil, FormatError("packet codeword length overflow")
		}
	}
	var result []packetContribution
	success := false
	defer func() {
		if !success {
			releasePacketContributions(result)
		}
	}()
	for count > 0 {
		first, used := block.passes, 0
		if len(block.segments) > 0 {
			used = block.segments[len(block.segments)-1].passes
			first -= used
		}
		capacity := blockSegmentPasses(first, total, style)
		if len(block.segments) == 0 || used == capacity {
			block.segments, err = appendBudgeted(block.segments, []packetSegment{{}}, block.budget, 40)
			if err != nil {
				return nil, err
			}
			used = 0
			capacity = blockSegmentPasses(block.passes, total, style)
		}
		added := min(count, capacity-used)
		length, err := readCodewordLength(ctx, r, block.lengthBits+bits.Len(uint(added))-1)
		if err != nil {
			return nil, err
		}
		segment := len(block.segments) - 1
		block.segments[segment].passes += added
		block.passes += added
		count -= added
		result, err = appendBudgeted(result, []packetContribution{{block: block, segment: segment, length: length}}, block.budget, 24)
		if err != nil {
			return nil, err
		}
	}
	success = true
	return result, nil
}

// releasePacketContributions 归还当前数据包的临时码字索引预算
// 入参: parts 当前数据包的编码段描述
func releasePacketContributions(parts []packetContribution) {
	if len(parts) > 0 && parts[0].block.budget != nil {
		parts[0].block.budget.used -= uint64(cap(parts)) * 24
	}
}

// readCodewordLength 读取码字长度，允许前导零并检查数值是否超出输入地址范围
// 入参: ctx 上下文, r 位流, width 字段位数
// 返回: uint64 字节数, error 错误信息
func readCodewordLength(ctx context.Context, r *packetBits, width int) (uint64, error) {
	for width > 63 {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		value, err := r.bit()
		if err != nil {
			return 0, err
		}
		if value != 0 {
			return 0, &LimitError{Resource: "codeword length", Limit: math.MaxInt64, Required: math.MaxUint64}
		}
		width--
	}
	return r.bits(width)
}

// locatePacketPrefix 记录码字位置，尾部截断时保留此前的完整编码遍
// 入参: parts 编码段描述, offset 包体起点, end 包体上限, retain 是否保留码字索引, truncated 是否允许最终瓦片分段的尾部截断
// 返回: int64 包体结束位置, error 错误信息
func locatePacketPrefix(parts []packetContribution, offset, end int64, retain, truncated bool) (int64, error) {
	if offset < 0 || end < offset {
		return 0, FormatError("packet body extent")
	}
	complete := true
	for _, part := range parts {
		if !complete || part.length > uint64(end-offset) {
			if !truncated {
				return 0, FormatError("packet codeword exceeds body")
			}
			complete, offset = false, end
			continue
		}
		segment := &part.block.segments[part.segment]
		if retain && !part.block.discard {
			segment.retained = segment.passes
			if part.length > 0 {
				var err error
				segment.parts, err = appendBudgeted(segment.parts, []inputRange{{offset: offset, length: int64(part.length)}}, part.block.budget, 16)
				if err != nil {
					return 0, err
				}
			}
		}
		offset += int64(part.length)
	}
	return offset, nil
}

// loadPacketBlock 合并相邻码字读取，各编码段共用精确分配的缓冲
// 入参: ctx 上下文, source 输入源, block 码块, limits 资源限制
// 返回: encodedBlock 已编码的码块, error 错误信息
func loadPacketBlock(ctx context.Context, source *inputSource, block *packetBlock, limits Limits) (encodedBlock, error) {
	if err := ctx.Err(); err != nil {
		return encodedBlock{}, err
	}
	limits = limits.normalized()
	count := 0
	for count < len(block.segments) && block.segments[count].retained > 0 {
		count++
	}
	memory, err := checkedProduct("codeword memory", uint64(count), 32, limits.MaxMemoryBytes)
	if err != nil {
		return encodedBlock{}, err
	}
	var length uint64
	for _, segment := range block.segments[:count] {
		if err := ctx.Err(); err != nil {
			return encodedBlock{}, err
		}
		for i, part := range segment.parts {
			if i&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return encodedBlock{}, err
				}
			}
			if part.offset < 0 || part.length < 0 || part.offset > source.size || part.length > source.size-part.offset {
				return encodedBlock{}, FormatError("codeword input extent")
			}
			var err error
			length, err = checkTotal("codeword memory", length, uint64(part.length), min(limits.MaxMemoryBytes, uint64(^uint(0)>>1)))
			if err != nil {
				return encodedBlock{}, err
			}
			memory, err = checkTotal("codeword memory", memory, uint64(part.length), limits.MaxMemoryBytes)
			if err != nil {
				return encodedBlock{}, err
			}
		}
	}
	result := encodedBlock{bitPlanes: block.bitPlanes, segments: make([]blockSegment, count)}
	data := make([]byte, int(length))
	pos := 0
	for i, segment := range block.segments[:count] {
		if err := ctx.Err(); err != nil {
			return encodedBlock{}, err
		}
		start := pos
		for part := 0; part < len(segment.parts); {
			if err := ctx.Err(); err != nil {
				return encodedBlock{}, err
			}
			span := segment.parts[part]
			part++
			for part < len(segment.parts) && segment.parts[part].offset == span.offset+span.length {
				span.length += segment.parts[part].length
				part++
				if part&1023 == 0 {
					if err := ctx.Err(); err != nil {
						return encodedBlock{}, err
					}
				}
			}
			if _, err := source.readAt(ctx, data[pos:pos+int(span.length)], span.offset); err != nil {
				return encodedBlock{}, err
			}
			pos += int(span.length)
		}
		result.segments[i] = blockSegment{data: data[start:pos:pos], passes: segment.retained}
	}
	return result, ctx.Err()
}
