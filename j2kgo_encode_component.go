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
	"fmt"
	"image"
	"slices"
)

// componentEncoding 保存分量专用的编码及量化参数
type componentEncoding struct {
	coding codingStyle
	quant  quantization
}

// componentCoding 返回分量专用编码参数，未指定时使用默认参数
// 入参: index 分量索引
// 返回: codingStyle 编码参数
func (p encodingPlan) componentCoding(index int) codingStyle {
	if component, ok := p.components[index]; ok {
		return component.coding
	}
	return p.coding
}

// componentQuantization 优先使用分量专用量化参数，其次使用默认参数，均未指定时自动生成
// 入参: index 分量索引
// 返回: quantization 量化参数
func (p encodingPlan) componentQuantization(index int) quantization {
	if component, ok := p.components[index]; ok {
		return component.quant
	}
	if p.quant != nil {
		return *p.quant
	}
	return encodingQuantization(p.info.Components[index].Precision, p.coding)
}

// maxLevels 返回所有分量的最大分解级数
// 返回: int 分解级数
func (p encodingPlan) maxLevels() int {
	if len(p.components) == 0 {
		return p.coding.levels
	}
	levels := 0
	for c := range p.info.Components {
		levels = max(levels, p.componentCoding(c).levels)
	}
	return levels
}

// setComponents 校验并复制默认量化参数和分量专用参数
// 入参: quant 默认量化参数, options 分量选项
// 返回: error 错误信息
func (p *encodingPlan) setComponents(quant *Quantization, options []ComponentOptions) error {
	budget := layoutBudget{limit: p.limits.MaxMemoryBytes}
	if len(options) > len(p.info.Components) {
		return fmt.Errorf("j2kgo: too many component options")
	}
	if quant != nil {
		q, err := makeEncodingQuantization(quant, p.coding, &budget)
		if err != nil {
			return err
		}
		p.quant = &q
	}
	if len(options) > 0 {
		if err := budget.add(uint64(len(options)), 256); err != nil {
			return err
		}
		p.components = make(map[int]componentEncoding, len(options))
	}
	for _, option := range options {
		if option.Index < 0 || option.Index >= len(p.info.Components) {
			return fmt.Errorf("j2kgo: invalid component index")
		}
		if _, ok := p.components[option.Index]; ok {
			return fmt.Errorf("j2kgo: duplicate component options")
		}
		coding, err := overrideCoding(p.coding, option, &budget)
		if err != nil {
			return fmt.Errorf("component %d: %w", option.Index, err)
		}
		var q quantization
		switch {
		case option.Quantization != nil:
			q, err = makeEncodingQuantization(option.Quantization, coding, &budget)
		case p.quant != nil:
			q = *p.quant
			if q.kind != 1 {
				if len(q.steps) < 3*coding.levels+1 {
					return fmt.Errorf("j2kgo: component %d requires more quantization steps", option.Index)
				}
				q.steps = q.steps[:3*coding.levels+1]
			}
			if (q.kind == 0) != coding.reversible {
				return fmt.Errorf("j2kgo: component %d quantization does not match wavelet", option.Index)
			}
		default:
			if err = budget.add(uint64(3*coding.levels+1), 16); err == nil {
				q = encodingQuantization(p.info.Components[option.Index].Precision, coding)
			}
		}
		if err != nil {
			return fmt.Errorf("component %d: %w", option.Index, err)
		}
		p.components[option.Index] = componentEncoding{coding: coding, quant: q}
	}
	if budget.used >= p.limits.MaxMemoryBytes {
		return &LimitError{Resource: "encoding parameter memory", Limit: p.limits.MaxMemoryBytes, Required: budget.used + 1}
	}
	p.limits.MaxMemoryBytes -= budget.used
	return nil
}

