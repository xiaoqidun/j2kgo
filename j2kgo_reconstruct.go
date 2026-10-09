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

// tileSamples 保存小波逆变换后的分量样本，尚未进行分量逆变换和直流偏移恢复
type tileSamples struct {
	bounds   image.Rectangle
	integers []int64
	floats   []float64
}

// reconstructComponent 解码瓦片分量的各子带并执行逆小波变换
// 入参: ctx 上下文, source 输入源, layout 分量分区, quant 量化参数, precision 分量精度, limits 可用资源限制, workers 并发上限
// 返回: tileSamples 分量样本, error 错误信息
func reconstructComponent(ctx context.Context, source *inputSource, layout *componentLayout, quant quantization, precision uint8, limits Limits, workers int) (tileSamples, error) {
	if err := ctx.Err(); err != nil {
		return tileSamples{}, err
	}
	limits = limits.normalized()
	if precision < 1 || precision > 38 || (layout.coding.reversible && quant.kind != 0) {
		return tileSamples{}, FormatError("component precision or quantization")
	}
	result := tileSamples{bounds: layout.bounds}
	if layout.bounds.Empty() {
		return result, nil
	}
	count, err := checkedProduct("samples", uint64(layout.bounds.Dx()), uint64(layout.bounds.Dy()), limits.MaxSamples)
	if err != nil {
		return tileSamples{}, err
	}
	memory, err := checkedProduct("wavelet memory", count, 8, min(limits.MaxMemoryBytes, uint64(^uint(0)>>1)))
	if err != nil {
		return tileSamples{}, err
	}
	scratchSize := uint64(max(layout.bounds.Dx(), layout.bounds.Dy())) * 2
	memory, err = checkTotal("wavelet memory", memory, scratchSize*8, limits.MaxMemoryBytes)
	if err != nil {
		return tileSamples{}, err
	}
	available := limits.MaxMemoryBytes - memory
	if layout.coding.reversible {
		result.integers = make([]int64, int(count))
	} else {
		result.floats = make([]float64, int(count))
	}
	if err := loadComponentBands(ctx, source, layout, quant, precision, result, available, workers); err != nil {
		return tileSamples{}, err
	}
	if layout.coding.reversible {
		err = transformWavelet(ctx, result.integers, layout.bounds.Dx(), layout.bounds, layout.coding.levels, true, make([]int64, int(scratchSize)), wavelet53)
	} else {
		err = transformWavelet(ctx, result.floats, layout.bounds.Dx(), layout.bounds, layout.coding.levels, true, make([]float64, int(scratchSize)), wavelet97)
	}
	if err != nil {
		return tileSamples{}, err
	}
	return result, nil
}

// loadComponentBands 在共享预算内并行解码子带并写入不重叠的系数区域
// 入参: ctx 上下文, source 输入源, layout 分量分区, quant 量化参数, precision 分量精度, result 系数缓冲, memory 可用内存, workers 并发上限
// 返回: error 错误信息
func loadComponentBands(ctx context.Context, source *inputSource, layout *componentLayout, quant quantization, precision uint8, result tileSamples, memory uint64, workers int) error {
	var work decodeWork
	for _, resolution := range layout.resolutions {
		for _, precinct := range resolution.precincts {
			for _, band := range precinct.bands {
				for i := range band.blocks {
					if err := ctx.Err(); err != nil {
						return err
					}
					block := &band.blocks[i]
					if len(block.segments) != 0 && block.segments[0].retained != 0 {
						if err := work.add(source, block, memory); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	return work.run(ctx, workers, memory, func(submit func(uint64, func(context.Context) error) error) error {
		for level, resolution := range layout.resolutions {
			low := reduceBounds(layout.bounds, layout.coding.levels-level+1)
			for _, precinct := range resolution.precincts {
				for _, band := range precinct.bands {
					index := 0
					if level > 0 {
						index = 3*(level-1) + int(band.orientation)
					}
					step, err := quant.step(index, layout.coding.levels)
					if err != nil {
						return err
					}
					scale := quantizationScale(precision, band.orientation, step)
					xOffset, yOffset := 0, 0
					if band.orientation&1 != 0 {
						xOffset = low.Dx()
					}
					if band.orientation&2 != 0 {
						yOffset = low.Dy()
					}
					for i := range band.blocks {
						block := &band.blocks[i]
						if len(block.segments) == 0 || block.segments[0].retained == 0 {
							continue
						}
						need, err := packetBlockMemory(source, block, memory)
						if err != nil {
							return err
						}
						if err := submit(need, func(ctx context.Context) error {
							decoded, err := decodePacketBlock(ctx, source, block, band.orientation, layout.coding.style, layout.roi, band.maxPlanes-int(layout.roi), need)
							if err != nil {
								return err
							}
							for y := block.bounds.Min.Y; y < block.bounds.Max.Y; y++ {
								for x := block.bounds.Min.X; x < block.bounds.Max.X; x++ {
									index := (y-block.bounds.Min.Y)*block.bounds.Dx() + x - block.bounds.Min.X
									target := (y-band.bounds.Min.Y+yOffset)*layout.bounds.Dx() + x - band.bounds.Min.X + xOffset
									if layout.coding.reversible {
										result.integers[target] = decoded.coefficient(index, true)
									} else {
										result.floats[target] = decoded.coefficientFloat(index, scale)
									}
								}
							}
							return ctx.Err()
						}); err != nil {
							return err
						}
					}
				}
			}
		}
		return nil
	})
}
