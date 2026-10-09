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
	"image/color"
	"math"
)

// displayMetadataMemory 显示参数及标准图像结构的内存预算
const displayMetadataMemory uint64 = 512

// displayPlan 保存显示网格、通道顺序及输出颜色模型
type displayPlan struct {
	grid          image.Rectangle
	step          int
	colors        [3]int
	count         int
	alpha         int
	premultiplied bool
	wide          bool
	space         ColorSpace
	icc           *iccTransform
	model         color.Model
	bytesPerPixel int
}

// makeDisplayPlan 根据颜色空间和通道定义确定输出格式，不推断多分量裸码流的颜色空间
// 入参: ctx 上下文, info 图像信息
// 返回: displayPlan 显示参数, error 错误信息
func makeDisplayPlan(ctx context.Context, info Info) (displayPlan, error) {
	plan := displayPlan{colors: [3]int{0, 1, 2}, alpha: -1, space: info.ColorSpace}
	if err := ctx.Err(); err != nil {
		return plan, err
	}
	if len(info.ICCProfile) != 0 {
		transform, err := readRestrictedICC(ctx, info.ICCProfile)
		if err != nil {
			return plan, err
		}
		plan.icc = &transform
		plan.space = ColorSRGB
		if transform.count == 1 {
			plan.space = ColorGray
		}
	}
	if plan.space == ColorUnknown && channelCount(info) == 1 {
		plan.space = ColorGray
	}
	switch plan.space {
	case ColorGray:
		plan.count = 1
	case ColorSRGB, ColorSYCC:
		plan.count = 3
	default:
		return plan, UnsupportedError("display color space is unspecified or unsupported")
	}
	if channelCount(info) < plan.count {
		return plan, FormatError("insufficient display channels")
	}
	alpha := [3]int{-1, -1, -1}
	var premultiplied [3]bool
	for _, channel := range info.Channels {
		if channel.Association == 65535 || channel.Type == ChannelUnspecified {
			continue
		}
		if channel.Type == ChannelColor {
			if channel.Association >= 1 && int(channel.Association) <= plan.count {
				plan.colors[channel.Association-1] = int(channel.Index)
			}
			continue
		}
		for i := range plan.count {
			if channel.Association == 0 || int(channel.Association) == i+1 {
				alpha[i] = int(channel.Index)
				premultiplied[i] = channel.Type == ChannelPremultiplied
			}
		}
	}
	plan.alpha, plan.premultiplied = alpha[0], premultiplied[0]
	for i := range plan.count {
		if alpha[i] != plan.alpha || premultiplied[i] != plan.premultiplied {
			return plan, UnsupportedError("independent channel opacity requires raw raster access")
		}
	}
	channels := []int{plan.colors[0]}
	if plan.count == 3 {
		channels = append(channels, plan.colors[1:]...)
	}
	if plan.alpha >= 0 {
		channels = append(channels, plan.alpha)
	}
	for _, index := range channels {
		c := channelSpec(info, index)
		if c.Signed && (plan.icc == nil || index == plan.alpha) {
			return plan, FormatError("enumerated display requires unsigned channels")
		}
		if c.Precision > 16 {
			return plan, UnsupportedError("display precision exceeds standard color models")
		}
		if componentBounds(info.Bounds, c).Empty() {
			return plan, FormatError("empty display channel")
		}
		plan.wide = plan.wide || c.Precision > 8
	}
	plan.step = displayStep(info)
	plan.grid = displayGrid(info.Bounds, plan.step)
	plan.model, plan.bytesPerPixel = color.NRGBAModel, 4
	if plan.wide {
		plan.model, plan.bytesPerPixel = color.NRGBA64Model, 8
	}
	if plan.premultiplied {
		plan.model = color.RGBAModel
		if plan.wide {
			plan.model = color.RGBA64Model
		}
	}
	if plan.count == 1 && plan.alpha < 0 {
		plan.model, plan.bytesPerPixel = color.GrayModel, 1
		if plan.wide {
			plan.model, plan.bytesPerPixel = color.Gray16Model, 2
		}
	}
	return plan, nil
}

