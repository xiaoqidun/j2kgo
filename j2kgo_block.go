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
	"math/bits"
)

// 子带方向，低频在前，其余按水平高频、垂直高频和双向高频排列
const (
	bandLL bandOrientation = iota
	bandHL
	bandLH
	bandHH
)

// 码块系数的显著性、符号、遍历、细化和ROI状态
const (
	blockSignificant uint8 = 1 << iota
	blockNegative
	blockVisited
	blockRefined
	blockROI
)

// blockContexts 保存各子带方向和邻接显著状态对应的上下文索引
var blockContexts = makeBlockContexts()

// bandOrientation 小波子带方向
type bandOrientation uint8

// blockSegment 保存一个编码段的编码遍数和数据
type blockSegment struct {
	data   []byte
	passes int
}

// encodedBlock 保存码块有效位平面及编码段
type encodedBlock struct {
	bitPlanes int
	segments  []blockSegment
}

// codeBlock 保存系数幅度、符号与逐遍解码状态
type codeBlock struct {
	width, height int
	orientation   bandOrientation
	style         CodeBlockStyle
	roi           uint8
	precision     uint8
	magnitude     []uint64
	flags         []uint8
	lowest        []uint8
	neighbors     []uint8
}

// blockCoder 管理当前编码段的MQ编码器或旁路位流
type blockCoder struct {
	encoding bool
	raw      bool
	err      error
	encoder  mqEncoder
	decoder  mqDecoder
	writer   packetWriter
	reader   rawBits
}

// newCodeBlock 校验码块尺寸并分配工作缓冲
// 入参: width 宽度, height 高度, orientation 子带方向, style 编码方式, limits 资源限制
// 返回: *codeBlock 码块, error 错误信息
func newCodeBlock(width, height int, orientation bandOrientation, style CodeBlockStyle, limits Limits) (*codeBlock, error) {
	return reuseCodeBlock(nil, width, height, orientation, style, limits)
}

// reuseCodeBlock 校验码块参数，清空并复用容量足够的工作缓冲
// 入参: buffer 已单独计入预算的复用缓冲，可为nil, width 宽度, height 高度, orientation 子带方向, style 编码方式, limits 当前码块资源限制
// 返回: *codeBlock 码块, error 错误信息
func reuseCodeBlock(buffer *codeBlock, width, height int, orientation bandOrientation, style CodeBlockStyle, limits Limits) (*codeBlock, error) {
	if width < 1 || height < 1 || width > 1024 || height > 1024 || width*height > 4096 || orientation > bandHH || style&^63 != 0 {
		return nil, FormatError("code-block dimensions or coding style")
	}
	limits = limits.normalized()
	n := width * height
	if _, err := checkedProduct("code-block memory", uint64(n), 11, limits.MaxMemoryBytes); err != nil {
		return nil, err
	}
	if buffer == nil || cap(buffer.magnitude) < n || cap(buffer.flags) < n || cap(buffer.lowest) < n || cap(buffer.neighbors) < n {
		buffer = newBlockBuffer(n)
	} else {
		clear(buffer.magnitude[:n])
		clear(buffer.flags[:n])
		clear(buffer.lowest[:n])
		clear(buffer.neighbors[:n])
	}
	*buffer = codeBlock{width: width, height: height, orientation: orientation, style: style, precision: 62,
		magnitude: buffer.magnitude[:n], flags: buffer.flags[:n], lowest: buffer.lowest[:n], neighbors: buffer.neighbors[:n]}
	return buffer, nil
}

// newBlockBuffer 分配已纳入重建预算的码块幅度和状态缓冲
// 入参: samples 最大码块样本数
// 返回: *codeBlock 空白工作缓冲
func newBlockBuffer(samples int) *codeBlock {
	state := make([]uint8, 3*samples)
	return &codeBlock{magnitude: make([]uint64, samples), flags: state[:samples:samples],
		lowest: state[samples : 2*samples : 2*samples], neighbors: state[2*samples:]}
}

