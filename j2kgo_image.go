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
)

// Info 图像的参考网格、分量及颜色信息
type Info struct {
	Bounds            image.Rectangle
	Components        []ComponentInfo
	Channels          []ChannelInfo
	Mapping           []ChannelMapping
	Palette           []PaletteColumn
	ColorSpace        ColorSpace
	ICCProfile        []byte
	CaptureResolution Resolution
	DisplayResolution Resolution
}

// ChannelType JP2通道类型
type ChannelType uint16

const (
	ChannelColor         ChannelType = iota  // 颜色通道
	ChannelOpacity                           // 不透明度通道
	ChannelPremultiplied                     // 对应预乘颜色的不透明度通道
	ChannelUnspecified   ChannelType = 65535 // 未指定类型
)

// ChannelInfo 描述JP2通道及其颜色关联，Association为零表示整幅图像
type ChannelInfo struct {
	Index       uint16
	Type        ChannelType
	Association uint16
}

// ComponentInfo 分量精度、采样间隔及配准偏移
// XOffset、YOffset以对应方向的采样间隔为单位，范围为[0,1)，不改变样本坐标
type ComponentInfo struct {
	Precision uint8
	Signed    bool
	XStep     uint8
	YStep     uint8
	XOffset   float64
	YOffset   float64
}

// Raster 保存图像元数据及未经显示颜色转换的原始分量
type Raster struct {
	info       Info
	components []Component
	extent     image.Rectangle
}

// Component 按精度紧凑存储原始样本，坐标位于分量网格
type Component struct {
	info    ComponentInfo
	bounds  image.Rectangle
	storage image.Rectangle
	data    []byte
	width   int
}

// NewRaster 创建原始图像并复制元数据
// 入参: info 图像信息, limits 资源限制
// 返回: *Raster 原始图像, error 错误信息
func NewRaster(info Info, limits Limits) (*Raster, error) {
	return newRaster(info, limits)
}

// newRaster 在资源限制内分配原始图像
// 入参: info 图像信息, limits 资源限制
// 返回: *Raster 原始图像, error 错误信息
func newRaster(info Info, limits Limits) (*Raster, error) {
	return newRasterContext(context.Background(), info, limits)
}

// newRasterContext 在资源限制内分配原始图像，支持取消操作
// 入参: ctx 上下文, info 图像信息, limits 资源限制
// 返回: *Raster 原始图像, error 错误信息
func newRasterContext(ctx context.Context, info Info, limits Limits) (*Raster, error) {
	return newRasterExtent(ctx, info, info.Bounds, 0, limits)
}

