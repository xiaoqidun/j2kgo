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
	"fmt"
	"image"
	"io"
)

// Encode 编码标准图像，opts为nil时使用无损JP2格式
// 入参: w 输出流, m 图像, opts 编码选项
// 返回: error 错误信息
func Encode(w io.Writer, m image.Image, opts *EncodeOptions) error {
	return EncodeContext(context.Background(), w, m, opts)
}

// EncodeContext 按瓦片编码标准图像，输出图像的原点为零
// 入参: ctx 上下文, w 输出流, m 图像, opts 编码选项
// 返回: error 错误信息
func EncodeContext(ctx context.Context, w io.Writer, m image.Image, opts *EncodeOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil || m == nil {
		return fmt.Errorf("j2kgo: nil writer or image")
	}
	info := standardImageInfo(m)
	plan, err := makeEncodingPlanContext(ctx, info, opts)
	if err != nil {
		return err
	}
	return encodeImage(ctx, w, plan, func(index int, limits Limits) (*Raster, uint64, error) {
		raster, err := standardImageTile(ctx, m, info, plan.tileBounds(index), limits)
		if err != nil {
			return nil, 0, err
		}
		memory := rasterMetadataSize(info)
		for _, c := range raster.components {
			memory += uint64(len(c.data))
		}
		return raster, memory, nil
	})
}

// standardImageInfo 根据标准图像类型确定分量精度及透明度类型
// 入参: m 图像
// 返回: Info 编码信息
func standardImageInfo(m image.Image) Info {
	info := Info{Bounds: image.Rect(0, 0, m.Bounds().Dx(), m.Bounds().Dy()), ColorSpace: ColorSRGB}
	precision, count, alpha := uint8(16), 4, ChannelPremultiplied
	switch m.(type) {
	case *image.Gray:
		precision, count, info.ColorSpace = 8, 1, ColorGray
	case *image.Gray16:
		count, info.ColorSpace = 1, ColorGray
	case *image.NRGBA:
		precision, alpha = 8, ChannelOpacity
	case *image.NRGBA64:
		alpha = ChannelOpacity
	case *image.RGBA:
		precision = 8
	}
	info.Components = make([]ComponentInfo, count)
	for c := range info.Components {
		info.Components[c] = ComponentInfo{Precision: precision, XStep: 1, YStep: 1}
	}
	if count == 4 {
		info.Channels = []ChannelInfo{{Index: 0, Type: ChannelColor, Association: 1}, {Index: 1, Type: ChannelColor, Association: 2}, {Index: 2, Type: ChannelColor, Association: 3}, {Index: 3, Type: alpha}}
	}
	return info
}

// standardImageTile 按分量精度提取单个瓦片的样本
// 入参: ctx 上下文, m 图像, info 图像信息, bounds 瓦片边界, limits 资源限制
// 返回: *Raster 瓦片样本, error 错误信息
func standardImageTile(ctx context.Context, m image.Image, info Info, bounds image.Rectangle, limits Limits) (*Raster, error) {
	info.Bounds = bounds
	raster, err := newRasterContext(ctx, info, limits)
	if err != nil {
		return nil, err
	}
	origin := m.Bounds().Min
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			values := standardPixel(m, origin.X+x, origin.Y+y)
			for c := range raster.components {
				if err := raster.components[c].SetSample(x, y, values[c]); err != nil {
					return nil, err
				}
			}
		}
	}
	return raster, nil
}

// standardPixel 读取像素通道值，保留标准图像的精度和预乘状态
// 自定义图像通过Color.RGBA读取16位预乘颜色
// 入参: m 图像, x 横坐标, y 纵坐标
// 返回: [4]int64 通道样本
func standardPixel(m image.Image, x, y int) [4]int64 {
	switch m := m.(type) {
	case *image.Gray:
		return [4]int64{int64(m.GrayAt(x, y).Y)}
	case *image.Gray16:
		return [4]int64{int64(m.Gray16At(x, y).Y)}
	case *image.NRGBA:
		c := m.NRGBAAt(x, y)
		return [4]int64{int64(c.R), int64(c.G), int64(c.B), int64(c.A)}
	case *image.NRGBA64:
		c := m.NRGBA64At(x, y)
		return [4]int64{int64(c.R), int64(c.G), int64(c.B), int64(c.A)}
	case *image.RGBA:
		c := m.RGBAAt(x, y)
		return [4]int64{int64(c.R), int64(c.G), int64(c.B), int64(c.A)}
	case *image.RGBA64:
		c := m.RGBA64At(x, y)
		return [4]int64{int64(c.R), int64(c.G), int64(c.B), int64(c.A)}
	default:
		r, g, b, a := m.At(x, y).RGBA()
		return [4]int64{int64(r), int64(g), int64(b), int64(a)}
	}
}
