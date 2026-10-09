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

// selectRegionBlocks 标记区域重建不需要的码块，仍解析其包头但不保存码字位置
// 入参: layout 分量分区, region 缩小后的目标区域, reduce 丢弃的分辨率级数
func selectRegionBlocks(layout *componentLayout, region image.Rectangle, reduce int) {
	top := layout.coding.levels - reduce
	region = region.Intersect(layout.resolutions[top].bounds)
	if region == layout.resolutions[top].bounds {
		return
	}
	for level := len(layout.resolutions) - 1; level >= 0; level-- {
		resolution := &layout.resolutions[level]
		var window image.Rectangle
		if level <= top && !region.Empty() {
			window = region
			if level > 0 {
				window = waveletWindow(region, resolution.bounds, layout.coding.reversible)
			}
		}
		for p := range resolution.precincts {
			for b := range resolution.precincts[p].bands {
				band := &resolution.precincts[p].bands[b]
				area := window
				if level > 0 {
					area = subbandBounds(window, 0, band.orientation)
				}
				for k := range band.blocks {
					band.blocks[k].discard = !band.blocks[k].bounds.Overlaps(area)
				}
			}
		}
		if level <= top {
			region = reduceBounds(window, 1)
		}
	}
}

// reconstructRegion 只重建目标区域及小波提升所需的邻域
// 入参: ctx 上下文, source 输入源, layout 分量分区, quant 量化参数, precision 精度, region 区域, limits 资源限制, workers 并发上限
// 返回: tileSamples 区域样本, error 错误信息
func reconstructRegion(ctx context.Context, source *inputSource, layout *componentLayout, quant quantization, precision uint8, region image.Rectangle, limits Limits, workers int) (tileSamples, error) {
	if err := ctx.Err(); err != nil {
		return tileSamples{}, err
	}
	result := tileSamples{bounds: region.Intersect(layout.bounds)}
	if precision < 1 || precision > 38 || (layout.coding.reversible && quant.kind != 0) {
		return tileSamples{}, FormatError("component precision or quantization")
	}
	if result.bounds.Empty() {
		return result, nil
	}
	var err error
	if layout.coding.reversible {
		result.integers, err = reconstructWindow[int64](ctx, source, layout, quant, precision, layout.coding.levels, result.bounds, limits.normalized(), workers, wavelet53)
	} else {
		result.floats, err = reconstructWindow[float64](ctx, source, layout, quant, precision, layout.coding.levels, result.bounds, limits.normalized(), workers, wavelet97)
	}
	return result, err
}

// waveletWindow 扩展目标区域，包含小波提升计算所需的相邻样本
// 入参: region 目标区域, bounds 当前分辨率边界, reversible 是否5/3变换
// 返回: image.Rectangle 工作区域
func waveletWindow(region, bounds image.Rectangle, reversible bool) image.Rectangle {
	margin := int64(4)
	if reversible {
		margin = 2
	}
	return image.Rect(int(max(int64(bounds.Min.X), int64(region.Min.X)-margin)), int(max(int64(bounds.Min.Y), int64(region.Min.Y)-margin)),
		int(min(int64(bounds.Max.X), int64(region.Max.X)+margin)), int(min(int64(bounds.Max.Y), int64(region.Max.Y)+margin)))
}

// reconstructWindow 复用同一缓冲逐级重建低频窗口并合并高频子带
// 入参: ctx 上下文, source 输入源, layout 分量分区, quant 量化参数, precision 分量精度, level 分辨率级, region 目标区域, limits 资源限制, workers 并发上限, line 单轴逆变换
// 返回: []T 区域样本, error 错误信息
func reconstructWindow[T waveletSample](ctx context.Context, source *inputSource, layout *componentLayout, quant quantization, precision uint8, level int, region image.Rectangle, limits Limits, workers int, line func([]T, int, bool)) ([]T, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if region.Empty() {
		return nil, nil
	}
	var regions [33]image.Rectangle
	count, column := uint64(0), uint64(0)
	for current, area := level, region; current >= 0 && !area.Empty(); current-- {
		regions[current] = area
		window := area
		if current > 0 {
			window = waveletWindow(area, reduceBounds(layout.bounds, layout.coding.levels-current), layout.coding.reversible)
			column = max(column, uint64(window.Dy()))
		}
		samples, err := checkedProduct("region samples", uint64(window.Dx()), uint64(window.Dy()), min(limits.MaxSamples, uint64(^uint(0)>>1)/8))
		if err != nil {
			return nil, err
		}
		count = max(count, samples)
		area = reduceBounds(window, 1)
	}
	memory, err := checkedProduct("region wavelet memory", count+column, 8, limits.MaxMemoryBytes)
	if err != nil {
		return nil, err
	}
	data := make([]T, int(count))
	lineBuffer := make([]T, int(column))
	for current := 0; current <= level; current++ {
		area := regions[current]
		if area.Empty() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		window := area
		var low image.Rectangle
		if current > 0 {
			window = waveletWindow(area, reduceBounds(layout.bounds, layout.coding.levels-current), layout.coding.reversible)
			low = reduceBounds(window, 1)
		}
		work := data[:window.Dx()*window.Dy()]
		clear(work[low.Dx()*low.Dy():])
		for y := low.Max.Y - 1; y >= low.Min.Y; y-- {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			for x := low.Max.X - 1; x >= low.Min.X; x-- {
				from := (y-low.Min.Y)*low.Dx() + x - low.Min.X
				to := (2*y-window.Min.Y)*window.Dx() + 2*x - window.Min.X
				value := work[from]
				work[from] = 0
				work[to] = value
			}
		}
		if err := loadWindowBands(ctx, source, layout, quant, precision, current, window, work, limits.MaxMemoryBytes-memory, workers); err != nil {
			return nil, err
		}
		if current > 0 {
			if err := inverseWindow(ctx, work, window, lineBuffer[:window.Dy()], line); err != nil {
				return nil, err
			}
		}
		if window != area {
			for y := area.Min.Y; y < area.Max.Y; y++ {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				from := (y-window.Min.Y)*window.Dx() + area.Min.X - window.Min.X
				to := (y - area.Min.Y) * area.Dx()
				copy(work[to:to+area.Dx()], work[from:from+area.Dx()])
			}
		}
	}
	return data[:region.Dx()*region.Dy()], nil
}

