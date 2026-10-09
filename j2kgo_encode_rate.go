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
	"fmt"
	"math"
	"math/bits"
)

// allocateLayers 将实际包头长度计入码率预算，选择各质量层的码块截断点
// 入参: ctx 上下文, plan 编码参数, index 瓦片索引
// 返回: error 错误信息
func (t *tileEncoding) allocateLayers(ctx context.Context, plan encodingPlan, index int) error {
	if t.rates == nil {
		return nil
	}
	targets, err := t.layerTargets(ctx, plan, index)
	if err != nil {
		return err
	}
	defer func() { t.budget.used -= uint64(len(targets)) * 8 }()
	if err := t.budget.add(uint64(len(t.blocks)), uint64(plan.layerCount)*24+128); err != nil {
		return err
	}
	t.layers = make(map[*packetBlock][]ratePoint, len(t.blocks))
	var maximum float64
	for block, points := range t.rates {
		if err := ctx.Err(); err != nil {
			return err
		}
		t.layers[block] = make([]ratePoint, plan.layerCount)
		for i := 1; i < len(points); i++ {
			a, b := points[i-1], points[i]
			maximum = max(maximum, (a.distortion-b.distortion)/float64(b.rate-a.rate))
		}
	}
	for layer := range plan.layerCount {
		var options Layer
		if len(plan.layers) > 0 {
			options = plan.layers[layer]
		}
		if options.BitsPerPixel == 0 {
			t.selectLayer(layer, 0, true)
			if layer == plan.layerCount-1 {
				for block, points := range t.prefixes {
					if t.layers[block][layer].passes != len(points)-1 {
						return FormatError("ROI quality layers cannot contain all coding passes")
					}
				}
			}
			continue
		}
		target := targets[layer]
		partial := plan
		partial.layerCount = layer + 1
		t.selectLayer(layer, 0, true)
		length, err := t.layerSize(ctx, partial)
		if err != nil {
			return err
		}
		if length <= target {
			continue
		}
		t.restoreLayer(layer)
		length, err = t.layerSize(ctx, partial)
		if err != nil {
			return err
		}
		if length > target {
			return fmt.Errorf("quality layer %d: %w", layer+1, &LimitError{Resource: "encoded layer bytes", Limit: target, Required: length})
		}
		lowBits, highBits := uint64(0), math.Float64bits(math.Nextafter(maximum, math.Inf(1)))
		best := math.Inf(1)
		for highBits-lowBits > 1 {
			thresholdBits := lowBits + (highBits-lowBits)/2
			threshold := math.Float64frombits(thresholdBits)
			t.selectLayer(layer, threshold, false)
			length, err = t.layerSize(ctx, partial)
			if err != nil {
				return err
			}
			if length <= target {
				best, highBits = threshold, thresholdBits
			} else {
				lowBits = thresholdBits
			}
		}
		if math.IsInf(best, 1) {
			t.restoreLayer(layer)
		} else {
			t.selectLayer(layer, best, false)
		}
	}
	return ctx.Err()
}

// layerTargets 为后续质量层的空包和分段头预留空间，计算各层可用预算
// 入参: ctx 上下文, plan 编码参数, index 瓦片索引
// 返回: []uint64 各层字节上限，使用后须归还内存预算, error 错误信息
func (t *tileEncoding) layerTargets(ctx context.Context, plan encodingPlan, index int) ([]uint64, error) {
	if len(plan.layers) == 0 {
		return nil, nil
	}
	var packets uint64
	for _, component := range t.layouts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, resolution := range component.resolutions {
			var err error
			packets, err = checkTotal("packet count", packets, uint64(len(resolution.precincts)), math.MaxUint64)
			if err != nil {
				return nil, err
			}
		}
	}
	if err := t.budget.add(uint64(len(plan.layers)), 8); err != nil {
		return nil, err
	}
	targets := make([]uint64, len(plan.layers))
	complete := false
	defer func() {
		if !complete {
			t.budget.used -= uint64(len(targets)) * 8
		}
	}()
	margin := uint64(math.MaxUint64)
	roi := t.roiHeaderSize() + plan.parameterBytes
	for layer := len(plan.layers) - 1; layer >= 0; layer-- {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if plan.layers[layer].BitsPerPixel == 0 {
			continue
		}
		cost, err := emptyLayerCost(plan, packets, layer+1)
		if err != nil {
			return nil, err
		}
		cost, err = checkTotal("layer header bytes", cost, roi, math.MaxUint64)
		if err != nil {
			return nil, err
		}
		target := plan.layerBudget(plan.layers[layer].BitsPerPixel, index, layer)
		if cost > target {
			return nil, fmt.Errorf("quality layer %d: %w", layer+1, &LimitError{Resource: "encoded layer bytes", Limit: target, Required: cost})
		}
		margin = min(margin, target-cost)
		targets[layer] = cost + margin
	}
	complete = true
	return targets, nil
}

