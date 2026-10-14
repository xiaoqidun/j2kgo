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
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"unicode/utf8"
)

// MetadataBox 保存JP2文件顶层的一个可选元数据框，不包含框头
// Type为四字节类型标识，支持"xml "、"uuid"、"uinf"和"jp2i"
// Data保留原始内容；XML文档、UUID关联数据和版权信息的含义由调用方处理
// XML内容由调用方保证良构，库不展开实体或验证供应方的元数据格式
type MetadataBox struct {
	Type string
	Data []byte
}

// Metadata 按文件顺序读取JP2顶层的XML、UUID、UUID信息和版权元数据
// 返回数据由调用方独立持有，不影响图像解码，也不会在重新编码时自动写入
// 裸码流返回空列表；未知框不返回，外部URL不会被访问
// 入参: ctx 上下文
// 返回: []MetadataBox 元数据框, error 错误信息
func (d *Decoder) Metadata(ctx context.Context) ([]MetadataBox, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if d.closed || d.index == nil || d.source == nil {
		return nil, fmt.Errorf("decoder is closed")
	}
	limits, err := d.decodeLimits()
	if err != nil {
		return nil, err
	}
	cache := d.tileCacheMemory()
	if cache >= limits.MaxMemoryBytes {
		return nil, &LimitError{Resource: "metadata memory", Limit: limits.MaxMemoryBytes, Required: cache + 1}
	}
	limits.MaxMemoryBytes -= cache
	var signature [2]byte
	if _, err := d.source.readAt(ctx, signature[:], 0); err != nil {
		return nil, err
	}
	if signature == [2]byte{0xff, 0x4f} {
		return nil, nil
	}
	reader := io.NewSectionReader(contextReaderAt{ctx: ctx, source: d.source}, 0, d.source.size)
	h := headerReader{ctx: ctx, r: reader, limits: limits, read: 12}
	if _, err := reader.Seek(12, io.SeekStart); err != nil {
		return nil, err
	}
	budget := layoutBudget{limit: limits.MaxMemoryBytes}
	var result []MetadataBox
	for h.read < uint64(d.source.size) {
		kind, end, err := h.box(uint64(d.source.size))
		if err != nil {
			return nil, err
		}
		if metadataType(kind) {
			length := end - h.read
			if _, err := checkedProduct("metadata size", length, 1, uint64(^uint(0)>>1)); err != nil {
				return nil, err
			}
			if err := budget.add(1, length); err != nil {
				return nil, err
			}
			box := MetadataBox{Type: kind, Data: make([]byte, int(length))}
			if err := h.full(box.Data); err != nil {
				return nil, err
			}
			if err := validateMetadataBox(ctx, box); err != nil {
				return nil, err
			}
			result, err = appendBudgeted(result, []MetadataBox{box}, &budget, 48)
			if err != nil {
				return nil, err
			}
		}
		if _, err := reader.Seek(int64(end), io.SeekStart); err != nil {
			return nil, err
		}
		h.read = end
	}
	return result, ctx.Err()
}

// metadataType 判断框类型是否属于支持的顶层元数据
// 入参: kind 四字节类型标识
// 返回: bool 是否支持
func metadataType(kind string) bool {
	switch kind {
	case "xml ", "uuid", "uinf", "jp2i":
		return true
	}
	return false
}

// validateMetadataBox 校验元数据框的结构，不解释供应方定义的载荷
// 入参: ctx 上下文, box 元数据框
// 返回: error 错误信息
func validateMetadataBox(ctx context.Context, box MetadataBox) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch box.Type {
	case "xml ", "jp2i":
		return nil
	case "uuid":
		if len(box.Data) < 16 {
			return FormatError("UUID box length")
		}
		return nil
	case "uinf":
		return validateUUIDInfo(ctx, box.Data)
	default:
		return UnsupportedError("metadata box type")
	}
}

// validateUUIDInfo 校验UUID信息框中的标识列表和URL字段
// 入参: ctx 上下文, data 超框内容
// 返回: error 错误信息
func validateUUIDInfo(ctx context.Context, data []byte) error {
	h := headerReader{ctx: ctx, r: bytes.NewReader(data), limits: Limits{MaxInputBytes: uint64(len(data))}}
	var list, location bool
	for h.read < uint64(len(data)) {
		kind, end, err := h.box(uint64(len(data)))
		if err != nil {
			return err
		}
		payload := data[h.read:end]
		switch kind {
		case "ulst":
			if list || len(payload) < 2 || len(payload) != 2+16*int(binary.BigEndian.Uint16(payload)) {
				return FormatError("UUID list length or duplicate")
			}
			list = true
		case "url ":
			if location || len(payload) < 5 || binary.BigEndian.Uint32(payload) != 0 || payload[len(payload)-1] != 0 || bytes.IndexByte(payload[4:len(payload)-1], 0) >= 0 || !utf8.Valid(payload[4:len(payload)-1]) {
				return FormatError("UUID URL box")
			}
			location = true
		}
		if err := h.skip(end - h.read); err != nil {
			return err
		}
	}
	if !list || !location {
		return FormatError("missing UUID list or URL")
	}
	return nil
}

// setMetadata 校验并复制显式指定的元数据，将副本计入编码内存预算
// 入参: ctx 上下文, boxes 元数据框
// 返回: error 错误信息
func (p *encodingPlan) setMetadata(ctx context.Context, boxes []MetadataBox) error {
	if len(boxes) == 0 {
		return nil
	}
	if p.format != FormatJP2 {
		return fmt.Errorf("metadata requires JP2 output")
	}
	budget := layoutBudget{limit: p.limits.MaxMemoryBytes}
	if err := budget.add(uint64(len(boxes)), 48); err != nil {
		return err
	}
	for _, box := range boxes {
		if err := validateMetadataBox(ctx, box); err != nil {
			return err
		}
		if err := budget.add(uint64(len(box.Data)), 1); err != nil {
			return err
		}
	}
	if budget.used >= p.limits.MaxMemoryBytes {
		return &LimitError{Resource: "metadata memory", Limit: p.limits.MaxMemoryBytes, Required: budget.used + 1}
	}
	p.metadata = make([]MetadataBox, len(boxes))
	for i, box := range boxes {
		data, err := cloneSliceContext(ctx, box.Data)
		if err != nil {
			return err
		}
		p.metadata[i] = MetadataBox{Type: box.Type, Data: data}
	}
	p.limits.MaxMemoryBytes -= budget.used
	return nil
}

// metadataBox 写入元数据框，必要时使用64位扩展长度
// 入参: box 元数据框
// 返回: error 错误信息
func (w *codestreamWriter) metadataBox(box MetadataBox) error {
	var header [16]byte
	copy(header[4:8], box.Type)
	length := uint64(len(box.Data)) + 8
	size := 8
	if length > uint64(^uint32(0)) {
		binary.BigEndian.PutUint32(header[:4], 1)
		binary.BigEndian.PutUint64(header[8:], length+8)
		size = 16
	} else {
		binary.BigEndian.PutUint32(header[:4], uint32(length))
	}
	if err := w.write(header[:size]); err != nil {
		return err
	}
	for data := box.Data; len(data) > 0; {
		n := min(len(data), 32768)
		if err := w.write(data[:n]); err != nil {
			return err
		}
		data = data[n:]
	}
	return w.ctx.Err()
}
