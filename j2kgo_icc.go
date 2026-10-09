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
	"math"
)

// iccTransformMemory 颜色转换参数的内存预算，不含原始配置及查找表
const iccTransformMemory uint64 = 512

// iccDisplayMatrix D50白点下XYZ到线性sRGB的转换矩阵
var iccDisplayMatrix = makeICCDisplayMatrix()

// iccTransform 保存JP2受限ICC的曲线与合成矩阵
type iccTransform struct {
	count  int
	curves [3]iccCurve
	matrix [3][3]float64
	input  [3][]float64
}

// iccCurve 引用ICC曲线数据，不复制采样表
type iccCurve struct {
	table []byte
	kind  uint16
	param [7]float64
}

// iccTag 保存受限ICC转换所需的标签
type iccTag struct {
	name string
	data []byte
}

// readRestrictedICC 按ISO15076-1解析矩阵或灰度ICC配置，引用原始数据
// 入参: ctx 上下文, profile ICC配置
// 返回: iccTransform 颜色转换参数, error 错误信息
func readRestrictedICC(ctx context.Context, profile []byte) (iccTransform, error) {
	var result iccTransform
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if len(profile) < 132 || uint64(binary.BigEndian.Uint32(profile)) != uint64(len(profile)) || string(profile[36:40]) != "acsp" {
		return result, FormatError("ICC profile header")
	}
	if profile[8] != 2 && profile[8] != 4 {
		return result, UnsupportedError("ICC profile version")
	}
	if class := string(profile[12:16]); class != "scnr" && class != "mntr" {
		return result, UnsupportedError("JP2 requires an ICC input or display profile")
	}
	if string(profile[20:24]) != "XYZ " {
		return result, UnsupportedError("JP2 requires an ICC XYZ connection space")
	}
	switch string(profile[16:20]) {
	case "GRAY":
		result.count = 1
	case "RGB ":
		result.count = 3
	default:
		return result, UnsupportedError("JP2 requires an ICC gray or matrix RGB profile")
	}
	tags := [7]iccTag{{name: "kTRC"}, {name: "rTRC"}, {name: "gTRC"}, {name: "bTRC"}, {name: "rXYZ"}, {name: "gXYZ"}, {name: "bXYZ"}}
	count := uint64(binary.BigEndian.Uint32(profile[128:132]))
	start := uint64(132) + 12*count
	if start > uint64(len(profile)) {
		return result, FormatError("ICC tag table length")
	}
	for pos := 132; uint64(pos) < start; pos += 12 {
		if (pos-132)%12288 == 0 {
			if err := ctx.Err(); err != nil {
				return result, err
			}
		}
		name := string(profile[pos : pos+4])
		offset := uint64(binary.BigEndian.Uint32(profile[pos+4 : pos+8]))
		size := uint64(binary.BigEndian.Uint32(profile[pos+8 : pos+12]))
		if offset < start || offset&3 != 0 || size < 8 || offset+size > uint64(len(profile)) {
			return result, FormatError("ICC tag extent")
		}
		if (name[:3] == "A2B" || name[:3] == "D2B") && name[3] >= '0' && name[3] <= '3' {
			return result, UnsupportedError("JP2 restricted ICC excludes multidimensional transforms")
		}
		for i := range tags {
			if name == tags[i].name {
				if tags[i].data != nil {
					return result, FormatError("duplicate ICC transform tag")
				}
				tags[i].data = profile[offset : offset+size]
			}
		}
	}
	if result.count == 1 {
		curve, err := readICCCurve(ctx, tags[0].data, profile[8])
		result.curves[0] = curve
		return result, err
	}
	for i := range result.count {
		curve, err := readICCCurve(ctx, tags[1+i].data, profile[8])
		if err != nil {
			return result, err
		}
		result.curves[i] = curve
		data := tags[4+i].data
		if len(data) != 20 || string(data[:4]) != "XYZ " || binary.BigEndian.Uint32(data[4:8]) != 0 {
			return result, FormatError("ICC colorant tag")
		}
		for row := range 3 {
			for k := range 3 {
				result.matrix[row][i] += iccDisplayMatrix[row][k] * iccFixed(data[8+4*k:])
			}
		}
	}
	return result, nil
}

