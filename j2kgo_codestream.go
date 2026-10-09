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
	"encoding/binary"
	"fmt"
	"image"
	"io"
)

// streamIndex 保存主头参数及瓦片分段索引
type streamIndex struct {
	info          Info
	main          codingHeader
	tileOrigin    image.Point
	tileSize      [2]uint32
	columns       int
	rows          int
	tiles         []tileIndex
	parts         []*tilePart
	memory        uint64
	minLevels     int
	registration  bool
	tileLengths   map[byte][]tileLength
	packetLengths packetLengthTable
}

// tileIndex 保存瓦片专用编码参数及按序排列的分段
type tileIndex struct {
	header  codingHeader
	parts   []*tilePart
	total   int
	volumes []progressionVolume
}

// tilePart 记录瓦片分段的数据位置、集中包头和渐进顺序变更
type tilePart struct {
	start        int64
	tile         uint16
	data         inputRange
	packed       map[byte]inputRange
	headers      []inputRange
	progressions []progressionVolume
	lengths      packetLengthTable
	plm, plt     []inputRange
}

// tileParameters 保存继承主头参数后的瓦片配置
type tileParameters struct {
	defaults defaultCoding
	styles   []codingStyle
	quant    []quantization
	roi      []uint8
	volumes  []progressionVolume
}

// readCodestream 建立码流索引，不读取码字数据
// 入参: ctx 上下文, source 输入源, extent 码流范围, limits 资源限制, warning 分段总数修正回调，nil时不允许修正
// 返回: *streamIndex 码流索引, error 错误信息
func readCodestream(ctx context.Context, source *inputSource, extent inputRange, limits Limits, warning func(error)) (*streamIndex, error) {
	limits = limits.normalized()
	if extent.offset < 0 || extent.length < 2 || extent.offset > source.size || extent.length > source.size-extent.offset {
		return nil, FormatError("codestream extent")
	}
	info, err := readHeader(ctx, io.NewSectionReader(contextReaderAt{ctx: ctx, source: source}, extent.offset, extent.length), limits)
	if err != nil {
		return nil, err
	}
	result := &streamIndex{info: info, minLevels: 32}
	reader := markerReader{source: source, position: extent.offset, end: extent.offset + extent.length, limits: limits}
	marker, err := reader.next(ctx)
	if err != nil {
		return nil, err
	}
	if marker.code != markerSOC {
		return nil, FormatError("missing SOC")
	}
	marker, err = reader.next(ctx)
	if err != nil {
		return nil, err
	}
	if marker.code != markerSIZ || len(marker.data) < 36 {
		return nil, FormatError("missing SIZ")
	}
	if binary.BigEndian.Uint16(marker.data)&0xc000 != 0 {
		return nil, UnsupportedError("extended JPEG2000 capabilities")
	}
	result.tileSize = [2]uint32{binary.BigEndian.Uint32(marker.data[18:]), binary.BigEndian.Uint32(marker.data[22:])}
	result.tileOrigin = image.Pt(int(binary.BigEndian.Uint32(marker.data[26:])), int(binary.BigEndian.Uint32(marker.data[30:])))
	result.columns = int(ceilSigned(int64(info.Bounds.Max.X-result.tileOrigin.X), int64(result.tileSize[0])))
	result.rows = int(ceilSigned(int64(info.Bounds.Max.Y-result.tileOrigin.Y), int64(result.tileSize[1])))
	budget := layoutBudget{limit: limits.MaxMemoryBytes}
	if err := budget.add(uint64(result.columns*result.rows), 192); err != nil {
		return nil, err
	}
	if err := budget.add(uint64(len(info.Components)), componentHeaderMemory); err != nil {
		return nil, err
	}
	result.tiles = make([]tileIndex, result.columns*result.rows)
	for {
		marker, err = reader.next(ctx)
		if err != nil {
			return nil, err
		}
		if marker.code == markerSOT {
			break
		}
		if err := result.readMainMarker(marker, &budget); err != nil {
			return nil, err
		}
	}
	if result.main.defaults == nil || result.main.quantization == nil {
		return nil, FormatError("missing COD or QCD")
	}
	for marker.code == markerSOT {
		if err := result.readTilePart(ctx, &reader, marker, &budget, warning != nil); err != nil {
			return nil, err
		}
		marker, err = reader.next(ctx)
		if err != nil {
			return nil, err
		}
	}
	if marker.code != markerEOC || reader.position != reader.end {
		return nil, FormatError("missing or misplaced EOC")
	}
	if err := result.validateTileLengths(ctx, &budget); err != nil {
		return nil, err
	}
	parameterMemory := uint64(len(info.Components))*128 + 48
	if err := budget.add(1, parameterMemory); err != nil {
		return nil, err
	}
	for i := range result.tiles {
		tile := &result.tiles[i]
		if len(tile.parts) == 0 || tile.total > len(tile.parts) || (warning == nil && tile.total != 0 && tile.total != len(tile.parts)) {
			return nil, FormatError("missing tile or tile-part")
		}
		if err := tile.indexProgressions(&budget); err != nil {
			return nil, err
		}
		params, err := result.parameters(i)
		if err != nil {
			return nil, err
		}
		for _, style := range params.styles {
			result.minLevels = min(result.minLevels, style.levels)
		}
	}
	budget.used -= parameterMemory
	if err := result.indexPackedHeaders(ctx, source, &budget); err != nil {
		return nil, err
	}
	if err := result.indexPacketLengths(ctx, source, &budget); err != nil {
		return nil, err
	}
	result.memory = budget.used
	if warning != nil {
		for i := range result.tiles {
			tile := &result.tiles[i]
			if tile.total != 0 && tile.total < len(tile.parts) {
				warning(FormatError(fmt.Sprintf("tile %d declares %d tile-parts; recovered %d sequential tile-parts", i, tile.total, len(tile.parts))))
			}
		}
	}
	return result, nil
}

