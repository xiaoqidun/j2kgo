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
)

// tileEncoding 保存单个瓦片的分区及已编码码块
type tileEncoding struct {
	layouts  []componentLayout
	blocks   map[*packetBlock]encodedBlock
	layers   map[*packetBlock][]ratePoint
	rates    map[*packetBlock][]ratePoint
	prefixes map[*packetBlock][]ratePoint
	budget   layoutBudget
}

// tile 编码一个瓦片并直接写出各段码字
// 入参: raster 原始图像, plan 编码参数, index 瓦片序号
// 返回: error 错误信息
func (w *codestreamWriter) tile(raster *Raster, plan encodingPlan, index int) error {
	tile := tileEncoding{budget: layoutBudget{limit: plan.limits.MaxMemoryBytes}}
	if err := tile.budget.add(uint64(len(plan.info.Components)), 256); err != nil {
		return err
	}
	tile.layouts = make([]componentLayout, len(plan.info.Components))
	tile.blocks = make(map[*packetBlock]encodedBlock)
	if (len(plan.layers) > 0 && plan.layers[0].BitsPerPixel > 0) || (len(plan.roi) > 0 && plan.layerCount > 1) {
		tile.rates = make(map[*packetBlock][]ratePoint)
	}
	for c, info := range plan.info.Components {
		var err error
		tile.layouts[c], err = makeComponentLayout(w.ctx, plan.tileBounds(index), info, plan.componentCoding(c), plan.componentQuantization(c), &tile.budget)
		if err != nil {
			return err
		}
	}
	for first := 0; first < len(tile.layouts); {
		count := 1
		if first == 0 && plan.mct {
			count = 3
		}
		var planes [3]tileSamples
		var reserved uint64
		reversible := tile.layouts[first].coding.reversible
		for c := range count {
			component := &raster.components[first+c]
			bounds := tile.layouts[first+c].bounds
			n, err := checkedProduct("tile samples", uint64(bounds.Dx()), uint64(bounds.Dy()), min(plan.limits.MaxSamples, uint64(^uint(0)>>1)))
			if err != nil {
				return err
			}
			if err := tile.budget.add(n, 8); err != nil {
				return err
			}
			reserved += n * 8
			planes[c].bounds = bounds
			if reversible {
				planes[c].integers = make([]int64, int(n))
			} else {
				planes[c].floats = make([]float64, int(n))
			}
			offset := int64(0)
			if !component.info.Signed {
				offset = int64(1) << (component.info.Precision - 1)
			}
			for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
				if err := w.ctx.Err(); err != nil {
					return err
				}
				if reversible {
					start := (y - bounds.Min.Y) * bounds.Dx()
					row := planes[c].integers[start : start+bounds.Dx()]
					if err := component.ReadSamples(row, bounds.Min.X, y); err != nil {
						return err
					}
					for x := range row {
						row[x] -= offset
					}
					continue
				}
				for x := bounds.Min.X; x < bounds.Max.X; x++ {
					i := (y-bounds.Min.Y)*bounds.Dx() + x - bounds.Min.X
					value := component.Sample(x, y) - offset
					planes[c].floats[i] = float64(value)
				}
			}
		}
		if count == 3 {
			var err error
			if reversible {
				err = transformRCT(w.ctx, [3][]int64{planes[0].integers, planes[1].integers, planes[2].integers}, false)
			} else {
				err = transformICT(w.ctx, [3][]float64{planes[0].floats, planes[1].floats, planes[2].floats}, false)
			}
			if err != nil {
				return err
			}
		}
		for c := range count {
			if err := tile.component(w.ctx, first+c, planes[c], plan.info.Components[first+c], plan.componentQuantization(first+c), plan.roi, plan.componentRateWeight(first+c), plan.limits); err != nil {
				return err
			}
		}
		tile.budget.used -= reserved
		first += count
	}
	if err := tile.allocateLayers(w.ctx, plan, index); err != nil {
		return err
	}
	return tile.write(w, plan, index)
}

