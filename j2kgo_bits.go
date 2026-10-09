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

import "io"

// packetBits 读取数据包头，处理0xFF后的位填充
type packetBits struct {
	data      []byte
	source    io.ByteReader
	position  int
	current   byte
	remaining uint8
}

// packetWriter 按高位优先顺序写入数据包头
type packetWriter struct {
	data      []byte
	current   byte
	used      uint8
	last      byte
	length    int
	countOnly bool
}

// bit 读取一个有效数据位
// 返回: uint8 数据位, error 错误信息
func (r *packetBits) bit() (uint8, error) {
	if r.remaining == 0 {
		stuffed := r.position > 0 && r.current == 255
		var err error
		r.current, err = r.readByte()
		if err != nil {
			return 0, err
		}
		r.remaining = 8
		if stuffed {
			if r.current&128 != 0 {
				return 0, FormatError("packet header stuffed bit")
			}
			r.remaining = 7
		}
	}
	r.remaining--
	return (r.current >> r.remaining) & 1, nil
}

// readByte 读取下一个包头字节并更新已读长度
// 返回: byte 数据字节, error 错误信息
func (r *packetBits) readByte() (byte, error) {
	if r.source != nil {
		value, err := r.source.ReadByte()
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		if err == nil {
			r.position++
		}
		return value, err
	}
	if r.position == len(r.data) {
		return 0, io.ErrUnexpectedEOF
	}
	value := r.data[r.position]
	r.position++
	return value, nil
}

// bits 按指定位数读取无符号整数
// 入参: count 位数
// 返回: uint64 数值, error 错误信息
func (r *packetBits) bits(count int) (uint64, error) {
	if count < 0 || count > 64 {
		return 0, FormatError("packet field width")
	}
	var value uint64
	for range count {
		bit, err := r.bit()
		if err != nil {
			return 0, err
		}
		value = value<<1 | uint64(bit)
	}
	return value, nil
}

// align 跳过数据包头的剩余填充位，并读取末尾0xFF后的填充字节
// 返回: error 错误信息
func (r *packetBits) align() error {
	if r.position > 0 && r.current == 255 {
		value, err := r.readByte()
		if err != nil {
			return err
		}
		if value&128 != 0 {
			return FormatError("packet header terminal stuffed bit")
		}
		r.current = value
	}
	r.remaining = 0
	return nil
}

// bit 写入一个数据位
// 入参: value 数据位
func (w *packetWriter) bit(value uint8) {
	capacity := uint8(8)
	if w.length > 0 && w.last == 255 {
		capacity = 7
	}
	w.current |= (value & 1) << (capacity - w.used - 1)
	w.used++
	if w.used == capacity {
		w.appendByte(w.current)
		w.current, w.used = 0, 0
	}
}

// appendByte 追加包头字节并更新位填充状态，长度统计模式下不保存数据
// 入参: value 字节
func (w *packetWriter) appendByte(value byte) {
	if !w.countOnly {
		w.data = append(w.data, value)
	}
	w.last = value
	w.length++
}

// bits 从高位到低位写入数值的指定低位
// 入参: value 数值, count 位数
// 返回: error 错误信息
func (w *packetWriter) bits(value uint64, count int) error {
	if count < 0 || count > 64 {
		return FormatError("packet field width")
	}
	for i := count - 1; i >= 0; i-- {
		w.bit(uint8(value >> i))
	}
	return nil
}

// align 补齐数据包头的填充位，避免末字节为0xFF
func (w *packetWriter) align() {
	if w.used > 0 {
		w.appendByte(w.current)
		w.current, w.used = 0, 0
	}
	if w.length > 0 && w.last == 255 {
		w.appendByte(0)
	}
}

// readPassCount 读取当前数据包中码块新增的编码遍数
// 返回: int 编码遍数, error 错误信息
func (r *packetBits) readPassCount() (int, error) {
	for i := 0; i < 2; i++ {
		value, err := r.bit()
		if err != nil {
			return 0, err
		}
		if value == 0 {
			return i + 1, nil
		}
	}
	value, err := r.bits(2)
	if err != nil {
		return 0, err
	}
	if value < 3 {
		return 3 + int(value), nil
	}
	value, err = r.bits(5)
	if err != nil {
		return 0, err
	}
	if value < 31 {
		return 6 + int(value), nil
	}
	value, err = r.bits(7)
	return 37 + int(value), err
}

// writePassCount 写入当前数据包中码块新增的编码遍数
// 入参: count 编码遍数
// 返回: error 错误信息
func (w *packetWriter) writePassCount(count int) error {
	switch {
	case count == 1:
		return w.bits(0, 1)
	case count == 2:
		return w.bits(2, 2)
	case count >= 3 && count <= 5:
		return w.bits(uint64(12+count-3), 4)
	case count >= 6 && count <= 36:
		return w.bits(uint64(480+count-6), 9)
	case count >= 37 && count <= 164:
		return w.bits(uint64(65408+count-37), 16)
	default:
		return FormatError("packet coding pass count")
	}
}
