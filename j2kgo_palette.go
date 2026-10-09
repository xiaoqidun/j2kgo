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

// MappingType 通道映射方式，分为直接映射和调色板映射
type MappingType uint8

const (
	MappingDirect  MappingType = iota // 分量直接映射到通道
	MappingPalette                    // 分量作为调色板索引
)

// ChannelMapping 描述一个逻辑通道的分量来源及调色板列
type ChannelMapping struct {
	Component uint16
	Type      MappingType
	Column    uint8
}

// PaletteColumn 保存一列调色板的精度、符号及样本
type PaletteColumn struct {
	Precision uint8
	Signed    bool
	Values    []int64
}

// validateMapping 校验调色板列及通道映射
// 入参: info 图像信息
// 返回: error 错误信息
func validateMapping(info Info) error {
	if (len(info.Palette) == 0) != (len(info.Mapping) == 0) || len(info.Palette) > 255 || len(info.Mapping) > 65536 {
		return FormatError("palette and component mapping")
	}
	for _, column := range info.Palette {
		if column.Precision == 0 || column.Precision > 38 || len(column.Values) == 0 || len(column.Values) > 1024 || len(column.Values) != len(info.Palette[0].Values) {
			return FormatError("palette precision or entry count")
		}
		lo, hi := sampleRange(ComponentInfo{Precision: column.Precision, Signed: column.Signed})
		for _, value := range column.Values {
			if value < lo || value > hi {
				return FormatError("palette sample range")
			}
		}
	}
	for _, mapping := range info.Mapping {
		if int(mapping.Component) >= len(info.Components) || mapping.Type > MappingPalette || (mapping.Type == MappingDirect && mapping.Column != 0) || (mapping.Type == MappingPalette && int(mapping.Column) >= len(info.Palette)) {
			return FormatError("component mapping fields")
		}
	}
	return nil
}

// channelSpec 返回映射后通道的精度、符号及网格
// 入参: info 图像信息, index 通道索引
// 返回: ComponentInfo 通道样本描述
func channelSpec(info Info, index int) ComponentInfo {
	if len(info.Mapping) == 0 {
		return info.Components[index]
	}
	m := info.Mapping[index]
	c := info.Components[m.Component]
	if m.Type == MappingPalette {
		column := info.Palette[m.Column]
		c.Precision, c.Signed = column.Precision, column.Signed
	}
	return c
}

// channelCount 返回逻辑通道数量
// 入参: info 图像信息
// 返回: int 通道数量
func channelCount(info Info) int {
	if len(info.Mapping) > 0 {
		return len(info.Mapping)
	}
	return len(info.Components)
}

// paletteBox 读取调色板并检查各列精度与填充位
// 入参: header JP2头部信息, length 内容字节数
// 返回: error 错误信息
func (h *headerReader) paletteBox(header *jp2Header, length uint64) error {
	if header.palette != nil || length < 3 {
		return FormatError("palette box")
	}
	var fixed [3]byte
	if err := h.full(fixed[:]); err != nil {
		return err
	}
	entries, columns := int(binary.BigEndian.Uint16(fixed[:2])), int(fixed[2])
	if entries < 1 || entries > 1024 || columns < 1 || length < uint64(3+columns) {
		return FormatError("palette dimensions")
	}
	if err := h.reserve(uint64(columns), uint64(33+8*entries)); err != nil {
		return err
	}
	depths := make([]byte, columns)
	if err := h.full(depths); err != nil {
		return err
	}
	stride := 0
	for _, depth := range depths {
		precision := (depth & 127) + 1
		if precision > 38 {
			return FormatError("palette precision")
		}
		stride += int(precision+7) / 8
	}
	if length != uint64(3+columns+stride*entries) {
		return FormatError("palette length")
	}
	header.palette = make([]PaletteColumn, columns)
	for i, depth := range depths {
		header.palette[i] = PaletteColumn{Precision: (depth & 127) + 1, Signed: depth&128 != 0, Values: make([]int64, entries)}
	}
	var data [5]byte
	for row := range entries {
		for _, column := range header.palette {
			n := int(column.Precision+7) / 8
			if err := h.full(data[:n]); err != nil {
				return err
			}
			var value uint64
			for _, b := range data[:n] {
				value = value<<8 | uint64(b)
			}
			if value>>column.Precision != 0 {
				return FormatError("palette padding bits")
			}
			v := int64(value)
			if column.Signed {
				shift := 64 - column.Precision
				v = v << shift >> shift
			}
			column.Values[row] = v
		}
	}
	return nil
}

// mappingBox 读取通道映射，待JP2头部读取完成后校验分量和调色板引用
// 入参: header JP2头部信息, length 内容字节数
// 返回: error 错误信息
func (h *headerReader) mappingBox(header *jp2Header, length uint64) error {
	if header.mapping != nil || length == 0 || length%4 != 0 || length/4 > 65536 {
		return FormatError("component mapping box")
	}
	if err := h.reserve(length/4, 8); err != nil {
		return err
	}
	header.mapping = make([]ChannelMapping, int(length/4))
	var data [4]byte
	for i := range header.mapping {
		if err := h.full(data[:]); err != nil {
			return err
		}
		header.mapping[i] = ChannelMapping{Component: binary.BigEndian.Uint16(data[:2]), Type: MappingType(data[2]), Column: data[3]}
	}
	return nil
}

// appendPaletteBoxes 写入调色板及对应通道映射
// 入参: header 容器头缓冲, info 图像信息
// 返回: []byte 容器头缓冲
func appendPaletteBoxes(header []byte, info Info) []byte {
	if len(info.Palette) == 0 {
		return header
	}
	entries := len(info.Palette[0].Values)
	data := binary.BigEndian.AppendUint16(nil, uint16(entries))
	data = append(data, byte(len(info.Palette)))
	for _, column := range info.Palette {
		data = append(data, encodedPrecision(ComponentInfo{Precision: column.Precision, Signed: column.Signed}))
	}
	for row := range entries {
		for _, column := range info.Palette {
			value := uint64(column.Values[row]) & (uint64(1)<<column.Precision - 1)
			for shift := (int(column.Precision+7)/8 - 1) * 8; shift >= 0; shift -= 8 {
				data = append(data, byte(value>>shift))
			}
		}
	}
	header = appendBox(header, "pclr", data)
	data = make([]byte, 0, len(info.Mapping)*4)
	for _, mapping := range info.Mapping {
		data = binary.BigEndian.AppendUint16(data, mapping.Component)
		data = append(data, byte(mapping.Type), mapping.Column)
	}
	return appendBox(header, "cmap", data)
}
