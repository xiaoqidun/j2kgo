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
	"encoding/binary"
	"math"
)

// tileLengthEntries 每条记录占六字节时，单个TLM标记段的最大记录数
const tileLengthEntries = (65535 - 4) / 6

// tileLengthEncoding 保存瓦片分段长度表及当前记录位置
type tileLengthEncoding struct {
	data    []byte
	count   int
	next    int
	collect bool
}

// prepareTileLengths 统计瓦片分段数量并分配长度表，未启用TLM时返回nil
// 入参: ctx 上下文
// 返回: *tileLengthEncoding 分段长度表, error 错误信息
func (p *encodingPlan) prepareTileLengths(ctx context.Context) (*tileLengthEncoding, error) {
	if !p.tlm {
		return nil, nil
	}
	var count uint64
	for tile := 0; tile < p.columns*p.rows; tile++ {
		local := p.tilePlan(tile)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var cost tileLayerCost
		if p.tileCosts != nil {
			cost = p.tileCosts[tile]
		} else {
			var err error
			cost, err = local.tileLayerCost(ctx, tile)
			if err != nil {
				return nil, err
			}
		}
		packets, err := checkedProduct("packet count", cost.packets, uint64(local.layerCount), math.MaxUint64)
		if err != nil {
			return nil, err
		}
		parts := uint64(1)
		if local.partPackets > 0 && packets > 0 {
			parts = (packets-1)/uint64(local.partPackets) + 1
		}
		if parts > 255 {
			return nil, &LimitError{Resource: "encoded tile-parts", Limit: 255, Required: parts}
		}
		count, err = checkTotal("encoded TLM entries", count, parts, tileLengthEntries*256)
		if err != nil {
			return nil, err
		}
	}
	result, err := newTileLengthEncoding(int(count), p.limits.MaxMemoryBytes)
	if err != nil {
		return nil, err
	}
	p.limits.MaxMemoryBytes -= uint64(len(result.data)) + 48
	return result, nil
}

// newTileLengthEncoding 创建TLM长度表，使用两字节瓦片编号和四字节分段长度
// 入参: count 分段总数, memory 可用内存字节数
// 返回: *tileLengthEncoding 分段长度表, error 错误信息
func newTileLengthEncoding(count int, memory uint64) (*tileLengthEncoding, error) {
	if count < 1 {
		return nil, FormatError("encoded TLM tile-part count")
	}
	if count > tileLengthEntries*256 {
		return nil, &LimitError{Resource: "encoded TLM entries", Limit: tileLengthEntries * 256, Required: uint64(count)}
	}
	markers := (count-1)/tileLengthEntries + 1
	size := 6 * (count + markers)
	if uint64(size)+48 >= memory {
		return nil, &LimitError{Resource: "encoded TLM memory", Limit: memory, Required: uint64(size) + 49}
	}
	result := &tileLengthEncoding{data: make([]byte, size), count: count, collect: true}
	for offset, index, remaining := 0, 0, count; remaining > 0; index++ {
		entries := min(remaining, tileLengthEntries)
		data := result.data[offset:]
		binary.BigEndian.PutUint16(data, markerTLM)
		binary.BigEndian.PutUint16(data[2:], uint16(4+6*entries))
		data[4], data[5] = byte(index), 0x60
		offset += 6 + 6*entries
		remaining -= entries
	}
	return result, nil
}

// record 预编码时记录瓦片编号和分段长度，输出时核对是否一致
// 入参: tile 瓦片编号, parts 瓦片分段信息
// 返回: error 错误信息
func (e *tileLengthEncoding) record(tile int, parts []encodedTilePart) error {
	if tile < 0 || tile > 65534 || len(parts) > e.count-e.next {
		return FormatError("encoded TLM tile index or count")
	}
	for _, part := range parts {
		if part.length < 14 || part.length > math.MaxUint32 {
			return FormatError("encoded TLM tile-part length")
		}
		offset := 6 * (e.next + 1 + e.next/tileLengthEntries)
		data := e.data[offset : offset+6]
		if e.collect {
			binary.BigEndian.PutUint16(data, uint16(tile))
			binary.BigEndian.PutUint32(data[2:], uint32(part.length))
		} else if binary.BigEndian.Uint16(data) != uint16(tile) || binary.BigEndian.Uint32(data[2:]) != uint32(part.length) {
			return FormatError("tile-part changed between encoding passes")
		}
		e.next++
	}
	return nil
}
