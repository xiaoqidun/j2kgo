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
	"math/bits"
)

// codestreamWriter 写入JP2容器和码流标记，支持取消操作
type codestreamWriter struct {
	ctx           context.Context
	w             io.Writer
	written       uint64
	tileLengths   *tileLengthEncoding
	packetLengths *mainPacketLengthEncoding
	packetHeaders *mainPacketHeaderEncoding
	measureOnly   bool
	markerBuffer  [16]byte
}

// write 完整写入数据，返回原始写入错误
// 入参: data 数据
// 返回: error 错误信息
func (w *codestreamWriter) write(data []byte) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	n, err := w.w.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	w.written += uint64(n)
	return nil
}

// marker 写入独立标记或带长度的标记段
// 入参: code 标记代码, data 参数，nil表示独立标记
// 返回: error 错误信息
func (w *codestreamWriter) marker(code uint16, data []byte) error {
	if len(data) <= len(w.markerBuffer)-4 {
		return w.shortMarker(code, data)
	}
	if len(data) > 65533 {
		return FormatError("encoded marker length")
	}
	binary.BigEndian.PutUint16(w.markerBuffer[:2], code)
	binary.BigEndian.PutUint16(w.markerBuffer[2:4], uint16(len(data)+2))
	if err := w.write(w.markerBuffer[:4]); err != nil {
		return err
	}
	return w.write(data)
}

// shortMarker 复用缓冲写入独立标记或参数不超过12字节的标记段
// 入参: code 标记代码, data 参数，nil表示独立标记
// 返回: error 错误信息
func (w *codestreamWriter) shortMarker(code uint16, data []byte) error {
	if len(data) > len(w.markerBuffer)-4 {
		return FormatError("encoded short marker length")
	}
	binary.BigEndian.PutUint16(w.markerBuffer[:2], code)
	length := 2
	if data != nil {
		binary.BigEndian.PutUint16(w.markerBuffer[2:4], uint16(len(data)+2))
		length = 4 + len(data)
		copy(w.markerBuffer[4:length], data)
	}
	return w.write(w.markerBuffer[:length])
}

// appendBox 追加使用32位长度字段的JP2框
// 入参: target 目标缓冲, kind 类型标识, data 内容数据
// 返回: []byte 追加后的缓冲
func appendBox(target []byte, kind string, data []byte) []byte {
	target = binary.BigEndian.AppendUint32(target, uint32(len(data)+8))
	target = append(target, kind...)
	return append(target, data...)
}

// header 写入容器头及码流主头
// 入参: plan 编码参数
// 返回: error 错误信息
func (w *codestreamWriter) header(plan encodingPlan) error {
	if plan.format == FormatJP2 {
		if err := w.jp2Header(plan.info, plan.limits); err != nil {
			return err
		}
	}
	if err := w.marker(markerSOC, nil); err != nil {
		return err
	}
	info := plan.info
	siz := binary.BigEndian.AppendUint16(nil, 0)
	for _, value := range []int{info.Bounds.Max.X, info.Bounds.Max.Y, info.Bounds.Min.X, info.Bounds.Min.Y, plan.tileSize.X, plan.tileSize.Y, plan.tileOrigin.X, plan.tileOrigin.Y} {
		siz = binary.BigEndian.AppendUint32(siz, uint32(value))
	}
	siz = binary.BigEndian.AppendUint16(siz, uint16(len(info.Components)))
	for _, c := range info.Components {
		siz = append(siz, encodedPrecision(c), c.XStep, c.YStep)
	}
	if err := w.marker(markerSIZ, siz); err != nil {
		return err
	}
	if err := w.registration(info, plan.limits); err != nil {
		return err
	}
	return w.codingHeader(plan)
}

// codingHeader 写入默认及分量专用编码参数，供主头和瓦片首个分段共用
// 入参: plan 编码参数
// 返回: error 错误信息
func (w *codestreamWriter) codingHeader(plan encodingPlan) error {
	info := plan.info
	mct := byte(0)
	if plan.mct {
		mct = 1
	}
	cod := []byte{1, byte(plan.progression), byte(plan.layerCount >> 8), byte(plan.layerCount), mct}
	if plan.sop {
		cod[0] |= 2
	}
	if plan.eph {
		cod[0] |= 4
	}
	if err := w.marker(markerCOD, appendCodingStyle(cod, plan.coding)); err != nil {
		return err
	}
	for c := range info.Components {
		coding := plan.componentCoding(c)
		if equalCoding(coding, plan.coding) {
			continue
		}
		data := appendComponentIndex(nil, c, len(info.Components))
		data = append(data, 1)
		if err := w.marker(markerCOC, appendCodingStyle(data, coding)); err != nil {
			return err
		}
	}
	if len(plan.volumes) > 0 {
		if err := w.marker(markerPOC, encodeProgressions(plan.volumes, len(info.Components))); err != nil {
			return err
		}
	}
	quant := encodingQuantization(info.Components[0].Precision, plan.coding)
	if plan.quant != nil {
		quant = *plan.quant
	}
	if err := w.marker(markerQCD, appendQuantization(nil, quant)); err != nil {
		return err
	}
	for c := range info.Components {
		q := plan.componentQuantization(c)
		if equalQuantization(q, quant) {
			continue
		}
		data := appendComponentIndex(nil, c, len(info.Components))
		if err := w.marker(markerQCC, appendQuantization(data, q)); err != nil {
			return err
		}
	}
	return nil
}