// encodeCodeBlockROI 按掩码优先编码ROI系数，不扩展样本存储精度
// 入参: ctx 上下文, values 系数, mask ROI掩码，可为nil, shift ROI移位量, width 宽度, height 高度, orientation 子带方向, style 编码方式, limits 资源限制
// 返回: encodedBlock 已编码的码块, error 错误信息
func encodeCodeBlockROI(ctx context.Context, values []int64, mask []bool, shift uint8, width, height int, orientation bandOrientation, style CodeBlockStyle, limits Limits) (encodedBlock, error) {
	if err := ctx.Err(); err != nil {
		return encodedBlock{}, err
	}
	b, err := newCodeBlock(width, height, orientation, style, limits)
	if err != nil {
		return encodedBlock{}, err
	}
	if len(values) != len(b.magnitude) || (mask != nil && len(mask) != len(values)) {
		return encodedBlock{}, FormatError("code-block sample count")
	}
	b.roi = shift
	planes := 0
	for i, value := range values {
		if value == math.MinInt64 {
			return encodedBlock{}, UnsupportedError("code-block coefficient precision")
		}
		if value < 0 {
			b.flags[i] = blockNegative
			value = -value
		}
		b.magnitude[i] = uint64(value)
		depth := bits.Len64(uint64(value))
		if depth > 62 {
			return encodedBlock{}, UnsupportedError("code-block coefficient precision")
		}
		if mask != nil && mask[i] && shift != 0 {
			b.flags[i] |= blockROI
			if depth != 0 {
				depth += int(shift)
			}
		} else if shift != 0 && depth > int(shift) {
			return encodedBlock{}, FormatError("ROI shift below background precision")
		}
		planes = max(planes, depth)
	}
	if planes > 292 {
		return encodedBlock{}, FormatError("ROI code-block precision")
	}
	result := encodedBlock{bitPlanes: planes}
	if planes == 0 {
		return result, nil
	}
	total := 3*planes - 2
	states := initialMQStates()
	for first := 0; first < total; {
		count := blockSegmentPasses(first, total, style)
		coder := blockCoder{encoding: true, raw: blockRawPass(first, style), encoder: newMQEncoder()}
		coder.encoder.states = states
		for pass := first; pass < first+count; pass++ {
			if err := b.processPass(ctx, planes-1-(pass+2)/3, (pass+2)%3, &coder); err != nil {
				return encodedBlock{}, err
			}
			if style&CodeBlockReset != 0 {
				coder.encoder.states = initialMQStates()
			}
		}
		states = coder.encoder.states
		var data []byte
		if coder.raw {
			coder.writer.finishRaw(style&CodeBlockPredictable != 0)
			data = coder.writer.data
		} else if style&CodeBlockPredictable != 0 {
			data = coder.encoder.finishPredictable()
		} else {
			data = coder.encoder.finish()
		}
		if len(data) > 0 && data[len(data)-1] == 255 {
			data = data[:len(data)-1]
		}
		result.segments = append(result.segments, blockSegment{data: data, passes: count})
		first += count
	}
	return result, nil
}

// decodeCodeBlockBuffer 使用当前工作协程的缓冲解码码块，不保留上一码块状态
// 入参: ctx 上下文, encoded 码块数据, shift ROI移位量, precision 量化幅度位数, width 宽度, height 高度, orientation 子带方向, style 编码方式, limits 当前码块资源限制, buffer 已单独计入预算的复用缓冲，可为nil
// 返回: *codeBlock 系数与细化状态，下一次复用前有效, error 错误信息
func decodeCodeBlockBuffer(ctx context.Context, encoded encodedBlock, shift uint8, precision int, width, height int, orientation bandOrientation, style CodeBlockStyle, limits Limits, buffer *codeBlock) (*codeBlock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if precision < 0 || precision > 62 || encoded.bitPlanes < 0 || encoded.bitPlanes > min(292, precision+int(shift)) {
		return nil, UnsupportedError("code-block coefficient precision")
	}
	b, err := reuseCodeBlock(buffer, width, height, orientation, style, limits)
	if err != nil {
		return nil, err
	}
	b.roi = shift
	b.precision = uint8(precision)
	if err := b.decode(ctx, encoded, nil); err != nil {
		return nil, err
	}
	return b, nil
}

