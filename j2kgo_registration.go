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

// readRegistration 读取分量配准偏移并检查重复标记，不改变编码网格
// 入参: data 标记内容
// 返回: error 错误信息
func (s *streamIndex) readRegistration(data []byte) error {
	if s.registration || len(data) != 4*len(s.info.Components) {
		return FormatError("component registration length or duplicate")
	}
	s.registration = true
	for i := range s.info.Components {
		c := &s.info.Components[i]
		c.XOffset = float64(binary.BigEndian.Uint16(data[4*i:])) / 65536
		c.YOffset = float64(binary.BigEndian.Uint16(data[4*i+2:])) / 65536
	}
	return nil
}

// hasRegistration 检查是否需要写入分量配准标记
// 入参: info 图像信息
// 返回: bool 是否存在非零偏移
func hasRegistration(info Info) bool {
	for _, c := range info.Components {
		if c.XOffset != 0 || c.YOffset != 0 {
			return true
		}
	}
	return false
}

// registration 写入可选配准标记，偏移舍入至采样间隔的1/65536
// 入参: info 图像信息, limits 资源限制
// 返回: error 错误信息
func (w *codestreamWriter) registration(info Info, limits Limits) error {
	if !hasRegistration(info) {
		return nil
	}
	size := uint64(len(info.Components)) * 4
	if size > 65532 {
		return FormatError("component registration exceeds marker length")
	}
	if size > limits.MaxMemoryBytes {
		return &LimitError{Resource: "registration memory", Limit: limits.MaxMemoryBytes, Required: size}
	}
	data := make([]byte, int(size))
	for i, c := range info.Components {
		binary.BigEndian.PutUint16(data[4*i:], uint16(min(65535, math.Round(c.XOffset*65536))))
		binary.BigEndian.PutUint16(data[4*i+2:], uint16(min(65535, math.Round(c.YOffset*65536))))
	}
	return w.marker(markerCRG, data)
}

// reduceRegistration 调整降低分辨率后的分量配准偏移
// 入参: reduce 分辨率缩减级数
func (r *Raster) reduceRegistration(reduce int) {
	if reduce == 0 {
		return
	}
	for i := range r.info.Components {
		c := &r.info.Components[i]
		c.XOffset = math.Ldexp(c.XOffset, -reduce)
		c.YOffset = math.Ldexp(c.YOffset, -reduce)
		r.components[i].info = *c
	}
}

// registeredCoordinate 根据采样间隔和配准偏移计算分量坐标，采用零阶保持采样
// 入参: coordinate 参考网格坐标, step 采样间隔, offset 配准偏移，以采样间隔为单位
// 返回: int 分量坐标
func registeredCoordinate(coordinate int, step uint8, offset float64) int {
	if offset == 0 {
		return coordinate / int(step)
	}
	return int(math.Floor(float64(coordinate)/float64(step) - offset))
}
