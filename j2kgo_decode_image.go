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
	"image"
	"io"
)

// init 向image包注册JP2和J2K解码器
func init() {
	image.RegisterFormat("jp2", "\x00\x00\x00\x0cjP  \r\n\x87\n", Decode, DecodeConfig)
	image.RegisterFormat("j2k", "\xff\x4f\xff\x51", Decode, DecodeConfig)
}

// Decode 解码为标准图像，多分量裸码流需使用DecodeContext指定颜色空间
// 入参: r 输入流
// 返回: image.Image 图像, error 错误信息
func Decode(r io.Reader) (image.Image, error) {
	return DecodeContext(context.Background(), r, nil)
}

// DecodeContext 在资源限制内解码并转换为标准图像，不关闭调用方的输入流
// 入参: ctx 上下文, r 输入流, opts 解码选项
// 返回: image.Image 图像, error 错误信息
func DecodeContext(ctx context.Context, r io.Reader, opts *DecodeOptions) (image.Image, error) {
	d, err := NewDecoder(ctx, r, opts)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	limits := d.options.Limits
	info := d.info
	info.Bounds = reduceBounds(info.Bounds, d.options.Reduce)
	plan, err := makeDisplayPlan(ctx, info)
	if err != nil {
		return nil, err
	}
	memory, err := plan.memory(limits)
	if err != nil {
		return nil, err
	}
	if memory >= d.options.Limits.MaxMemoryBytes {
		return nil, &LimitError{Resource: "display memory", Limit: d.options.Limits.MaxMemoryBytes, Required: memory + 1}
	}
	d.options.Limits.MaxMemoryBytes -= memory
	raster, err := d.Decode(ctx)
	if err != nil {
		return nil, err
	}
	if err := d.Close(); err != nil {
		return nil, err
	}
	live := rasterMetadataSize(raster.info) + uint64(len(info.ICCProfile))
	for _, component := range raster.components {
		live += uint64(len(component.data))
	}
	if _, err := checkTotal("display memory", live, memory, limits.MaxMemoryBytes); err != nil {
		return nil, err
	}
	limits.MaxMemoryBytes -= live
	return renderRasterImage(ctx, raster, plan, limits)
}
