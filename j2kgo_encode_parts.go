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

// encodedTilePartMemory 单个瓦片分段索引的内存预算，不含PLT和PPT缓冲
const encodedTilePartMemory = 120

// encodedTilePart 保存瓦片分段的长度、数据包数量及PLT和PPT数据
type encodedTilePart struct {
	length  uint64
	packets uint64
	headers uint64
	table   packetLengthEncoding
	packed  packedHeaderEncoding
}

// roiHeaderSize 计算首个瓦片分段中ROI标记的总字节数
// 返回: uint64 字节数
func (t *tileEncoding) roiHeaderSize() uint64 {
	var length uint64
	for _, layout := range t.layouts {
		if layout.roi != 0 {
			length += 7
			if len(t.layouts) > 256 {
				length++
			}
		}
	}
	return length
}

// measureTileParts 统计瓦片分段长度，按编码选项处理长度表和集中包头的开销
// 入参: ctx 上下文, plan 编码参数, main PLM长度表，nil时跳过记录和核对
// 返回: []encodedTilePart 分段索引，使用后须调用releaseTileParts归还预算, int 最大包头字节数, error 错误信息
func (t *tileEncoding) measureTileParts(ctx context.Context, plan encodingPlan, main *mainPacketLengthEncoding) ([]encodedTilePart, int, error) {
	parts, err := appendBudgeted(nil, []encodedTilePart{{length: 14 + t.roiHeaderSize() + plan.parameterBytes}}, &t.budget, encodedTilePartMemory)
	if err != nil {
		return nil, 0, err
	}
	var buffer [255]byte
	group := buffer[:0]
	_, maximum, err := t.measurePackets(ctx, plan, func(length uint64, header int) error {
		if plan.partPackets > 0 && parts[len(parts)-1].packets == uint64(plan.partPackets) {
			if len(parts) == 255 {
				return &LimitError{Resource: "encoded tile-parts", Limit: 255, Required: 256}
			}
			if main != nil {
				if err := main.record(group, &t.budget); err != nil {
					return err
				}
				group = buffer[:0]
			}
			var err error
			parts, err = appendBudgeted(parts, []encodedTilePart{{length: 14}}, &t.budget, encodedTilePartMemory)
			if err != nil {
				return err
			}
		}
		part := &parts[len(parts)-1]
		if plan.ppt || plan.ppm {
			length -= uint64(header)
		}
		part.headers += uint64(header)
		if main != nil {
			var err error
			group, err = appendMainPacketLength(group, length)
			if err != nil {
				return err
			}
		}
		if plan.plt {
			before := part.table.length
			if err := part.table.add(length, &t.budget); err != nil {
				return err
			}
			length += part.table.length - before
		}
		if plan.ppt {
			before := part.packed.length
			if _, err := part.packed.add(header); err != nil {
				return err
			}
			length += part.packed.length - before
		}
		length, err := checkTotal("encoded tile-part", part.length, length, math.MaxUint32)
		if err != nil {
			return err
		}
		part.length = length
		part.packets++
		return nil
	})
	if err == nil && main != nil {
		err = main.record(group, &t.budget)
	}
	if err != nil {
		t.releaseTileParts(parts)
		return nil, 0, err
	}
	return parts, maximum, nil
}

// releaseTileParts 归还分段索引、长度表及集中包头的内存预算
// 入参: parts 瓦片分段信息
func (t *tileEncoding) releaseTileParts(parts []encodedTilePart) {
	for _, part := range parts {
		t.budget.used -= uint64(cap(part.table.data)) + uint64(cap(part.packed.data))
	}
	t.budget.used -= uint64(cap(parts)) * encodedTilePartMemory
}

