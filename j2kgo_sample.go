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
	"fmt"
)

// ReadSamples 将同一行的连续样本复制到目标切片，校验失败时不修改目标
// 入参: dst 目标切片, x 分量起始横坐标, y 分量行坐标
// 返回: error 错误信息
func (c *Component) ReadSamples(dst []int64, x, y int) error {
	data, err := c.sampleSpan(x, y, len(dst))
	if err != nil {
		return err
	}
	switch c.width {
	case 1:
		for i := range dst {
			dst[i] = int64(data[i])
		}
	case 2:
		for i := range dst {
			dst[i] = int64(binary.LittleEndian.Uint16(data[2*i:]))
		}
	case 4:
		for i := range dst {
			dst[i] = int64(binary.LittleEndian.Uint32(data[4*i:]))
		}
	case 8:
		for i := range dst {
			dst[i] = int64(binary.LittleEndian.Uint64(data[8*i:]))
		}
	}
	if c.info.Signed {
		shift := 64 - c.info.Precision
		for i, value := range dst {
			dst[i] = value << shift >> shift
		}
	}
	return nil
}

// WriteSamples 写入同一行的连续样本，校验失败时不修改分量
// 入参: src 样本切片, x 分量起始横坐标, y 分量行坐标
// 返回: error 错误信息
func (c *Component) WriteSamples(src []int64, x, y int) error {
	data, err := c.sampleSpan(x, y, len(src))
	if err != nil {
		return err
	}
	lo, hi := sampleRange(c.info)
	for _, value := range src {
		if value < lo || value > hi {
			return fmt.Errorf("j2kgo: sample %d outside [%d, %d]", value, lo, hi)
		}
	}
	switch c.width {
	case 1:
		for i, value := range src {
			data[i] = byte(value)
		}
	case 2:
		for i, value := range src {
			binary.LittleEndian.PutUint16(data[2*i:], uint16(value))
		}
	case 4:
		for i, value := range src {
			binary.LittleEndian.PutUint32(data[4*i:], uint32(value))
		}
	case 8:
		for i, value := range src {
			binary.LittleEndian.PutUint64(data[8*i:], uint64(value))
		}
	}
	return nil
}

// sampleSpan 校验行内范围并定位连续样本存储
// 入参: x 起始横坐标, y 行坐标, count 样本数
// 返回: []byte 存储范围, error 错误信息
func (c *Component) sampleSpan(x, y, count int) ([]byte, error) {
	b := c.bounds
	if y < b.Min.Y || y >= b.Max.Y || x < b.Min.X || x > b.Max.X || count > b.Max.X-x {
		return nil, fmt.Errorf("j2kgo: sample span out of bounds")
	}
	s := c.storage
	offset := ((y-s.Min.Y)*s.Dx() + x - s.Min.X) * c.width
	return c.data[offset : offset+count*c.width], nil
}