// component 对瓦片分量执行小波正变换、量化和码块编码
// 入参: ctx 上下文, index 分量索引, samples 样本, info 分量信息, quant 量化参数, regions 优先区域, weight 分量失真权重, limits 资源限制
// 返回: error 错误信息
func (t *tileEncoding) component(ctx context.Context, index int, samples tileSamples, info ComponentInfo, quant quantization, regions []image.Rectangle, weight float64, limits Limits) error {
	layout := &t.layouts[index]
	if samples.bounds.Empty() {
		return nil
	}
	mask, err := makeROIMask(ctx, layout, info, regions, &t.budget)
	if err != nil {
		return err
	}
	defer func() { t.budget.used -= uint64(len(mask)) }()
	scratchSize := max(samples.bounds.Dx(), samples.bounds.Dy()) * 2
	blockSize := layout.coding.blockSize.X * layout.coding.blockSize.Y
	work := uint64(scratchSize)*8 + uint64(blockSize)*19 + 512
	if mask != nil {
		work += uint64(blockSize)
	}
	if err := t.budget.add(1, work); err != nil {
		return err
	}
	defer func() { t.budget.used -= work }()
	if layout.coding.reversible {
		err = transformWavelet(ctx, samples.integers, samples.bounds.Dx(), samples.bounds, layout.coding.levels, false, make([]int64, scratchSize), wavelet53)
	} else {
		err = transformWavelet(ctx, samples.floats, samples.bounds.Dx(), samples.bounds, layout.coding.levels, false, make([]float64, scratchSize), wavelet97)
	}
	if err != nil {
		return err
	}
	values := make([]int64, blockSize)
	var blockMask []bool
	if mask != nil {
		blockMask = make([]bool, blockSize)
		for _, step := range quant.steps {
			layout.roi = max(layout.roi, uint8(max(0, quant.guard+step.exponent-1)))
		}
	}
	for level, resolution := range layout.resolutions {
		low := reduceBounds(layout.bounds, layout.coding.levels-level+1)
		for _, precinct := range resolution.precincts {
			for _, band := range precinct.bands {
				xOffset, yOffset, bandIndex := 0, 0, 0
				if band.orientation&1 != 0 {
					xOffset = low.Dx()
				}
				if band.orientation&2 != 0 {
					yOffset = low.Dy()
				}
				if level > 0 {
					bandIndex = 3*(level-1) + int(band.orientation)
				}
				step, err := quant.step(bandIndex, layout.coding.levels)
				if err != nil {
					return err
				}
				scale := quantizationScale(info.Precision, band.orientation, step)
				distortionWeight := weight * layout.coding.rateWeight(level, band.orientation)
				if !layout.coding.reversible {
					distortionWeight *= scale * scale
				}
				limit := int64(1) << max(0, band.maxPlanes)
				for i := range band.blocks {
					block := &band.blocks[i]
					for y := block.bounds.Min.Y; y < block.bounds.Max.Y; y++ {
						for x := block.bounds.Min.X; x < block.bounds.Max.X; x++ {
							source := (y-band.bounds.Min.Y+yOffset)*layout.bounds.Dx() + x - band.bounds.Min.X + xOffset
							target := (y-block.bounds.Min.Y)*block.bounds.Dx() + x - block.bounds.Min.X
							if layout.coding.reversible {
								values[target] = samples.integers[source]
							} else {
								value := samples.floats[source] / scale
								if math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) >= 1<<62 {
									return FormatError("quantized coefficient range")
								}
								values[target] = int64(value)
							}
							if values[target] <= -limit || values[target] >= limit {
								return FormatError("coefficient exceeds JPEG2000 dynamic range")
							}
							if mask != nil {
								blockMask[target] = mask[source]
							}
						}
					}
					count := block.bounds.Dx() * block.bounds.Dy()
					region := blockMask
					if region != nil {
						region = region[:count]
					}
					encoded, err := encodeCodeBlockROI(ctx, values[:count], region, layout.roi, block.bounds.Dx(), block.bounds.Dy(), band.orientation, layout.coding.style, limits)
					if err != nil {
						return err
					}
					if err := t.budget.add(uint64(cap(encoded.segments)), 32); err != nil {
						return err
					}
					for _, segment := range encoded.segments {
						if err := t.budget.add(uint64(cap(segment.data)), 1); err != nil {
							return err
						}
					}
					t.blocks[block] = encoded
					if t.rates != nil {
						remaining := limits
						if t.budget.used >= t.budget.limit {
							return &LimitError{Resource: "rate analysis memory", Limit: t.budget.limit, Required: t.budget.used + 1}
						}
						remaining.MaxMemoryBytes = t.budget.limit - t.budget.used
						points, err := analyzeCodeBlockROI(ctx, encoded, values[:count], region, layout.roi, block.bounds.Dx(), block.bounds.Dy(), band.orientation, layout.coding.style, remaining)
						if err != nil {
							return err
						}
						if err := t.budget.add(uint64(cap(points)), 24); err != nil {
							return err
						}
						if err := t.budget.add(1, 128); err != nil {
							return err
						}
						for n := range points {
							points[n].distortion *= distortionWeight
						}
						if len(points) > 165 {
							if err := t.budget.add(uint64(len(points)), 24); err != nil {
								return err
							}
							if err := t.budget.add(1, 128); err != nil {
								return err
							}
							if t.prefixes == nil {
								t.prefixes = make(map[*packetBlock][]ratePoint)
							}
							t.prefixes[block] = points
							points = make([]ratePoint, len(points))
							copy(points, t.prefixes[block])
						}
						t.rates[block] = rateHull(points)
					}
				}
			}
		}
	}
	if layout.roi != 0 {
		for _, resolution := range layout.resolutions {
			for _, precinct := range resolution.precincts {
				for i := range precinct.bands {
					precinct.bands[i].maxPlanes += int(layout.roi)
				}
			}
		}
	}
	return nil
}