// readICCCurve 读取恒等、幂函数、采样及参数曲线
// 入参: ctx 上下文, data 标签数据, version ICC配置文件的主版本号
// 返回: iccCurve 曲线, error 错误信息
func readICCCurve(ctx context.Context, data []byte, version byte) (iccCurve, error) {
	curve := iccCurve{param: [7]float64{1}}
	if err := ctx.Err(); err != nil {
		return curve, err
	}
	if len(data) < 12 || binary.BigEndian.Uint32(data[4:8]) != 0 {
		return curve, FormatError("ICC tone curve tag")
	}
	switch string(data[:4]) {
	case "curv":
		count := uint64(binary.BigEndian.Uint32(data[8:12]))
		if 12+count*2 != uint64(len(data)) {
			return curve, FormatError("ICC sampled curve length")
		}
		if count == 1 {
			curve.param[0] = float64(binary.BigEndian.Uint16(data[12:14])) / 256
		} else if count > 1 {
			curve.table = data[12:]
			for i := 2; i < len(curve.table); i += 2 {
				if i&8191 == 0 {
					if err := ctx.Err(); err != nil {
						return curve, err
					}
				}
				if binary.BigEndian.Uint16(curve.table[i:]) < binary.BigEndian.Uint16(curve.table[i-2:]) {
					return curve, FormatError("ICC tone curve is not increasing")
				}
			}
		}
	case "para":
		if version != 4 {
			return curve, FormatError("ICC parametric curve requires version 4")
		}
		curve.kind = binary.BigEndian.Uint16(data[8:10])
		if curve.kind > 4 {
			return curve, UnsupportedError("ICC parametric curve function")
		}
		count := [...]int{1, 3, 4, 5, 7}
		if binary.BigEndian.Uint16(data[10:12]) != 0 || len(data) != 12+4*count[curve.kind] {
			return curve, FormatError("ICC parametric curve length")
		}
		for i := range count[curve.kind] {
			curve.param[i] = iccFixed(data[12+4*i:])
		}
	default:
		return curve, UnsupportedError("ICC tone curve type")
	}
	if err := curve.validate(); err != nil {
		return curve, err
	}
	return curve, nil
}

// iccFixed 读取ICC有符号15.16定点数
// 入参: data 定点数数据
// 返回: float64 数值
func iccFixed(data []byte) float64 {
	return float64(int32(binary.BigEndian.Uint32(data))) / 65536
}

// validate 检查曲线参数定义域及单调性
// 返回: error 错误信息
func (c iccCurve) validate() error {
	g, a, b, slope, d, e, f := c.param[0], c.param[1], c.param[2], c.param[3], c.param[4], c.param[5], c.param[6]
	if g <= 0 {
		return FormatError("ICC tone curve gamma")
	}
	if c.kind == 1 || c.kind == 2 {
		if a <= 0 {
			return FormatError("ICC tone curve scale")
		}
	}
	if c.kind == 3 || c.kind == 4 {
		if a < 0 || slope < 0 || (d <= 1 && a*max(0, d)+b < 0) {
			return FormatError("ICC tone curve domain")
		}
		if c.kind == 3 {
			e, f = 0, 0
		}
		if d > 0 && d <= 1 && math.Pow(a*d+b, g)+e+1.0/65536 < slope*d+f {
			return FormatError("ICC tone curve is not increasing")
		}
	}
	return nil
}

// value 将设备值转换为归一化线性强度
// 入参: x 归一化设备值
// 返回: float64 线性强度
func (c iccCurve) value(x float64) float64 {
	x = max(0, min(1, x))
	if len(c.table) > 0 {
		position := x * float64(len(c.table)/2-1)
		index := min(int(position), len(c.table)/2-2)
		lo := float64(binary.BigEndian.Uint16(c.table[2*index:]))
		hi := float64(binary.BigEndian.Uint16(c.table[2*index+2:]))
		return (lo + (hi-lo)*(position-float64(index))) / 65535
	}
	g, a, b, slope, d, e, f := c.param[0], c.param[1], c.param[2], c.param[3], c.param[4], c.param[5], c.param[6]
	var value float64
	switch c.kind {
	case 0:
		value = math.Pow(x, g)
	case 1, 2:
		if x >= -b/a {
			value = math.Pow(max(0, a*x+b), g)
		}
		if c.kind == 2 {
			value += slope
		}
	case 3, 4:
		if x >= d {
			value = math.Pow(max(0, a*x+b), g)
			if c.kind == 4 {
				value += e
			}
		} else {
			value = slope * x
			if c.kind == 4 {
				value += f
			}
		}
	}
	return max(0, min(1, value))
}

