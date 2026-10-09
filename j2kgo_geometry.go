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

// componentLayout 保存瓦片分量在各分辨率级的分区信息
type componentLayout struct {
	bounds      image.Rectangle
	coding      codingStyle
	roi         uint8
	resolutions []resolutionLayout
}

// resolutionLayout 保存一个分辨率级的边界与区域
type resolutionLayout struct {
	bounds    image.Rectangle
	precincts []precinctLayout
}

// precinctLayout 保存区域的排序坐标、子带及下一质量层编号
type precinctLayout struct {
	position  image.Point
	bands     []packetBand
	nextLayer int
}

// layoutBudget 记录内存用量及分配上限
type layoutBudget struct {
	used  uint64
	limit uint64
}

// add 检查新增分配并更新内存用量
// 入参: count 数量, size 单项字节数
// 返回: error 错误信息
func (b *layoutBudget) add(count, size uint64) error {
	n, err := checkedProduct("layout memory", count, size, b.limit)
	if err != nil {
		return err
	}
	used, err := checkTotal("layout memory", b.used, n, b.limit)
	if err == nil {
		b.used = used
	}
	return err
}

// makeComponentLayout 按T.800附录B划分分辨率、区域、子带与码块
// 入参: ctx 上下文, tile 瓦片的参考网格边界, component 分量信息, coding 编码参数, quant 量化参数, budget 分区内存预算
// 返回: componentLayout 瓦片分量分区, error 错误信息
func makeComponentLayout(ctx context.Context, tile image.Rectangle, component ComponentInfo, coding codingStyle, quant quantization, budget *layoutBudget) (componentLayout, error) {
	if tile.Empty() || tile.Min.X < 0 || tile.Min.Y < 0 || component.XStep == 0 || component.YStep == 0 || coding.levels < 0 || coding.levels > 32 || len(coding.precincts) != coding.levels+1 {
		return componentLayout{}, FormatError("tile-component layout")
	}
	if coding.blockSize.X < 4 || coding.blockSize.Y < 4 || coding.blockSize.X > 1024 || coding.blockSize.Y > 1024 || coding.blockSize.X*coding.blockSize.Y > 4096 || !powerOfTwo(coding.blockSize.X) || !powerOfTwo(coding.blockSize.Y) {
		return componentLayout{}, FormatError("code-block partition size")
	}
	if err := budget.add(uint64(coding.levels+1), 64); err != nil {
		return componentLayout{}, err
	}
	result := componentLayout{bounds: componentBounds(tile, component), coding: coding, resolutions: make([]resolutionLayout, coding.levels+1)}
	for level := range result.resolutions {
		if err := ctx.Err(); err != nil {
			return componentLayout{}, err
		}
		r := &result.resolutions[level]
		shift := coding.levels - level
		r.bounds = reduceBounds(result.bounds, shift)
		size := coding.precincts[level]
		if !powerOfTwo(size.X) || !powerOfTwo(size.Y) || size.X > 32768 || size.Y > 32768 || (level > 0 && (size.X < 2 || size.Y < 2)) {
			return componentLayout{}, FormatError("precinct partition size")
		}
		if r.bounds.Empty() {
			continue
		}
		grid := image.Rect(r.bounds.Min.X/size.X, r.bounds.Min.Y/size.Y, ceilDiv(r.bounds.Max.X, size.X), ceilDiv(r.bounds.Max.Y, size.Y))
		count, err := checkedProduct("precinct count", uint64(grid.Dx()), uint64(grid.Dy()), uint64(^uint(0)>>1))
		if err != nil {
			return componentLayout{}, err
		}
		if err := budget.add(count, 64); err != nil {
			return componentLayout{}, err
		}
		r.precincts = make([]precinctLayout, int(count))
		for py := grid.Min.Y; py < grid.Max.Y; py++ {
			for px := grid.Min.X; px < grid.Max.X; px++ {
				if err := ctx.Err(); err != nil {
					return componentLayout{}, err
				}
				p := &r.precincts[(py-grid.Min.Y)*grid.Dx()+px-grid.Min.X]
				x := max(int64(tile.Min.X), int64(px)*int64(size.X)*(int64(1)<<shift)*int64(component.XStep))
				y := max(int64(tile.Min.Y), int64(py)*int64(size.Y)*(int64(1)<<shift)*int64(component.YStep))
				if x > int64(^uint(0)>>1) || y > int64(^uint(0)>>1) {
					return componentLayout{}, FormatError("precinct reference coordinate")
				}
				p.position = image.Pt(int(x), int(y))
				orientations := []bandOrientation{bandLL}
				bandSize := size
				if level > 0 {
					orientations = []bandOrientation{bandHL, bandLH, bandHH}
					bandSize = image.Pt(size.X/2, size.Y/2)
				}
				if err := budget.add(uint64(len(orientations)), 128); err != nil {
					return componentLayout{}, err
				}
				p.bands = make([]packetBand, len(orientations))
				for i, orientation := range orientations {
					band := &p.bands[i]
					band.orientation = orientation
					band.bounds = subbandBounds(result.bounds, shift, orientation)
					index := 0
					if level > 0 {
						index = 3*(level-1) + int(orientation)
					}
					step, err := quant.step(index, coding.levels)
					if err != nil {
						return componentLayout{}, err
					}
					band.maxPlanes = quant.guard + step.exponent - 1
					area := intersectGridCell(band.bounds, px, py, bandSize)
					blockSize := image.Pt(min(coding.blockSize.X, bandSize.X), min(coding.blockSize.Y, bandSize.Y))
					if err := partitionPacketBand(band, area, blockSize, budget); err != nil {
						return componentLayout{}, err
					}
				}
			}
		}
	}
	return result, nil
}

