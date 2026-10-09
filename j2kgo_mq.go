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

// mqProbability T.800表C.2的概率估计状态
type mqProbability struct {
	qe        uint32
	mps       uint8
	lps       uint8
	switchMPS bool
}

// mqState 算术编码上下文状态
type mqState struct {
	index uint8
	mps   uint8
}

// mqEncoder MQ算术编码寄存器及上下文
type mqEncoder struct {
	a, c   uint32
	ct     uint8
	states [19]mqState
	data   []byte
}

// mqDecoder MQ算术解码寄存器及上下文
type mqDecoder struct {
	a, c     uint32
	ct       uint8
	states   [19]mqState
	data     []byte
	position int
}

// mqProbabilities T.800表C.2规定的47组概率与状态转移
var mqProbabilities = [...]mqProbability{
	{0x5601, 1, 1, true}, {0x3401, 2, 6, false}, {0x1801, 3, 9, false}, {0x0ac1, 4, 12, false},
	{0x0521, 5, 29, false}, {0x0221, 38, 33, false}, {0x5601, 7, 6, true}, {0x5401, 8, 14, false},
	{0x4801, 9, 14, false}, {0x3801, 10, 14, false}, {0x3001, 11, 17, false}, {0x2401, 12, 18, false},
	{0x1c01, 13, 20, false}, {0x1601, 29, 21, false}, {0x5601, 15, 14, true}, {0x5401, 16, 14, false},
	{0x5101, 17, 15, false}, {0x4801, 18, 16, false}, {0x3801, 19, 17, false}, {0x3401, 20, 18, false},
	{0x3001, 21, 19, false}, {0x2801, 22, 19, false}, {0x2401, 23, 20, false}, {0x2201, 24, 21, false},
	{0x1c01, 25, 22, false}, {0x1801, 26, 23, false}, {0x1601, 27, 24, false}, {0x1401, 28, 25, false},
	{0x1201, 29, 26, false}, {0x1101, 30, 27, false}, {0x0ac1, 31, 28, false}, {0x09c1, 32, 29, false},
	{0x08a1, 33, 30, false}, {0x0521, 34, 31, false}, {0x0441, 35, 32, false}, {0x02a1, 36, 33, false},
	{0x0221, 37, 34, false}, {0x0141, 38, 35, false}, {0x0111, 39, 36, false}, {0x0085, 40, 37, false},
	{0x0049, 41, 38, false}, {0x0025, 42, 39, false}, {0x0015, 43, 40, false}, {0x0009, 44, 41, false},
	{0x0005, 45, 42, false}, {0x0001, 45, 43, false}, {0x5601, 46, 46, false},
}

// initialMQStates 初始化码块的19个上下文
// 返回: [19]mqState 上下文状态
func initialMQStates() [19]mqState {
	var states [19]mqState
	states[0].index = 4
	states[17].index = 3
	states[18].index = 46
	return states
}

// newMQEncoder 初始化MQ编码器
// 返回: mqEncoder 算术编码器
func newMQEncoder() mqEncoder {
	return mqEncoder{a: 0x8000, ct: 12, states: initialMQStates(), data: []byte{0}}
}

// encode 在指定上下文中编码一个二值符号
// 入参: context 上下文索引, value 二值符号
func (e *mqEncoder) encode(context int, value uint8) {
	state := &e.states[context]
	p := mqProbabilities[state.index]
	e.a -= p.qe
	if value == state.mps {
		if e.a >= 0x8000 {
			e.c += p.qe
			return
		}
		if e.a < p.qe {
			e.a = p.qe
		} else {
			e.c += p.qe
		}
		state.index = p.mps
	} else {
		if e.a < p.qe {
			e.c += p.qe
		} else {
			e.a = p.qe
		}
		if p.switchMPS {
			state.mps ^= 1
		}
		state.index = p.lps
	}
	e.renormalize()
}

