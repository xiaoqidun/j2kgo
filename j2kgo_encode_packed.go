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

import "encoding/binary"

// packedHeaderPayload PPT标记段可容纳的最大包头字节数
const packedHeaderPayload = 65532

// packedHeaderEncoding 记录集中包头的分段信息及编码数据
type packedHeaderEncoding struct {
	length  uint64
	payload int
	markers int
	data    []byte
}

// add 统计PPT所需字节数，包头及其EPH不得跨越标记段
// 入参: length 包头字节数，包含可选EPH
// 返回: bool 是否开始新标记段, error 错误信息
func (p *packedHeaderEncoding) add(length int) (bool, error) {
	if length < 1 || length > packedHeaderPayload {
		return false, FormatError("packet header exceeds PPT marker capacity")
	}
	start := p.markers == 0 || length > packedHeaderPayload-p.payload
	if start {
		if p.markers == 256 {
			return false, &LimitError{Resource: "encoded PPT markers", Limit: 256, Required: 257}
		}
		p.markers++
		p.payload = 0
		p.length += 5
	}
	p.payload += length
	p.length += uint64(length)
	return start, nil
}

// append 将包头写入预分配缓冲，并更新所在PPT标记段的长度
// 入参: header 包头及可选EPH
// 返回: error 错误信息
func (p *packedHeaderEncoding) append(header []byte) error {
	next := *p
	start, err := next.add(len(header))
	if err != nil {
		return err
	}
	if next.length > uint64(cap(p.data)) {
		return &LimitError{Resource: "packed header buffer", Limit: uint64(cap(p.data)), Required: next.length}
	}
	data := p.data[:int(next.length)]
	offset := int(p.length)
	if start {
		binary.BigEndian.PutUint16(data[offset:], markerPPT)
		data[offset+4] = byte(next.markers - 1)
		offset += 5
	}
	copy(data[offset:], header)
	binary.BigEndian.PutUint16(data[len(data)-next.payload-3:], uint16(next.payload+3))
	next.data = data
	*p = next
	return nil
}

// emptyPackedHeaderSize 计算空数据包对应的PPT总长度
// 入参: count 数据包数量, eph 是否包含EPH
// 返回: uint64 PPT总字节数, error 错误信息
func emptyPackedHeaderSize(count uint64, eph bool) (uint64, error) {
	if count == 0 {
		return 0, nil
	}
	size := uint64(1)
	if eph {
		size += 2
	}
	markers := (count-1)/(packedHeaderPayload/size) + 1
	if markers > 256 {
		return 0, &LimitError{Resource: "encoded PPT markers", Limit: 256, Required: markers}
	}
	return count*size + markers*5, nil
}
