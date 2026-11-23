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
	"sync"
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

// encodedBlockResult 保存并行编码后的单个码块结果及截断点
type encodedBlockResult struct {
	block    *packetBlock
	encoded  encodedBlock
	points   []ratePoint
	prefixes []ratePoint
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
			if err := tile.component(w.ctx, first+c, planes[c], plan.info.Components[first+c], plan.componentQuantization(first+c), plan.roi, plan.componentRateWeight(first+c), plan.workers); err != nil {
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

// component 对瓦片分量执行多核小波正变换、量化和码块并行编码
// 入参: ctx 上下文, index 分量索引, samples 样本, info 分量信息, quant 量化参数, regions 优先区域, weight 分量失真权重, workers 并发上限
// 返回: error 错误信息
func (t *tileEncoding) component(ctx context.Context, index int, samples tileSamples, info ComponentInfo, quant quantization, regions []image.Rectangle, weight float64, workers int) error {
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
	minWavelet := uint64(scratchSize) * 8
	if err := t.budget.add(1, minWavelet); err != nil {
		return err
	}
	waveletBudget := (t.budget.limit - t.budget.used) + minWavelet
	waveletWorkers := resolveWorkers(workers)
	if layout.coding.reversible {
		err = transformWaveletWorkers(ctx, samples.integers, samples.bounds.Dx(), samples.bounds, layout.coding.levels, false, waveletBudget, waveletWorkers, wavelet53)
	} else {
		err = transformWaveletWorkers(ctx, samples.floats, samples.bounds.Dx(), samples.bounds, layout.coding.levels, false, waveletBudget, waveletWorkers, wavelet97)
	}
	t.budget.used -= minWavelet
	if err != nil {
		return err
	}
	if mask != nil {
		for _, step := range quant.steps {
			layout.roi = max(layout.roi, uint8(max(0, quant.guard+step.exponent-1)))
		}
	}
	blockCount := 0
	for _, resolution := range layout.resolutions {
		for _, precinct := range resolution.precincts {
			for _, band := range precinct.bands {
				blockCount += len(band.blocks)
			}
		}
	}
	if err := t.budget.add(uint64(blockCount), 88); err != nil {
		return err
	}
	resultsMem := uint64(blockCount) * 88
	defer func() { t.budget.used -= resultsMem }()
	var work decodeWork
	available := t.budget.limit - t.budget.used
	for _, resolution := range layout.resolutions {
		for _, precinct := range resolution.precincts {
			for _, band := range precinct.bands {
				for i := range band.blocks {
					block := &band.blocks[i]
					if err := work.addEncode(block.bounds.Dx()*block.bounds.Dy(), available); err != nil {
						return err
					}
				}
			}
		}
	}
	results := make([]encodedBlockResult, work.count)
	needRates := t.rates != nil
	var budgetMu sync.Mutex
	chargeOutput := func(encoded encodedBlock, points, prefixes []ratePoint) error {
		budgetMu.Lock()
		defer budgetMu.Unlock()
		if err := t.budget.add(uint64(cap(encoded.segments)), 32); err != nil {
			return err
		}
		for _, segment := range encoded.segments {
			if err := t.budget.add(uint64(cap(segment.data)), 1); err != nil {
				return err
			}
		}
		if needRates {
			if err := t.budget.add(uint64(cap(points)), 24); err != nil {
				return err
			}
			if err := t.budget.add(1, 128); err != nil {
				return err
			}
			if prefixes != nil {
				if err := t.budget.add(uint64(cap(prefixes)), 24); err != nil {
					return err
				}
				if err := t.budget.add(1, 128); err != nil {
					return err
				}
			}
		}
		return nil
	}
	err = work.run(ctx, workers, available, func(submit func(uint64, func(context.Context, *codeBlock) error) error) error {
		resultIdx := 0
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
					bandBounds := band.bounds
					orientation := band.orientation
					for i := range band.blocks {
						block := &band.blocks[i]
						idx := resultIdx
						resultIdx++
						count := block.bounds.Dx() * block.bounds.Dy()
						need := uint64(count)*11 + 512
						if err := submit(need, func(ctx context.Context, buffer *codeBlock) error {
							b, err := reuseCodeBlock(buffer, block.bounds.Dx(), block.bounds.Dy(), orientation, layout.coding.style, Limits{MaxMemoryBytes: need})
							if err != nil {
								return err
							}
							planes, initBase, initROI, err := loadEncodeBlock(b, samples, mask, layout.bounds.Dx(), bandBounds, block.bounds, xOffset, yOffset, layout.coding.reversible, scale, limit, layout.roi, needRates)
							if err != nil {
								return err
							}
							encoded, points, err := encodePreparedBlock(ctx, b, planes, initBase, initROI, needRates)
							if err != nil {
								return err
							}
							var prefixes []ratePoint
							if needRates {
								for n := range points {
									points[n].distortion *= distortionWeight
								}
								if len(points) > 165 {
									prefixes = points
									points = make([]ratePoint, len(prefixes))
									copy(points, prefixes)
								}
								points = rateHull(points)
							}
							if err := chargeOutput(encoded, points, prefixes); err != nil {
								return err
							}
							results[idx] = encodedBlockResult{block: block, encoded: encoded, points: points, prefixes: prefixes}
							return nil
						}); err != nil {
							return err
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, r := range results {
		t.blocks[r.block] = r.encoded
		if needRates {
			if r.prefixes != nil {
				if t.prefixes == nil {
					t.prefixes = make(map[*packetBlock][]ratePoint)
				}
				t.prefixes[r.block] = r.prefixes
			}
			t.rates[r.block] = r.points
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

// loadEncodeBlock 将子带样本量化并写入复用码块缓冲，同时计算有效位平面数和初始失真
// 入参: b 复用码块缓冲, samples 瓦片样本, mask ROI掩码, stride 样本行跨度, bandBounds 子带边界, blockBounds 码块边界, xOffset 子带水平偏移, yOffset 子带垂直偏移, reversible 是否可逆变换, scale 量化步长, limit 动态范围上限, shift ROI移位量, needRates 是否累计初始失真
// 返回: int 有效位平面数, float64 背景初始失真, float64 ROI未缩放初始失真, error 错误信息
func loadEncodeBlock(b *codeBlock, samples tileSamples, mask []bool, stride int, bandBounds, blockBounds image.Rectangle, xOffset, yOffset int, reversible bool, scale float64, limit int64, shift uint8, needRates bool) (int, float64, float64, error) {
	b.roi = shift
	bw := blockBounds.Dx()
	planes := 0
	var initBase, initROI float64
	for y := blockBounds.Min.Y; y < blockBounds.Max.Y; y++ {
		srcRow := (y-bandBounds.Min.Y+yOffset)*stride - bandBounds.Min.X + xOffset
		dstRow := (y - blockBounds.Min.Y) * bw
		for x := blockBounds.Min.X; x < blockBounds.Max.X; x++ {
			source := srcRow + x
			target := dstRow + x - blockBounds.Min.X
			var value int64
			if reversible {
				value = samples.integers[source]
			} else {
				v := samples.floats[source] / scale
				if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) >= 1<<62 {
					return 0, 0, 0, FormatError("quantized coefficient range")
				}
				value = int64(v)
			}
			if value <= -limit || value >= limit {
				return 0, 0, 0, FormatError("coefficient exceeds JPEG2000 dynamic range")
			}
			if value == math.MinInt64 {
				return 0, 0, 0, UnsupportedError("code-block coefficient precision")
			}
			if value < 0 {
				b.flags[target] = blockNegative
				value = -value
			}
			mag := uint64(value)
			b.magnitude[target] = mag
			depth := bits.Len64(mag)
			if depth > 62 {
				return 0, 0, 0, UnsupportedError("code-block coefficient precision")
			}
			isROI := mask != nil && mask[source] && shift != 0
			if isROI {
				b.flags[target] |= blockROI
				if depth != 0 {
					depth += int(shift)
				}
			} else if shift != 0 && depth > int(shift) {
				return 0, 0, 0, FormatError("ROI shift below background precision")
			}
			if needRates && mag != 0 {
				d := float64(mag) * float64(mag)
				if isROI {
					initROI += d
				} else {
					initBase += d
				}
			}
			planes = max(planes, depth)
		}
	}
	if planes > 292 {
		return 0, 0, 0, FormatError("ROI code-block precision")
	}
	return planes, initBase, initROI, nil
}