// decode 逐遍解码码块，可通过回调查看每遍结束后的状态
// 入参: ctx 上下文, encoded 码块数据, visit 只读状态回调，可为nil
// 返回: error 错误信息
func (b *codeBlock) decode(ctx context.Context, encoded encodedBlock, visit func(int, int, *blockCoder) error) error {
	total, first := max(0, 3*encoded.bitPlanes-2), 0
	states := initialMQStates()
	for i, segment := range encoded.segments {
		maximum := blockSegmentPasses(first, total, b.style)
		if segment.passes < 1 || segment.passes > maximum || (segment.passes < maximum && i+1 < len(encoded.segments)) {
			return FormatError("code-block segment pass count")
		}
		coder := blockCoder{raw: blockRawPass(first, b.style), reader: rawBits{data: segment.data}}
		data := segment.data
		if len(data) == 0 {
			data = []byte{255, 255}
		}
		var err error
		coder.decoder, err = newMQDecoder(data)
		if err != nil {
			return err
		}
		coder.decoder.states = states
		for pass := first; pass < first+segment.passes; pass++ {
			if err := b.processPass(ctx, encoded.bitPlanes-1-(pass+2)/3, (pass+2)%3, &coder); err != nil {
				return err
			}
			if visit != nil {
				if err := visit(pass+1, i, &coder); err != nil {
					return err
				}
			}
			if b.style&CodeBlockReset != 0 {
				coder.decoder.states = initialMQStates()
			}
		}
		states = coder.decoder.states
		first += segment.passes
	}
	return ctx.Err()
}

// blockRawPass 判断当前遍是否使用算术编码旁路
// 入参: pass 编码遍索引, style 编码方式
// 返回: bool 是否使用旁路位流
func blockRawPass(pass int, style CodeBlockStyle) bool {
	return style&CodeBlockBypass != 0 && pass >= 10 && (pass+2)%3 != 2
}

// blockSegmentPasses 计算当前编码段可容纳的编码遍数
// 入参: first 当前遍索引, total 总遍数, style 编码方式
// 返回: int 编码遍数
func blockSegmentPasses(first, total int, style CodeBlockStyle) int {
	if first >= total {
		return 0
	}
	if style&CodeBlockTerminate != 0 {
		return 1
	}
	if style&CodeBlockBypass == 0 {
		return total - first
	}
	if first < 10 {
		return min(10-first, total-first)
	}
	if (first+2)%3 == 0 {
		return min(2, total-first)
	}
	return 1
}

// bit 在指定上下文中编码或解码一个二值符号
// 入参: context 上下文索引, value 待编码值
// 返回: uint8 编解码结果
func (c *blockCoder) bit(context int, value uint8) uint8 {
	if c.err != nil {
		return 0
	}
	if c.encoding {
		if c.raw {
			c.writer.bit(value)
		} else {
			c.encoder.encode(context, value)
		}
		return value
	}
	if c.raw {
		var value uint8
		value, c.err = c.reader.bit()
		return value
	}
	return c.decoder.decode(context)
}

// processPass 执行显著性传播、幅度细化或清理遍
// 入参: ctx 上下文, plane 位平面, kind 编码遍类型, coder 编码段处理器
// 返回: error 错误信息
func (b *codeBlock) processPass(ctx context.Context, plane, kind int, coder *blockCoder) error {
	for stripe := 0; stripe < b.height; stripe += 4 {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(stripe+4, b.height)
		for x := 0; x < b.width; x++ {
			y0 := stripe
			if kind == 2 && end-stripe == 4 && b.canAggregate(x, stripe) {
				run := 0
				present := uint8(0)
				if coder.encoding {
					for run < 4 && b.magnitudeBit((stripe+run)*b.width+x, plane) == 0 {
						run++
					}
					if run < 4 {
						present = 1
					}
				}
				if coder.bit(17, present) == 0 {
					continue
				}
				hi := coder.bit(18, uint8(run>>1))
				lo := coder.bit(18, uint8(run&1))
				y0 = stripe + int(hi<<1|lo)
				b.codeSign(x, y0, plane, coder)
				y0++
			}
			for y := y0; y < end; y++ {
				i := y*b.width + x
				flag := b.flags[i]
				switch kind {
				case 0:
					if flag&blockSignificant != 0 {
						continue
					}
					cx := b.significanceContext(x, y)
					if cx == 0 {
						continue
					}
					b.flags[i] |= blockVisited
					value := uint8(0)
					if coder.encoding {
						value = b.magnitudeBit(i, plane)
					}
					if coder.bit(cx, value) != 0 {
						b.codeSign(x, y, plane, coder)
					}
				case 1:
					if flag&blockSignificant == 0 || flag&blockVisited != 0 {
						continue
					}
					cx := 14
					if flag&blockRefined != 0 {
						cx = 16
					} else if b.significanceContext(x, y) != 0 {
						cx = 15
					}
					value := uint8(0)
					if coder.encoding {
						value = b.magnitudeBit(i, plane)
					}
					value = coder.bit(cx, value)
					b.storeMagnitudeBit(i, plane, value, coder)
					b.flags[i] |= blockRefined
				case 2:
					if flag&(blockSignificant|blockVisited) != 0 {
						continue
					}
					value := uint8(0)
					if coder.encoding {
						value = b.magnitudeBit(i, plane)
					}
					if coder.bit(b.significanceContext(x, y), value) != 0 {
						b.codeSign(x, y, plane, coder)
					}
				}
			}
		}
		if coder.err != nil {
			return coder.err
		}
	}
	if kind == 2 {
		if b.style&CodeBlockSegmentation != 0 {
			var symbol uint8
			for _, value := range [4]uint8{1, 0, 1, 0} {
				symbol = symbol<<1 | coder.bit(18, value)
			}
			if symbol != 10 {
				return FormatError("code-block segmentation symbol")
			}
		}
		for i := range b.flags {
			b.flags[i] &^= blockVisited
		}
	}
	return coder.err
}

