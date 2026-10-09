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
	"fmt"
	"image"
	"io"
	"math"
)

// componentHeaderMemory 读取头部时为每个分量及其SIZ字段预留的字节数
const componentHeaderMemory uint64 = 32

// headerReader 在资源限制内读取图像头部，不关闭输入流
type headerReader struct {
	ctx    context.Context
	r      io.Reader
	limits Limits
	read   uint64
	memory uint64
}

// reserve 为头部元数据预留内存
// 入参: count 项数, size 单项字节数
// 返回: error 错误信息
func (h *headerReader) reserve(count, size uint64) error {
	n, err := checkedProduct("header memory", count, size, h.limits.MaxMemoryBytes)
	if err != nil {
		return err
	}
	h.memory, err = checkTotal("header memory", h.memory, n, h.limits.MaxMemoryBytes)
	return err
}

// jp2Header 保存JP2图像头及颜色信息
type jp2Header struct {
	width         uint32
	height        uint32
	components    uint16
	precision     byte
	bits          []byte
	channels      []ChannelInfo
	mapping       []ChannelMapping
	palette       []PaletteColumn
	color         ColorSpace
	icc           []byte
	hasColor      bool
	capture       Resolution
	display       Resolution
	hasResolution bool
}

// DecodeConfig 读取图像尺寸与显示颜色模型，不解码图像样本
// 入参: r 输入流
// 返回: image.Config 图像配置, error 错误信息
func DecodeConfig(r io.Reader) (image.Config, error) {
	return DecodeConfigContext(context.Background(), r, nil)
}

// DecodeConfigContext 读取显示尺寸及颜色模型，不解码样本或建立整幅图像索引
// 入参: ctx 上下文, r 输入流, opts 解码选项
// 返回: image.Config 图像配置, error 错误信息
func DecodeConfigContext(ctx context.Context, r io.Reader, opts *DecodeOptions) (image.Config, error) {
	options, err := decodeOptions(opts)
	if err != nil {
		return image.Config{}, err
	}
	info, err := readHeader(ctx, r, options.Limits)
	if err != nil {
		return image.Config{}, err
	}
	info, err = decodingColor(info, options.ColorSpace)
	if err != nil {
		return image.Config{}, err
	}
	info.Bounds = reduceBounds(info.Bounds, options.Reduce)
	if len(info.ICCProfile) != 0 {
		used := colorMetadataSize(info) + uint64(len(info.Components))*componentHeaderMemory
		if _, err := checkTotal("display header memory", used, iccTransformMemory, options.Limits.MaxMemoryBytes); err != nil {
			return image.Config{}, err
		}
	}
	plan, err := makeDisplayPlan(ctx, info)
	if err != nil {
		return image.Config{}, err
	}
	return image.Config{Width: plan.grid.Dx(), Height: plan.grid.Dy(), ColorModel: plan.model}, nil
}

// readHeader 识别J2K码流或JP2容器并检查图像尺寸
// 入参: ctx 上下文, r 输入流, limits 资源限制
// 返回: Info 图像信息, error 错误信息
func readHeader(ctx context.Context, r io.Reader, limits Limits) (Info, error) {
	if r == nil {
		return Info{}, fmt.Errorf("j2kgo: nil reader")
	}
	h := &headerReader{ctx: ctx, r: r, limits: limits.normalized()}
	var prefix [2]byte
	if err := h.full(prefix[:]); err != nil {
		return Info{}, err
	}
	if prefix == [2]byte{0xff, 0x4f} {
		return h.siz()
	}
	var signature [12]byte
	copy(signature[:], prefix[:])
	if err := h.full(signature[2:]); err != nil {
		return Info{}, err
	}
	if string(signature[:]) != "\x00\x00\x00\x0cjP  \r\n\x87\n" {
		return Info{}, FormatError("signature")
	}
	return h.jp2()
}

