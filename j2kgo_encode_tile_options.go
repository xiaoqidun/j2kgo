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
	"slices"
)

// encodingParameters 保存整幅图像或单个瓦片使用的编码参数
type encodingParameters struct {
	coding         codingStyle
	quant          *quantization
	components     map[int]componentEncoding
	progression    Progression
	volumes        []progressionVolume
	layerCount     int
	layers         []Layer
	roi            []image.Rectangle
	partPackets    int
	parameterBytes uint64
	mct            bool
	sop, eph       bool
	plt, ppt       bool
}

// setTiles 校验并复制瓦片选项，统计瓦片首段的参数长度和内存用量
// 入参: ctx 上下文, options 全局编码选项
// 返回: error 错误信息
func (p *encodingPlan) setTiles(ctx context.Context, options EncodeOptions) error {
	if len(options.Tiles) == 0 {
		return nil
	}
	if len(options.Tiles) > p.columns*p.rows {
		return fmt.Errorf("j2kgo: too many tile options")
	}
	memory, err := checkedProduct("tile parameter memory", uint64(len(options.Tiles)), 512, p.limits.MaxMemoryBytes)
	if err != nil {
		return err
	}
	if memory == p.limits.MaxMemoryBytes {
		return &LimitError{Resource: "tile parameter memory", Limit: p.limits.MaxMemoryBytes, Required: memory + 1}
	}
	p.limits.MaxMemoryBytes -= memory
	p.tiles = make(map[int]encodingParameters, len(options.Tiles))
	for _, tile := range options.Tiles {
		if err := ctx.Err(); err != nil {
			return err
		}
		if tile.Index < 0 || tile.Index >= p.columns*p.rows {
			return fmt.Errorf("j2kgo: invalid tile index")
		}
		if _, ok := p.tiles[tile.Index]; ok {
			return fmt.Errorf("j2kgo: duplicate tile options")
		}
		selected, temporary, err := p.tileOptions(options, tile)
		if err != nil {
			return fmt.Errorf("tile %d: %w", tile.Index, err)
		}
		local, err := makeEncodingPlanContext(ctx, p.info, selected)
		if err != nil {
			return fmt.Errorf("tile %d: %w", tile.Index, err)
		}
		local.limits.MaxMemoryBytes += temporary
		if len(local.volumes) == 0 && len(p.volumes) != 0 {
			if err := local.setProgressions([]ProgressionChange{{Order: local.progression}}); err != nil {
				return fmt.Errorf("tile %d: %w", tile.Index, err)
			}
		}
		if !p.sameCodingHeader(local) {
			writer := codestreamWriter{ctx: ctx, w: io.Discard}
			if err := writer.codingHeader(local); err != nil {
				return fmt.Errorf("tile %d: %w", tile.Index, err)
			}
			local.parameterBytes = writer.written
		}
		p.tiles[tile.Index] = local.encodingParameters
		p.tileRates = p.tileRates || tile.Layers != nil
		p.limits = local.limits
	}
	return nil
}