// readMainMarker 读取主头中的编码参数及辅助标记
// 入参: segment 标记段, budget 元数据预算
// 返回: error 错误信息
func (s *streamIndex) readMainMarker(segment markerSegment, budget *layoutBudget) error {
	if handled, err := s.main.parseCodingMarker(segment, len(s.info.Components), budget); handled || err != nil {
		return err
	}
	switch segment.code {
	case markerPPM:
		return s.main.addPacked(segment, budget)
	case markerTLM:
		return s.readTileLengths(segment.data, budget)
	case markerPLM:
		return s.packetLengths.add(segment, budget)
	case markerCOM:
		if len(segment.data) < 2 {
			return FormatError("COM length")
		}
	case markerCRG:
		return s.readRegistration(segment.data)
	case markerSOC, markerSIZ, markerSOD, markerEOC, markerSOP, markerEPH, markerPPT, markerPLT:
		return FormatError("marker not allowed in main header")
	default:
		return UnsupportedError("codestream marker")
	}
	return nil
}

// readTilePart 解析瓦片分段头并跳过数据区
// 入参: ctx 上下文, reader 标记读取器, sot 起始标记, budget 元数据预算, recoverCount 是否允许修正小于实际值的分段总数
// 返回: error 错误信息
func (s *streamIndex) readTilePart(ctx context.Context, reader *markerReader, sot markerSegment, budget *layoutBudget, recoverCount bool) error {
	data := sot.data
	if len(data) != 8 {
		return FormatError("SOT length")
	}
	index := int(binary.BigEndian.Uint16(data))
	number, total := int(data[6]), int(data[7])
	if index >= len(s.tiles) || number > 254 {
		return FormatError("SOT tile index")
	}
	tile := &s.tiles[index]
	if number != len(tile.parts) || (total != 0 && ((!recoverCount && total <= number) || (tile.total != 0 && total != tile.total))) {
		return FormatError("SOT tile-part sequence")
	}
	if total != 0 {
		tile.total = total
	}
	length := int64(binary.BigEndian.Uint32(data[2:]))
	end := sot.offset + length
	if length == 0 {
		end = reader.end - 2
	} else if length < 14 {
		return FormatError("SOT tile-part length")
	}
	if end > reader.end-2 || end < reader.position+2 {
		return FormatError("tile-part exceeds codestream")
	}
	if err := budget.add(1, 256); err != nil {
		return err
	}
	part := &tilePart{start: sot.offset, tile: uint16(index)}
	var header codingHeader
	streamEnd := reader.end
	reader.end = end
	for {
		marker, err := reader.next(ctx)
		if err != nil {
			return err
		}
		if marker.code == markerSOD {
			break
		}
		if marker.code == markerPPT {
			if len(s.main.packed) != 0 {
				return FormatError("PPM and PPT used together")
			}
			if err := header.addPacked(marker, budget); err != nil {
				return err
			}
			continue
		}
		if marker.code == markerPLT {
			if err := part.lengths.add(marker, budget); err != nil {
				return err
			}
			continue
		}
		if marker.code == markerCOM {
			if len(marker.data) < 2 {
				return FormatError("COM length")
			}
			continue
		}
		if number > 0 && marker.code != markerPOC {
			return FormatError("coding marker outside first tile-part")
		}
		handled, err := header.parseCodingMarker(marker, len(s.info.Components), budget)
		if err != nil {
			return err
		}
		if !handled {
			return FormatError("marker not allowed in tile header")
		}
	}
	if number == 0 {
		tile.header = header
	} else if len(header.progressions) > 0 && len(tile.header.progressions) == 0 {
		return FormatError("POC missing from first tile-part")
	}
	part.data = inputRange{offset: reader.position, length: end - reader.position}
	part.packed, part.progressions = header.packed, header.progressions
	tile.parts = append(tile.parts, part)
	s.parts = append(s.parts, part)
	reader.position, reader.end = end, streamEnd
	return nil
}