// loadWindowBands 只解码与当前小波窗口相交的码块
// 入参: ctx 上下文, source 输入源, layout 分量分区, quant 量化参数, precision 分量精度, level 分辨率级, window 重建窗口, data 样本缓冲, memory 码块内存预算, workers 并发上限
// 返回: error 错误信息
func loadWindowBands[T waveletSample](ctx context.Context, source *inputSource, layout *componentLayout, quant quantization, precision uint8, level int, window image.Rectangle, data []T, memory uint64, workers int) error {
	var work decodeWork
	for _, precinct := range layout.resolutions[level].precincts {
		for _, band := range precinct.bands {
			area := window
			if level > 0 {
				area = subbandBounds(window, 0, band.orientation)
			}
			for i := range band.blocks {
				if err := ctx.Err(); err != nil {
					return err
				}
				block := &band.blocks[i]
				if block.bounds.Overlaps(area) && len(block.segments) != 0 && block.segments[0].retained != 0 {
					if err := work.add(source, block, memory); err != nil {
						return err
					}
				}
			}
		}
	}
	return work.run(ctx, workers, memory, func(submit func(uint64, func(context.Context, *codeBlock) error) error) error {
		for _, precinct := range layout.resolutions[level].precincts {
			for _, band := range precinct.bands {
				area := window
				index := 0
				if level > 0 {
					area = subbandBounds(window, 0, band.orientation)
					index = 3*(level-1) + int(band.orientation)
				}
				step, err := quant.step(index, layout.coding.levels)
				if err != nil {
					return err
				}
				scale := quantizationScale(precision, band.orientation, step)
				for i := range band.blocks {
					block := &band.blocks[i]
					intersection := block.bounds.Intersect(area)
					if intersection.Empty() || len(block.segments) == 0 || block.segments[0].retained == 0 {
						continue
					}
					need, err := packetBlockMemory(source, block, memory)
					if err != nil {
						return err
					}
					if err := submit(need, func(ctx context.Context, buffer *codeBlock) error {
						decoded, err := decodePacketBlock(ctx, source, block, band.orientation, layout.coding.style, layout.roi, band.maxPlanes-int(layout.roi), need, buffer)
						if err != nil {
							return err
						}
						for y := intersection.Min.Y; y < intersection.Max.Y; y++ {
							for x := intersection.Min.X; x < intersection.Max.X; x++ {
								index := (y-block.bounds.Min.Y)*block.bounds.Dx() + x - block.bounds.Min.X
								tx, ty := x, y
								if level > 0 {
									tx, ty = x*2+int(band.orientation&1), y*2+int(band.orientation>>1)
								}
								target := (ty-window.Min.Y)*window.Dx() + tx - window.Min.X
								if layout.coding.reversible {
									data[target] = T(decoded.coefficient(index, true))
								} else {
									data[target] = T(decoded.coefficientFloat(index, scale))
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
		return nil
	})
}

// inverseWindow 对交错系数执行单级二维逆变换
// 入参: ctx 上下文, data 交错样本, window 窗口, column 列缓冲, line 单轴逆变换
// 返回: error 错误信息
func inverseWindow[T waveletSample](ctx context.Context, data []T, window image.Rectangle, column []T, line func([]T, int, bool)) error {
	w, h := window.Dx(), window.Dy()
	for y := range h {
		if err := ctx.Err(); err != nil {
			return err
		}
		line(data[y*w:(y+1)*w], window.Min.X, true)
	}
	for x := range w {
		if err := ctx.Err(); err != nil {
			return err
		}
		for y := range h {
			column[y] = data[y*w+x]
		}
		line(column, window.Min.Y, true)
		for y := range h {
			data[y*w+x] = column[y]
		}
	}
	return nil
}
