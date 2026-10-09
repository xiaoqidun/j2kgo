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

// ratePoint 保存累计编码遍数、码字长度及系数重建误差
type ratePoint struct {
	passes     int
	rate       int
	distortion float64
}

// analyzeCodeBlockROI 计算各编码遍的累计码长，并对ROI重建误差加权
// 入参: ctx 上下文, encoded 完整码块, values 原始系数, mask ROI掩码, shift ROI移位量, width 宽度, height 高度, orientation 子带方向, style 编码方式, limits 可用预算
// 返回: []ratePoint 逐遍截断信息，首项表示不编码, error 错误信息
func analyzeCodeBlockROI(ctx context.Context, encoded encodedBlock, values []int64, mask []bool, shift uint8, width, height int, orientation bandOrientation, style CodeBlockStyle, limits Limits) ([]ratePoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if encoded.bitPlanes < 0 || encoded.bitPlanes > min(292, 62+int(shift)) {
		return nil, UnsupportedError("code-block coefficient precision")
	}
	limits = limits.normalized()
	count := max(0, 3*encoded.bitPlanes-2)
	metadata := uint64(count+1)*24 + 512
	if _, err := checkTotal("rate analysis memory", metadata, uint64(len(values))*11, limits.MaxMemoryBytes); err != nil {
		return nil, err
	}
	b, err := newCodeBlock(width, height, orientation, style, limits)
	if err != nil {
		return nil, err
	}
	if len(values) != len(b.magnitude) || (mask != nil && len(mask) != len(values)) {
		return nil, FormatError("rate analysis coefficient count")
	}
	b.roi = shift
	points := make([]ratePoint, count+1)
	for i, value := range values {
		if value <= -(1<<62) || value >= 1<<62 {
			return nil, UnsupportedError("rate analysis coefficient precision")
		}
		distortion := float64(value) * float64(value)
		if mask != nil && mask[i] {
			distortion = math.Ldexp(distortion, 2*int(shift))
		}
		points[0].distortion += distortion
	}
	segmentIndex, byteOffset, passOffset := 0, 0, 0
	err = b.decode(ctx, encoded, func(pass, index int, coder *blockCoder) error {
		for segmentIndex < index {
			segment := encoded.segments[segmentIndex]
			byteOffset += len(segment.data)
			passOffset += segment.passes
			segmentIndex++
		}
		segment := encoded.segments[index]
		length := min(len(segment.data), coder.decoder.position+1)
		if coder.raw {
			length = min(len(segment.data), coder.reader.position)
		}
		if pass == passOffset+segment.passes {
			length = len(segment.data)
		}
		if length > 0 && segment.data[length-1] == 255 {
			length--
		}
		point := ratePoint{passes: pass, rate: byteOffset + length}
		for i, value := range values {
			delta := float64(value - b.coefficient(i, true))
			distortion := delta * delta
			if mask != nil && mask[i] {
				distortion = math.Ldexp(distortion, 2*int(shift))
			}
			point.distortion += distortion
		}
		points[pass] = point
		return nil
	})
	if err != nil {
		return nil, err
	}
	if points[count].passes != count {
		return nil, FormatError("rate analysis requires a complete code-block")
	}
	return points, nil
}

// rateHull 原地筛选有效截断点，构造码长递增、失真递减的下凸包
// 入参: points 按编码遍排列的截断点
// 返回: []ratePoint 有效截断点
func rateHull(points []ratePoint) []ratePoint {
	hull := points[:0]
	for _, point := range points {
		if len(hull) > 0 && point.distortion >= hull[len(hull)-1].distortion {
			continue
		}
		for len(hull) > 0 && point.rate == hull[len(hull)-1].rate {
			hull = hull[:len(hull)-1]
		}
		for len(hull) > 1 {
			a, b := hull[len(hull)-2], hull[len(hull)-1]
			if (a.distortion-b.distortion)*float64(point.rate-b.rate) > (b.distortion-point.distortion)*float64(b.rate-a.rate) {
				break
			}
			hull = hull[:len(hull)-1]
		}
		hull = append(hull, point)
	}
	return hull
}

// selectRatePoint 按率失真斜率选择截断点，零阈值保留凸包最后一项
// 入参: points 凸包截断点, threshold 单位码长的最小失真下降量
// 返回: ratePoint 选择的截断点
func selectRatePoint(points []ratePoint, threshold float64) ratePoint {
	if len(points) == 0 {
		return ratePoint{}
	}
	selected := points[0]
	for _, point := range points[1:] {
		slope := math.Inf(1)
		if point.rate > selected.rate {
			slope = (selected.distortion - point.distortion) / float64(point.rate-selected.rate)
		}
		if slope < threshold {
			break
		}
		selected = point
	}
	return selected
}
