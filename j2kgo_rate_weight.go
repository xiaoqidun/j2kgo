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

// waveletRateWeights 保存各小波子带的失真权重，用于估算重建误差
var waveletRateWeights = makeWaveletRateWeights()

// makeWaveletRateWeights 通过合成滤波器的自相关计算子带权重，不计边界效应和系数间的相关性
// 返回: [2][33][4]float64 按小波、合成级数和子带方向排列的失真权重
func makeWaveletRateWeights() [2][33][4]float64 {
	var result [2][33][4]float64
	for wavelet := range result {
		var correlation [2][9]float64
		for band := range correlation {
			var values [32]float64
			values[16+band] = 1
			if wavelet == int(Wavelet53) {
				lift97(values[:], 0, -0.25)
				lift97(values[:], 1, 0.5)
			} else {
				wavelet97(values[:], 0, true)
			}
			for lag := range correlation[band] {
				for i := lag; i < len(values); i++ {
					correlation[band][lag] += values[i] * values[i-lag]
				}
			}
		}
		var previous [17]float64
		previous[8] = 1
		result[wavelet][0][0] = 1
		for level := 1; level <= 32; level++ {
			var current [17]float64
			var high float64
			for k := -8; k <= 8; k++ {
				for lag := -8; lag <= 8; lag++ {
					index := 2*k - lag
					if index >= -8 && index <= 8 {
						current[k+8] += correlation[0][max(lag, -lag)] * previous[index+8]
					}
				}
				high += correlation[1][max(k, -k)] * previous[k+8]
			}
			low := current[8]
			result[wavelet][level] = [4]float64{low * low, high * low, low * high, high * high}
			previous = current
		}
	}
	return result
}

// rateWeight 返回子带的重建误差权重，可逆小波变换按线性变换近似计算
// 入参: resolution 分辨率级, orientation 子带方向
// 返回: float64 子带失真权重
func (c codingStyle) rateWeight(resolution int, orientation bandOrientation) float64 {
	wavelet := Wavelet97
	if c.reversible {
		wavelet = Wavelet53
	}
	level := c.levels
	if orientation != 0 {
		level = c.levels - resolution + 1
	}
	return waveletRateWeights[wavelet][level][orientation]
}

// componentRateWeight 根据分量逆变换矩阵各列的平方和计算失真权重，忽略可逆变换的整数舍入
// 入参: index 分量索引
// 返回: float64 分量失真权重
func (p encodingPlan) componentRateWeight(index int) float64 {
	if !p.mct || index >= 3 {
		return 1
	}
	if index == 0 {
		return 3
	}
	if p.componentCoding(index).reversible {
		return 0.25*0.25 + 0.25*0.25 + 0.75*0.75
	}
	if index == 1 {
		return 0.34413*0.34413 + 1.772*1.772
	}
	return 1.402*1.402 + 0.71414*0.71414
}
