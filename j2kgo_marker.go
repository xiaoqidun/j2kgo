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
	"io"
)

// JPEG2000码流标记值
const (
	markerSOC = 0xff4f
	markerSIZ = 0xff51
	markerCOD = 0xff52
	markerCOC = 0xff53
	markerTLM = 0xff55
	markerPLM = 0xff57
	markerPLT = 0xff58
	markerQCD = 0xff5c
	markerQCC = 0xff5d
	markerRGN = 0xff5e
	markerPOC = 0xff5f
	markerPPM = 0xff60
	markerPPT = 0xff61
	markerCRG = 0xff63
	markerCOM = 0xff64
	markerSOT = 0xff90
	markerSOP = 0xff91
	markerEPH = 0xff92
	markerSOD = 0xff93
	markerEOC = 0xffd9
)

// markerReader 在给定码流范围内读取标记段
type markerReader struct {
	source   *inputSource
	position int64
	end      int64
	limits   Limits
}

// markerSegment 保存标记类型、起点及参数
type markerSegment struct {
	code   uint16
	offset int64
	data   []byte
}

// next 读取一个标记，码字数据由调用方跳过
// 入参: ctx 上下文
// 返回: markerSegment 标记段, error 错误信息
func (r *markerReader) next(ctx context.Context) (markerSegment, error) {
	result := markerSegment{offset: r.position}
	var data [4]byte
	if err := r.full(ctx, data[:2]); err != nil {
		return result, err
	}
	result.code = binary.BigEndian.Uint16(data[:2])
	if result.code < 0xff00 || result.code == 0xffff || result.code == 0xff00 {
		return result, FormatError("codestream marker")
	}
	switch result.code {
	case markerSOC, markerSOD, markerEOC, markerEPH:
		return result, nil
	}
	if result.code >= 0xff30 && result.code <= 0xff3f {
		return result, nil
	}
	if err := r.full(ctx, data[2:]); err != nil {
		return result, err
	}
	length := int(binary.BigEndian.Uint16(data[2:]))
	if length < 2 {
		return result, FormatError("marker segment length")
	}
	length -= 2
	if int64(length) > r.end-r.position {
		return result, io.ErrUnexpectedEOF
	}
	if uint64(length) > r.limits.MaxMemoryBytes {
		return result, &LimitError{Resource: "marker memory", Limit: r.limits.MaxMemoryBytes, Required: uint64(length)}
	}
	result.data = make([]byte, length)
	if err := r.full(ctx, result.data); err != nil {
		return result, err
	}
	return result, nil
}