// renormalize 归一化编码区间并输出完整字节
func (e *mqEncoder) renormalize() {
	for e.a < 0x8000 {
		e.a <<= 1
		e.c <<= 1
		e.ct--
		if e.ct == 0 {
			e.byteOut()
		}
	}
}

// byteOut 处理进位及0xFF后的位填充
func (e *mqEncoder) byteOut() {
	last := len(e.data) - 1
	if e.data[last] != 255 && e.c&0x08000000 != 0 {
		e.data[last]++
		e.c &= 0x07ffffff
	}
	if e.data[last] == 255 {
		e.data = append(e.data, byte(e.c>>20))
		e.c &= 0x000fffff
		e.ct = 7
	} else {
		e.data = append(e.data, byte(e.c>>19))
		e.c &= 0x0007ffff
		e.ct = 8
	}
}

// finish 终止码段并返回编码数据，不包含前导进位缓冲
// 返回: []byte 编码数据
func (e *mqEncoder) finish() []byte {
	end := e.c + e.a
	e.c |= 0xffff
	if e.c >= end {
		e.c -= 0x8000
	}
	e.c <<= e.ct
	e.byteOut()
	e.c <<= e.ct
	e.byteOut()
	length := len(e.data)
	if e.data[length-1] == 255 {
		length--
	}
	return e.data[1:length]
}

// finishPredictable 按T.800规定输出可预测终止码段
// 返回: []byte 编码数据
func (e *mqEncoder) finishPredictable() []byte {
	remaining := 12 - int(e.ct)
	for remaining > 0 {
		e.c <<= e.ct
		e.byteOut()
		remaining -= int(e.ct)
	}
	if e.data[len(e.data)-1] != 255 {
		e.byteOut()
	}
	return e.data[1 : len(e.data)-1]
}

// newMQDecoder 初始化只读MQ解码器，不覆盖输入末尾字节
// 入参: data 编码数据
// 返回: mqDecoder 算术解码器, error 错误信息
func newMQDecoder(data []byte) (mqDecoder, error) {
	if len(data) == 0 {
		return mqDecoder{}, io.ErrUnexpectedEOF
	}
	d := mqDecoder{a: 0x8000, c: uint32(data[0]) << 16, states: initialMQStates(), data: data}
	d.byteIn()
	d.c <<= 7
	d.ct -= 7
	return d, nil
}

// decode 在指定上下文中解码一个二值符号
// 入参: context 上下文索引
// 返回: uint8 二值符号
func (d *mqDecoder) decode(context int) uint8 {
	state := &d.states[context]
	p := mqProbabilities[state.index]
	d.a -= p.qe
	value := state.mps
	if d.c>>16 < p.qe {
		if d.a < p.qe {
			state.index = p.mps
		} else {
			value ^= 1
			if p.switchMPS {
				state.mps ^= 1
			}
			state.index = p.lps
		}
		d.a = p.qe
	} else {
		d.c -= p.qe << 16
		if d.a >= 0x8000 {
			return value
		}
		if d.a < p.qe {
			value ^= 1
			if p.switchMPS {
				state.mps ^= 1
			}
			state.index = p.lps
		} else {
			state.index = p.mps
		}
	}
	d.renormalize()
	return value
}

// renormalize 归一化解码区间并补充输入字节
func (d *mqDecoder) renormalize() {
	for d.a < 0x8000 {
		if d.ct == 0 {
			d.byteIn()
		}
		d.a <<= 1
		d.c <<= 1
		d.ct--
	}
}

// byteIn 按标记边界规则读取字节，码段之外使用虚拟0xFF填充
func (d *mqDecoder) byteIn() {
	current, next := byte(255), byte(255)
	if d.position < len(d.data) {
		current = d.data[d.position]
	}
	if d.position+1 < len(d.data) {
		next = d.data[d.position+1]
	}
	if current == 255 {
		if next > 0x8f {
			d.c += 0xff00
			d.ct = 8
			return
		}
		d.position++
		d.c += uint32(next) << 9
		d.ct = 7
	} else {
		d.position++
		d.c += uint32(next) << 8
		d.ct = 8
	}
}
