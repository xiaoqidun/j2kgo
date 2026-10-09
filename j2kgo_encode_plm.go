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
	"encoding/binary"
	"math/bits"
)

// mainPacketLengthEncoding 保存PLM长度表及预编码和输出时的读写位置
type mainPacketLengthEncoding struct {
	data     []byte
	payload  int
	markers  int
	position int
	collect  bool
}

// record 预编码时记录数据包长度，输出时核对是否一致
// 入参: group 当前瓦片分段的包长编码, budget 编码内存预算
// 返回: error 错误信息
func (p *mainPacketLengthEncoding) record(group []byte, budget *layoutBudget) error {
	if len(group) > 255 {
		return &LimitError{Resource: "PLM tile-part bytes", Limit: 255, Required: uint64(len(group))}
	}
	if !p.collect {
		position, payload := p.position, p.payload
		for i := 0; i <= len(group); i++ {
			if payload == 0 {
				if len(p.data)-position < 6 {
					return FormatError("encoded PLM tile-part count")
				}
				payload = int(binary.BigEndian.Uint16(p.data[position+2:])) - 3
				position += 5
			}
			value := byte(len(group))
			if i > 0 {
				value = group[i-1]
			}
			if payload < 1 || position >= len(p.data) || p.data[position] != value {
				return FormatError("packet lengths changed between encoding passes")
			}
			position++
			payload--
		}
		p.position, p.payload = position, payload
		return nil
	}
	var buffer [266]byte
	offset, head, payload, markers := 0, -1, p.payload, p.markers
	previousPayload := p.payload
	finish := func() {
		if head < 0 {
			previousPayload = payload
		} else {
			binary.BigEndian.PutUint16(buffer[head+2:], uint16(payload+3))
		}
	}
	start := func() error {
		if markers == 256 {
			return &LimitError{Resource: "encoded PLM markers", Limit: 256, Required: 257}
		}
		finish()
		head = offset
		binary.BigEndian.PutUint16(buffer[offset:], markerPLM)
		buffer[offset+4] = byte(markers)
		offset += 5
		markers++
		payload = 0
		return nil
	}
	for first, i := true, 0; first || i < len(group); first = false {
		end := i
		for end < len(group) {
			value := group[end]
			end++
			if value&128 == 0 {
				break
			}
		}
		if end > i && group[end-1]&128 != 0 {
			return FormatError("incomplete PLM packet length")
		}
		size := end - i
		if first {
			size++
		}
		if markers == 0 || size > packetLengthPayload-payload {
			if err := start(); err != nil {
				return err
			}
		}
		if first {
			buffer[offset] = byte(len(group))
			offset++
		}
		copy(buffer[offset:], group[i:end])
		offset += end - i
		payload += size
		i = end
	}
	finish()
	data, err := appendBudgeted(p.data, buffer[:offset], budget, 1)
	if err != nil {
		return err
	}
	if p.markers > 0 {
		binary.BigEndian.PutUint16(data[len(p.data)-p.payload-3:], uint16(previousPayload+3))
	}
	p.markers, p.payload = markers, payload
	p.data = data
	return nil
}

// appendMainPacketLength 按七位分组追加包长，同一瓦片分段的包长编码不得超过255字节
// 入参: group 当前瓦片分段的包长编码, length 数据包字节数
// 返回: []byte 追加后的包长编码, error 错误信息
func appendMainPacketLength(group []byte, length uint64) ([]byte, error) {
	count := max(1, (bits.Len64(length)+6)/7)
	if len(group)+count > 255 {
		return group, &LimitError{Resource: "PLM tile-part bytes", Limit: 255, Required: uint64(len(group) + count)}
	}
	for i := range count {
		value := byte(length >> (7 * (count - i - 1)) & 127)
		if i != count-1 {
			value |= 128
		}
		group = append(group, value)
	}
	return group, nil
}