// full 在输入限制内读满缓冲区，保留底层读取错误
// 入参: data 目标缓冲
// 返回: error 错误信息
func (h *headerReader) full(data []byte) error {
	if err := h.ctx.Err(); err != nil {
		return err
	}
	total, err := checkTotal("input", h.read, uint64(len(data)), h.limits.MaxInputBytes)
	if err != nil {
		return err
	}
	err = readFullContext(h.ctx, h.r, data)
	if err != nil {
		return fmt.Errorf("read header: %w", err)
	}
	h.read = total
	return h.ctx.Err()
}

// skip 跳过不影响图像解码的容器内容
// 入参: size 跳过字节数
// 返回: error 错误信息
func (h *headerReader) skip(size uint64) error {
	if _, err := checkTotal("input", h.read, size, h.limits.MaxInputBytes); err != nil {
		return err
	}
	var scratch [4096]byte
	for size > 0 {
		n := min(size, uint64(len(scratch)))
		if err := h.full(scratch[:n]); err != nil {
			return err
		}
		size -= n
	}
	return nil
}

// siz 读取SOC后的SIZ标记段，校验图像和瓦片网格
// 返回: Info 图像信息, error 错误信息
func (h *headerReader) siz() (Info, error) {
	var fixed [40]byte
	if err := h.full(fixed[:]); err != nil {
		return Info{}, err
	}
	if binary.BigEndian.Uint16(fixed[:2]) != 0xff51 {
		return Info{}, FormatError("SIZ must follow SOC")
	}
	length := int(binary.BigEndian.Uint16(fixed[2:4]))
	count := int(binary.BigEndian.Uint16(fixed[38:40]))
	if count < 1 || count > 16384 || length != 38+3*count {
		return Info{}, FormatError("SIZ length or component count")
	}
	var grid [8]uint64
	for i := range grid {
		grid[i] = uint64(binary.BigEndian.Uint32(fixed[6+i*4 : 10+i*4]))
	}
	x, y, x0, y0 := grid[0], grid[1], grid[2], grid[3]
	tw, th, tx, ty := grid[4], grid[5], grid[6], grid[7]
	if x <= x0 || y <= y0 || tw == 0 || th == 0 || tx > x0 || ty > y0 || tx+tw <= x0 || ty+th <= y0 {
		return Info{}, FormatError("reference or tile grid")
	}
	if _, err := checkedProduct("tiles", (x-tx+tw-1)/tw, (y-ty+th-1)/th, 65535); err != nil {
		return Info{}, err
	}
	maxInt := uint64(^uint(0) >> 1)
	if x > maxInt || y > maxInt {
		return Info{}, &LimitError{Resource: "coordinates", Limit: maxInt, Required: max(x, y)}
	}
	if err := h.reserve(uint64(count), componentHeaderMemory); err != nil {
		return Info{}, err
	}
	data := make([]byte, 3*count)
	if err := h.full(data); err != nil {
		return Info{}, err
	}
	info := Info{Bounds: image.Rect(int(x0), int(y0), int(x), int(y)), Components: make([]ComponentInfo, count)}
	for i := range info.Components {
		info.Components[i] = ComponentInfo{
			Precision: (data[3*i] & 0x7f) + 1,
			Signed:    data[3*i]&0x80 != 0,
			XStep:     data[3*i+1],
			YStep:     data[3*i+2],
		}
	}
	if err := validateInfo(info); err != nil {
		return Info{}, err
	}
	return info, nil
}

// box 读取JP2框头并检查长度是否超出父框范围
// 入参: parentEnd 父框末尾的字节偏移
// 返回: string 框类型, uint64 框末尾的字节偏移, error 错误信息
func (h *headerReader) box(parentEnd uint64) (string, uint64, error) {
	start := h.read
	if parentEnd < start || parentEnd-start < 8 {
		return "", 0, FormatError("box header exceeds parent")
	}
	var header [16]byte
	if err := h.full(header[:8]); err != nil {
		return "", 0, err
	}
	size := uint64(binary.BigEndian.Uint32(header[:4]))
	headerSize := uint64(8)
	if size == 1 {
		if parentEnd-start < 16 {
			return "", 0, FormatError("extended box header exceeds parent")
		}
		if err := h.full(header[8:]); err != nil {
			return "", 0, err
		}
		size = binary.BigEndian.Uint64(header[8:])
		headerSize = 16
	} else if size == 0 {
		return string(header[4:8]), parentEnd, nil
	}
	if size < headerSize || size > parentEnd-start {
		return "", 0, FormatError("box length")
	}
	return string(header[4:8]), start + size, nil
}