// write 按渐进顺序写入瓦片分段，保留跨分段的包头状态及包序号
// 入参: w 码流写入器, plan 编码参数, index 瓦片序号
// 返回: error 错误信息
func (t *tileEncoding) write(w *codestreamWriter, plan encodingPlan, index int) error {
	parts, headerSize, err := t.measureTileParts(w.ctx, plan, w.packetLengths)
	if err != nil {
		return err
	}
	defer t.releaseTileParts(parts)
	if w.tileLengths != nil {
		if err := w.tileLengths.record(index, parts); err != nil {
			return err
		}
	}
	if w.measureOnly && w.packetHeaders == nil {
		return nil
	}
	if err := t.budget.add(uint64(headerSize), 1); err != nil {
		return err
	}
	defer func() { t.budget.used -= uint64(headerSize) }()
	header := make([]byte, 0, headerSize)
	if w.packetHeaders != nil {
		if err := t.packMainHeaders(w.ctx, plan, parts, header, w.packetHeaders); err != nil {
			return err
		}
	}
	if w.measureOnly {
		return nil
	}
	if plan.ppt {
		if err := t.packHeaders(w.ctx, plan, parts, header); err != nil {
			return err
		}
	}
	if err := t.partHeader(w, plan, index, 0, len(parts), parts[0]); err != nil {
		return err
	}
	part, packets, sequence := 0, uint64(0), uint16(0)
	return t.walkPackets(w.ctx, plan, func(packet packetAddress) error {
		if packets == parts[part].packets {
			part++
			packets = 0
			if err := t.partHeader(w, plan, index, part, len(parts), parts[part]); err != nil {
				return err
			}
		}
		packets++
		if plan.sop {
			var data [2]byte
			binary.BigEndian.PutUint16(data[:], sequence)
			if err := w.shortMarker(markerSOP, data[:]); err != nil {
				return err
			}
		}
		sequence++
		if !plan.ppt && !plan.ppm {
			writer := packetWriter{data: header[:0]}
			if err := t.packetHeader(&writer, packet.area, packet.layer); err != nil {
				return err
			}
			if err := w.write(writer.data); err != nil {
				return err
			}
			if plan.eph {
				if err := w.marker(markerEPH, nil); err != nil {
					return err
				}
			}
		}
		for _, band := range packet.area.bands {
			for i := range band.blocks {
				if err := t.contribution(&band.blocks[i], packet.layer, func(segment blockSegment) error { return w.write(segment.data) }); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// partHeader 写入瓦片分段头，首段包含专用编码及ROI参数
// 入参: w 码流写入器, plan 编码参数, tile 瓦片索引, part 分段索引, count 分段总数, encoded 分段信息
// 返回: error 错误信息
func (t *tileEncoding) partHeader(w *codestreamWriter, plan encodingPlan, tile, part, count int, encoded encodedTilePart) error {
	var data [8]byte
	binary.BigEndian.PutUint16(data[:2], uint16(tile))
	binary.BigEndian.PutUint32(data[2:6], uint32(encoded.length))
	data[6], data[7] = byte(part), byte(count)
	if err := w.shortMarker(markerSOT, data[:]); err != nil {
		return err
	}
	if part == 0 {
		if plan.parameterBytes != 0 {
			if err := w.codingHeader(plan); err != nil {
				return err
			}
		}
		for c, layout := range t.layouts {
			if layout.roi == 0 {
				continue
			}
			var buffer [4]byte
			region := appendComponentIndex(buffer[:0], c, len(t.layouts))
			region = append(region, 0, layout.roi)
			if err := w.shortMarker(markerRGN, region); err != nil {
				return err
			}
		}
	}
	if err := w.write(encoded.table.data); err != nil {
		return err
	}
	if err := w.write(encoded.packed.data); err != nil {
		return err
	}
	return w.marker(markerSOD, nil)
}

// packHeaders 为各瓦片分段生成PPT数据，缓冲占用计入瓦片内存预算
// 入参: ctx 上下文, plan 编码参数, parts 分段索引, header 可复用包头缓冲
// 返回: error 错误信息
func (t *tileEncoding) packHeaders(ctx context.Context, plan encodingPlan, parts []encodedTilePart, header []byte) error {
	for i := range parts {
		if err := ctx.Err(); err != nil {
			return err
		}
		size := parts[i].packed.length
		if err := t.budget.add(size, 1); err != nil {
			return err
		}
		parts[i].packed = packedHeaderEncoding{data: make([]byte, 0, int(size))}
	}
	part, packets := 0, uint64(0)
	return t.walkPackets(ctx, plan, func(packet packetAddress) error {
		if packets == parts[part].packets {
			part++
			packets = 0
		}
		packets++
		writer := packetWriter{data: header[:0]}
		if err := t.packetHeader(&writer, packet.area, packet.layer); err != nil {
			return err
		}
		if plan.eph {
			writer.data = append(writer.data, 0xff, 0x92)
		}
		return parts[part].packed.append(writer.data)
	})
}
