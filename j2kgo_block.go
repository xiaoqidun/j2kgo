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

// codeBlock 保存系数幅度、符号与逐遍编解码状态
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

// blockEncoder 管理编码段的MQ编码器或旁路写入器
type blockEncoder struct {
	raw     bool
	encoder mqEncoder
	writer  packetWriter
}

// blockDecoder 管理编码段的MQ解码器或旁路读取器
type blockDecoder struct {
	raw     bool
	err     error
	decoder mqDecoder
	reader  rawBits
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

// encodePreparedBlock 在已填充样本的复用码块上执行T1编码并按需增量计算率失真点
// 入参: ctx 上下文, b 已加载幅度与标志的码块, planes 有效位平面数, initBase 背景初始失真, initROI ROI未缩放初始失真, needRates 是否计算逐遍截断点
// 返回: encodedBlock 已编码码块, []ratePoint 逐遍截断点, error 错误信息
func encodePreparedBlock(ctx context.Context, b *codeBlock, planes int, initBase, initROI float64, needRates bool) (encodedBlock, []ratePoint, error) {
	if err := ctx.Err(); err != nil {
		return encodedBlock{}, nil, err
	}
	result := encodedBlock{bitPlanes: planes}
	if planes == 0 {
		if needRates {
			return result, []ratePoint{{}}, nil
		}
		return result, nil, nil
	}
	total := 3*planes - 2
	var points []ratePoint
	var shiftBuf [876]int
	distBase, distROI := initBase, initROI
	byteOffset := 0
	roiShift2 := 2 * int(b.roi)
	if needRates {
		points = make([]ratePoint, total+1)
		initDist := max(0.0, distBase)
		if distROI > 0 {
			initDist += math.Ldexp(distROI, roiShift2)
		}
		points[0].distortion = initDist
	}
	style := b.style
	states := initialMQStates()
	for first := 0; first < total; {
		count := blockSegmentPasses(first, total, style)
		coder := blockEncoder{raw: blockRawPass(first, style), encoder: newMQEncoder()}
		coder.encoder.states = states
		for pass := first; pass < first+count; pass++ {
			plane := planes - 1 - (pass+2)/3
			kind := (pass + 2) % 3
			if kind == 0 || pass == first {
				if err := ctx.Err(); err != nil {
					return encodedBlock{}, nil, err
				}
			}
			var dBase, dROI float64
			switch kind {
			case 0:
				dBase, dROI = b.encodeSPP(plane, &coder, needRates)
			case 1:
				dBase, dROI = b.encodeMRP(plane, &coder, needRates)
			case 2:
				dBase, dROI = b.encodeCUP(plane, &coder, needRates)
			}
			if needRates {
				distBase += dBase
				distROI += dROI
				if plane < int(b.roi) || (plane == int(b.roi) && kind == 2) {
					distROI = 0
				}
				if pass == total-1 {
					distBase, distROI = 0, 0
				}
				distortion := max(0.0, distBase)
				if distROI > 0 {
					distortion += math.Ldexp(distROI, roiShift2)
				}
				points[pass+1] = ratePoint{passes: pass + 1, distortion: distortion}
				if coder.raw {
					pos := coder.writer.length
					if coder.writer.used > 0 {
						pos++
					}
					shiftBuf[pass-first] = pos
				} else {
					shiftBuf[pass-first] = coder.encoder.shifts()
				}
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
		if needRates {
			if coder.raw {
				fillRawPassRates(points[first+1:first+count+1], shiftBuf[:count], data, byteOffset)
			} else {
				fillMQPassRates(points[first+1:first+count+1], shiftBuf[:count], data, byteOffset)
			}
			byteOffset += len(data)
		}
		result.segments = append(result.segments, blockSegment{data: data, passes: count})
		first += count
	}
	return result, points, nil
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
	if err := b.decode(ctx, encoded); err != nil {
		return nil, err
	}
	return b, nil
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

// encodeBit 在指定上下文中编码一个二值符号
// 入参: context 上下文索引, value 待编码值
func (c *blockEncoder) encodeBit(context int, value uint8) {
	if !c.raw {
		c.encoder.encode(context, value)
		return
	}
	c.writer.bit(value)
}

// decodeBit 在指定上下文中解码一个二值符号
// 入参: context 上下文索引
// 返回: uint8 解码结果
func (c *blockDecoder) decodeBit(context int) uint8 {
	if !c.raw {
		return c.decoder.decode(context)
	}
	if c.err != nil {
		return 0
	}
	value, err := c.reader.bit()
	if err != nil {
		c.err = err
		return 0
	}
	return value
}

// decode 逐遍解码码块
// 入参: ctx 上下文, encoded 码块数据
// 返回: error 错误信息
func (b *codeBlock) decode(ctx context.Context, encoded encodedBlock) error {
	total, first := max(0, 3*encoded.bitPlanes-2), 0
	states := initialMQStates()
	for i, segment := range encoded.segments {
		maximum := blockSegmentPasses(first, total, b.style)
		if segment.passes < 1 || segment.passes > maximum || (segment.passes < maximum && i+1 < len(encoded.segments)) {
			return FormatError("code-block segment pass count")
		}
		coder := blockDecoder{raw: blockRawPass(first, b.style), reader: rawBits{data: segment.data}}
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
			plane := encoded.bitPlanes - 1 - (pass+2)/3
			kind := (pass + 2) % 3
			if kind == 0 || pass == first {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			switch kind {
			case 0:
				err = b.decodeSPP(plane, &coder)
			case 1:
				err = b.decodeMRP(plane, &coder)
			case 2:
				err = b.decodeCUP(plane, &coder)
			}
			if err != nil {
				return err
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

// decodeSPP 执行解码端显著性传播遍
// 入参: plane 位平面, coder 解码器
// 返回: error 错误信息
func (b *codeBlock) decodeSPP(plane int, coder *blockDecoder) error {
	w, h := b.width, b.height
	flags, neighbors := b.flags, b.neighbors
	ctxTable := &blockContexts[b.orientation]
	causal := b.style&CodeBlockCausal != 0
	for stripe := 0; stripe < h; stripe += 4 {
		end := min(stripe+4, h)
		for x := 0; x < w; x++ {
			for y := stripe; y < end; y++ {
				i := y*w + x
				if flags[i]&blockSignificant != 0 {
					continue
				}
				mask := neighbors[i]
				if mask == 0 {
					continue
				}
				if causal && y&3 == 3 {
					mask &= 0x37
					if mask == 0 {
						continue
					}
				}
				flags[i] |= blockVisited
				if coder.decodeBit(int(ctxTable[mask])) != 0 {
					b.decodeSign(x, y, plane, coder)
				}
			}
		}
		if coder.err != nil {
			return coder.err
		}
	}
	return coder.err
}

// decodeMRP 执行解码端幅度细化遍
// 入参: plane 位平面, coder 解码器
// 返回: error 错误信息
func (b *codeBlock) decodeMRP(plane int, coder *blockDecoder) error {
	w, h := b.width, b.height
	flags, neighbors := b.flags, b.neighbors
	causal := b.style&CodeBlockCausal != 0
	for stripe := 0; stripe < h; stripe += 4 {
		end := min(stripe+4, h)
		for x := 0; x < w; x++ {
			for y := stripe; y < end; y++ {
				i := y*w + x
				flag := flags[i]
				if flag&(blockSignificant|blockVisited) != blockSignificant {
					continue
				}
				cx := 14
				if flag&blockRefined != 0 {
					cx = 16
				} else {
					mask := neighbors[i]
					if causal && y&3 == 3 {
						mask &= 0x37
					}
					if mask != 0 {
						cx = 15
					}
				}
				value := coder.decodeBit(cx)
				b.decodeStoreMagnitudeBit(i, plane, value)
				flags[i] = flag | blockRefined
			}
		}
		if coder.err != nil {
			return coder.err
		}
	}
	return coder.err
}

// decodeCUP 执行解码端清理遍
// 入参: plane 位平面, coder 解码器
// 返回: error 错误信息
func (b *codeBlock) decodeCUP(plane int, coder *blockDecoder) error {
	w, h := b.width, b.height
	flags, neighbors := b.flags, b.neighbors
	ctxTable := &blockContexts[b.orientation]
	causal := b.style&CodeBlockCausal != 0
	for stripe := 0; stripe < h; stripe += 4 {
		end := min(stripe+4, h)
		for x := 0; x < w; x++ {
			y0 := stripe
			if end-stripe == 4 && b.canAggregateFast(x, stripe, causal) {
				if coder.decodeBit(17) == 0 {
					continue
				}
				hi := coder.decodeBit(18)
				lo := coder.decodeBit(18)
				y0 = stripe + int(hi<<1|lo)
				b.decodeSign(x, y0, plane, coder)
				y0++
			}
			for y := y0; y < end; y++ {
				i := y*w + x
				if flags[i]&(blockSignificant|blockVisited) != 0 {
					continue
				}
				mask := neighbors[i]
				if causal && y&3 == 3 {
					mask &= 0x37
				}
				if coder.decodeBit(int(ctxTable[mask])) != 0 {
					b.decodeSign(x, y, plane, coder)
				}
			}
		}
		if coder.err != nil {
			return coder.err
		}
	}
	if b.style&CodeBlockSegmentation != 0 {
		var symbol uint8
		for range 4 {
			symbol = symbol<<1 | coder.decodeBit(18)
		}
		if symbol != 10 {
			return FormatError("code-block segmentation symbol")
		}
	}
	for i := range flags {
		flags[i] &^= blockVisited
	}
	return coder.err
}

// encodeSPP 执行编码端显著性传播遍并按需累计失真变化量
// 入参: plane 位平面, coder 编码器, needRates 是否累计失真变化量
// 返回: float64 非ROI失真变化量, float64 ROI未缩放失真变化量
func (b *codeBlock) encodeSPP(plane int, coder *blockEncoder, needRates bool) (float64, float64) {
	w, h := b.width, b.height
	flags, neighbors := b.flags, b.neighbors
	ctxTable := &blockContexts[b.orientation]
	causal := b.style&CodeBlockCausal != 0
	var dBase, dROI float64
	for stripe := 0; stripe < h; stripe += 4 {
		end := min(stripe+4, h)
		for x := 0; x < w; x++ {
			for y := stripe; y < end; y++ {
				i := y*w + x
				if flags[i]&blockSignificant != 0 {
					continue
				}
				mask := neighbors[i]
				if mask == 0 {
					continue
				}
				if causal && y&3 == 3 {
					mask &= 0x37
					if mask == 0 {
						continue
					}
				}
				flags[i] |= blockVisited
				value := b.magnitudeBit(i, plane)
				coder.encodeBit(int(ctxTable[mask]), value)
				if value != 0 {
					b.encodeSign(x, y, plane, coder, needRates, &dBase, &dROI)
				}
			}
		}
	}
	return dBase, dROI
}

// encodeMRP 执行编码端幅度细化遍并按需累计失真变化量
// 入参: plane 位平面, coder 编码器, needRates 是否累计失真变化量
// 返回: float64 非ROI失真变化量, float64 ROI未缩放失真变化量
func (b *codeBlock) encodeMRP(plane int, coder *blockEncoder, needRates bool) (float64, float64) {
	w, h := b.width, b.height
	flags, neighbors := b.flags, b.neighbors
	causal := b.style&CodeBlockCausal != 0
	var dBase, dROI float64
	for stripe := 0; stripe < h; stripe += 4 {
		end := min(stripe+4, h)
		for x := 0; x < w; x++ {
			for y := stripe; y < end; y++ {
				i := y*w + x
				flag := flags[i]
				if flag&(blockSignificant|blockVisited) != blockSignificant {
					continue
				}
				cx := 14
				if flag&blockRefined != 0 {
					cx = 16
				} else {
					mask := neighbors[i]
					if causal && y&3 == 3 {
						mask &= 0x37
					}
					if mask != 0 {
						cx = 15
					}
				}
				value := b.magnitudeBit(i, plane)
				coder.encodeBit(cx, value)
				if needRates {
					b.addRefineDelta(i, plane, value, &dBase, &dROI)
				}
				flags[i] = flag | blockRefined
			}
		}
	}
	return dBase, dROI
}

// encodeCUP 执行编码端清理遍并按需累计失真变化量
// 入参: plane 位平面, coder 编码器, needRates 是否累计失真变化量
// 返回: float64 非ROI失真变化量, float64 ROI未缩放失真变化量
func (b *codeBlock) encodeCUP(plane int, coder *blockEncoder, needRates bool) (float64, float64) {
	w, h := b.width, b.height
	flags, neighbors := b.flags, b.neighbors
	ctxTable := &blockContexts[b.orientation]
	causal := b.style&CodeBlockCausal != 0
	var dBase, dROI float64
	for stripe := 0; stripe < h; stripe += 4 {
		end := min(stripe+4, h)
		for x := 0; x < w; x++ {
			y0 := stripe
			if end-stripe == 4 && b.canAggregateFast(x, stripe, causal) {
				run := 0
				for run < 4 && b.magnitudeBit((stripe+run)*w+x, plane) == 0 {
					run++
				}
				if run == 4 {
					coder.encodeBit(17, 0)
					continue
				}
				coder.encodeBit(17, 1)
				coder.encodeBit(18, uint8(run>>1))
				coder.encodeBit(18, uint8(run&1))
				y0 = stripe + run
				b.encodeSign(x, y0, plane, coder, needRates, &dBase, &dROI)
				y0++
			}
			for y := y0; y < end; y++ {
				i := y*w + x
				if flags[i]&(blockSignificant|blockVisited) != 0 {
					continue
				}
				mask := neighbors[i]
				if causal && y&3 == 3 {
					mask &= 0x37
				}
				value := b.magnitudeBit(i, plane)
				coder.encodeBit(int(ctxTable[mask]), value)
				if value != 0 {
					b.encodeSign(x, y, plane, coder, needRates, &dBase, &dROI)
				}
			}
		}
	}
	if b.style&CodeBlockSegmentation != 0 {
		coder.encodeBit(18, 1)
		coder.encodeBit(18, 0)
		coder.encodeBit(18, 1)
		coder.encodeBit(18, 0)
	}
	for i := range flags {
		flags[i] &^= blockVisited
	}
	return dBase, dROI
}

// decodeSign 在系数首次显著时解码其符号并更新状态
// 入参: x 横坐标, y 纵坐标, plane 位平面, coder 解码器
func (b *codeBlock) decodeSign(x, y, plane int, coder *blockDecoder) {
	i := y*b.width + x
	cx, xor := b.signContext(x, y)
	if coder.raw {
		xor = 0
	}
	negative := coder.decodeBit(cx) ^ xor
	flag := (b.flags[i] &^ blockNegative) | blockSignificant
	if negative != 0 {
		flag |= blockNegative
	}
	if b.roi != 0 && plane >= int(b.roi) {
		flag |= blockROI
	}
	b.flags[i] = flag
	b.decodeStoreMagnitudeBit(i, plane, 1)
	b.markNeighbors(x, y)
}

// encodeSign 在系数首次显著时编码其符号并按需累计失真变化量
// 入参: x 横坐标, y 纵坐标, plane 位平面, coder 编码器, needRates 是否累计失真, dBase 非ROI失真累计地址, dROI ROI失真累计地址
func (b *codeBlock) encodeSign(x, y, plane int, coder *blockEncoder, needRates bool, dBase, dROI *float64) {
	i := y*b.width + x
	cx, xor := b.signContext(x, y)
	if coder.raw {
		xor = 0
	}
	negative := uint8(0)
	if b.flags[i]&blockNegative != 0 {
		negative = 1
	}
	coder.encodeBit(cx, negative^xor)
	b.flags[i] |= blockSignificant
	if needRates {
		b.addSignDelta(i, plane, dBase, dROI)
	}
	b.markNeighbors(x, y)
}

// addSignDelta 累加样本首次显著带来的重建失真变化量
// 入参: index 系数索引, plane 码流位平面, dBase 非ROI失真累计地址, dROI ROI失真累计地址
func (b *codeBlock) addSignDelta(index, plane int, dBase, dROI *float64) {
	isROI := b.flags[index]&blockROI != 0
	eff := plane
	if isROI {
		eff -= int(b.roi)
	}
	m := b.magnitude[index]
	recon := uint64(1)
	if eff > 0 {
		recon = (uint64(1) << eff) | (uint64(1) << (eff - 1))
	}
	oldErr := float64(m)
	newErr := float64(m) - float64(recon)
	delta := (newErr - oldErr) * (newErr + oldErr)
	if isROI {
		*dROI += delta
		return
	}
	*dBase += delta
}

// addRefineDelta 累加样本幅度细化带来的重建失真变化量
// 入参: index 系数索引, plane 码流位平面, value 细化位值, dBase 非ROI失真累计地址, dROI ROI失真累计地址
func (b *codeBlock) addRefineDelta(index, plane int, value uint8, dBase, dROI *float64) {
	isROI := b.flags[index]&blockROI != 0
	eff := plane
	if isROI {
		eff -= int(b.roi)
	}
	if eff < 0 {
		return
	}
	r := b.magnitude[index] & ((uint64(1) << eff) - 1)
	eOld := float64(r)
	if value == 0 {
		eOld -= float64(uint64(1) << eff)
	}
	var eNew float64
	if eff > 0 {
		eNew = float64(r) - float64(uint64(1)<<(eff-1))
	}
	delta := (eNew - eOld) * (eNew + eOld)
	if isROI {
		*dROI += delta
		return
	}
	*dBase += delta
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

// decodeStoreMagnitudeBit 根据ROI移位量还原系数位，更新幅度和已解码精度
// 入参: index 系数索引, plane 码流位平面, value 位值
func (b *codeBlock) decodeStoreMagnitudeBit(index, plane int, value uint8) {
	if b.flags[index]&blockROI != 0 {
		plane -= int(b.roi)
	}
	if plane >= int(b.precision) {
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

// canAggregateFast 判断纵向连续四个系数是否满足清理遍游程编码条件
// 入参: x 横坐标, stripe 条带起点, causal 是否启用垂直因果模式
// 返回: bool 是否可使用游程编码
func (b *codeBlock) canAggregateFast(x, stripe int, causal bool) bool {
	w := b.width
	flags, neighbors := b.flags, b.neighbors
	i0 := stripe*w + x
	i1 := i0 + w
	i2 := i1 + w
	i3 := i2 + w
	if (flags[i0]|flags[i1]|flags[i2]|flags[i3])&(blockSignificant|blockVisited) != 0 {
		return false
	}
	n3 := neighbors[i3]
	if causal {
		n3 &= 0x37
	}
	return (neighbors[i0] | neighbors[i1] | neighbors[i2] | n3) == 0
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