// Image 将原始分量转换为标准图像，输出原点为零，不修改原图
// 资源限制仅用于输出图像和转换缓存，不包含原图内存
// 入参: ctx 上下文, limits 输出及转换缓存的资源限制
// 返回: image.Image 图像副本, error 错误信息
func (r *Raster) Image(ctx context.Context, limits Limits) (image.Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil {
		return nil, fmt.Errorf("j2kgo: nil raster")
	}
	if err := validateInfo(r.info); err != nil {
		return nil, err
	}
	limits = limits.normalized()
	metadata := displayMetadataMemory
	if len(r.info.ICCProfile) != 0 {
		metadata += iccTransformMemory
	}
	if _, err := checkTotal("display memory", 0, metadata, limits.MaxMemoryBytes); err != nil {
		return nil, err
	}
	info := r.info
	info.Bounds = r.extent
	plan, err := makeDisplayPlan(ctx, info)
	if err != nil {
		return nil, err
	}
	plan.grid = displayGrid(r.info.Bounds, plan.step)
	return renderRasterImage(ctx, r, plan, limits)
}

// memory 检查输出尺寸并计算图像及转换参数的内存用量，不含可选查找表
// 入参: limits 资源限制
// 返回: uint64 内存字节数, error 错误信息
func (p displayPlan) memory(limits Limits) (uint64, error) {
	pixels, err := checkedProduct("display samples", uint64(p.grid.Dx()), uint64(p.grid.Dy()), limits.MaxSamples)
	if err != nil {
		return 0, err
	}
	memory, err := checkedProduct("display memory", pixels, uint64(p.bytesPerPixel), min(limits.MaxMemoryBytes, uint64(^uint(0)>>1)))
	if err != nil {
		return 0, err
	}
	metadata := displayMetadataMemory
	if p.icc != nil {
		metadata += iccTransformMemory
	}
	return checkTotal("display memory", memory, metadata, limits.MaxMemoryBytes)
}

