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

import "math"

// ratePoint 保存累计编码遍数、码字长度及系数重建误差
type ratePoint struct {
	passes     int
	rate       int
	distortion float64
}

// fillMQPassRates 根据编码段各遍累计移位数直接计算截断码长，无需二次MQ解码
// 入参: points 当前段各遍截断点切片, shifts 各遍结束时的MQ累计移位数, data 编码段字节数据, byteOffset 前序编码段累计字节数
func fillMQPassRates(points []ratePoint, shifts []int, data []byte, byteOffset int) {
	if len(points) == 0 {
		return
	}
	if len(data) == 0 {
		for k := range points {
			points[k].rate = byteOffset
		}
		return
	}
	pos, ct, consumed := 0, 0, 0
	stepByteIn := func() {
		current, next := byte(255), byte(255)
		if pos < len(data) {
			current = data[pos]
		}
		if pos+1 < len(data) {
			next = data[pos+1]
		}
		if current == 255 {
			if next > 0x8f {
				ct = 8
				return
			}
			pos++
			ct = 7
			return
		}
		pos++
		ct = 8
	}
	stepByteIn()
	ct -= 7
	last := len(points) - 1
	for k := range points {
		var length int
		if k == last {
			length = len(data)
		} else {
			target := shifts[k]
			for consumed < target {
				if pos >= len(data) {
					consumed = target
					break
				}
				if ct == 0 {
					stepByteIn()
				}
				step := min(target-consumed, ct)
				consumed += step
				ct -= step
			}
			length = min(len(data), pos+1)
		}
		if length > 0 && data[length-1] == 255 {
			length--
		}
		points[k].rate = byteOffset + length
	}
}

// fillRawPassRates 根据旁路编码段各遍写入位置填充截断码长
// 入参: points 当前段各遍截断点切片, positions 各遍结束时的字节位置, data 编码段字节数据, byteOffset 前序编码段累计字节数
func fillRawPassRates(points []ratePoint, positions []int, data []byte, byteOffset int) {
	last := len(points) - 1
	for k := range points {
		length := min(len(data), positions[k])
		if k == last {
			length = len(data)
		}
		if length > 0 && data[length-1] == 255 {
			length--
		}
		points[k].rate = byteOffset + length
	}
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
