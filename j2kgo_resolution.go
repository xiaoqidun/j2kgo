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
	"math"
)

// Resolution 表示JP2参考网格的物理分辨率，单位为每米网格点数，零值表示未指定
type Resolution struct {
	X float64
	Y float64
}

// validateResolution 检查水平和垂直分辨率，两者须同时为零或均为有限正数
// 入参: resolution 分辨率
// 返回: error 错误信息
func validateResolution(resolution Resolution) error {
	if resolution == (Resolution{}) {
		return nil
	}
	if !(resolution.X > 0) || !(resolution.Y > 0) || math.IsInf(resolution.X, 0) || math.IsInf(resolution.Y, 0) {
		return FormatError("invalid grid resolution")
	}
	return nil
}

// reduceResolution 按缩减级数调整物理分辨率，不修改原始信息
// 入参: info 图像信息, reduce 分辨率缩减级数
// 返回: Info 缩减后的图像信息
func reduceResolution(info Info, reduce int) Info {
	info.CaptureResolution.X = math.Ldexp(info.CaptureResolution.X, -reduce)
	info.CaptureResolution.Y = math.Ldexp(info.CaptureResolution.Y, -reduce)
	info.DisplayResolution.X = math.Ldexp(info.DisplayResolution.X, -reduce)
	info.DisplayResolution.Y = math.Ldexp(info.DisplayResolution.Y, -reduce)
	return info
}

// resolutionBox 读取并校验采集分辨率和显示分辨率
// 入参: header JP2头部信息, end 分辨率框末尾的字节偏移
// 返回: error 错误信息
func (h *headerReader) resolutionBox(header *jp2Header, end uint64) error {
	if header.hasResolution {
		return FormatError("duplicate resolution box")
	}
	header.hasResolution = true
	for h.read < end {
		kind, childEnd, err := h.box(end)
		if err != nil {
			return err
		}
		if kind != "resc" && kind != "resd" {
			if err := h.skip(childEnd - h.read); err != nil {
				return err
			}
			continue
		}
		target := &header.capture
		if kind == "resd" {
			target = &header.display
		}
		if *target != (Resolution{}) || childEnd-h.read != 10 {
			return FormatError("resolution field length or duplicate")
		}
		var data [10]byte
		if err := h.full(data[:]); err != nil {
			return err
		}
		vn, vd := binary.BigEndian.Uint16(data[:2]), binary.BigEndian.Uint16(data[2:4])
		hn, hd := binary.BigEndian.Uint16(data[4:6]), binary.BigEndian.Uint16(data[6:8])
		if vn == 0 || vd == 0 || hn == 0 || hd == 0 {
			return FormatError("zero resolution numerator or denominator")
		}
		target.X = float64(hn) / float64(hd) * math.Pow10(int(int8(data[9])))
		target.Y = float64(vn) / float64(vd) * math.Pow10(int(int8(data[8])))
	}
	if header.capture == (Resolution{}) && header.display == (Resolution{}) {
		return FormatError("missing capture or display resolution")
	}
	return nil
}

// appendResolutionBox 写入已指定的物理分辨率
// 入参: target 容器头缓冲, info 图像信息
// 返回: []byte 容器头缓冲, error 错误信息
func appendResolutionBox(target []byte, info Info) ([]byte, error) {
	var fields [36]byte
	data := fields[:0]
	for i, resolution := range []Resolution{info.CaptureResolution, info.DisplayResolution} {
		if resolution == (Resolution{}) {
			continue
		}
		hn, hd, he, err := encodeResolution(resolution.X)
		if err != nil {
			return nil, err
		}
		vn, vd, ve, err := encodeResolution(resolution.Y)
		if err != nil {
			return nil, err
		}
		var value [10]byte
		binary.BigEndian.PutUint16(value[:2], vn)
		binary.BigEndian.PutUint16(value[2:4], vd)
		binary.BigEndian.PutUint16(value[4:6], hn)
		binary.BigEndian.PutUint16(value[6:8], hd)
		value[8], value[9] = byte(ve), byte(he)
		kind := "resc"
		if i == 1 {
			kind = "resd"
		}
		data = appendBox(data, kind, value[:])
	}
	if len(data) > 0 {
		target = appendBox(target, "res ", data)
	}
	return target, nil
}

// encodeResolution 将分辨率编码为16位分子、分母及有符号十进制指数
// 入参: value 每米参考网格点数
// 返回: uint16 分子, uint16 分母, int8 指数, error 错误信息
func encodeResolution(value float64) (uint16, uint16, int8, error) {
	if !(value >= math.Pow10(-128)/65535) || value > 65535*math.Pow10(127) {
		return 0, 0, 0, FormatError("grid resolution exceeds JP2 range")
	}
	var numerator, denominator uint16
	var exponent int8
	best := math.Inf(1)
	for e := -128; e <= 127; e++ {
		scale := math.Pow10(e)
		ratio := value / scale
		if ratio < 1.0/65535 || ratio > 65535 {
			continue
		}
		n, d := resolutionFraction(ratio)
		delta := math.Abs(float64(n)/float64(d)*scale - value)
		if delta < best {
			numerator, denominator, exponent, best = n, d, int8(e), delta
			if delta == 0 {
				break
			}
		}
	}
	if denominator == 0 {
		return 0, 0, 0, FormatError("unrepresentable grid resolution")
	}
	return numerator, denominator, exponent, nil
}

// resolutionFraction 在分子和分母的取值范围内，用连分数逼近给定正数
// 入参: value 目标比值
// 返回: uint16 分子, uint16 分母
func resolutionFraction(value float64) (uint16, uint16) {
	p0, q0, p1, q1 := uint64(0), uint64(1), uint64(1), uint64(0)
	x := value
	for {
		limit := uint64(65535)
		if p1 != 0 {
			limit = min(limit, (65535-p0)/p1)
		}
		if q1 != 0 {
			limit = min(limit, (65535-q0)/q1)
		}
		a := math.Floor(x)
		if a > float64(limit) {
			p, q := p0+limit*p1, q0+limit*q1
			if q != 0 && p != 0 && (q1 == 0 || p1 == 0 || math.Abs(float64(p)/float64(q)-value) < math.Abs(float64(p1)/float64(q1)-value)) {
				return uint16(p), uint16(q)
			}
			return uint16(p1), uint16(q1)
		}
		p0, q0, p1, q1 = p1, q1, p0+uint64(a)*p1, q0+uint64(a)*q1
		if x == a {
			return uint16(p1), uint16(q1)
		}
		x = 1 / (x - a)
	}
}