// jp2 读取JP2头部及码流中的图像信息
// 返回: Info 图像信息, error 错误信息
func (h *headerReader) jp2() (Info, error) {
	var header *jp2Header
	for index := 0; ; index++ {
		kind, end, err := h.box(math.MaxUint64)
		if err != nil {
			return Info{}, err
		}
		if index == 0 && kind != "ftyp" {
			return Info{}, FormatError("missing file type box")
		}
		switch kind {
		case "ftyp":
			if index != 0 {
				return Info{}, FormatError("duplicate file type box")
			}
			if err := h.fileType(end); err != nil {
				return Info{}, err
			}
		case "jp2h":
			if header != nil {
				return Info{}, FormatError("duplicate JP2 header")
			}
			header, err = h.containerHeader(end)
			if err != nil {
				return Info{}, err
			}
		case "jp2c":
			if header == nil {
				return Info{}, FormatError("missing JP2 header")
			}
			original := h.r
			if end != math.MaxUint64 {
				h.r = io.LimitReader(original, int64(min(end-h.read, uint64(math.MaxInt64))))
			}
			var soc [2]byte
			if err := h.full(soc[:]); err != nil {
				return Info{}, err
			}
			if soc != [2]byte{0xff, 0x4f} {
				return Info{}, FormatError("codestream signature")
			}
			info, err := h.siz()
			h.r = original
			if err != nil {
				return Info{}, err
			}
			return header.apply(info)
		default:
			if err := h.skip(end - h.read); err != nil {
				return Info{}, err
			}
		}
	}
}

// fileType 读取文件类型信息，确认兼容JP2格式
// 入参: end 文件类型框末尾的字节偏移
// 返回: error 错误信息
func (h *headerReader) fileType(end uint64) error {
	length := end - h.read
	if length < 12 || length%4 != 0 {
		return FormatError("file type length")
	}
	var word [4]byte
	compatible := false
	for i := uint64(0); i < length; i += 4 {
		if err := h.full(word[:]); err != nil {
			return err
		}
		if i >= 8 && string(word[:]) == "jp2 " {
			compatible = true
		}
	}
	if !compatible {
		return UnsupportedError("container without JP2 compatibility")
	}
	return nil
}

