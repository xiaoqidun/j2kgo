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
	"errors"
	"slices"
)

// packetProbeState 保存预读前的区域状态，预读结束后恢复
type packetProbeState struct {
	bands []packetBand
	layer int
}

// zeroPacketPrefix 解析连续的零包体数据包，不保留对包头状态和内存预算的修改
// 入参: ctx 上下文, headers 当前集中包头, future 后续分段, layouts 分量分区, params 瓦片参数, limits 资源限制, budget 内存预算, limit 最多预读的数据包数
// 返回: int64 零包体数据包数, error 读取或解析错误
func zeroPacketPrefix(ctx context.Context, headers packedHeaders, future []*tilePart, layouts []componentLayout, params tileParameters, limits Limits, budget *layoutBudget, limit int64) (int64, error) {
	used := budget.used
	var states map[*precinctLayout]packetProbeState
	defer func() {
		for area, state := range states {
			area.bands, area.nextLayer = state.bands, state.layer
		}
		budget.used = used
	}()
	if err := budget.add(uint64(len(headers.ranges)), 16); err != nil {
		return 0, err
	}
	headers.ranges = slices.Clone(headers.ranges)
	for _, part := range future {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		ranges, err := orderedPacked(part.packed, budget)
		if err != nil {
			return 0, err
		}
		err = headers.append(ranges, budget)
		budget.used -= uint64(len(ranges)) * 16
		if err != nil {
			return 0, err
		}
	}
	count := int64(0)
	for _, volume := range params.volumes {
		err := walkPacketVolumeBudget(ctx, layouts, volume, limits, budget, func(address packetAddress) error {
			if count == limit || headers.remaining == 0 {
				return errTilePartEnd
			}
			if _, exists := states[address.area]; !exists {
				if err := budget.add(1, 256); err != nil {
					return err
				}
				bands, err := clonePacketProbeBands(ctx, address.area.bands, budget)
				if err != nil {
					return err
				}
				if states == nil {
					states = make(map[*precinctLayout]packetProbeState)
				}
				states[address.area] = packetProbeState{bands: address.area.bands, layer: address.area.nextLayer}
				address.area.bands = bands
			}
			if err := headers.beginPacket(); err != nil {
				return err
			}
			reader := packetBits{source: &headers}
			parts, err := readPacketHeader(ctx, &reader, address.area.bands, address.layer, params.styles[address.component].style)
			if err != nil {
				return err
			}
			defer releasePacketContributions(parts)
			if params.defaults.eph {
				for _, expected := range []byte{255, 0x92} {
					value, err := headers.ReadByte()
					if err != nil {
						return err
					}
					if value != expected {
						return FormatError("missing EPH")
					}
				}
			}
			for _, part := range parts {
				if part.length != 0 {
					return errTilePartEnd
				}
			}
			count++
			return nil
		})
		if errors.Is(err, errTilePartEnd) {
			return count, nil
		}
		if err != nil {
			return 0, err
		}
	}
	return count, ctx.Err()
}

// clonePacketProbeBands 复制包头解析会修改的码块和标签树状态，码字位置仍引用原数据
// 入参: ctx 上下文, bands 子带状态, budget 内存预算
// 返回: []packetBand 独立包头状态, error 资源或取消错误
func clonePacketProbeBands(ctx context.Context, bands []packetBand, budget *layoutBudget) ([]packetBand, error) {
	if err := budget.add(uint64(len(bands)), 128); err != nil {
		return nil, err
	}
	result := slices.Clone(bands)
	for i := range result {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		band := &result[i]
		if err := budget.add(uint64(len(band.blocks)), 128); err != nil {
			return nil, err
		}
		band.blocks = slices.Clone(band.blocks)
		for b := range band.blocks {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			block := &band.blocks[b]
			if err := budget.add(uint64(len(block.segments)), 40); err != nil {
				return nil, err
			}
			block.segments = slices.Clone(block.segments)
			block.budget = budget
		}
		for _, target := range []**tagTree{&band.inclusion, &band.zeroPlanes} {
			if *target == nil {
				continue
			}
			if err := budget.add(uint64(len((*target).nodes))+1, 32); err != nil {
				return nil, err
			}
			*target = &tagTree{nodes: slices.Clone((*target).nodes), leaves: (*target).leaves}
		}
	}
	return result, nil
}