// parameters 按T.800规定的继承顺序确定瓦片编码参数
// 入参: index 瓦片索引
// 返回: tileParameters 有效参数, error 错误信息
func (s *streamIndex) parameters(index int) (tileParameters, error) {
	tile := &s.tiles[index]
	main := &s.main
	defaults := main.defaults
	if tile.header.defaults != nil {
		defaults = tile.header.defaults
	}
	count := len(s.info.Components)
	result := tileParameters{defaults: *defaults, styles: make([]codingStyle, count), quant: make([]quantization, count), roi: make([]uint8, count)}
	for c := range count {
		style := main.defaults.component
		if value, ok := main.coding[c]; ok {
			style = value
		}
		if tile.header.defaults != nil {
			style = tile.header.defaults.component
		}
		if value, ok := tile.header.coding[c]; ok {
			style = value
		}
		quant := *main.quantization
		if value, ok := main.quant[c]; ok {
			quant = value
		}
		if tile.header.quantization != nil {
			quant = *tile.header.quantization
		}
		if value, ok := tile.header.quant[c]; ok {
			quant = value
		}
		if _, err := quant.step(3*style.levels, style.levels); err != nil {
			return tileParameters{}, err
		}
		if style.reversible != (quant.kind == 0) {
			return tileParameters{}, FormatError("wavelet and quantization mismatch")
		}
		result.styles[c], result.quant[c] = style, quant
		result.roi[c] = main.roi[c]
		if value, ok := tile.header.roi[c]; ok {
			result.roi[c] = value
		}
	}
	if defaults.mct {
		if count < 3 {
			return tileParameters{}, FormatError("MCT requires three components")
		}
		for c := 1; c < 3; c++ {
			a, b := s.info.Components[0], s.info.Components[c]
			if a.Precision != b.Precision || a.XStep != b.XStep || a.YStep != b.YStep || result.styles[0].reversible != result.styles[c].reversible {
				return tileParameters{}, FormatError("MCT component mismatch")
			}
		}
	}
	result.volumes = main.progressions
	if len(tile.header.progressions) > 0 {
		result.volumes = tile.volumes
	}
	if len(result.volumes) == 0 {
		result.volumes = []progressionVolume{{order: defaults.order, componentEnd: count, resolutionEnd: 33, layerEnd: defaults.layers}}
	}
	for _, v := range result.volumes {
		if v.layerEnd > defaults.layers {
			return tileParameters{}, FormatError("POC exceeds quality layers")
		}
	}
	return result, nil
}

// indexProgressions 合并瓦片各分段的渐进范围，供参数查询和后续解码复用
// 入参: budget 元数据预算
// 返回: error 错误信息
func (t *tileIndex) indexProgressions(budget *layoutBudget) error {
	if len(t.header.progressions) == 0 {
		return nil
	}
	count := 0
	for _, part := range t.parts {
		count += len(part.progressions)
	}
	if count == len(t.header.progressions) {
		t.volumes = t.header.progressions
		return nil
	}
	if err := budget.add(uint64(count), 48); err != nil {
		return err
	}
	t.volumes = make([]progressionVolume, 0, count)
	for _, part := range t.parts {
		t.volumes = append(t.volumes, part.progressions...)
	}
	return nil
}

// tileBounds 返回参考网格内裁剪后的瓦片边界
// 入参: index 瓦片索引
// 返回: image.Rectangle 瓦片边界
func (s *streamIndex) tileBounds(index int) image.Rectangle {
	x, y := int64(index%s.columns), int64(index/s.columns)
	x0, y0 := int64(s.tileOrigin.X)+x*int64(s.tileSize[0]), int64(s.tileOrigin.Y)+y*int64(s.tileSize[1])
	b := s.info.Bounds
	return image.Rect(int(max(x0, int64(b.Min.X))), int(max(y0, int64(b.Min.Y))), int(min(x0+int64(s.tileSize[0]), int64(b.Max.X))), int(min(y0+int64(s.tileSize[1]), int64(b.Max.Y))))
}