// codeSign 在系数首次显著时编解码其符号
// 入参: x 横坐标, y 纵坐标, plane 位平面, coder 编码段处理器
func (b *codeBlock) codeSign(x, y, plane int, coder *blockCoder) {
	i := y*b.width + x
	cx, xor := b.signContext(x, y)
	if coder.raw {
		xor = 0
	}
	negative := uint8(0)
	if b.flags[i]&blockNegative != 0 {
		negative = 1
	}
	negative = coder.bit(cx, negative^xor) ^ xor
	b.flags[i] &^= blockNegative
	if negative != 0 {
		b.flags[i] |= blockNegative
	}
	b.flags[i] |= blockSignificant
	if !coder.encoding && b.roi != 0 && plane >= int(b.roi) {
		b.flags[i] |= blockROI
	}
	b.storeMagnitudeBit(i, plane, 1, coder)
	b.markNeighbors(x, y)
}

// magnitudeBit 读取系数对应的码流位平面，ROI低位保持为零
// 入参: index 系数索引, plane 码流位平面
// 返回: uint8 位值
func (b *codeBlock) magnitudeBit(index, plane int) uint8 {
	if b.flags[index]&blockROI != 0 {
		plane -= int(b.roi)
	}
	if plane < 0 {
		return 0
	}
	return uint8(b.magnitude[index] >> plane & 1)
}

// storeMagnitudeBit 根据ROI移位量还原系数位，更新幅度和已解码精度
// 入参: index 系数索引, plane 码流位平面, value 位值, coder 编码段处理器
func (b *codeBlock) storeMagnitudeBit(index, plane int, value uint8, coder *blockCoder) {
	if b.flags[index]&blockROI != 0 {
		plane -= int(b.roi)
	}
	if !coder.encoding && plane >= int(b.precision) {
		b.lowest[index] = b.precision
		return
	}
	if plane >= 0 {
		b.magnitude[index] |= uint64(value) << plane
	}
	b.lowest[index] = uint8(max(0, plane))
}

// markNeighbors 更新新显著系数的邻接状态
// 入参: x 横坐标, y 纵坐标
func (b *codeBlock) markNeighbors(x, y int) {
	i := y*b.width + x
	if x > 0 {
		b.neighbors[i-1] |= 2
	}
	if x+1 < b.width {
		b.neighbors[i+1] |= 1
	}
	if y > 0 {
		up := i - b.width
		b.neighbors[up] |= 8
		if x > 0 {
			b.neighbors[up-1] |= 128
		}
		if x+1 < b.width {
			b.neighbors[up+1] |= 64
		}
	}
	if y+1 < b.height {
		down := i + b.width
		b.neighbors[down] |= 4
		if x > 0 {
			b.neighbors[down-1] |= 32
		}
		if x+1 < b.width {
			b.neighbors[down+1] |= 16
		}
	}
}

// canAggregate 判断纵向连续的四个系数是否满足清理遍的游程编码条件
// 入参: x 横坐标, y 条带起点
// 返回: bool 是否可使用游程编码
func (b *codeBlock) canAggregate(x, y int) bool {
	for dy := 0; dy < 4; dy++ {
		if b.flags[(y+dy)*b.width+x]&(blockSignificant|blockVisited) != 0 || b.significanceContext(x, y+dy) != 0 {
			return false
		}
	}
	return true
}