// emptyLayerCost 计算空包和分段头开销，包含长度表及集中包头的预算
// 入参: plan 编码参数, packets 每层数据包数, layers 累计层数
// 返回: uint64 字节数, error 错误信息
func emptyLayerCost(plan encodingPlan, packets uint64, layers int) (uint64, error) {
	count, err := checkedProduct("packet count", packets, uint64(layers), math.MaxUint64)
	if err != nil {
		return 0, err
	}
	parts := uint64(1)
	if plan.partPackets > 0 && count > 0 {
		parts = (count-1)/uint64(plan.partPackets) + 1
	}
	if parts > 255 {
		return 0, &LimitError{Resource: "encoded tile-parts", Limit: 255, Required: parts}
	}
	if plan.plm {
		perPart := count
		if plan.partPackets > 0 {
			perPart = min(perPart, uint64(plan.partPackets))
		}
		if perPart > 255 {
			return 0, &LimitError{Resource: "PLM tile-part bytes", Limit: 255, Required: perPart}
		}
	}
	size := uint64(1)
	if plan.sop {
		size += 6
	}
	if plan.eph {
		size += 2
	}
	length, err := checkedProduct("packet header bytes", count, size, math.MaxUint64)
	if err != nil {
		return 0, err
	}
	if plan.plt || plan.ppt || plan.ppm {
		remaining := count
		for range parts {
			n := remaining
			if plan.partPackets > 0 {
				n = min(n, uint64(plan.partPackets))
			}
			var cost uint64
			if plan.plt {
				cost, err = emptyPacketLengthSize(n)
				if err != nil {
					return 0, err
				}
			}
			if plan.ppt || plan.ppm {
				packed, err := emptyPackedHeaderSize(n, plan.eph)
				if err != nil {
					return 0, err
				}
				header := uint64(1)
				if plan.eph {
					header += 2
				}
				cost += packed - n*header
			}
			if plan.ppm {
				cost += 9
			}
			length, err = checkTotal("layer header bytes", length, cost, math.MaxUint64)
			if err != nil {
				return 0, err
			}
			remaining -= n
		}
	}
	if plan.plm {
		length, err = checkTotal("layer header bytes", length, count+parts*6, math.MaxUint64)
		if err != nil {
			return 0, err
		}
	}
	return checkTotal("layer header bytes", length, parts*14, math.MaxUint64)
}

// selectLayer 按率失真斜率选择码块截断点，保留前层结果并限制新增编码遍数
// 入参: layer 层索引, threshold 斜率阈值, full 是否忽略阈值并保留允许的最多编码遍
func (t *tileEncoding) selectLayer(layer int, threshold float64, full bool) {
	for block, layers := range t.layers {
		selected := selectRatePoint(t.rates[block], threshold)
		if full {
			selected = ratePoint{}
			for _, segment := range t.blocks[block].segments {
				selected.passes += segment.passes
				selected.rate += len(segment.data)
			}
		}
		if layer > 0 && selected.passes < layers[layer-1].passes {
			selected = layers[layer-1]
		}
		if points := t.prefixes[block]; len(points) > 0 {
			limit := 164
			if layer > 0 {
				limit += layers[layer-1].passes
			}
			if selected.passes > limit {
				selected = points[limit]
			}
		}
		layers[layer] = selected
	}
}

