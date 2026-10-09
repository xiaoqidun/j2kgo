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
	"math"
	"math/bits"
)

// encodingPlan 保存校验后的网格、变换与输出参数
type encodingPlan struct {
	encodingParameters
	info        Info
	format      Format
	tileSize    image.Point
	tileOrigin  image.Point
	columns     int
	rows        int
	tiles       map[int]encodingParameters
	tileRates   bool
	headerBytes uint64
	tileCosts   []tileLayerCost
	layerCosts  []uint64
	tlm         bool
	plm         bool
	ppm         bool
	limits      Limits
}

// EncodeRaster 编码原始分量，保留样本精度、符号及采样间隔
// 入参: ctx 上下文, w 输出流, raster 原始图像, opts 编码选项
// 返回: error 错误信息
func EncodeRaster(ctx context.Context, w io.Writer, raster *Raster, opts *EncodeOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil || raster == nil {
		return fmt.Errorf("j2kgo: nil writer or raster")
	}
	plan, err := makeEncodingPlanContext(ctx, raster.info, opts)
	if err != nil {
		return err
	}
	return encodeImage(ctx, w, plan, func(index int, limits Limits) (*Raster, uint64, error) {
		return raster, 0, nil
	})
}

// makeEncodingPlanContext 校验编码选项并准备全局和瓦片参数，支持取消操作
// 入参: ctx 上下文, info 原始图像信息, opts 编码选项
// 返回: encodingPlan 编码参数, error 错误信息
func makeEncodingPlanContext(ctx context.Context, info Info, opts *EncodeOptions) (encodingPlan, error) {
	if err := ctx.Err(); err != nil {
		return encodingPlan{}, err
	}
	if err := validateInfo(info); err != nil {
		return encodingPlan{}, err
	}
	if len(info.Components) > 16383 && hasRegistration(info) {
		return encodingPlan{}, FormatError("component registration exceeds marker length")
	}
	var options EncodeOptions
	if opts != nil {
		options = *opts
	}
	if options.Format > FormatJ2K || options.Progression > CPRL || options.ColorTransform > ColorTransformIrreversible || options.Wavelet > Wavelet97 || options.CodeBlockStyle&^63 != 0 || options.TilePartPackets < 0 || options.PPM && options.PPT {
		return encodingPlan{}, fmt.Errorf("j2kgo: invalid encoding options")
	}
	if options.TileSize == (image.Point{}) {
		options.TileSize = image.Pt(min(1024, info.Bounds.Dx()), min(1024, info.Bounds.Dy()))
	}
	if options.TileSize.X < 1 || options.TileSize.Y < 1 || uint64(options.TileSize.X) > 0xffffffff || uint64(options.TileSize.Y) > 0xffffffff {
		return encodingPlan{}, fmt.Errorf("j2kgo: invalid tile size")
	}
	if options.CodeBlockSize == (image.Point{}) {
		options.CodeBlockSize = image.Pt(64, 64)
	}
	if options.CodeBlockSize.X < 4 || options.CodeBlockSize.Y < 4 || options.CodeBlockSize.X > 1024 || options.CodeBlockSize.Y > 1024 || !powerOfTwo(options.CodeBlockSize.X) || !powerOfTwo(options.CodeBlockSize.Y) || options.CodeBlockSize.X*options.CodeBlockSize.Y > 4096 {
		return encodingPlan{}, fmt.Errorf("j2kgo: invalid code-block size")
	}
	plan := encodingPlan{info: info, format: options.Format, tileSize: options.TileSize, limits: options.Limits.normalized()}
	plan.encodingParameters = encodingParameters{progression: options.Progression, layerCount: 1, sop: options.SOP, eph: options.EPH}
	plan.partPackets = options.TilePartPackets
	plan.tlm = options.TLM
	plan.plm = options.PLM
	plan.plt = options.PLT
	plan.ppt = options.PPT
	plan.ppm = options.PPM
	plan.tileOrigin = info.Bounds.Min
	if options.TileOrigin != nil {
		plan.tileOrigin = *options.TileOrigin
	}
	if plan.tileOrigin.X < 0 || plan.tileOrigin.Y < 0 || plan.tileOrigin.X > info.Bounds.Min.X || plan.tileOrigin.Y > info.Bounds.Min.Y || info.Bounds.Min.X-plan.tileOrigin.X >= plan.tileSize.X || info.Bounds.Min.Y-plan.tileOrigin.Y >= plan.tileSize.Y {
		return encodingPlan{}, fmt.Errorf("j2kgo: invalid tile origin")
	}
	if len(options.Layers) > 65535 {
		return encodingPlan{}, fmt.Errorf("j2kgo: too many quality layers")
	}
	for i, layer := range options.Layers {
		if math.IsNaN(layer.BitsPerPixel) || math.IsInf(layer.BitsPerPixel, 0) || layer.BitsPerPixel < 0 || (layer.BitsPerPixel == 0 && i != len(options.Layers)-1) || (i > 0 && layer.BitsPerPixel != 0 && layer.BitsPerPixel <= options.Layers[i-1].BitsPerPixel) {
			return encodingPlan{}, fmt.Errorf("j2kgo: invalid quality layer rate")
		}
	}
	if len(options.Layers) > 0 {
		memory := uint64(len(options.Layers)) * 8
		if memory >= plan.limits.MaxMemoryBytes {
			return encodingPlan{}, &LimitError{Resource: "quality layer memory", Limit: plan.limits.MaxMemoryBytes, Required: memory + 1}
		}
		plan.layers = append([]Layer(nil), options.Layers...)
		plan.layerCount = len(plan.layers)
		plan.limits.MaxMemoryBytes -= memory
	}
	plan.columns, plan.rows = ceilDiv(info.Bounds.Max.X-plan.tileOrigin.X, plan.tileSize.X), ceilDiv(info.Bounds.Max.Y-plan.tileOrigin.Y, plan.tileSize.Y)
	if _, err := checkedProduct("tile count", uint64(plan.columns), uint64(plan.rows), 65535); err != nil {
		return encodingPlan{}, err
	}
	minimum := min(plan.tileSize.X, plan.tileSize.Y)
	var samples uint64
	for _, c := range info.Components {
		bounds := componentBounds(info.Bounds, c)
		n, err := checkedProduct("samples", uint64(bounds.Dx()), uint64(bounds.Dy()), plan.limits.MaxSamples)
		if err != nil {
			return encodingPlan{}, err
		}
		samples, err = checkTotal("samples", samples, n, plan.limits.MaxSamples)
		if err != nil {
			return encodingPlan{}, err
		}
		minimum = min(minimum, max(1, plan.tileSize.X/int(c.XStep)), max(1, plan.tileSize.Y/int(c.YStep)))
		if len(options.ProgressionChanges) == 0 && (options.Progression == RPCL || options.Progression == PCRL) && (!powerOfTwo(int(c.XStep)) || !powerOfTwo(int(c.YStep))) {
			return encodingPlan{}, fmt.Errorf("j2kgo: progression requires power-of-two sampling")
		}
	}
	levels := min(5, bits.Len(uint(minimum))-1)
	if options.DecompositionLevels != nil {
		levels = *options.DecompositionLevels
	}
	if levels < 0 || levels > 32 {
		return encodingPlan{}, fmt.Errorf("j2kgo: invalid decomposition levels")
	}
	if len(options.PrecinctSizes) != 0 && len(options.PrecinctSizes) != levels+1 {
		return encodingPlan{}, fmt.Errorf("j2kgo: precinct count must match resolution count")
	}
	memory := uint64(levels+1) * 16
	if memory >= plan.limits.MaxMemoryBytes {
		return encodingPlan{}, &LimitError{Resource: "encoding parameter memory", Limit: plan.limits.MaxMemoryBytes, Required: memory + 1}
	}
	plan.limits.MaxMemoryBytes -= memory
	plan.coding = codingStyle{levels: levels, blockSize: options.CodeBlockSize, style: options.CodeBlockStyle, reversible: options.Wavelet == Wavelet53, precincts: make([]image.Point, levels+1)}
	for level := range plan.coding.precincts {
		size := image.Pt(32768, 32768)
		if len(options.PrecinctSizes) != 0 {
			size = options.PrecinctSizes[level]
		}
		if !powerOfTwo(size.X) || !powerOfTwo(size.Y) || size.X > 32768 || size.Y > 32768 || (level > 0 && (size.X < 2 || size.Y < 2)) {
			return encodingPlan{}, fmt.Errorf("j2kgo: invalid precinct size")
		}
		plan.coding.precincts[level] = size
	}
	if err := plan.setComponents(options.Quantization, options.Components); err != nil {
		return encodingPlan{}, err
	}
	if err := plan.setROI(options.ROI); err != nil {
		return encodingPlan{}, err
	}
	if err := plan.setProgressions(options.ProgressionChanges); err != nil {
		return encodingPlan{}, err
	}
	rgb := info.ColorSpace == ColorSRGB
	if len(info.ICCProfile) != 0 {
		rgb = len(info.ICCProfile) >= 20 && string(info.ICCProfile[16:20]) == "RGB "
	}
	plan.mct = rgb && standardMCTChannels(info) && len(info.Components) >= 3 && info.Components[0] == info.Components[1] && info.Components[0] == info.Components[2]
	switch options.ColorTransform {
	case ColorTransformNone:
		plan.mct = false
	case ColorTransformReversible, ColorTransformIrreversible:
		plan.mct = true
		if (options.ColorTransform == ColorTransformReversible) != plan.componentCoding(0).reversible {
			return encodingPlan{}, fmt.Errorf("j2kgo: color and wavelet transforms must use the same reversibility")
		}
	}
	if plan.mct {
		first := plan.componentCoding(0)
		for c := 1; c < 3; c++ {
			style := plan.componentCoding(c)
			if first.reversible != style.reversible {
				if options.ColorTransform != ColorTransformAuto {
					return encodingPlan{}, fmt.Errorf("j2kgo: incompatible component transform coding")
				}
				plan.mct = false
				break
			}
		}
	}
	if plan.mct && (len(info.Components) < 3 || info.Components[0].Precision != info.Components[1].Precision || info.Components[0].Precision != info.Components[2].Precision || info.Components[0].XStep != info.Components[1].XStep || info.Components[0].XStep != info.Components[2].XStep || info.Components[0].YStep != info.Components[1].YStep || info.Components[0].YStep != info.Components[2].YStep) {
		return encodingPlan{}, fmt.Errorf("j2kgo: incompatible component transform")
	}
	if plan.format == FormatJP2 && info.ColorSpace == ColorUnknown && len(info.ICCProfile) == 0 {
		return encodingPlan{}, fmt.Errorf("j2kgo: JP2 encoding requires a color specification")
	}
	if plan.format == FormatJP2 {
		if err := validateColorChannels(info); err != nil {
			return encodingPlan{}, err
		}
		if len(info.ICCProfile) == 0 && info.ColorSpace != ColorSRGB && info.ColorSpace != ColorGray && info.ColorSpace != ColorSYCC {
			return encodingPlan{}, UnsupportedError("JP2 color specification")
		}
		if plan.mct && (!rgb || !standardMCTChannels(info)) {
			return encodingPlan{}, fmt.Errorf("j2kgo: JP2 component transform requires ordered RGB channels")
		}
	}
	if err := plan.setTiles(ctx, options); err != nil {
		return encodingPlan{}, err
	}
	return plan, nil
}

