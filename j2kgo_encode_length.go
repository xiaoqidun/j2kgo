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

// packetLengthPayload PLT标记段中长度列表的最大字节数
const packetLengthPayload = 65532

// packetLengthEncoding 记录PLT标记段的长度和可选编码数据
type packetLengthEncoding struct {
	length  uint64
	payload int
	markers int
	data    []byte
}

// add 按七位分组编码数据包长度，每个长度值完整写入同一标记段
// 入参: length 数据包字节数, budget 内存预算，nil时仅统计长度
// 返回: error 错误信息
func (p *packetLengthEncoding) add(length uint64, budget *layoutBudget) error {
	count := max(1, (bits.Len64(length)+6)/7)
	start := p.markers == 0 || p.payload+count > packetLengthPayload
	if start && p.markers == 256 {
		return &LimitError{Resource: "encoded PLT markers", Limit: 256, Required: 257}
	}
	var buffer [15]byte
	offset := 0
	if start {
		binary.BigEndian.PutUint16(buffer[:2], markerPLT)
		buffer[4] = byte(p.markers)
		offset = 5
	}
	for i := range count {
		value := byte(length >> (7 * (count - i - 1)) & 127)
		if i != count-1 {
			value |= 128
		}
		buffer[offset+i] = value
	}
	if budget != nil {
		var err error
		p.data, err = appendBudgeted(p.data, buffer[:offset+count], budget, 1)
		if err != nil {
			return err
		}
	}
	if start {
		p.markers++
		p.payload = 0
	}
	p.payload += count
	p.length += uint64(offset + count)
	if budget != nil {
		binary.BigEndian.PutUint16(p.data[len(p.data)-p.payload-3:], uint16(p.payload+3))
	}
	return nil
}

// emptyPacketLengthSize 计算空数据包对应的PLT总长度，每个包长占一个编码字节
// 入参: count 数据包数量
// 返回: uint64 PLT总字节数, error 错误信息
func emptyPacketLengthSize(count uint64) (uint64, error) {
	if count == 0 {
		return 0, nil
	}
	markers := (count-1)/packetLengthPayload + 1
	if markers > 256 {
		return 0, &LimitError{Resource: "encoded PLT markers", Limit: 256, Required: markers}
	}
	return count + markers*5, nil
}
