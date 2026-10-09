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
	"math"
	"math/bits"
)

// walkPackets 重置跨层状态并按渐进顺序访问数据包，不复制码字
// 入参: ctx 上下文, plan 编码参数, visit 数据包回调
// 返回: error 错误信息
func (t *tileEncoding) walkPackets(ctx context.Context, plan encodingPlan, visit func(packetAddress) error) error {
	for _, layout := range t.layouts {
		for _, resolution := range layout.resolutions {
			for p := range resolution.precincts {
				if err := ctx.Err(); err != nil {
					return err
				}
				precinct := &resolution.precincts[p]
				precinct.nextLayer = 0
				for _, band := range precinct.bands {
					if len(band.blocks) == 0 {
						continue
					}
					initializeTagTree(band.inclusion, func(i int) uint32 {
						block := &band.blocks[i]
						block.passes, block.lengthBits = 0, 3
						if layers, ok := t.layers[block]; ok {
							for layer, point := range layers {
								if point.passes > 0 {
									return uint32(layer)
								}
							}
							return uint32(plan.layerCount)
						}
						if t.blocks[block].bitPlanes == 0 {
							return uint32(plan.layerCount)
						}
						return 0
					})
					initializeTagTree(band.zeroPlanes, func(i int) uint32 {
						return uint32(band.maxPlanes - t.blocks[&band.blocks[i]].bitPlanes)
					})
				}
			}
		}
	}
	volume := progressionVolume{order: plan.progression, componentEnd: len(t.layouts), resolutionEnd: plan.maxLevels() + 1, layerEnd: plan.layerCount}
	if len(plan.volumes) == 0 {
		return walkPacketVolumeBudget(ctx, t.layouts, volume, plan.limits, &t.budget, visit)
	}
	for _, volume := range plan.volumes {
		volume.layerEnd = min(volume.layerEnd, plan.layerCount)
		if err := walkPacketVolumeBudget(ctx, t.layouts, volume, plan.limits, &t.budget, visit); err != nil {
			return err
		}
	}
	return nil
}

// measurePackets 统计数据包长度，通过回调提供各包及包头的字节数
// 包长包含已启用的SOP和EPH，包头长度包含已启用的EPH但不包含SOP
// 入参: ctx 上下文, plan 编码参数, visit 长度回调，nil时仅统计
// 返回: uint64 数据包总字节数, int 最大包头字节数, error 错误信息
func (t *tileEncoding) measurePackets(ctx context.Context, plan encodingPlan, visit func(uint64, int) error) (uint64, int, error) {
	var length uint64
	maximum := 0
	err := t.walkPackets(ctx, plan, func(address packetAddress) error {
		writer := packetWriter{countOnly: true}
		if err := t.packetHeader(&writer, address.area, address.layer); err != nil {
			return err
		}
		headerLength := writer.length
		if plan.eph {
			headerLength += 2
		}
		packetLength := uint64(headerLength)
		if plan.sop {
			packetLength += 6
		}
		maximum = max(maximum, headerLength)
		for _, band := range address.area.bands {
			for i := range band.blocks {
				if err := t.contribution(&band.blocks[i], address.layer, func(segment blockSegment) error {
					packetLength += uint64(len(segment.data))
					return nil
				}); err != nil {
					return err
				}
			}
		}
		length += packetLength
		if visit != nil {
			return visit(packetLength, headerLength)
		}
		return nil
	})
	return length, maximum, err
}

// initializeTagTree 设置标签树的叶节点值，并逐层计算父节点最小值
// 入参: tree 标签树, value 叶节点取值
func initializeTagTree(tree *tagTree, value func(int) uint32) {
	for i := range tree.nodes {
		node := &tree.nodes[i]
		node.value, node.low, node.known = math.MaxUint32, 0, false
		if i < tree.leaves {
			node.value = value(i)
		}
	}
	for i := range tree.nodes {
		node := &tree.nodes[i]
		if node.parent >= 0 {
			tree.nodes[node.parent].value = min(tree.nodes[node.parent].value, node.value)
		}
	}
}