// standardMCTChannels 检查通道映射是否保持前三个颜色分量的顺序
// 入参: info 图像信息
// 返回: bool 是否保持原分量顺序
func standardMCTChannels(info Info) bool {
	if len(info.Mapping) > 0 {
		if len(info.Mapping) < 3 {
			return false
		}
		for i, mapping := range info.Mapping[:3] {
			if mapping.Type != MappingDirect || int(mapping.Component) != i {
				return false
			}
		}
	}
	for _, channel := range info.Channels {
		if channel.Type == ChannelColor && channel.Association >= 1 && channel.Association <= 3 && channel.Index != channel.Association-1 {
			return false
		}
	}
	return true
}

// tileBounds 返回编码瓦片的参考网格边界
// 入参: index 瓦片序号
// 返回: image.Rectangle 瓦片边界
func (p encodingPlan) tileBounds(index int) image.Rectangle {
	x, y := int64(index%p.columns), int64(index/p.columns)
	x0, y0 := int64(p.tileOrigin.X)+x*int64(p.tileSize.X), int64(p.tileOrigin.Y)+y*int64(p.tileSize.Y)
	return image.Rect(int(max(int64(p.info.Bounds.Min.X), x0)), int(max(int64(p.info.Bounds.Min.Y), y0)), int(min(int64(p.info.Bounds.Max.X), x0+int64(p.tileSize.X))), int(min(int64(p.info.Bounds.Max.Y), y0+int64(p.tileSize.Y))))
}

// encodingQuantization 生成保护位及各子带的量化参数
// 入参: precision 分量精度, coding 编码方式
// 返回: quantization 量化参数
func encodingQuantization(precision uint8, coding codingStyle) quantization {
	q := quantization{guard: max(2, min(7, int(precision)-27)), steps: make([]quantStep, 3*coding.levels+1)}
	if !coding.reversible {
		q.kind = 2
	}
	for i := range q.steps {
		gain := 0
		if i > 0 {
			gain = 1
			if i%3 == 0 {
				gain = 2
			}
		}
		q.steps[i].exponent = min(31, int(precision)+gain)
	}
	return q
}