// overrideCoding 将分量选项应用到默认编码参数
// 入参: base 默认参数, option 分量选项, budget 参数内存预算
// 返回: codingStyle 合并后的参数, error 错误信息
func overrideCoding(base codingStyle, option ComponentOptions, budget *layoutBudget) (codingStyle, error) {
	coding := base
	if option.Wavelet != nil {
		if *option.Wavelet > Wavelet97 {
			return codingStyle{}, fmt.Errorf("invalid wavelet")
		}
		coding.reversible = *option.Wavelet == Wavelet53
	}
	if option.DecompositionLevels != nil {
		coding.levels = *option.DecompositionLevels
	}
	if coding.levels < 0 || coding.levels > 32 {
		return codingStyle{}, fmt.Errorf("invalid decomposition levels")
	}
	if option.CodeBlockSize != (image.Point{}) {
		coding.blockSize = option.CodeBlockSize
	}
	size := coding.blockSize
	if size.X < 4 || size.Y < 4 || size.X > 1024 || size.Y > 1024 || !powerOfTwo(size.X) || !powerOfTwo(size.Y) || size.X*size.Y > 4096 {
		return codingStyle{}, fmt.Errorf("invalid code-block size")
	}
	if option.CodeBlockStyle != nil {
		coding.style = *option.CodeBlockStyle
	}
	if coding.style&^63 != 0 {
		return codingStyle{}, fmt.Errorf("invalid code-block style")
	}
	if len(option.PrecinctSizes) == 0 && coding.levels == base.levels {
		return coding, nil
	}
	if len(option.PrecinctSizes) != 0 && len(option.PrecinctSizes) != coding.levels+1 {
		return codingStyle{}, fmt.Errorf("precinct count must match resolution count")
	}
	if err := budget.add(uint64(coding.levels+1), 16); err != nil {
		return codingStyle{}, err
	}
	coding.precincts = make([]image.Point, coding.levels+1)
	for level := range coding.precincts {
		size := image.Pt(32768, 32768)
		if len(option.PrecinctSizes) != 0 {
			size = option.PrecinctSizes[level]
		}
		if !powerOfTwo(size.X) || !powerOfTwo(size.Y) || size.X > 32768 || size.Y > 32768 || level > 0 && (size.X < 2 || size.Y < 2) {
			return codingStyle{}, fmt.Errorf("invalid precinct size")
		}
		coding.precincts[level] = size
	}
	return coding, nil
}

// makeEncodingQuantization 校验量化选项并复制子带参数
// 入参: options 量化选项, coding 编码参数, budget 参数内存预算
// 返回: quantization 量化参数, error 错误信息
func makeEncodingQuantization(options *Quantization, coding codingStyle, budget *layoutBudget) (quantization, error) {
	if options.Style > QuantizationExpounded || options.GuardBits > 7 || (options.Style == QuantizationNone) != coding.reversible {
		return quantization{}, fmt.Errorf("j2kgo: invalid quantization style or guard bits")
	}
	count := 3*coding.levels + 1
	if options.Style == QuantizationDerived {
		count = 1
	}
	if len(options.Steps) != count {
		return quantization{}, fmt.Errorf("j2kgo: quantization step count does not match coding style")
	}
	if err := budget.add(1, 48+uint64(count)*16); err != nil {
		return quantization{}, err
	}
	q := quantization{kind: uint8(options.Style), guard: int(options.GuardBits), steps: make([]quantStep, count)}
	for i, step := range options.Steps {
		if step.Exponent > 31 || step.Mantissa > 2047 || q.kind == 0 && step.Mantissa != 0 {
			return quantization{}, fmt.Errorf("j2kgo: invalid quantization step")
		}
		q.steps[i] = quantStep{exponent: int(step.Exponent), mantissa: step.Mantissa}
	}
	return q, nil
}

// equalCoding 比较编码方式及各级区域尺寸
// 入参: a 第一组参数, b 第二组参数
// 返回: bool 是否相同
func equalCoding(a, b codingStyle) bool {
	return a.levels == b.levels && a.blockSize == b.blockSize && a.style == b.style && a.reversible == b.reversible && slices.Equal(a.precincts, b.precincts)
}

// equalQuantization 比较量化方式、保护位及子带参数
// 入参: a 第一组参数, b 第二组参数
// 返回: bool 是否相同
func equalQuantization(a, b quantization) bool {
	return a.kind == b.kind && a.guard == b.guard && slices.Equal(a.steps, b.steps)
}