// containerHeader 解析JP2头部，检查各子框的内容和顺序
// 入参: end JP2头框末尾的字节偏移
// 返回: *jp2Header 图像头部信息, error 错误信息
func (h *headerReader) containerHeader(end uint64) (*jp2Header, error) {
	result := &jp2Header{}
	for index := 0; h.read < end; index++ {
		kind, childEnd, err := h.box(end)
		if err != nil {
			return nil, err
		}
		length := childEnd - h.read
		if index == 0 && kind != "ihdr" {
			return nil, FormatError("image header must be first")
		}
		switch kind {
		case "ihdr":
			if index != 0 || length != 14 {
				return nil, FormatError("image header length or position")
			}
			var data [14]byte
			if err := h.full(data[:]); err != nil {
				return nil, err
			}
			result.height = binary.BigEndian.Uint32(data[:4])
			result.width = binary.BigEndian.Uint32(data[4:8])
			result.components = binary.BigEndian.Uint16(data[8:10])
			result.precision = data[10]
			if result.height == 0 || result.width == 0 || result.components == 0 || result.components > 16384 || data[11] != 7 || data[12] > 1 || data[13] > 1 {
				return nil, FormatError("image header fields")
			}
		case "bpcc":
			if result.bits != nil || length != uint64(result.components) {
				return nil, FormatError("bits per component box")
			}
			if err := h.reserve(length, 1); err != nil {
				return nil, err
			}
			result.bits = make([]byte, int(length))
			if err := h.full(result.bits); err != nil {
				return nil, err
			}
		case "colr":
			if !result.hasColor {
				if err := h.colorBox(result, length); err != nil {
					return nil, err
				}
			}
		case "cdef":
			if err := h.channelBox(result, length); err != nil {
				return nil, err
			}
		case "pclr":
			if err := h.paletteBox(result, length); err != nil {
				return nil, err
			}
		case "cmap":
			if err := h.mappingBox(result, length); err != nil {
				return nil, err
			}
		case "res ":
			if err := h.resolutionBox(result, childEnd); err != nil {
				return nil, err
			}
		}
		if err := h.skip(childEnd - h.read); err != nil {
			return nil, err
		}
	}
	if result.components == 0 || !result.hasColor {
		return nil, FormatError("missing image or color header")
	}
	if result.precision == 255 && result.bits == nil {
		return nil, FormatError("missing bits per component box")
	}
	if result.precision != 255 && result.bits != nil {
		return nil, FormatError("unexpected bits per component box")
	}
	if len(result.bits) > 0 {
		varied := false
		for _, depth := range result.bits[1:] {
			varied = varied || depth != result.bits[0]
		}
		if !varied {
			return nil, FormatError("uniform precision in bits per component box")
		}
	}
	return result, nil
}

// colorBox 读取颜色空间信息，使用ICC配置时检查其有效性
// 入参: header JP2头部信息, length 内容字节数
// 返回: error 错误信息
func (h *headerReader) colorBox(header *jp2Header, length uint64) error {
	if length < 3 {
		return FormatError("color box length")
	}
	var prefix [3]byte
	if err := h.full(prefix[:]); err != nil {
		return err
	}
	header.hasColor = true
	switch prefix[0] {
	case 1:
		if length != 7 {
			return FormatError("enumerated color box length")
		}
		var data [4]byte
		if err := h.full(data[:]); err != nil {
			return err
		}
		header.color = ColorSpace(binary.BigEndian.Uint32(data[:]))
	case 2:
		if length < 131 {
			return FormatError("ICC profile length")
		}
		size, err := checkedProduct("memory", length-3, 1, min(h.limits.MaxMemoryBytes, uint64(^uint(0)>>1)))
		if err != nil {
			return err
		}
		if _, err := checkTotal("input", h.read, size, h.limits.MaxInputBytes); err != nil {
			return err
		}
		if err := h.reserve(size, 1); err != nil {
			return err
		}
		header.icc = make([]byte, int(size))
		if err := h.full(header.icc); err != nil {
			return err
		}
		_, err = readRestrictedICC(h.ctx, header.icc)
		return err
	default:
		return UnsupportedError("JP2 color method")
	}
	return nil
}

// apply 核对容器与码流的图像信息，合并颜色和分辨率元数据
// 入参: info 码流图像信息
// 返回: Info 合并后的图像信息, error 错误信息
func (h *jp2Header) apply(info Info) (Info, error) {
	if uint64(info.Bounds.Dx()) != uint64(h.width) || uint64(info.Bounds.Dy()) != uint64(h.height) || len(info.Components) != int(h.components) {
		return Info{}, FormatError("container and codestream dimensions differ")
	}
	for i, c := range info.Components {
		b := h.precision
		if b == 255 {
			b = h.bits[i]
		}
		if c.Precision != (b&127)+1 || c.Signed != (b&128 != 0) {
			return Info{}, FormatError("container and codestream precision differs")
		}
	}
	info.ColorSpace = h.color
	info.ICCProfile = h.icc
	info.Channels = h.channels
	info.Mapping, info.Palette = h.mapping, h.palette
	info.CaptureResolution, info.DisplayResolution = h.capture, h.display
	if err := validateInfo(info); err != nil {
		return Info{}, err
	}
	if err := validateColorChannels(info); err != nil {
		return Info{}, err
	}
	return info, nil
}
