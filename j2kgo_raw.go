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

// rawBits 读取算术编码旁路位流，允许码段末尾虚拟0xFF填充
type rawBits struct {
	data      []byte
	position  int
	current   byte
	remaining uint8
}

// bit 读取一个旁路数据位
// 返回: uint8 数据位, error 错误信息
func (r *rawBits) bit() (uint8, error) {
	if r.remaining == 0 {
		stuffed := r.position > 0 && r.current == 255
		if r.position < len(r.data) {
			r.current = r.data[r.position]
			if stuffed && r.current&128 != 0 {
				return 0, FormatError("raw code-block stuffed bit")
			}
		} else {
			r.current = 255
		}
		r.position++
		r.remaining = 8
		if stuffed {
			r.remaining = 7
		}
	}
	r.remaining--
	return (r.current >> r.remaining) & 1, nil
}

// finishRaw 按码块终止方式补齐旁路位流
// 入参: predictable 是否使用可预测终止
func (w *packetWriter) finishRaw(predictable bool) {
	if predictable && (w.used > 0 || (len(w.data) > 0 && w.data[len(w.data)-1] == 255)) {
		capacity := uint8(8)
		if len(w.data) > 0 && w.data[len(w.data)-1] == 255 {
			capacity = 7
		}
		count := capacity - w.used
		for i := uint8(0); i < count; i++ {
			w.bit(i & 1)
		}
	}
	w.align()
}
