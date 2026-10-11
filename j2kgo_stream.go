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

// tileDecoding 保存单瓦片可复用的码字位置及重建参数
type tileDecoding struct {
	params  tileParameters
	layouts []componentLayout
	memory  uint64
}

// DecodeBlocks 按瓦片顺序解码图块，各瓦片内按行遍历
// 回调依次执行，返回的图块互相独立；后续解码失败不影响已返回的图块，其内存不计入后续解码预算
// 入参: ctx 上下文, size 降低分辨率后的最大块尺寸，以参考网格点计，零值使用256×256, visit 图块回调，返回错误时停止解码
// 返回: error 错误信息
func (d *Decoder) DecodeBlocks(ctx context.Context, size image.Point, visit func(*Raster) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.closed || d.index == nil || d.source == nil {
		return fmt.Errorf("j2kgo: decoder is closed")
	}
	if size == (image.Point{}) {
		size = image.Pt(256, 256)
	}
	if size.X <= 0 || size.Y <= 0 || visit == nil {
		return fmt.Errorf("j2kgo: invalid block size or callback")
	}
	d.cache = nil
	for index := range d.index.tiles {
		if err := d.decodeTileBlocks(ctx, index, size, visit); err != nil {
			return err
		}
	}
	return nil
}

// decodeTileBlocks 逐块重建单个瓦片，回调执行期间保留索引的内存占用
// 入参: ctx 上下文, index 瓦片索引, size 最大图块尺寸, visit 图块回调
// 返回: error 解码、回调或取消错误
func (d *Decoder) decodeTileBlocks(ctx context.Context, index int, size image.Point, visit func(*Raster) error) error {
	bounds := reduceBounds(d.index.tileBounds(index), d.options.Reduce)
	if bounds.Empty() {
		return nil
	}
	limits, err := d.decodeLimits()
	if err != nil {
		return err
	}
	tile, err := d.prepareTile(ctx, index, bounds, limits)
	if err != nil {
		return fmt.Errorf("tile %d: %w", index, err)
	}
	if tile.memory >= limits.MaxMemoryBytes {
		return &LimitError{Resource: "tile memory", Limit: limits.MaxMemoryBytes, Required: tile.memory + 1}
	}
	d.heldMemory += tile.memory
	defer func() { d.heldMemory -= tile.memory }()
	for y := bounds.Min.Y; y < bounds.Max.Y; {
		bottom := y + min(size.Y, bounds.Max.Y-y)
		for x := bounds.Min.X; x < bounds.Max.X; {
			right := x + min(size.X, bounds.Max.X-x)
			available, err := d.decodeLimits()
			if err != nil {
				return err
			}
			block, err := d.decodeBlock(ctx, index, tile, image.Rect(x, y, right, bottom), available)
			if err != nil {
				return fmt.Errorf("tile %d block (%d,%d): %w", index, x, y, err)
			}
			err = d.visitBlock(block, visit)
			d.cache = nil
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if d.closed {
				return fmt.Errorf("j2kgo: decoder is closed")
			}
			x = right
		}
		y = bottom
	}
	return nil
}

// decodeBlock 在剩余资源限制内重建图块及显示所需的相邻样本
// 入参: ctx 上下文, index 瓦片索引, tile 瓦片解码状态, bounds 图块边界, limits 剩余资源限制
// 返回: *Raster 图块, error 错误信息
func (d *Decoder) decodeBlock(ctx context.Context, index int, tile *tileDecoding, bounds image.Rectangle, limits Limits) (*Raster, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info := d.info
	info.Bounds = bounds
	info = reduceResolution(info, d.options.Reduce)
	result, err := newRasterExtent(ctx, info, reduceBounds(d.info.Bounds, d.options.Reduce), d.options.Reduce, limits)
	if err != nil {
		return nil, err
	}
	live := rasterMetadataSize(info)
	for _, c := range result.components {
		live += uint64(len(c.data))
	}
	if live >= limits.MaxMemoryBytes {
		return nil, &LimitError{Resource: "block memory", Limit: limits.MaxMemoryBytes, Required: live + 1}
	}
	limits.MaxMemoryBytes -= live
	if err := d.reconstructTile(ctx, tile, result, limits); err != nil {
		return nil, err
	}
	if err := d.decodeSupportingTiles(ctx, result, limits, index); err != nil {
		return nil, err
	}
	return result, nil
}

// visitBlock 释放解码器锁后调用图块回调，允许回调查询或关闭解码器
// 入参: block 独立图块, visit 图块回调
// 返回: error 回调错误
func (d *Decoder) visitBlock(block *Raster, visit func(*Raster) error) error {
	d.mu.Unlock()
	defer d.mu.Lock()
	return visit(block)
}