// neighbor 读取邻接样本状态，排除越界及垂直因果模式下不可用的样本
// 入参: x 相邻样本横坐标, y 相邻样本纵坐标, currentY 当前样本纵坐标
// 返回: uint8 相邻样本状态
func (b *codeBlock) neighbor(x, y, currentY int) uint8 {
	if x < 0 || x >= b.width || y < 0 || y >= b.height || (b.style&CodeBlockCausal != 0 && currentY&3 == 3 && y > currentY) {
		return 0
	}
	return b.flags[y*b.width+x]
}

// significanceContext 依据T.800表D.1选择显著性上下文
// 入参: x 横坐标, y 纵坐标
// 返回: int 上下文索引
func (b *codeBlock) significanceContext(x, y int) int {
	mask := b.neighbors[y*b.width+x]
	if b.style&CodeBlockCausal != 0 && y&3 == 3 {
		mask &= 0x37
	}
	return int(blockContexts[b.orientation][mask])
}

// makeBlockContexts 构造各子带的显著性上下文表
// 返回: [4][256]uint8 上下文表
func makeBlockContexts() [4][256]uint8 {
	var table [4][256]uint8
	for orientation := bandLL; orientation <= bandHH; orientation++ {
		for mask := range 256 {
			h := bits.OnesCount8(uint8(mask) & 3)
			v := bits.OnesCount8(uint8(mask) & 12)
			d := bits.OnesCount8(uint8(mask) & 240)
			table[orientation][mask] = uint8(blockSignificanceContext(orientation, h, v, d))
		}
	}
	return table
}

// blockSignificanceContext 根据水平、垂直和对角方向的显著系数数量确定上下文
// 入参: orientation 子带方向, h 水平计数, v 垂直计数, d 对角计数
// 返回: int 上下文索引
func blockSignificanceContext(orientation bandOrientation, h, v, d int) int {
	if orientation == bandHH {
		hv := h + v
		switch d {
		case 0:
			return min(hv, 2)
		case 1:
			return 3 + min(hv, 2)
		case 2:
			if hv == 0 {
				return 6
			}
			return 7
		default:
			return 8
		}
	}
	if orientation == bandHL {
		h, v = v, h
	}
	if h == 2 {
		return 8
	}
	if h == 1 {
		if v != 0 {
			return 7
		}
		if d != 0 {
			return 6
		}
		return 5
	}
	if v == 2 {
		return 4
	}
	if v == 1 {
		return 3
	}
	return min(d, 2)
}

// signContext 依据T.800表D.2和D.3选择符号上下文及预测位
// 入参: x 横坐标, y 纵坐标
// 返回: int 上下文索引, uint8 符号预测位
func (b *codeBlock) signContext(x, y int) (int, uint8) {
	h := signContribution(b.neighbor(x-1, y, y)) + signContribution(b.neighbor(x+1, y, y))
	v := signContribution(b.neighbor(x, y-1, y)) + signContribution(b.neighbor(x, y+1, y))
	h, v = max(-1, min(1, h)), max(-1, min(1, v))
	xor := uint8(0)
	if h < 0 || (h == 0 && v < 0) {
		h, v, xor = -h, -v, 1
	}
	if h == 0 {
		if v == 0 {
			return 9, xor
		}
		return 10, xor
	}
	return 12 + v, xor
}

// signContribution 根据邻接系数的显著性和符号计算预测值
// 入参: flag 样本状态
// 返回: int 符号预测值，取值为-1、0或1
func signContribution(flag uint8) int {
	if flag&blockSignificant == 0 {
		return 0
	}
	if flag&blockNegative != 0 {
		return -1
	}
	return 1
}

// coefficient 返回有符号系数，可使用未解码区间的中点重建
// 入参: index 系数索引, midpoint 是否使用中点
// 返回: int64 系数
func (b *codeBlock) coefficient(index int, midpoint bool) int64 {
	if b.flags[index]&blockSignificant == 0 || b.magnitude[index] == 0 {
		return 0
	}
	v := b.magnitude[index]
	if midpoint && b.lowest[index] > 0 {
		v |= uint64(1) << (b.lowest[index] - 1)
	}
	if b.flags[index]&blockNegative != 0 {
		return -int64(v)
	}
	return int64(v)
}
