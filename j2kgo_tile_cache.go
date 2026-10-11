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
	"errors"
)

// tileCache 保存区域读取的瓦片索引及建立索引时的预算
type tileCache struct {
	index  int
	budget uint64
	tile   *tileDecoding
}

// tileCacheMemory 统计缓存条目及瓦片索引占用
// 返回: uint64 缓存内存字节数
func (d *Decoder) tileCacheMemory() uint64 {
	memory := uint64(cap(d.cache)) * 32
	for _, entry := range d.cache {
		if entry.tile != nil {
			memory += entry.tile.memory
		}
	}
	return memory
}

// prepareTileCache 保留当前区域的局部瓦片索引，缓存不超过64项及可用内存的八分之一
// 入参: ctx 上下文, result 目标图像, limits 可用资源限制
// 返回: error 取消错误
func (d *Decoder) prepareTileCache(ctx context.Context, result *Raster, limits Limits) error {
	if result.info.Bounds == result.extent {
		d.cache = nil
		return ctx.Err()
	}
	var next [64]tileCache
	count := 0
	support := result.supportBounds()
	area := d.index.tileRange(support, d.options.Reduce)
	for y := area.Min.Y; y < area.Max.Y; y++ {
		for x := area.Min.X; x < area.Max.X; x++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			index := y*d.index.columns + x
			bounds := reduceBounds(d.index.tileBounds(index), d.options.Reduce)
			if bounds.In(support) || !result.needsTile(bounds) {
				continue
			}
			if count == len(next) {
				d.cache = nil
				return nil
			}
			next[count].index = index
			for _, entry := range d.cache {
				if entry.index == index {
					next[count] = entry
					break
				}
			}
			count++
		}
	}
	budget := min(uint64(8<<20), limits.MaxMemoryBytes/8)
	capacity := max(count, cap(d.cache))
	metadata := uint64(capacity) * 32
	if count == 0 || budget <= metadata {
		d.cache = nil
		return nil
	}
	share := (budget - metadata) / uint64(count)
	for i := range next[:count] {
		if next[i].tile != nil && next[i].tile.memory > share {
			next[i].tile = nil
			next[i].budget = share
		}
	}
	if cap(d.cache) < count {
		d.cache = make([]tileCache, count)
	} else {
		clear(d.cache)
		d.cache = d.cache[:count]
	}
	copy(d.cache, next[:count])
	return nil
}

// decodeCachedTile 复用局部瓦片的包索引，索引超出预算时按目标区域解码
// 入参: ctx 上下文, index 瓦片索引, result 目标图像, limits 可用资源限制
// 返回: error 错误信息
func (d *Decoder) decodeCachedTile(ctx context.Context, index int, result *Raster, limits Limits) error {
	var cached *tileCache
	for i := range d.cache {
		if d.cache[i].index == index {
			cached = &d.cache[i]
			break
		}
	}
	if cached != nil {
		budget := (min(uint64(8<<20), limits.MaxMemoryBytes/8) - uint64(cap(d.cache))*32) / uint64(len(d.cache))
		if cached.tile == nil && cached.budget < budget {
			cached.budget = budget
			available := limits
			available.MaxMemoryBytes = budget
			bounds := reduceBounds(d.index.tileBounds(index), d.options.Reduce)
			tile, err := d.prepareTile(ctx, index, bounds, available)
			if err != nil {
				var limitErr *LimitError
				if !errors.As(err, &limitErr) {
					cached.budget = 0
					return err
				}
			} else {
				cached.tile = tile
			}
		}
	}
	limits.MaxMemoryBytes -= d.tileCacheMemory()
	if cached != nil && cached.tile != nil {
		return d.reconstructTile(ctx, cached.tile, result, limits)
	}
	return d.decodeTile(ctx, index, result, limits)
}