// convert 将设备颜色转换为sRGB，灰度输出同样应用sRGB传递函数
// 入参: values 归一化设备值
// 返回: [3]float64 sRGB颜色
func (t *iccTransform) convert(values [3]float64) [3]float64 {
	for i := range t.count {
		if len(t.input[i]) == 0 {
			values[i] = t.curves[i].value(values[i])
		} else {
			position := max(0, min(1, values[i])) * float64(len(t.input[i])-1)
			index := min(int(position), len(t.input[i])-2)
			values[i] = t.input[i][index] + (t.input[i][index+1]-t.input[i][index])*(position-float64(index))
		}
	}
	if t.count == 1 {
		value := srgbTransfer(values[0])
		return [3]float64{value, value, value}
	}
	var output [3]float64
	for row := range output {
		for col := range values {
			output[row] += t.matrix[row][col] * values[col]
		}
		output[row] = srgbTransfer(output[row])
	}
	return output
}

// prepare 为低精度样本缓存曲线值，内存不足时仍逐值计算
// 入参: ctx 上下文, info 图像信息, channels 颜色通道, budget 可用字节数
// 返回: error 错误信息
func (t *iccTransform) prepare(ctx context.Context, info Info, channels [3]int, budget uint64) error {
	for i := range t.count {
		if err := ctx.Err(); err != nil {
			return err
		}
		precision := channelSpec(info, channels[i]).Precision
		if precision > 8 || len(t.input[i]) != 0 {
			continue
		}
		count := 1 << precision
		memory := uint64(count) * 8
		if memory > budget {
			continue
		}
		budget -= memory
		t.input[i] = make([]float64, count)
		for j := range t.input[i] {
			t.input[i][j] = t.curves[i].value(float64(j) / float64(count-1))
		}
	}
	return ctx.Err()
}

// srgbTransfer 将线性强度转换为sRGB编码值，限制在零到一之间
// 入参: value 线性强度
// 返回: float64 编码值
func srgbTransfer(value float64) float64 {
	value = max(0, min(1, value))
	if value <= 0.0031308 {
		return 12.92 * value
	}
	return 1.055*math.Pow(value, 1/2.4) - 0.055
}

// makeICCDisplayMatrix 通过Bradford色适应计算D50白点下XYZ到线性sRGB的转换矩阵
// 返回: [3][3]float64 XYZ到线性sRGB的转换矩阵
func makeICCDisplayMatrix() [3][3]float64 {
	primaries := [3][3]float64{{0.64 / 0.33, 0.30 / 0.60, 0.15 / 0.06}, {1, 1, 1}, {0.03 / 0.33, 0.10 / 0.60, 0.79 / 0.06}}
	d65 := [3]float64{0.3127 / 0.3290, 1, (1 - 0.3127 - 0.3290) / 0.3290}
	d50 := [3]float64{0.9642, 1, 0.8249}
	inverse := invertICCMatrix(primaries)
	for col := range 3 {
		var scale float64
		for k := range 3 {
			scale += inverse[col][k] * d65[k]
		}
		for row := range 3 {
			primaries[row][col] *= scale
		}
	}
	bradford := [3][3]float64{{0.8951, 0.2664, -0.1614}, {-0.7502, 1.7135, 0.0367}, {0.0389, -0.0685, 1.0296}}
	inverse = invertICCMatrix(bradford)
	for row := range 3 {
		var source, target float64
		for k := range 3 {
			source += bradford[row][k] * d50[k]
			target += bradford[row][k] * d65[k]
		}
		for k := range 3 {
			bradford[row][k] *= target / source
		}
	}
	return multiplyICCMatrix(invertICCMatrix(primaries), multiplyICCMatrix(inverse, bradford))
}

// multiplyICCMatrix 计算三阶矩阵乘积
// 入参: a 左矩阵, b 右矩阵
// 返回: [3][3]float64 乘积
func multiplyICCMatrix(a, b [3][3]float64) [3][3]float64 {
	var result [3][3]float64
	for row := range 3 {
		for col := range 3 {
			for k := range 3 {
				result[row][col] += a[row][k] * b[k][col]
			}
		}
	}
	return result
}

// invertICCMatrix 求可逆三阶矩阵的逆矩阵
// 入参: m 可逆矩阵
// 返回: [3][3]float64 逆矩阵
func invertICCMatrix(m [3][3]float64) [3][3]float64 {
	var result [3][3]float64
	for row := range 3 {
		for col := range 3 {
			result[col][row] = m[(row+1)%3][(col+1)%3]*m[(row+2)%3][(col+2)%3] - m[(row+1)%3][(col+2)%3]*m[(row+2)%3][(col+1)%3]
		}
	}
	determinant := m[0][0]*result[0][0] + m[0][1]*result[1][0] + m[0][2]*result[2][0]
	for row := range 3 {
		for col := range 3 {
			result[row][col] /= determinant
		}
	}
	return result
}