// packetHeader 写入当前质量层的数据包头，并更新码块及标签树状态
// 入参: writer 包头写入器, area 数据包区域, layer 质量层索引
// 返回: error 错误信息
func (t *tileEncoding) packetHeader(writer *packetWriter, area *precinctLayout, layer int) error {
	present := uint8(0)
	for _, band := range area.bands {
		for i := range band.blocks {
			previous, current := t.layerPoints(&band.blocks[i], layer)
			if current.passes > previous.passes {
				present = 1
			}
		}
	}
	writer.bit(present)
	if present == 0 {
		writer.align()
		return nil
	}
	for _, band := range area.bands {
		for i := range band.blocks {
			state := &band.blocks[i]
			block := t.blocks[state]
			previous, current := t.layerPoints(state, layer)
			passes := current.passes - previous.passes
			if state.passes == 0 {
				if err := band.inclusion.encode(writer, i, uint32(layer+1)); err != nil {
					return err
				}
			} else if passes > 0 {
				writer.bit(1)
			} else {
				writer.bit(0)
			}
			if passes == 0 {
				continue
			}
			if state.passes == 0 {
				if err := band.zeroPlanes.encode(writer, i, uint32(band.maxPlanes-block.bitPlanes+1)); err != nil {
					return err
				}
			}
			lengthBits := state.lengthBits
			if err := t.contribution(state, layer, func(segment blockSegment) error {
				lengthBits = max(lengthBits, bits.Len(uint(len(segment.data)))-bits.Len(uint(segment.passes))+1)
				return nil
			}); err != nil {
				return err
			}
			if err := writer.writePassCount(passes); err != nil {
				return err
			}
			for n := state.lengthBits; n < lengthBits; n++ {
				writer.bit(1)
			}
			writer.bit(0)
			if err := t.contribution(state, layer, func(segment blockSegment) error {
				return writer.bits(uint64(len(segment.data)), lengthBits+bits.Len(uint(segment.passes))-1)
			}); err != nil {
				return err
			}
			state.passes, state.lengthBits = current.passes, lengthBits
		}
	}
	writer.align()
	return nil
}

// layerPoints 返回码块在上一质量层和当前质量层的截断点
// 入参: block 码块, layer 质量层索引
// 返回: ratePoint 上一层截断点, ratePoint 当前层截断点
func (t *tileEncoding) layerPoints(block *packetBlock, layer int) (ratePoint, ratePoint) {
	var previous, current ratePoint
	if layers, ok := t.layers[block]; ok {
		if layer > 0 {
			previous = layers[layer-1]
		}
		return previous, layers[layer]
	}
	for _, segment := range t.blocks[block].segments {
		current.passes += segment.passes
		current.rate += len(segment.data)
	}
	if layer > 0 {
		previous = current
	}
	return previous, current
}

// contribution 逐段访问当前质量层新增的码字，不复制数据
// 入参: block 码块, layer 质量层索引, visit 码段回调
// 返回: error 错误信息
func (t *tileEncoding) contribution(block *packetBlock, layer int, visit func(blockSegment) error) error {
	previous, current := t.layerPoints(block, layer)
	if previous.passes == current.passes {
		return nil
	}
	passOffset, byteOffset := 0, 0
	for _, segment := range t.blocks[block].segments {
		start, end := max(previous.passes, passOffset), min(current.passes, passOffset+segment.passes)
		if end > start {
			lo, hi := max(0, previous.rate-byteOffset), min(len(segment.data), current.rate-byteOffset)
			if lo < 0 || hi < lo || hi > len(segment.data) {
				return FormatError("encoded layer segment range")
			}
			if err := visit(blockSegment{passes: end - start, data: segment.data[lo:hi]}); err != nil {
				return err
			}
		}
		passOffset += segment.passes
		byteOffset += len(segment.data)
		if passOffset >= current.passes {
			break
		}
	}
	return nil
}
