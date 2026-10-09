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
	"bytes"
	"context"
	"encoding/binary"
	"math"
)

// mainPacketHeaderEncoding 保存PPM包头及预编码和输出时的读写状态
type mainPacketHeaderEncoding struct {
	data      []byte
	payload   int
	markers   int
	position  int
	remaining uint64
	first     bool
	collect   bool
}

// begin 开始处理一个瓦片分段，设置待处理的包头总长度
// 入参: length 包头总字节数, budget 编码内存预算
// 返回: error 错误信息
func (p *mainPacketHeaderEncoding) begin(length uint64, budget *layoutBudget) error {
	if p.remaining != 0 || p.first {
		return FormatError("incomplete PPM tile-part")
	}
	if length > math.MaxUint32 {
		return &LimitError{Resource: "PPM tile-part header bytes", Limit: math.MaxUint32, Required: length}
	}
	p.remaining, p.first = length, true
	if length == 0 {
		return p.append(nil, budget)
	}
	return nil
}

// append 写入完整包头，或核对包头与预编码结果是否一致
// 入参: header 包头及可选EPH, budget 编码内存预算
// 返回: error 错误信息
func (p *mainPacketHeaderEncoding) append(header []byte, budget *layoutBudget) error {
	if uint64(len(header)) > p.remaining || len(header) == 0 && (!p.first || p.remaining != 0) {
		return FormatError("PPM tile-part header length")
	}
	var prefix [4]byte
	count := 0
	if p.first {
		binary.BigEndian.PutUint32(prefix[:], uint32(p.remaining))
		count = 4
	}
	size := count + len(header)
	if size > packedHeaderPayload {
		return &LimitError{Resource: "PPM packet header bytes", Limit: uint64(packedHeaderPayload - count), Required: uint64(len(header))}
	}
	if !p.collect {
		position, payload := p.position, p.payload
		if payload == 0 {
			if len(p.data)-position < 9 {
				return FormatError("encoded PPM marker count")
			}
			payload = int(binary.BigEndian.Uint16(p.data[position+2:])) - 3
			position += 5
		}
		if size > payload || size > len(p.data)-position || !bytes.Equal(p.data[position:position+count], prefix[:count]) || !bytes.Equal(p.data[position+count:position+size], header) {
			return FormatError("packet headers changed between encoding passes")
		}
		p.position, p.payload = position+size, payload-size
		p.remaining -= uint64(len(header))
		p.first = false
		return nil
	}
	tail := p.remaining - uint64(len(header))
	start := p.markers == 0 || size > packedHeaderPayload-p.payload || tail < 4 && uint64(size)+tail > uint64(packedHeaderPayload-p.payload)
	extra := size
	if start {
		if p.markers != 0 && p.payload < 4 {
			return FormatError("PPM marker payload is too short")
		}
		if p.markers == 256 {
			return &LimitError{Resource: "encoded PPM markers", Limit: 256, Required: 257}
		}
		if uint64(size)+tail < 4 {
			return FormatError("PPM marker cannot contain a complete packet header")
		}
		extra += 5
	}
	data, err := growBudgeted(p.data, extra, budget, 1)
	if err != nil {
		return err
	}
	if start {
		data = append(data, 0xff, 0x60, 0, 0, byte(p.markers))
		p.markers++
		p.payload = 0
	}
	data = append(data, prefix[:count]...)
	data = append(data, header...)
	p.payload += size
	binary.BigEndian.PutUint16(data[len(data)-p.payload-3:], uint16(p.payload+3))
	p.data, p.remaining, p.first = data, tail, false
	return nil
}

// packMainHeaders 按瓦片分段收集PPM包头，输出时核对是否与预编码一致
// 入参: ctx 上下文, plan 编码参数, parts 分段索引, header 可复用包头缓冲, main PPM包头及读写状态
// 返回: error 错误信息
func (t *tileEncoding) packMainHeaders(ctx context.Context, plan encodingPlan, parts []encodedTilePart, header []byte, main *mainPacketHeaderEncoding) error {
	part, packets := 0, uint64(0)
	if err := main.begin(parts[0].headers, &t.budget); err != nil {
		return err
	}
	return t.walkPackets(ctx, plan, func(packet packetAddress) error {
		if packets == parts[part].packets {
			part++
			packets = 0
			if err := main.begin(parts[part].headers, &t.budget); err != nil {
				return err
			}
		}
		packets++
		writer := packetWriter{data: header[:0]}
		if err := t.packetHeader(&writer, packet.area, packet.layer); err != nil {
			return err
		}
		if plan.eph {
			writer.data = append(writer.data, 0xff, 0x92)
		}
		return main.append(writer.data, &t.budget)
	})
}
