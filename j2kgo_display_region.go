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

// displaySampleBounds 计算目标区域及其显示所需的分量样本范围
// 入参: region 目标区域, extent 完整图像的参考网格边界, spec 分量信息, step 显示网格间隔
// 返回: image.Rectangle 分量存储范围
func displaySampleBounds(region, extent image.Rectangle, spec ComponentInfo, step int) image.Rectangle {
	bounds := componentBounds(region, spec)
	full := componentBounds(extent, spec)
	grid := displayGrid(region, step)
	if full.Empty() || grid.Empty() || region == extent {
		return bounds
	}
	x0 := max(full.Min.X, min(full.Max.X-1, registeredCoordinate(grid.Min.X*step, spec.XStep, spec.XOffset)))
	y0 := max(full.Min.Y, min(full.Max.Y-1, registeredCoordinate(grid.Min.Y*step, spec.YStep, spec.YOffset)))
	x1 := max(full.Min.X, min(full.Max.X-1, registeredCoordinate((grid.Max.X-1)*step, spec.XStep, spec.XOffset)))
	y1 := max(full.Min.Y, min(full.Max.Y-1, registeredCoordinate((grid.Max.Y-1)*step, spec.YStep, spec.YOffset)))
	return bounds.Union(image.Rect(x0, y0, x1+1, y1+1))
}

// supportBounds 返回覆盖所有分量存储范围的参考网格区域
// 返回: image.Rectangle 解码所需区域
func (r *Raster) supportBounds() image.Rectangle {
	bounds := r.info.Bounds
	for _, c := range r.components {
		if c.storage.Empty() {
			continue
		}
		x0 := int64(c.storage.Min.X) * int64(c.info.XStep)
		y0 := int64(c.storage.Min.Y) * int64(c.info.YStep)
		x1 := int64(c.storage.Max.X-1)*int64(c.info.XStep) + 1
		y1 := int64(c.storage.Max.Y-1)*int64(c.info.YStep) + 1
		bounds = bounds.Union(image.Rect(int(max(x0, int64(r.extent.Min.X))), int(max(y0, int64(r.extent.Min.Y))), int(min(x1, int64(r.extent.Max.X))), int(min(y1, int64(r.extent.Max.Y)))))
	}
	return bounds
}

// needsTile 判断瓦片是否包含目标区域或显示所需的相邻样本
// 入参: bounds 降低分辨率后的瓦片参考网格边界
// 返回: bool 是否需要解码
func (r *Raster) needsTile(bounds image.Rectangle) bool {
	for _, c := range r.components {
		if componentBounds(bounds, c.info).Overlaps(c.storage) {
			return true
		}
	}
	return false
}

// tileRange 计算目标区域覆盖的瓦片行列范围
// 入参: bounds 降低分辨率后的参考网格区域, reduce 分辨率缩减级数
// 返回: image.Rectangle 瓦片行列范围
func (s *streamIndex) tileRange(bounds image.Rectangle, reduce int) image.Rectangle {
	if bounds.Empty() {
		return image.Rectangle{}
	}
	scale := int64(1) << reduce
	x0 := max(0, int64(bounds.Min.X)*scale-int64(s.tileOrigin.X)) / int64(s.tileSize[0])
	y0 := max(0, int64(bounds.Min.Y)*scale-int64(s.tileOrigin.Y)) / int64(s.tileSize[1])
	x1 := max(0, int64(bounds.Max.X-1)*scale-int64(s.tileOrigin.X))/int64(s.tileSize[0]) + 1
	y1 := max(0, int64(bounds.Max.Y-1)*scale-int64(s.tileOrigin.Y))/int64(s.tileSize[1]) + 1
	return image.Rect(int(min(x0, int64(s.columns))), int(min(y0, int64(s.rows))), int(min(x1, int64(s.columns))), int(min(y1, int64(s.rows))))
}

// decodeSupportingTiles 解码目标区域及显示所需相邻样本所在的瓦片，跳过已重建的瓦片
// 入参: ctx 上下文, result 目标图像, limits 可用资源限制, skip 已重建瓦片索引，负值表示不跳过
// 返回: error 错误信息
func (d *Decoder) decodeSupportingTiles(ctx context.Context, result *Raster, limits Limits, skip int) error {
	area := d.index.tileRange(result.supportBounds(), d.options.Reduce)
	for y := area.Min.Y; y < area.Max.Y; y++ {
		for x := area.Min.X; x < area.Max.X; x++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			index := y*d.index.columns + x
			if index == skip || !result.needsTile(reduceBounds(d.index.tileBounds(index), d.options.Reduce)) {
				continue
			}
			if err := d.decodeTile(ctx, index, result, limits); err != nil {
				return fmt.Errorf("tile %d: %w", index, err)
			}
		}
	}
	return nil
}