// tileOptions 合并全局选项和瓦片选项，瓦片未指定的选项沿用全局值
// 入参: options 全局选项, tile 瓦片选项
// 返回: *EncodeOptions 合并后的选项, uint64 临时选项占用的内存预算, error 错误信息
func (p encodingPlan) tileOptions(options EncodeOptions, tile TileOptions) (*EncodeOptions, uint64, error) {
	inherited := options.Quantization
	options.Tiles = nil
	options.Limits = p.limits
	options.TileSize = p.tileSize
	options.TileOrigin = &p.tileOrigin
	options.DecompositionLevels = &p.coding.levels
	if tile.Wavelet != nil {
		options.Wavelet = *tile.Wavelet
	}
	if tile.DecompositionLevels != nil {
		options.DecompositionLevels = tile.DecompositionLevels
		if *tile.DecompositionLevels != p.coding.levels {
			options.PrecinctSizes = nil
		}
	}
	if tile.CodeBlockSize != (image.Point{}) {
		options.CodeBlockSize = tile.CodeBlockSize
	}
	if tile.CodeBlockStyle != nil {
		options.CodeBlockStyle = *tile.CodeBlockStyle
	}
	if tile.Quantization != nil {
		options.Quantization = tile.Quantization
	} else if options.Quantization != nil && options.Quantization.Style != QuantizationDerived {
		levels := *options.DecompositionLevels
		if levels >= 0 && levels <= 32 && len(options.Quantization.Steps) >= 3*levels+1 {
			quant := *options.Quantization
			quant.Steps = quant.Steps[:3*levels+1]
			options.Quantization = &quant
		}
	}
	if tile.Components != nil {
		options.Components = tile.Components
	}
	if len(tile.PrecinctSizes) != 0 {
		options.PrecinctSizes = tile.PrecinctSizes
	}
	if tile.Progression != nil {
		options.Progression = *tile.Progression
	}
	if tile.ProgressionChanges != nil {
		options.ProgressionChanges = tile.ProgressionChanges
	}
	if tile.ColorTransform != nil {
		options.ColorTransform = *tile.ColorTransform
	}
	if tile.ROI != nil {
		options.ROI = tile.ROI
	}
	if tile.Layers != nil {
		options.Layers = tile.Layers
	}
	if tile.TilePartPackets != nil {
		options.TilePartPackets = *tile.TilePartPackets
	}
	if tile.SOP != nil {
		options.SOP = *tile.SOP
	}
	if tile.EPH != nil {
		options.EPH = *tile.EPH
	}
	if tile.PLT != nil {
		options.PLT = *tile.PLT
	}
	if tile.PPT != nil {
		options.PPT = *tile.PPT
	}
	if inherited == nil || tile.Quantization != nil || len(options.Components) == 0 {
		return &options, 0, nil
	}
	memory, err := checkedProduct("tile option memory", uint64(len(options.Components)), 192, options.Limits.MaxMemoryBytes)
	if err != nil {
		return nil, 0, err
	}
	if memory == options.Limits.MaxMemoryBytes {
		return nil, 0, &LimitError{Resource: "tile option memory", Limit: options.Limits.MaxMemoryBytes, Required: memory + 1}
	}
	options.Limits.MaxMemoryBytes -= memory
	components := make([]ComponentOptions, len(options.Components))
	copy(components, options.Components)
	for i := range components {
		if components[i].Quantization != nil {
			continue
		}
		levels := *options.DecompositionLevels
		if components[i].DecompositionLevels != nil {
			levels = *components[i].DecompositionLevels
		}
		quant := *inherited
		if quant.Style != QuantizationDerived && levels >= 0 && levels <= 32 && len(quant.Steps) >= 3*levels+1 {
			quant.Steps = quant.Steps[:3*levels+1]
		}
		components[i].Quantization = &quant
	}
	options.Components = components
	return &options, memory, nil
}

// tilePlan 应用瓦片专用参数，沿用全局预算和图像网格
// 入参: index 瓦片索引
// 返回: encodingPlan 瓦片编码参数
func (p encodingPlan) tilePlan(index int) encodingPlan {
	if parameters, ok := p.tiles[index]; ok {
		p.encodingParameters = parameters
	}
	return p
}

// sameCodingHeader 判断瓦片能否直接沿用主头参数，避免重复写入相同标记
// 入参: local 瓦片编码参数
// 返回: bool 是否可沿用主头参数
func (p encodingPlan) sameCodingHeader(local encodingPlan) bool {
	if p.progression != local.progression || p.layerCount != local.layerCount || p.mct != local.mct || p.sop != local.sop || p.eph != local.eph || !equalCoding(p.coding, local.coding) || !slices.Equal(p.volumes, local.volumes) {
		return false
	}
	for c := range p.info.Components {
		if !equalCoding(p.componentCoding(c), local.componentCoding(c)) || !equalQuantization(p.componentQuantization(c), local.componentQuantization(c)) {
			return false
		}
	}
	return true
}