// renderRasterImage 按显示参数生成独立图像，并限制内存用量
// 入参: ctx 上下文, raster 原始图像, plan 显示参数, limits 可用预算
// 返回: image.Image 显示图像, error 错误信息
func renderRasterImage(ctx context.Context, raster *Raster, plan displayPlan, limits Limits) (image.Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	memory, err := plan.memory(limits)
	if err != nil {
		return nil, err
	}
	if plan.icc != nil && !plan.premultiplied && uint64(plan.grid.Dx())*uint64(plan.grid.Dy()) >= 256 {
		if err := plan.icc.prepare(ctx, raster.info, plan.colors, limits.MaxMemoryBytes-memory); err != nil {
			return nil, err
		}
	}
	m, data := displayImage(plan)
	for y := 0; y < plan.grid.Dy(); y++ {
		for x := 0; x < plan.grid.Dx(); x++ {
			if x&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			offset := (y*plan.grid.Dx() + x) * plan.bytesPerPixel
			if err := displayPixel(data[offset:], raster, plan, (x+plan.grid.Min.X)*plan.step, (y+plan.grid.Min.Y)*plan.step); err != nil {
				return nil, err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return m, nil
}

// displayStep 计算所有分量采样间隔的最大公约数，作为显示网格间隔
// 入参: info 图像信息
// 返回: int 显示网格间隔
func displayStep(info Info) int {
	step := 0
	for _, c := range info.Components {
		step = gcd(step, int(c.XStep))
		step = gcd(step, int(c.YStep))
	}
	return step
}

// displayGrid 将参考网格区域映射为显示网格区域
// 入参: bounds 参考网格边界, step 显示网格间隔
// 返回: image.Rectangle 显示网格边界
func displayGrid(bounds image.Rectangle, step int) image.Rectangle {
	return image.Rect(ceilDiv(bounds.Min.X, step), ceilDiv(bounds.Min.Y, step), ceilDiv(bounds.Max.X, step), ceilDiv(bounds.Max.Y, step))
}

// gcd 计算非负整数的最大公约数
// 入参: a 非负整数, b 非负整数
// 返回: int 最大公约数
func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// displayImage 按已校验的显示参数分配标准图像
// 入参: plan 显示参数
// 返回: image.Image 图像, []byte 像素存储
func displayImage(plan displayPlan) (image.Image, []byte) {
	b := image.Rect(0, 0, plan.grid.Dx(), plan.grid.Dy())
	switch plan.model {
	case color.GrayModel:
		m := image.NewGray(b)
		return m, m.Pix
	case color.Gray16Model:
		m := image.NewGray16(b)
		return m, m.Pix
	case color.RGBAModel:
		m := image.NewRGBA(b)
		return m, m.Pix
	case color.RGBA64Model:
		m := image.NewRGBA64(b)
		return m, m.Pix
	case color.NRGBA64Model:
		m := image.NewNRGBA64(b)
		return m, m.Pix
	default:
		m := image.NewNRGBA(b)
		return m, m.Pix
	}
}

// channelSample 按配准位置进行零阶保持采样，边缘使用最近有效样本
// 入参: raster 原始图像, channel 通道索引, x 参考网格横坐标, y 参考网格纵坐标
// 返回: int64 通道值, error 错误信息
func channelSample(raster *Raster, channel, x, y int) (int64, error) {
	mapping := ChannelMapping{Component: uint16(channel)}
	if len(raster.info.Mapping) > 0 {
		mapping = raster.info.Mapping[channel]
	}
	c := &raster.components[mapping.Component]
	x = max(c.storage.Min.X, min(c.storage.Max.X-1, registeredCoordinate(x, c.info.XStep, c.info.XOffset)))
	y = max(c.storage.Min.Y, min(c.storage.Max.Y-1, registeredCoordinate(y, c.info.YStep, c.info.YOffset)))
	value := c.sample(x, y)
	if mapping.Type == MappingPalette {
		column := raster.info.Palette[mapping.Column]
		if value < 0 || value >= int64(len(column.Values)) {
			return 0, FormatError("palette index out of range")
		}
		value = column.Values[value]
	}
	return value, nil
}

// displayPixel 转换并写入显示像素，按通道定义处理预乘或非预乘透明度
// 入参: dst 像素存储, raster 原始图像, plan 显示参数, x 参考网格横坐标, y 参考网格纵坐标
// 返回: error 错误信息
func displayPixel(dst []byte, raster *Raster, plan displayPlan, x, y int) error {
	maximum := int64(255)
	if plan.wide {
		maximum = 65535
	}
	values := [4]int64{0, 0, 0, maximum}
	alpha := float64(1)
	if plan.alpha >= 0 {
		value, err := channelSample(raster, plan.alpha, x, y)
		if err != nil {
			return err
		}
		scale := int64(1)<<channelSpec(raster.info, plan.alpha).Precision - 1
		values[3] = (value*maximum + scale/2) / scale
		alpha = float64(value) / float64(scale)
	}
	var normalized [3]float64
	for i := range plan.count {
		value, err := channelSample(raster, plan.colors[i], x, y)
		if err != nil {
			return err
		}
		precision := channelSpec(raster.info, plan.colors[i]).Precision
		scale := int64(1)<<precision - 1
		values[i] = (value*maximum + scale/2) / scale
		normalized[i] = float64(value) / float64(scale)
	}
	if plan.count == 1 {
		values[1], values[2] = values[0], values[0]
	}
	if plan.space == ColorSYCC || plan.icc != nil {
		if plan.premultiplied && alpha > 0 {
			for i := range normalized {
				normalized[i] /= alpha
			}
		}
		var rgb [3]float64
		if plan.icc != nil {
			rgb = plan.icc.convert(normalized)
		} else {
			yv, cb, cr := normalized[0]*255, normalized[1]*255-128, normalized[2]*255-128
			rgb = [3]float64{(yv + 1.402*cr) / 255, (yv - (0.114*1.772/0.587)*cb - (0.299*1.402/0.587)*cr) / 255, (yv + 1.772*cb) / 255}
		}
		for i, value := range rgb {
			value = max(0, min(1, value)) * float64(maximum)
			if plan.premultiplied {
				value *= alpha
			}
			values[i] = int64(math.Floor(value + 0.5))
		}
	}
	count := 4
	if plan.alpha < 0 && plan.count == 1 {
		count = 1
	}
	for i, value := range values[:count] {
		if plan.wide {
			binary.BigEndian.PutUint16(dst[2*i:], uint16(value))
		} else {
			dst[i] = byte(value)
		}
	}
	return nil
}
