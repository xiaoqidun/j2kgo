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

// validateChannels 校验通道定义，检查颜色和透明度的关联关系
// 入参: info 图像信息
// 返回: error 错误信息
func validateChannels(info Info) error {
	if len(info.Channels) == 0 {
		return nil
	}
	if len(info.Channels) > 65535 {
		return FormatError("channel description count")
	}
	colors, opacity := make(map[uint16]bool), make(map[uint16]bool)
	for _, c := range info.Channels {
		if int(c.Index) >= channelCount(info) || (c.Type > ChannelPremultiplied && c.Type != ChannelUnspecified) {
			return FormatError("channel index or type")
		}
		if (c.Type == ChannelOpacity || c.Type == ChannelPremultiplied) && channelSpec(info, int(c.Index)).Signed {
			return FormatError("signed opacity channel")
		}
		if c.Association == 65535 || c.Type == ChannelUnspecified {
			continue
		}
		if c.Type == ChannelColor {
			if colors[c.Association] {
				return FormatError("duplicate color association")
			}
			colors[c.Association] = true
		} else {
			if opacity[c.Association] || opacity[0] || (c.Association == 0 && len(opacity) != 0) {
				return FormatError("conflicting opacity association")
			}
			opacity[c.Association] = true
		}
	}
	count := colorChannelCount(info)
	for i := 1; i <= count; i++ {
		if !colors[uint16(i)] {
			return FormatError("missing color association")
		}
	}
	return nil
}

// colorChannelCount 返回颜色空间所需的通道数
// 入参: info 图像信息
// 返回: int 通道数，颜色空间未知时返回零
func colorChannelCount(info Info) int {
	if len(info.ICCProfile) != 0 {
		if len(info.ICCProfile) >= 20 {
			switch string(info.ICCProfile[16:20]) {
			case "GRAY":
				return 1
			case "RGB ":
				return 3
			}
		}
		return 0
	}
	switch info.ColorSpace {
	case ColorGray:
		return 1
	case ColorSRGB, ColorSYCC:
		return 3
	}
	return 0
}

// validateColorChannels 检查颜色通道数量，枚举颜色空间仅接受无符号样本
// 入参: info 图像信息
// 返回: error 错误信息
func validateColorChannels(info Info) error {
	count := colorChannelCount(info)
	if count == 0 {
		return nil
	}
	if channelCount(info) < count {
		return FormatError("insufficient color channels")
	}
	indices := [3]int{0, 1, 2}
	for _, channel := range info.Channels {
		if channel.Type == ChannelColor && channel.Association >= 1 && int(channel.Association) <= count {
			indices[channel.Association-1] = int(channel.Index)
		}
	}
	for _, index := range indices[:count] {
		if len(info.ICCProfile) == 0 && channelSpec(info, index).Signed {
			return FormatError("enumerated color space requires unsigned channels")
		}
	}
	return nil
}

// channelBox 读取JP2通道定义
// 入参: header JP2头部信息, length 内容字节数
// 返回: error 错误信息
func (h *headerReader) channelBox(header *jp2Header, length uint64) error {
	if header.channels != nil || length < 2 {
		return FormatError("channel definition box")
	}
	var data [6]byte
	if err := h.full(data[:2]); err != nil {
		return err
	}
	count := int(binary.BigEndian.Uint16(data[:2]))
	if count == 0 || uint64(count)*6+2 != length {
		return FormatError("channel definition length")
	}
	if err := h.reserve(uint64(count), 8); err != nil {
		return err
	}
	header.channels = make([]ChannelInfo, count)
	for i := range header.channels {
		if err := h.full(data[:]); err != nil {
			return err
		}
		header.channels[i] = ChannelInfo{Index: binary.BigEndian.Uint16(data[:2]), Type: ChannelType(binary.BigEndian.Uint16(data[2:4])), Association: binary.BigEndian.Uint16(data[4:6])}
	}
	return nil
}