// restoreLayer 撤销当前层新增的编码遍，恢复上一层的截断点
// 入参: layer 层索引
func (t *tileEncoding) restoreLayer(layer int) {
	for _, layers := range t.layers {
		layers[layer] = ratePoint{}
		if layer > 0 {
			layers[layer] = layers[layer-1]
		}
	}
}

// layerSize 计算当前瓦片各质量层的字节预算，包含长度表和集中包头的预留开销
// 入参: ctx 上下文, plan 包含当前质量层范围的编码参数
// 返回: uint64 字节预算, error 错误信息
func (t *tileEncoding) layerSize(ctx context.Context, plan encodingPlan) (uint64, error) {
	parts, packets := uint64(1), uint64(0)
	var tables uint64
	var mainLengths uint64
	var table packetLengthEncoding
	var packed packedHeaderEncoding
	length, _, err := t.measurePackets(ctx, plan, func(length uint64, header int) error {
		if plan.partPackets > 0 && packets == uint64(plan.partPackets) {
			parts++
			packets = 0
			tables += table.length + uint64(packed.markers)*5
			table = packetLengthEncoding{}
			packed = packedHeaderEncoding{}
		}
		packets++
		if parts > 255 {
			return &LimitError{Resource: "encoded tile-parts", Limit: 255, Required: parts}
		}
		if plan.ppt || plan.ppm {
			if _, err := packed.add(header); err != nil {
				return err
			}
			length -= uint64(header)
		}
		if plan.plt {
			if err := table.add(length, nil); err != nil {
				return err
			}
		}
		if plan.plm {
			mainLengths += uint64(max(1, (bits.Len64(length)+6)/7))
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	length += 14*parts + t.roiHeaderSize() + plan.parameterBytes + tables + table.length + uint64(packed.markers)*5
	if plan.plm {
		length += mainLengths + parts*6
	}
	if plan.ppm {
		length += parts * 9
	}
	return length, nil
}

// layerBudget 预留公共头部和各瓦片必要开销，再按面积分配剩余字节预算
// 入参: rate 每像素位数, index 瓦片索引, layer 质量层索引
// 返回: uint64 当前瓦片字节上限
func (p encodingPlan) layerBudget(rate float64, index, layer int) uint64 {
	pixels := uint64(p.info.Bounds.Dx()) * uint64(p.info.Bounds.Dy())
	bounds := p.tileBounds(index)
	area := uint64(bounds.Dx()) * uint64(bounds.Dy())
	if p.tileRates {
		return p.tileLayerBudget(rate, area, pixels)
	}
	size := math.Floor(rate * float64(pixels) / 8)
	if size <= float64(p.headerBytes) {
		return 0
	}
	target := uint64(math.MaxUint64)
	if size < 0x1p64 {
		target = uint64(size)
	}
	target -= p.headerBytes
	var base uint64
	if len(p.layerCosts) > 0 {
		if target < p.layerCosts[layer] {
			return 0
		}
		cost := p.tileCosts[index]
		var err error
		base, err = emptyLayerCost(p, cost.packets, layer+1)
		if err != nil {
			return 0
		}
		base += cost.roi + p.parameterBytes
		target -= p.layerCosts[layer]
	}
	hi, lo := bits.Mul64(target, area)
	budget, _ := bits.Div64(hi, lo, pixels)
	return base + budget
}

// tileLayerBudget 按瓦片码率计算字节预算，扣除按面积分摊并向上取整的公共头部开销
// 入参: rate 每像素位数, area 瓦片像素数, pixels 图像像素数
// 返回: uint64 瓦片字节上限
func (p encodingPlan) tileLayerBudget(rate float64, area, pixels uint64) uint64 {
	hi, lo := bits.Mul64(p.headerBytes, area)
	header, remainder := bits.Div64(hi, lo, pixels)
	if remainder != 0 {
		header++
	}
	size := math.Floor(rate * float64(area) / 8)
	if size <= float64(header) {
		return 0
	}
	if size >= 0x1p64 {
		return math.MaxUint64 - header
	}
	return uint64(size) - header
}
