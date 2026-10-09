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
	"math"
)

// transformRCT 对前三个等尺寸分量执行T.800附录G的可逆变换
// 入参: ctx 上下文, planes 分量样本, inverse 是否逆变换
// 返回: error 错误信息
func transformRCT(ctx context.Context, planes [3][]int64, inverse bool) error {
	if len(planes[0]) != len(planes[1]) || len(planes[0]) != len(planes[2]) {
		return FormatError("component transform dimensions")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for i := range planes[0] {
		if i&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		a, b, c := planes[0][i], planes[1][i], planes[2][i]
		if a < -1<<60 || a > 1<<60 || b < -1<<60 || b > 1<<60 || c < -1<<60 || c > 1<<60 {
			return FormatError("component transform arithmetic range")
		}
		if inverse {
			g := a - ((b + c) >> 2)
			planes[0][i], planes[1][i], planes[2][i] = c+g, g, b+g
		} else {
			planes[0][i], planes[1][i], planes[2][i] = (a+2*b+c)>>2, c-b, a-b
		}
	}
	return nil
}

// transformICT 对前三个等尺寸分量执行T.800附录G的不可逆变换
// 入参: ctx 上下文, planes 分量样本, inverse 是否逆变换
// 返回: error 错误信息
func transformICT(ctx context.Context, planes [3][]float64, inverse bool) error {
	if len(planes[0]) != len(planes[1]) || len(planes[0]) != len(planes[2]) {
		return FormatError("component transform dimensions")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for i := range planes[0] {
		if i&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		a, b, c := planes[0][i], planes[1][i], planes[2][i]
		if inverse {
			a, b, c = a+1.402*c, a-0.34413*b-0.71414*c, a+1.772*b
		} else {
			a, b, c = 0.299*a+0.587*b+0.114*c, -0.16875*a-0.33126*b+0.5*c, 0.5*a-0.41869*b-0.08131*c
		}
		if math.IsNaN(a) || math.IsNaN(b) || math.IsNaN(c) || math.IsInf(a, 0) || math.IsInf(b, 0) || math.IsInf(c, 0) {
			return FormatError("component transform nonfinite sample")
		}
		planes[0][i], planes[1][i], planes[2][i] = a, b, c
	}
	return nil
}

// quantizationScale 按T.800表E.1及式E-3计算不可逆子带步长
// 入参: precision 分量精度, orientation 子带方向, step 量化参数
// 返回: float64 量化步长
func quantizationScale(precision uint8, orientation bandOrientation, step quantStep) float64 {
	gain := int(orientation&1) + int((orientation>>1)&1)
	return math.Ldexp(1+float64(step.mantissa)/2048, int(precision)+gain-step.exponent)
}

// coefficientFloat 按当前精度区间中点重建不可逆系数
// 入参: index 系数索引, scale 量化步长
// 返回: float64 重建系数
func (b *codeBlock) coefficientFloat(index int, scale float64) float64 {
	if b.flags[index]&blockSignificant == 0 || b.magnitude[index] == 0 {
		return 0
	}
	value := (float64(b.magnitude[index]) + math.Ldexp(0.5, int(b.lowest[index]))) * scale
	if b.flags[index]&blockNegative != 0 {
		return -value
	}
	return value
}