// newRasterExtent 为目标区域和显示所需的相邻样本分配内存，并调整配准偏移
// 入参: ctx 上下文, info 区域信息, extent 缩减后的完整参考网格, reduce 缩减级数, limits 资源限制
// 返回: *Raster 原始图像, error 错误信息
func newRasterExtent(ctx context.Context, info Info, extent image.Rectangle, reduce int, limits Limits) (*Raster, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limits = limits.normalized()
	memory := rasterMetadataSize(info)
	if memory > limits.MaxMemoryBytes {
		return nil, &LimitError{Resource: "memory", Limit: limits.MaxMemoryBytes, Required: memory}
	}
	if err := validateInfo(info); err != nil {
		return nil, err
	}
	if !info.Bounds.In(extent) || extent.Min.X < 0 || extent.Min.Y < 0 || uint64(extent.Max.X) > 0xffffffff || uint64(extent.Max.Y) > 0xffffffff || reduce < 0 || reduce > 32 {
		return nil, FormatError("raster extent or reduction")
	}
	r := &Raster{components: make([]Component, len(info.Components)), extent: extent}
	var err error
	r.info, err = cloneInfoContext(ctx, info)
	if err != nil {
		return nil, err
	}
	r.reduceRegistration(reduce)
	step := displayStep(r.info)
	var samples uint64
	for i, spec := range r.info.Components {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		bounds := componentBounds(info.Bounds, spec)
		storage := displaySampleBounds(info.Bounds, extent, spec, step)
		n, err := checkedProduct("samples", uint64(storage.Dx()), uint64(storage.Dy()), limits.MaxSamples)
		if err != nil {
			return nil, err
		}
		samples, err = checkTotal("samples", samples, n, limits.MaxSamples)
		if err != nil {
			return nil, err
		}
		width := sampleWidth(spec.Precision)
		bytes, err := checkedProduct("memory", n, uint64(width), min(limits.MaxMemoryBytes, uint64(^uint(0)>>1)))
		if err != nil {
			return nil, err
		}
		memory, err = checkTotal("memory", memory, bytes, limits.MaxMemoryBytes)
		if err != nil {
			return nil, err
		}
		r.components[i] = Component{info: spec, bounds: bounds, storage: storage, width: width}
	}
	for i := range r.components {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c := &r.components[i]
		c.data = make([]byte, c.storage.Dx()*c.storage.Dy()*c.width)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r, nil
}

// rasterMetadataSize 估算图像及分量元数据所需的内存上限
// 入参: info 图像信息
// 返回: uint64 字节数
func rasterMetadataSize(info Info) uint64 {
	return 256 + uint64(len(info.Components))*160 + colorMetadataSize(info)
}

// Info 返回独立的图像信息副本
// 返回: Info 图像信息
func (r *Raster) Info() Info { return cloneInfo(r.info) }

// Bounds 返回参考网格边界
// 返回: image.Rectangle 图像边界
func (r *Raster) Bounds() image.Rectangle { return r.info.Bounds }

// Component 返回指定分量，索引越界时返回nil
// 入参: index 分量索引
// 返回: *Component 分量
func (r *Raster) Component(index int) *Component {
	if index < 0 || index >= len(r.components) {
		return nil
	}
	return &r.components[index]
}

// Clone 复制图像元数据及全部样本，原图内存不计入本次复制的资源限制
// 入参: ctx 上下文, limits 资源限制
// 返回: *Raster 独立图像副本, error 错误信息
func (r *Raster) Clone(ctx context.Context, limits Limits) (*Raster, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil {
		return nil, fmt.Errorf("j2kgo: nil raster")
	}
	result, err := newRasterExtent(ctx, r.info, r.extent, 0, limits)
	if err != nil {
		return nil, err
	}
	for i, c := range r.components {
		if err := copyContext(ctx, result.components[i].data, c.data); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// Info 返回分量信息
// 返回: ComponentInfo 分量信息
func (c *Component) Info() ComponentInfo { return c.info }

// Bounds 返回分量网格边界
// 返回: image.Rectangle 分量边界
func (c *Component) Bounds() image.Rectangle { return c.bounds }

// Sample 返回原始样本，坐标越界时返回零
// 入参: x 分量横坐标, y 分量纵坐标
// 返回: int64 样本值
func (c *Component) Sample(x, y int) int64 {
	if !image.Pt(x, y).In(c.bounds) {
		return 0
	}
	return c.sample(x, y)
}

// sample 读取已确认位于存储范围内的样本
// 入参: x 分量横坐标, y 分量纵坐标
// 返回: int64 样本值
func (c *Component) sample(x, y int) int64 {
	offset := ((y-c.storage.Min.Y)*c.storage.Dx() + x - c.storage.Min.X) * c.width
	p := c.data[offset : offset+c.width]
	var v uint64
	switch c.width {
	case 1:
		v = uint64(p[0])
	case 2:
		v = uint64(binary.LittleEndian.Uint16(p))
	case 4:
		v = uint64(binary.LittleEndian.Uint32(p))
	case 8:
		v = binary.LittleEndian.Uint64(p)
	}
	if c.info.Signed {
		shift := 64 - c.info.Precision
		return int64(v<<shift) >> shift
	}
	return int64(v)
}

// SetSample 设置原始样本，拒绝越界坐标及超出精度范围的值
// 入参: x 分量横坐标, y 分量纵坐标, value 样本值
// 返回: error 错误信息
func (c *Component) SetSample(x, y int, value int64) error {
	if !image.Pt(x, y).In(c.bounds) {
		return fmt.Errorf("j2kgo: sample coordinate out of bounds")
	}
	lo, hi := sampleRange(c.info)
	if value < lo || value > hi {
		return fmt.Errorf("j2kgo: sample %d outside [%d, %d]", value, lo, hi)
	}
	c.setSample(x, y, value)
	return nil
}

// setSample 写入已校验的样本，允许访问内部保留的邻域
// 入参: x 分量横坐标, y 分量纵坐标, value 样本值
func (c *Component) setSample(x, y int, value int64) {
	offset := ((y-c.storage.Min.Y)*c.storage.Dx() + x - c.storage.Min.X) * c.width
	p := c.data[offset : offset+c.width]
	switch c.width {
	case 1:
		p[0] = byte(value)
	case 2:
		binary.LittleEndian.PutUint16(p, uint16(value))
	case 4:
		binary.LittleEndian.PutUint32(p, uint32(value))
	case 8:
		binary.LittleEndian.PutUint64(p, uint64(value))
	}
}

// validateInfo 检查参考网格与分量描述
// 入参: info 图像信息
// 返回: error 错误信息
func validateInfo(info Info) error {
	if err := validateResolution(info.CaptureResolution); err != nil {
		return err
	}
	if err := validateResolution(info.DisplayResolution); err != nil {
		return err
	}
	b := info.Bounds
	if b.Empty() || b.Min.X < 0 || b.Min.Y < 0 || uint64(b.Max.X) > 0xffffffff || uint64(b.Max.Y) > 0xffffffff {
		return FormatError("image bounds")
	}
	if len(info.Components) == 0 || len(info.Components) > 16384 {
		return FormatError("component count")
	}
	for _, c := range info.Components {
		if c.Precision == 0 || c.Precision > 38 || c.XStep == 0 || c.YStep == 0 {
			return FormatError("component precision or sampling")
		}
		if !(c.XOffset >= 0 && c.XOffset < 1 && c.YOffset >= 0 && c.YOffset < 1) {
			return FormatError("component registration offset")
		}
	}
	if err := validateMapping(info); err != nil {
		return err
	}
	return validateChannels(info)
}

// cloneInfo 复制图像元数据
// 入参: info 原始信息
// 返回: Info 图像信息副本
func cloneInfo(info Info) Info {
	result, _ := cloneInfoContext(context.Background(), info)
	return result
}

// cloneInfoContext 分段复制元数据，支持取消操作
// 入参: ctx 上下文, info 原始信息
// 返回: Info 图像信息副本, error 错误信息
func cloneInfoContext(ctx context.Context, info Info) (Info, error) {
	result := info
	var err error
	if result.Components, err = cloneSliceContext(ctx, info.Components); err != nil {
		return Info{}, err
	}
	if result.Channels, err = cloneSliceContext(ctx, info.Channels); err != nil {
		return Info{}, err
	}
	if result.Mapping, err = cloneSliceContext(ctx, info.Mapping); err != nil {
		return Info{}, err
	}
	if result.Palette, err = cloneSliceContext(ctx, info.Palette); err != nil {
		return Info{}, err
	}
	for i := range result.Palette {
		if result.Palette[i].Values, err = cloneSliceContext(ctx, info.Palette[i].Values); err != nil {
			return Info{}, err
		}
	}
	if result.ICCProfile, err = cloneSliceContext(ctx, info.ICCProfile); err != nil {
		return Info{}, err
	}
	return result, nil
}

// colorMetadataSize 计算颜色、通道及调色板的存储预算
// 入参: info 图像信息
// 返回: uint64 字节数
func colorMetadataSize(info Info) uint64 {
	size := uint64(len(info.ICCProfile)) + uint64(len(info.Channels))*8 + uint64(len(info.Mapping))*8 + uint64(len(info.Palette))*32
	for _, column := range info.Palette {
		size += uint64(len(column.Values)) * 8
	}
	return size
}

// sampleRange 根据精度和符号标志计算样本取值范围
// 入参: info 分量信息
// 返回: int64 最小值, int64 最大值
func sampleRange(info ComponentInfo) (int64, int64) {
	if info.Signed {
		half := int64(1) << (info.Precision - 1)
		return -half, half - 1
	}
	return 0, int64(1)<<info.Precision - 1
}

// sampleWidth 计算存储一个样本所需的字节数
// 入参: precision 样本精度
// 返回: int 单个样本的字节数
func sampleWidth(precision uint8) int {
	switch {
	case precision <= 8:
		return 1
	case precision <= 16:
		return 2
	case precision <= 32:
		return 4
	default:
		return 8
	}
}

// componentBounds 将参考网格边界映射到分量网格
// 入参: bounds 参考网格边界, spec 分量信息
// 返回: image.Rectangle 分量网格边界
func componentBounds(bounds image.Rectangle, spec ComponentInfo) image.Rectangle {
	return image.Rectangle{
		Min: image.Pt(ceilDiv(bounds.Min.X, int(spec.XStep)), ceilDiv(bounds.Min.Y, int(spec.YStep))),
		Max: image.Pt(ceilDiv(bounds.Max.X, int(spec.XStep)), ceilDiv(bounds.Max.Y, int(spec.YStep))),
	}
}

// ceilDiv 计算非负整数除法的向上取整值，避免加法溢出
// 入参: n 被除数, d 除数
// 返回: int 向上取整后的商
func ceilDiv(n, d int) int { return n/d + min(n%d, 1) }
