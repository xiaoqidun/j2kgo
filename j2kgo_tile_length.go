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
)

// tileLength 保存TLM瓦片编号及分段长度，内部以65535表示省略编号
type tileLength struct {
	length uint32
	tile   uint16
}

// readTileLengths 读取TLM各条目并限制临时索引内存
// 入参: data 标记段内容, budget 索引预算
// 返回: error 错误信息
func (s *streamIndex) readTileLengths(data []byte, budget *layoutBudget) error {
	if len(data) < 4 {
		return FormatError("TLM length")
	}
	index, style := data[0], data[1]
	if _, exists := s.tileLengths[index]; exists {
		return FormatError("duplicate TLM index")
	}
	tileWidth, lengthWidth := int((style>>4)&3), 2+2*int((style>>6)&1)
	width := tileWidth + lengthWidth
	if style&0x8f != 0 || tileWidth == 3 || (len(data)-2)%width != 0 {
		return FormatError("TLM fields")
	}
	count := (len(data) - 2) / width
	for pos := 2; pos < len(data); pos += width {
		if tileWidth == 1 && data[pos] == 255 {
			return FormatError("TLM tile index")
		}
		if tileWidth == 2 && binary.BigEndian.Uint16(data[pos:]) == 65535 {
			return FormatError("TLM tile index")
		}
		length := uint32(binary.BigEndian.Uint16(data[pos+tileWidth:]))
		if lengthWidth == 4 {
			length = binary.BigEndian.Uint32(data[pos+tileWidth:])
		}
		if length < 14 {
			return FormatError("TLM tile-part length")
		}
	}
	if err := budget.add(1, uint64(count)*8+128); err != nil {
		return err
	}
	entries := make([]tileLength, count)
	for i := range entries {
		pos := 2 + i*width
		entry := &entries[i]
		entry.tile = 65535
		if tileWidth == 1 {
			entry.tile = uint16(data[pos])
		} else if tileWidth == 2 {
			entry.tile = binary.BigEndian.Uint16(data[pos:])
		}
		entry.length = uint32(binary.BigEndian.Uint16(data[pos+tileWidth:]))
		if lengthWidth == 4 {
			entry.length = binary.BigEndian.Uint32(data[pos+tileWidth:])
		}
	}
	if s.tileLengths == nil {
		s.tileLengths = make(map[byte][]tileLength)
	}
	s.tileLengths[index] = entries
	return nil
}

// validateTileLengths 按Ztlm顺序核对全部SOT分段，校验后释放临时表
// 入参: ctx 上下文, budget 索引预算
// 返回: error 错误信息
func (s *streamIndex) validateTileLengths(ctx context.Context, budget *layoutBudget) error {
	if len(s.tileLengths) == 0 {
		return ctx.Err()
	}
	position, implicit := 0, false
	for index := range 256 {
		for _, entry := range s.tileLengths[byte(index)] {
			if err := ctx.Err(); err != nil {
				return err
			}
			if position >= len(s.parts) {
				return FormatError("extra TLM tile-part")
			}
			part := s.parts[position]
			tile := int(entry.tile)
			if entry.tile == 65535 {
				implicit, tile = true, position
			}
			if tile != int(part.tile) || int64(entry.length) != part.data.offset+part.data.length-part.start {
				return FormatError("TLM disagrees with SOT tile-part")
			}
			position++
		}
	}
	if position != len(s.parts) {
		return FormatError("missing TLM tile-part")
	}
	if implicit {
		if len(s.parts) != len(s.tiles) {
			return FormatError("implicit TLM requires one tile-part per tile")
		}
		for i, part := range s.parts {
			if int(part.tile) != i {
				return FormatError("implicit TLM tile order")
			}
		}
	}
	s.releaseTileLengths(budget)
	return nil
}

// releaseTileLengths 释放TLM临时索引及其内存预算
// 入参: budget 索引预算
func (s *streamIndex) releaseTileLengths(budget *layoutBudget) {
	for _, entries := range s.tileLengths {
		budget.used -= uint64(cap(entries))*8 + 128
	}
	s.tileLengths = nil
}
