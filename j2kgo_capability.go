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

// capabilityHeader 校验主头中能力与Profile标记的结构及顺序
type capabilityHeader struct {
	rsiz    uint16
	cap     bool
	profile bool
	closed  bool
}

// parse 读取CAP和PRF，允许忽略未被声明为解码必需的扩展能力
// 入参: segment 标记段
// 返回: bool 是否为能力或Profile标记, error 错误信息
func (h *capabilityHeader) parse(segment markerSegment) (bool, error) {
	switch segment.code {
	case markerCAP:
		if h.closed || h.cap || h.profile {
			return true, FormatError("CAP order or duplicate")
		}
		data := segment.data
		if len(data) < 6 || len(data) != 4+2*bits.OnesCount32(binary.BigEndian.Uint32(data)) {
			return true, FormatError("CAP length")
		}
		h.cap = true
		if h.rsiz&0x4000 != 0 {
			return true, UnsupportedError("required extended JPEG2000 capabilities")
		}
		return true, nil
	case markerPRF:
		if h.closed || h.profile || h.rsiz < 0x0fff {
			return true, FormatError("PRF order, duplicate or conflicting Rsiz")
		}
		data := segment.data
		if len(data) < 2 || len(data)%2 != 0 || binary.BigEndian.Uint16(data[len(data)-2:]) == 0 {
			return true, FormatError("PRF length or profile number")
		}
		h.profile = true
		return true, nil
	case markerCOD, markerCOC, markerTLM, markerPLM, markerQCD, markerQCC, markerRGN, markerPOC, markerPPM, markerCRG, markerCOM, markerSOT:
		h.closed = true
	}
	return false, nil
}

// complete 检查SIZ声明所要求的能力与Profile标记是否存在
// 返回: error 错误信息
func (h capabilityHeader) complete() error {
	if h.rsiz&0x4000 != 0 && !h.cap {
		return FormatError("Rsiz requires CAP")
	}
	if h.rsiz&0x0fff == 0x0fff && !h.profile {
		return FormatError("Rsiz requires PRF")
	}
	return nil
}
