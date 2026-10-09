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
	"fmt"
	"image"
)

// Reduction 在宽高均满足目标尺寸的前提下，返回可用的最大缩减级数，不读取图像样本
// 目标尺寸按参考网格计算；零值、超出原图的尺寸及调色板图像均返回零
// 入参: size 目标尺寸，宽高须均为正数或均为零
// 返回: int 分辨率缩减级数, error 参数或解码器状态错误
func (d *Decoder) Reduction(size image.Point) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reduction(size)
}

// DecodeSize 按目标尺寸选择码流分辨率并解码原始分量，不进行像素重采样
// 指定尺寸时仅覆盖本次解码的Reduce选项，零值沿用原选项
// 入参: ctx 上下文, size 目标参考网格尺寸，宽高须均为正数或均为零
// 返回: *Raster 原始分量图像，尺寸可能大于目标, error 错误信息
func (d *Decoder) DecodeSize(ctx context.Context, size image.Point) (*Raster, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reduce, err := d.reduction(size)
	if err != nil {
		return nil, err
	}
	if size == (image.Point{}) {
		return d.decodeRegion(ctx, d.info.Bounds)
	}
	view := Decoder{source: d.source, index: d.index, info: d.info, options: d.options}
	view.options.Reduce = reduce
	return view.decodeRegion(ctx, view.info.Bounds)
}

// reduction 根据有效编码参数和分量网格选择缩减级数，调用前须持有解码器锁
// 入参: size 目标参考网格尺寸
// 返回: int 分辨率缩减级数, error 参数或解码器状态错误
func (d *Decoder) reduction(size image.Point) (int, error) {
	if d.closed || d.index == nil {
		return 0, fmt.Errorf("j2kgo: decoder is closed")
	}
	if size.X < 0 || size.Y < 0 || (size.X == 0) != (size.Y == 0) {
		return 0, fmt.Errorf("j2kgo: invalid target size")
	}
	if size == (image.Point{}) || len(d.info.Palette) != 0 {
		return 0, nil
	}
	reduce := 0
	for level := 1; level <= d.index.minLevels; level++ {
		bounds := reduceBounds(d.info.Bounds, level)
		if bounds.Dx() < size.X || bounds.Dy() < size.Y {
			break
		}
		for _, component := range d.info.Components {
			if componentBounds(bounds, component).Empty() {
				return reduce, nil
			}
		}
		reduce = level
	}
	return reduce, nil
}