// appendCodingStyle 写入COD和COC共用的分量编码参数
// 入参: data 目标缓冲, coding 编码参数
// 返回: []byte 编码数据
func appendCodingStyle(data []byte, coding codingStyle) []byte {
	wavelet := byte(0)
	if coding.reversible {
		wavelet = 1
	}
	data = append(data, byte(coding.levels), byte(bits.Len(uint(coding.blockSize.X))-3), byte(bits.Len(uint(coding.blockSize.Y))-3), byte(coding.style), wavelet)
	for _, size := range coding.precincts {
		data = append(data, byte(bits.Len(uint(size.X))-1)|byte(bits.Len(uint(size.Y))-1)<<4)
	}
	return data
}

// appendQuantization 写入QCD和QCC共用的量化参数
// 入参: data 目标缓冲, quant 量化参数
// 返回: []byte 编码数据
func appendQuantization(data []byte, quant quantization) []byte {
	data = append(data, byte(quant.guard<<5)|quant.kind)
	for _, step := range quant.steps {
		if quant.kind == 0 {
			data = append(data, byte(step.exponent<<3))
		} else {
			data = binary.BigEndian.AppendUint16(data, uint16(step.exponent<<11)|step.mantissa)
		}
	}
	return data
}

// encodedPrecision 将分量精度和符号标志编码为一个字节
// 入参: c 分量信息
// 返回: byte 精度与符号字段
func encodedPrecision(c ComponentInfo) byte {
	value := c.Precision - 1
	if c.Signed {
		value |= 128
	}
	return value
}

// appendComponentIndex 根据分量总数选择字段宽度并写入分量索引
// 入参: data 目标缓冲, index 分量索引, count 分量总数
// 返回: []byte 编码数据
func appendComponentIndex(data []byte, index, count int) []byte {
	if count >= 257 {
		return binary.BigEndian.AppendUint16(data, uint16(index))
	}
	return append(data, byte(index))
}

// jp2Header 写入JP2文件头，将码流框长度设为零，表示延伸至文件末尾
// 入参: info 图像信息, limits 资源限制
// 返回: error 错误信息
func (w *codestreamWriter) jp2Header(info Info, limits Limits) error {
	if metadata := rasterMetadataSize(info); metadata > min(limits.MaxMemoryBytes/4, (uint64(^uint32(0))-1024)/4) {
		return &LimitError{Resource: "JP2 header memory", Limit: limits.MaxMemoryBytes / 4, Required: metadata}
	}
	bpc := encodedPrecision(info.Components[0])
	for _, c := range info.Components {
		if encodedPrecision(c) != bpc {
			bpc = 255
			break
		}
	}
	ihdr := binary.BigEndian.AppendUint32(nil, uint32(info.Bounds.Dy()))
	ihdr = binary.BigEndian.AppendUint32(ihdr, uint32(info.Bounds.Dx()))
	ihdr = binary.BigEndian.AppendUint16(ihdr, uint16(len(info.Components)))
	ihdr = append(ihdr, bpc, 7, 0, 0)
	header := appendBox(nil, "ihdr", ihdr)
	if bpc == 255 {
		data := make([]byte, len(info.Components))
		for i, c := range info.Components {
			data[i] = encodedPrecision(c)
		}
		header = appendBox(header, "bpcc", data)
	}
	color := []byte{1, 0, 0}
	if len(info.ICCProfile) != 0 {
		if _, err := readRestrictedICC(w.ctx, info.ICCProfile); err != nil {
			return err
		}
		color[0] = 2
		color = append(color, info.ICCProfile...)
	} else {
		color = binary.BigEndian.AppendUint32(color, uint32(info.ColorSpace))
	}
	header = appendBox(header, "colr", color)
	header = appendPaletteBoxes(header, info)
	if len(info.Channels) > 0 {
		data := binary.BigEndian.AppendUint16(nil, uint16(len(info.Channels)))
		for _, c := range info.Channels {
			data = binary.BigEndian.AppendUint16(data, c.Index)
			data = binary.BigEndian.AppendUint16(data, uint16(c.Type))
			data = binary.BigEndian.AppendUint16(data, c.Association)
		}
		header = appendBox(header, "cdef", data)
	}
	header, err := appendResolutionBox(header, info)
	if err != nil {
		return err
	}
	if err := w.write([]byte("\x00\x00\x00\x0cjP  \r\n\x87\n")); err != nil {
		return err
	}
	if err := w.write(appendBox(nil, "ftyp", []byte("jp2 \x00\x00\x00\x00jp2 "))); err != nil {
		return err
	}
	if err := w.write(appendBox(nil, "jp2h", header)); err != nil {
		return err
	}
	return w.write([]byte("\x00\x00\x00\x00jp2c"))
}
