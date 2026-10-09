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
	"encoding/binary"
	"image"
)

// codingStyle 保存分量的小波分解、码块编码及区域划分参数
type codingStyle struct {
	levels     int
	blockSize  image.Point
	style      CodeBlockStyle
	reversible bool
	precincts  []image.Point
}

// quantization 保存量化方式、保护位数及各子带的步长参数
type quantization struct {
	kind  uint8
	guard int
	steps []quantStep
}

// quantStep 保存量化步长的指数与尾数
type quantStep struct {
	exponent int
	mantissa uint16
}

// readCodingStyle 读取COD或COC中的分量编码参数
// 入参: data 参数数据, precincts 是否包含区域尺寸
// 返回: codingStyle 分量编码参数, error 错误信息
func readCodingStyle(data []byte, precincts bool) (codingStyle, error) {
	if len(data) < 5 {
		return codingStyle{}, FormatError("coding style length")
	}
	levels := int(data[0])
	length := 5
	if precincts {
		length += levels + 1
	}
	if levels > 32 || len(data) != length || data[1] > 8 || data[2] > 8 || int(data[1])+int(data[2]) > 8 || data[3]&^63 != 0 || data[4] > 1 {
		return codingStyle{}, FormatError("coding style fields")
	}
	result := codingStyle{levels: levels, blockSize: image.Pt(1<<(data[1]+2), 1<<(data[2]+2)),
		style: CodeBlockStyle(data[3]), reversible: data[4] == 1, precincts: make([]image.Point, levels+1)}
	for i := range result.precincts {
		x, y := uint8(15), uint8(15)
		if precincts {
			x, y = data[5+i]&15, data[5+i]>>4
		}
		if i > 0 && (x == 0 || y == 0) {
			return codingStyle{}, FormatError("precinct size at nonzero resolution")
		}
		result.precincts[i] = image.Pt(1<<x, 1<<y)
	}
	return result, nil
}

// readQuantization 读取QCD或QCC中的量化参数
// 入参: data 参数数据
// 返回: quantization 量化信息, error 错误信息
func readQuantization(data []byte) (quantization, error) {
	if len(data) < 2 {
		return quantization{}, FormatError("quantization length")
	}
	result := quantization{kind: data[0] & 31, guard: int(data[0] >> 5)}
	width := 1
	if result.kind != 0 {
		width = 2
	}
	if result.kind > 2 || (len(data)-1)%width != 0 || (result.kind == 1 && len(data) != 3) {
		return quantization{}, FormatError("quantization style or length")
	}
	count := (len(data) - 1) / width
	if count > 97 {
		return quantization{}, FormatError("quantization subband count")
	}
	result.steps = make([]quantStep, count)
	for i := range result.steps {
		if width == 1 {
			if data[1+i]&7 != 0 {
				return quantization{}, FormatError("quantization reserved bits")
			}
			result.steps[i].exponent = int(data[1+i] >> 3)
		} else {
			v := binary.BigEndian.Uint16(data[1+2*i:])
			result.steps[i] = quantStep{exponent: int(v >> 11), mantissa: v & 2047}
		}
	}
	return result, nil
}

// step 返回指定子带的量化参数，派生量化时按子带调整指数
// 入参: band 子带序号, levels 分解级数
// 返回: quantStep 量化参数, error 错误信息
func (q quantization) step(band, levels int) (quantStep, error) {
	if levels < 0 || levels > 32 || band < 0 || band > 3*levels || len(q.steps) == 0 {
		return quantStep{}, FormatError("quantization subband index")
	}
	if q.kind == 1 {
		step := q.steps[0]
		if band > 0 {
			step.exponent -= (band - 1) / 3
		}
		return step, nil
	}
	if len(q.steps) < 3*levels+1 {
		return quantStep{}, FormatError("missing subband quantization")
	}
	return q.steps[band], nil
}