// full 读满目标缓冲区，读取范围不得超出当前头部
// 入参: ctx 上下文, data 接收缓冲
// 返回: error 错误信息
func (r *markerReader) full(ctx context.Context, data []byte) error {
	if r.position < 0 || r.position > r.end || int64(len(data)) > r.end-r.position {
		return io.ErrUnexpectedEOF
	}
	n, err := r.source.readAt(ctx, data, r.position)
	r.position += int64(n)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// defaultCoding 保存COD规定的默认编码参数
type defaultCoding struct {
	component codingStyle
	order     Progression
	layers    int
	mct       bool
	sop, eph  bool
}

// codingHeader 保存当前头部明确声明的参数，不提前应用继承规则
type codingHeader struct {
	defaults     *defaultCoding
	coding       map[int]codingStyle
	quantization *quantization
	quant        map[int]quantization
	roi          map[int]uint8
	progressions []progressionVolume
	packed       map[byte]inputRange
}

// parseCodingMarker 读取主头或瓦片首个分段头中的编码参数
// 入参: segment 标记, count 分量数, budget 元数据预算
// 返回: bool 是否为编码参数, error 错误信息
func (h *codingHeader) parseCodingMarker(segment markerSegment, count int, budget *layoutBudget) (bool, error) {
	data := segment.data
	switch segment.code {
	case markerCOD, markerCOC, markerQCD, markerQCC, markerRGN, markerPOC:
		if err := budget.add(1, uint64(len(data))+256); err != nil {
			return true, err
		}
	default:
		return false, nil
	}
	switch segment.code {
	case markerCOD:
		if h.defaults != nil || len(data) < 10 || data[0]&^7 != 0 || data[1] > byte(CPRL) || binary.BigEndian.Uint16(data[2:]) == 0 || data[4] > 1 {
			return true, FormatError("COD fields or duplication")
		}
		style, err := readCodingStyle(data[5:], data[0]&1 != 0)
		if err != nil {
			return true, err
		}
		h.defaults = &defaultCoding{component: style, order: Progression(data[1]), layers: int(binary.BigEndian.Uint16(data[2:])), mct: data[4] == 1, sop: data[0]&2 != 0, eph: data[0]&4 != 0}
	case markerCOC:
		index, tail, err := markerComponent(data, count)
		if err != nil {
			return true, err
		}
		if _, exists := h.coding[index]; exists || len(tail) < 1 || tail[0]&^1 != 0 {
			return true, FormatError("COC fields or duplication")
		}
		style, err := readCodingStyle(tail[1:], tail[0]&1 != 0)
		if err != nil {
			return true, err
		}
		if h.coding == nil {
			h.coding = make(map[int]codingStyle)
		}
		h.coding[index] = style
	case markerQCD:
		if h.quantization != nil {
			return true, FormatError("duplicate QCD")
		}
		quant, err := readQuantization(data)
		if err != nil {
			return true, err
		}
		h.quantization = &quant
	case markerQCC:
		index, tail, err := markerComponent(data, count)
		if err != nil {
			return true, err
		}
		if _, exists := h.quant[index]; exists {
			return true, FormatError("duplicate QCC")
		}
		quant, err := readQuantization(tail)
		if err != nil {
			return true, err
		}
		if h.quant == nil {
			h.quant = make(map[int]quantization)
		}
		h.quant[index] = quant
	case markerRGN:
		index, tail, err := markerComponent(data, count)
		if err != nil {
			return true, err
		}
		if _, exists := h.roi[index]; exists || len(tail) != 2 || tail[0] != 0 {
			return true, FormatError("RGN fields or duplication")
		}
		if h.roi == nil {
			h.roi = make(map[int]uint8)
		}
		h.roi[index] = tail[1]
	case markerPOC:
		if h.progressions != nil {
			return true, FormatError("duplicate POC")
		}
		var err error
		h.progressions, err = readProgressions(data, count)
		return true, err
	}
	return true, nil
}

// markerComponent 读取分量索引，字段宽度由分量总数决定
// 入参: data 标记参数, count 分量总数
// 返回: int 分量索引, []byte 剩余参数, error 错误信息
func markerComponent(data []byte, count int) (int, []byte, error) {
	width := 1
	if count >= 257 {
		width = 2
	}
	if count < 1 || count > 16384 || len(data) < width {
		return 0, nil, FormatError("component marker length")
	}
	index := int(data[0])
	if width == 2 {
		index = int(binary.BigEndian.Uint16(data))
	}
	if index >= count {
		return 0, nil, FormatError("component marker index")
	}
	return index, data[width:], nil
}

// readProgressions 解析POC中的渐进范围
// 入参: data 标记参数, count 分量总数
// 返回: []progressionVolume 渐进范围, error 错误信息
func readProgressions(data []byte, count int) ([]progressionVolume, error) {
	width := 1
	if count >= 257 {
		width = 2
	}
	size := 5 + 2*width
	if len(data) == 0 || len(data)%size != 0 {
		return nil, FormatError("POC length")
	}
	result := make([]progressionVolume, len(data)/size)
	for i := range result {
		p := data[i*size:]
		componentStart, componentEnd := int(p[1]), int(p[width+4])
		if width == 2 {
			componentStart, componentEnd = int(binary.BigEndian.Uint16(p[1:])), int(binary.BigEndian.Uint16(p[width+4:]))
		}
		if componentEnd == 0 {
			componentEnd = 256
		}
		v := progressionVolume{componentStart: componentStart, componentEnd: componentEnd, resolutionStart: int(p[0]), resolutionEnd: int(p[width+3]), layerEnd: int(binary.BigEndian.Uint16(p[width+1:])), order: Progression(p[size-1])}
		if v.componentStart >= v.componentEnd || v.componentEnd > count || v.resolutionStart >= v.resolutionEnd || v.resolutionEnd > 33 || v.layerEnd == 0 || v.order > CPRL {
			return nil, FormatError("POC bounds")
		}
		result[i] = v
	}
	return result, nil
}

// addPacked 记录PPT或PPM数据位置，不复制包头内容
// 入参: segment 标记段, budget 元数据预算
// 返回: error 错误信息
func (h *codingHeader) addPacked(segment markerSegment, budget *layoutBudget) error {
	if len(segment.data) < 2 {
		return FormatError("packed packet header length")
	}
	index := segment.data[0]
	if _, exists := h.packed[index]; exists {
		return FormatError("duplicate packed packet header index")
	}
	if err := budget.add(1, 64); err != nil {
		return err
	}
	if h.packed == nil {
		h.packed = make(map[byte]inputRange)
	}
	h.packed[index] = inputRange{offset: segment.offset + 5, length: int64(len(segment.data) - 1)}
	return nil
}
