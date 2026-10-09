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
	"io"
)

// encodingSource 按需提供瓦片样本及库内样本缓冲的字节数
type encodingSource func(index int, limits Limits) (*Raster, uint64, error)

// encodeImage 写入主头、瓦片及结束标记，按需预编码主头长度表和集中包头
// 入参: ctx 上下文, output 输出流, plan 编码参数, source 瓦片样本来源
// 返回: error 错误信息
func encodeImage(ctx context.Context, output io.Writer, plan encodingPlan, source encodingSource) error {
	if err := plan.prepareLayerBudgets(ctx); err != nil {
		return err
	}
	lengths, err := plan.prepareTileLengths(ctx)
	if err != nil {
		return err
	}
	var packets *mainPacketLengthEncoding
	if plan.plm {
		if plan.limits.MaxMemoryBytes <= 64 {
			return &LimitError{Resource: "encoded PLM memory", Limit: plan.limits.MaxMemoryBytes, Required: 65}
		}
		plan.limits.MaxMemoryBytes -= 64
		packets = &mainPacketLengthEncoding{collect: true}
	}
	var headers *mainPacketHeaderEncoding
	if plan.ppm {
		if plan.limits.MaxMemoryBytes <= 64 {
			return &LimitError{Resource: "encoded PPM memory", Limit: plan.limits.MaxMemoryBytes, Required: 65}
		}
		plan.limits.MaxMemoryBytes -= 64
		headers = &mainPacketHeaderEncoding{collect: true}
	}
	w := codestreamWriter{ctx: ctx, w: output}
	if err := w.header(plan); err != nil {
		return err
	}
	plan.headerBytes = w.written + 2
	if lengths != nil {
		plan.headerBytes += uint64(len(lengths.data))
	}
	if lengths != nil || packets != nil || headers != nil {
		measure := codestreamWriter{ctx: ctx, w: io.Discard, tileLengths: lengths, packetLengths: packets, packetHeaders: headers, measureOnly: true}
		if err := measure.tiles(plan, source); err != nil {
			return err
		}
	}
	if lengths != nil {
		if lengths.next != lengths.count {
			return FormatError("encoded TLM tile-part count")
		}
		if err := w.write(lengths.data); err != nil {
			return err
		}
		lengths.next, lengths.collect = 0, false
		w.tileLengths = lengths
	}
	if packets != nil {
		if err := w.write(packets.data); err != nil {
			return err
		}
		packets.collect, packets.payload, packets.position = false, 0, 0
		w.packetLengths = packets
	}
	if headers != nil {
		if headers.first || headers.remaining != 0 || headers.payload < 4 {
			return FormatError("incomplete encoded PPM headers")
		}
		if err := w.write(headers.data); err != nil {
			return err
		}
		headers.collect, headers.payload, headers.position = false, 0, 0
		w.packetHeaders = headers
	}
	if err := w.tiles(plan, source); err != nil {
		return err
	}
	if lengths != nil && lengths.next != lengths.count {
		return FormatError("encoded TLM tile-part count")
	}
	if packets != nil && packets.position != len(packets.data) {
		return FormatError("encoded PLM tile-part count")
	}
	if headers != nil && (headers.first || headers.remaining != 0 || headers.position != len(headers.data)) {
		return FormatError("encoded PPM tile-part count")
	}
	return w.marker(markerEOC, nil)
}

// tiles 逐瓦片读取并编码样本，将库内样本缓冲计入内存预算
// 入参: plan 编码参数, source 瓦片样本来源
// 返回: error 错误信息
func (w *codestreamWriter) tiles(plan encodingPlan, source encodingSource) error {
	for index := 0; index < plan.columns*plan.rows; index++ {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		local := plan.tilePlan(index)
		if w.packetLengths != nil {
			memory := uint64(cap(w.packetLengths.data))
			if memory >= local.limits.MaxMemoryBytes {
				return &LimitError{Resource: "encoded PLM memory", Limit: local.limits.MaxMemoryBytes, Required: memory + 1}
			}
			local.limits.MaxMemoryBytes -= memory
		}
		if w.packetHeaders != nil {
			memory := uint64(cap(w.packetHeaders.data))
			if memory >= local.limits.MaxMemoryBytes {
				return &LimitError{Resource: "encoded PPM memory", Limit: local.limits.MaxMemoryBytes, Required: memory + 1}
			}
			local.limits.MaxMemoryBytes -= memory
		}
		raster, memory, err := source(index, local.limits)
		if err != nil {
			return fmt.Errorf("read tile %d: %w", index, err)
		}
		if memory >= local.limits.MaxMemoryBytes {
			return &LimitError{Resource: "image tile memory", Limit: local.limits.MaxMemoryBytes, Required: memory + 1}
		}
		local.limits.MaxMemoryBytes -= memory
		if err := w.tile(raster, local, index); err != nil {
			return fmt.Errorf("encode tile %d: %w", index, err)
		}
	}
	return w.ctx.Err()
}
