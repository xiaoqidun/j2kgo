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
)

// setROI 裁剪感兴趣区域，并按码块所需编码遍数确定默认质量层数
// 入参: regions 参考网格区域
// 返回: error 错误信息
func (p *encodingPlan) setROI(regions []image.Rectangle) error {
	count := 0
	for _, region := range regions {
		if region.Overlaps(p.info.Bounds) {
			count++
		}
	}
	if count == 0 {
		return nil
	}
	memory, err := checkedProduct("ROI region memory", uint64(count), 32, p.limits.MaxMemoryBytes)
	if err != nil {
		return err
	}
	if memory == p.limits.MaxMemoryBytes {
		return &LimitError{Resource: "ROI region memory", Limit: p.limits.MaxMemoryBytes, Required: memory + 1}
	}
	p.limits.MaxMemoryBytes -= memory
	p.roi = make([]image.Rectangle, 0, count)
	for _, region := range regions {
		if region.Overlaps(p.info.Bounds) {
			p.roi = append(p.roi, region.Intersect(p.info.Bounds))
		}
	}
	if len(p.layers) == 0 {
		for c := range p.info.Components {
			quant := p.componentQuantization(c)
			for _, step := range quant.steps {
				planes := quant.guard + step.exponent - 1
				p.layerCount = max(p.layerCount, ceilDiv(max(0, 6*planes-2), 164))
			}
		}
	}
	return nil
}

// makeROIMask 将参考网格区域映射到小波系数，包含逆变换依赖的邻域
// 入参: ctx 上下文, layout 分量分区, info 分量信息, regions 优先区域, budget 内存预算
// 返回: []bool 与变换缓冲顺序一致的掩码，nil表示无相交样本, error 错误信息
func makeROIMask(ctx context.Context, layout *componentLayout, info ComponentInfo, regions []image.Rectangle, budget *layoutBudget) ([]bool, error) {
	var mask []bool
	for _, area := range regions {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		region := componentBounds(area, info).Intersect(layout.bounds)
		if region.Empty() {
			continue
		}
		if mask == nil {
			count := uint64(layout.bounds.Dx()) * uint64(layout.bounds.Dy())
			if err := budget.add(count, 1); err != nil {
				return nil, err
			}
			mask = make([]bool, int(count))
		}
		for level := layout.coding.levels; level >= 0; level-- {
			shift := layout.coding.levels - level
			window := region
			first, last := bandLL, bandLL
			if level > 0 {
				window = waveletWindow(region, reduceBounds(layout.bounds, shift), layout.coding.reversible)
				first, last = bandHL, bandHH
			}
			low := reduceBounds(layout.bounds, shift+1)
			for orientation := first; orientation <= last; orientation++ {
				bounds := subbandBounds(layout.bounds, shift, orientation)
				selection := window
				if level > 0 {
					selection = subbandBounds(window, 0, orientation)
				}
				selection = selection.Intersect(bounds)
				xOffset, yOffset := 0, 0
				if orientation&1 != 0 {
					xOffset = low.Dx()
				}
				if orientation&2 != 0 {
					yOffset = low.Dy()
				}
				for y := selection.Min.Y; y < selection.Max.Y; y++ {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
					start := (y-bounds.Min.Y+yOffset)*layout.bounds.Dx() + selection.Min.X - bounds.Min.X + xOffset
					for i := start; i < start+selection.Dx(); i++ {
						mask[i] = true
					}
				}
			}
			region = reduceBounds(window, 1)
		}
	}
	return mask, nil
}
