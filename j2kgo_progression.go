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
	"cmp"
	"context"
	"slices"
)

// progressionVolume 描述渐进顺序及其覆盖的分量、分辨率和质量层范围
type progressionVolume struct {
	order                          Progression
	componentStart, componentEnd   int
	resolutionStart, resolutionEnd int
	layerEnd                       int
}

// packetAddress 标识一个数据包所属的区域与质量层
type packetAddress struct {
	component, resolution, precinct, layer int
	area                                   *precinctLayout
}

// walkPacketVolumeBudget 按渐进顺序访问数据包，排序索引与包头状态共用内存预算
// 入参: ctx 上下文, components 分量分区, volume 渐进范围, limits 资源限制, budget 累计预算, visit 数据包回调
// 返回: error 错误信息
func walkPacketVolumeBudget(ctx context.Context, components []componentLayout, volume progressionVolume, limits Limits, budget *layoutBudget, visit func(packetAddress) error) error {
	if volume.order > CPRL || volume.componentStart < 0 || volume.componentEnd > len(components) || volume.componentStart >= volume.componentEnd || volume.resolutionStart < 0 || volume.resolutionEnd > 33 || volume.resolutionStart >= volume.resolutionEnd || volume.layerEnd < 1 || volume.layerEnd > 65535 || visit == nil {
		return FormatError("progression volume")
	}
	limits = limits.normalized()
	var count uint64
	for c := volume.componentStart; c < volume.componentEnd; c++ {
		for r := volume.resolutionStart; r < min(volume.resolutionEnd, len(components[c].resolutions)); r++ {
			var err error
			count, err = checkTotal("packet index", count, uint64(len(components[c].resolutions[r].precincts)), min(limits.MaxMemoryBytes/40, uint64(^uint(0)>>1)))
			if err != nil {
				return err
			}
		}
	}
	if budget != nil {
		if err := budget.add(count, 40); err != nil {
			return err
		}
		defer func() { budget.used -= count * 40 }()
	}
	entries := make([]packetAddress, 0, int(count))
	for c := volume.componentStart; c < volume.componentEnd; c++ {
		for r := volume.resolutionStart; r < min(volume.resolutionEnd, len(components[c].resolutions)); r++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			for p := range components[c].resolutions[r].precincts {
				area := &components[c].resolutions[r].precincts[p]
				entries = append(entries, packetAddress{component: c, resolution: r, precinct: p, area: area})
			}
		}
	}
	slices.SortFunc(entries, func(a, b packetAddress) int {
		return comparePacketAddress(a, b, volume.order)
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	if volume.order == LRCP {
		for layer := 0; layer < volume.layerEnd; layer++ {
			for _, entry := range entries {
				if err := visitPacketLayer(ctx, entry, layer, visit); err != nil {
					return err
				}
			}
		}
	} else if volume.order == RLCP {
		for first := 0; first < len(entries); {
			end := first + 1
			for end < len(entries) && entries[end].resolution == entries[first].resolution {
				end++
			}
			for layer := 0; layer < volume.layerEnd; layer++ {
				for _, entry := range entries[first:end] {
					if err := visitPacketLayer(ctx, entry, layer, visit); err != nil {
						return err
					}
				}
			}
			first = end
		}
	} else {
		for _, entry := range entries {
			for layer := entry.area.nextLayer; layer < volume.layerEnd; layer++ {
				if err := visitPacketLayer(ctx, entry, layer, visit); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// visitPacketLayer 访问指定质量层的数据包，跳过已访问的层
// 入参: ctx 上下文, entry 数据包区域, layer 质量层, visit 回调
// 返回: error 错误信息
func visitPacketLayer(ctx context.Context, entry packetAddress, layer int, visit func(packetAddress) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if entry.area.nextLayer > layer {
		return nil
	}
	if entry.area.nextLayer != layer {
		return FormatError("packet layer gap")
	}
	entry.layer = layer
	if err := visit(entry); err != nil {
		return err
	}
	entry.area.nextLayer++
	return nil
}

// comparePacketAddress 比较同一渐进顺序内的区域次序
// 入参: a 第一区域, b 第二区域, order 渐进顺序
// 返回: int 排序结果
func comparePacketAddress(a, b packetAddress, order Progression) int {
	position := func() int {
		return cmp.Or(cmp.Compare(a.area.position.Y, b.area.position.Y), cmp.Compare(a.area.position.X, b.area.position.X))
	}
	switch order {
	case LRCP, RLCP:
		return cmp.Or(cmp.Compare(a.resolution, b.resolution), cmp.Compare(a.component, b.component), cmp.Compare(a.precinct, b.precinct))
	case RPCL:
		return cmp.Or(cmp.Compare(a.resolution, b.resolution), position(), cmp.Compare(a.component, b.component), cmp.Compare(a.precinct, b.precinct))
	case PCRL:
		return cmp.Or(position(), cmp.Compare(a.component, b.component), cmp.Compare(a.resolution, b.resolution), cmp.Compare(a.precinct, b.precinct))
	default:
		return cmp.Or(cmp.Compare(a.component, b.component), position(), cmp.Compare(a.resolution, b.resolution), cmp.Compare(a.precinct, b.precinct))
	}
}
