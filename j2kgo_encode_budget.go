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
)

// tileLayerCost 保存瓦片每层的数据包数量及整个瓦片的ROI标记开销
type tileLayerCost struct {
	packets uint64
	roi     uint64
}

// prepareLayerBudgets 统计各瓦片的固定开销，供质量层码率分配使用
// 入参: ctx 上下文
// 返回: error 错误信息
func (p *encodingPlan) prepareLayerBudgets(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.tileRates || len(p.layers) == 0 || p.layers[0].BitsPerPixel == 0 {
		return nil
	}
	count := p.columns * p.rows
	memory := uint64(count)*16 + uint64(len(p.layers))*8
	if memory >= p.limits.MaxMemoryBytes {
		return &LimitError{Resource: "layer budget memory", Limit: p.limits.MaxMemoryBytes, Required: memory + 1}
	}
	costs := make([]tileLayerCost, count)
	totals := make([]uint64, len(p.layers))
	for index := range count {
		local := p.tilePlan(index)
		cost, err := local.tileLayerCost(ctx, index)
		if err != nil {
			return err
		}
		costs[index] = cost
		for layer := range p.layers {
			if err := ctx.Err(); err != nil {
				return err
			}
			bytes, err := emptyLayerCost(local, cost.packets, layer+1)
			if err != nil {
				return err
			}
			bytes, err = checkTotal("layer header bytes", bytes, cost.roi+local.parameterBytes, math.MaxUint64)
			if err != nil {
				return err
			}
			totals[layer], err = checkTotal("layer header bytes", totals[layer], bytes, math.MaxUint64)
			if err != nil {
				return err
			}
		}
	}
	p.tileCosts, p.layerCosts = costs, totals
	p.limits.MaxMemoryBytes -= memory
	return nil
}

// tileLayerCost 根据瓦片划分计算每层数据包数量及ROI标记的预留字节数
// 入参: ctx 上下文, index 瓦片索引
// 返回: tileLayerCost 数据包数量及ROI标记开销, error 错误信息
func (p encodingPlan) tileLayerCost(ctx context.Context, index int) (tileLayerCost, error) {
	var result tileLayerCost
	tile := p.tileBounds(index)
	for c, spec := range p.info.Components {
		if err := ctx.Err(); err != nil {
			return tileLayerCost{}, err
		}
		bounds := componentBounds(tile, spec)
		coding := p.componentCoding(c)
		for level, size := range coding.precincts {
			r := reduceBounds(bounds, coding.levels-level)
			if r.Empty() {
				continue
			}
			x := uint64(ceilDiv(r.Max.X, size.X) - r.Min.X/size.X)
			y := uint64(ceilDiv(r.Max.Y, size.Y) - r.Min.Y/size.Y)
			packets, err := checkedProduct("packet count", x, y, math.MaxUint64)
			if err != nil {
				return tileLayerCost{}, err
			}
			result.packets, err = checkTotal("packet count", result.packets, packets, math.MaxUint64)
			if err != nil {
				return tileLayerCost{}, err
			}
		}
		for _, area := range p.roi {
			if err := ctx.Err(); err != nil {
				return tileLayerCost{}, err
			}
			if componentBounds(area, spec).Overlaps(bounds) {
				result.roi += 7
				if len(p.info.Components) > 256 {
					result.roi++
				}
				break
			}
		}
	}
	return result, nil
}