// partitionPacketBand 创建区域内码块及两棵标签树
// 入参: band 子带, area 区域边界, size 码块尺寸, budget 分区预算
// 返回: error 错误信息
func partitionPacketBand(band *packetBand, area image.Rectangle, size image.Point, budget *layoutBudget) error {
	if area.Empty() {
		return nil
	}
	grid := image.Rect(area.Min.X/size.X, area.Min.Y/size.Y, ceilDiv(area.Max.X, size.X), ceilDiv(area.Max.Y, size.Y))
	n := uint64(grid.Dx()) * uint64(grid.Dy())
	if err := budget.add(n, 128); err != nil {
		return err
	}
	nodes, err := tagTreeNodeCount(grid.Dx(), grid.Dy(), budget.limit/32)
	if err != nil {
		return err
	}
	if err := budget.add(uint64(nodes)*2+2, 32); err != nil {
		return err
	}
	band.blocks = make([]packetBlock, int(n))
	for y := grid.Min.Y; y < grid.Max.Y; y++ {
		for x := grid.Min.X; x < grid.Max.X; x++ {
			block := &band.blocks[(y-grid.Min.Y)*grid.Dx()+x-grid.Min.X]
			block.bounds = intersectGridCell(area, x, y, size)
			block.budget = budget
		}
	}
	band.inclusion, err = newTagTree(grid.Dx(), grid.Dy(), nil, Limits{MaxMemoryBytes: budget.limit})
	if err != nil {
		return err
	}
	band.zeroPlanes, err = newTagTree(grid.Dx(), grid.Dy(), nil, Limits{MaxMemoryBytes: budget.limit})
	return err
}

// reduceBounds 将非负边界映射到指定低分辨率网格
// 入参: bounds 原始边界, shift 分辨率缩减级数
// 返回: image.Rectangle 缩减后的边界
func reduceBounds(bounds image.Rectangle, shift int) image.Rectangle {
	step := int64(1) << shift
	return image.Rect(int(ceilSigned(int64(bounds.Min.X), step)), int(ceilSigned(int64(bounds.Min.Y), step)),
		int(ceilSigned(int64(bounds.Max.X), step)), int(ceilSigned(int64(bounds.Max.Y), step)))
}

// subbandBounds 计算子带自身坐标系内的边界
// 入参: bounds 分量边界, shift 分辨率缩减级数, orientation 子带方向
// 返回: image.Rectangle 子带边界
func subbandBounds(bounds image.Rectangle, shift int, orientation bandOrientation) image.Rectangle {
	if orientation == bandLL {
		return reduceBounds(bounds, shift)
	}
	step := int64(1) << shift
	x, y := int64(orientation&1)*step, int64(orientation>>1)*step
	return image.Rect(int(ceilSigned(int64(bounds.Min.X)-x, step*2)), int(ceilSigned(int64(bounds.Min.Y)-y, step*2)),
		int(ceilSigned(int64(bounds.Max.X)-x, step*2)), int(ceilSigned(int64(bounds.Max.Y)-y, step*2)))
}

// intersectGridCell 使用64位坐标计算网格单元与图像区域的交集
// 入参: bounds 图像区域, x 网格列, y 网格行, size 单元尺寸
// 返回: image.Rectangle 交集边界
func intersectGridCell(bounds image.Rectangle, x, y int, size image.Point) image.Rectangle {
	x0, y0 := max(int64(bounds.Min.X), int64(x)*int64(size.X)), max(int64(bounds.Min.Y), int64(y)*int64(size.Y))
	x1, y1 := min(int64(bounds.Max.X), (int64(x)+1)*int64(size.X)), min(int64(bounds.Max.Y), (int64(y)+1)*int64(size.Y))
	if x0 >= x1 || y0 >= y1 {
		return image.Rectangle{}
	}
	return image.Rect(int(x0), int(y0), int(x1), int(y1))
}

// ceilSigned 计算有符号整数除法的向上取整值，除数须为正数
// 入参: value 被除数, divisor 正除数
// 返回: int64 向上取整后的商
func ceilSigned(value, divisor int64) int64 {
	q := value / divisor
	if value%divisor > 0 {
		q++
	}
	return q
}

// powerOfTwo 判断正整数是否为二的幂
// 入参: n 数值
// 返回: bool 是否为二的幂
func powerOfTwo(n int) bool { return n > 0 && n&(n-1) == 0 }
