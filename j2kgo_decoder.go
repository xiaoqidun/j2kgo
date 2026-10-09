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
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"sync"
)

// errTilePartEnd 表示当前瓦片分段已读完
var errTilePartEnd = errors.New("j2kgo: end of tile-part")

// Decoder 保存输入及解码索引，输入流由调用方关闭
type Decoder struct {
	mu      sync.Mutex
	source  *inputSource
	index   *streamIndex
	info    Info
	options DecodeOptions
	closed  bool
}

// NewDecoder 检查码流并建立解码索引，不解码图像样本
// Warning仅在建立索引时使用，不由解码器保留
// 入参: ctx 上下文, r 输入流, opts 解码选项
// 返回: *Decoder 解码器, error 错误信息
func NewDecoder(ctx context.Context, r io.Reader, opts *DecodeOptions) (*Decoder, error) {
	options, err := decodeOptions(opts)
	if err != nil {
		return nil, err
	}
	source, err := newInputSource(ctx, r, options.Limits)
	if err != nil {
		return nil, err
	}
	limits := options.Limits
	if source.memory() >= limits.MaxMemoryBytes {
		return nil, &LimitError{Resource: "decoder memory", Limit: limits.MaxMemoryBytes, Required: source.memory() + 1}
	}
	limits.MaxMemoryBytes -= source.memory()
	extent, header, err := locateCodestream(ctx, source, limits)
	if err != nil {
		return nil, err
	}
	if header != nil {
		used := uint64(len(header.bits)) + colorMetadataSize(Info{ICCProfile: header.icc, Channels: header.channels, Mapping: header.mapping, Palette: header.palette})
		if used >= limits.MaxMemoryBytes {
			return nil, &LimitError{Resource: "header memory", Limit: limits.MaxMemoryBytes, Required: used + 1}
		}
		limits.MaxMemoryBytes -= used
	}
	index, err := readCodestream(ctx, source, extent, limits, options.Warning)
	if err != nil {
		return nil, err
	}
	if header != nil {
		index.info, err = header.apply(index.info)
		if err != nil {
			return nil, err
		}
	}
	index.info, err = decodingColor(index.info, options.ColorSpace)
	if err != nil {
		return nil, err
	}
	if options.Reduce > index.minLevels {
		return nil, FormatError("requested resolution reduction exceeds decomposition levels")
	}
	options.Warning = nil
	return &Decoder{source: source, index: index, info: index.info, options: options}, nil
}

// Info 返回完整分辨率的图像信息副本，解码器关闭后返回零值
// 返回: Info 图像信息
func (d *Decoder) Info() Info {
	d.mu.Lock()
	defer d.mu.Unlock()
	return cloneInfo(d.info)
}

// Close 释放解码索引与内部缓存，可重复调用，不关闭调用方输入流
// 返回: error 错误信息
func (d *Decoder) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.source, d.index, d.closed = nil, nil, true
	d.info = Info{}
	return nil
}

// Decode 解码原始分量，保留精度、符号和采样间隔
// 入参: ctx 上下文
// 返回: *Raster 原始图像, error 错误信息
func (d *Decoder) Decode(ctx context.Context) (*Raster, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.decodeRegion(ctx, d.info.Bounds)
}

// DecodeRegion 解码指定区域，输出坐标使用Reduce指定的分辨率
// 为显示保留的相邻样本不会扩大返回图像的边界
// 入参: ctx 上下文, bounds 完整分辨率下的参考网格区域
// 返回: *Raster 区域图像, error 错误信息
func (d *Decoder) DecodeRegion(ctx context.Context, bounds image.Rectangle) (*Raster, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.decodeRegion(ctx, bounds)
}

// decodeRegion 分配区域图像并解码所需瓦片，调用前须持有解码器锁
// 入参: ctx 上下文, bounds 完整分辨率下的参考网格区域
// 返回: *Raster 区域图像, error 错误信息
func (d *Decoder) decodeRegion(ctx context.Context, bounds image.Rectangle) (*Raster, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if d.closed {
		return nil, fmt.Errorf("j2kgo: decoder is closed")
	}
	info := d.info
	bounds = bounds.Intersect(info.Bounds)
	info.Bounds = reduceBounds(bounds, d.options.Reduce)
	info = reduceResolution(info, d.options.Reduce)
	if info.Bounds.Empty() {
		return nil, fmt.Errorf("j2kgo: requested region has no image samples")
	}
	limits := d.options.Limits
	live := d.index.memory + colorMetadataSize(info) + d.source.memory()
	if live >= limits.MaxMemoryBytes {
		return nil, &LimitError{Resource: "decoder memory", Limit: limits.MaxMemoryBytes, Required: live + 1}
	}
	limits.MaxMemoryBytes -= live
	result, err := newRasterExtent(ctx, info, reduceBounds(d.info.Bounds, d.options.Reduce), d.options.Reduce, limits)
	if err != nil {
		return nil, err
	}
	for _, component := range result.components {
		live += uint64(len(component.data))
	}
	live += rasterMetadataSize(info)
	if live >= d.options.Limits.MaxMemoryBytes {
		return nil, &LimitError{Resource: "decoder memory", Limit: d.options.Limits.MaxMemoryBytes, Required: live + 1}
	}
	limits.MaxMemoryBytes = d.options.Limits.MaxMemoryBytes - live
	if err := d.decodeSupportingTiles(ctx, result, limits, -1); err != nil {
		return nil, err
	}
	return result, nil
}

// decodeOptions 校验解码选项并填充默认资源限制
// 入参: opts 解码选项
// 返回: DecodeOptions 已补充默认值的选项, error 错误信息
func decodeOptions(opts *DecodeOptions) (DecodeOptions, error) {
	var options DecodeOptions
	if opts != nil {
		options = *opts
	}
	if options.Reduce < 0 || options.Reduce > 32 || options.MaxLayers < 0 || options.Workers < 0 || (options.ColorSpace != ColorUnknown && options.ColorSpace != ColorGray && options.ColorSpace != ColorSRGB && options.ColorSpace != ColorSYCC) {
		return options, fmt.Errorf("j2kgo: invalid decode options")
	}
	options.Limits = options.Limits.normalized()
	return options, nil
}

// decodingColor 为颜色空间未知的裸码流补充调用方指定的颜色空间
// 入参: info 图像信息, space 调用方颜色空间
// 返回: Info 图像信息, error 错误信息
func decodingColor(info Info, space ColorSpace) (Info, error) {
	if space == ColorUnknown {
		return info, nil
	}
	if len(info.ICCProfile) != 0 || (info.ColorSpace != ColorUnknown && info.ColorSpace != space) {
		return Info{}, FormatError("color option conflicts with image specification")
	}
	info.ColorSpace = space
	if err := validateColorChannels(info); err != nil {
		return Info{}, err
	}
	return info, nil
}

// locateCodestream 定位裸码流或JP2容器内的首个码流
// 入参: ctx 上下文, source 输入源, limits 资源限制
// 返回: inputRange 码流范围, *jp2Header 容器头, error 错误信息
func locateCodestream(ctx context.Context, source *inputSource, limits Limits) (inputRange, *jp2Header, error) {
	h := &headerReader{ctx: ctx, r: source.reader(ctx), limits: limits}
	var signature [12]byte
	if err := h.full(signature[:2]); err != nil {
		return inputRange{}, nil, err
	}
	if signature[0] == 255 && signature[1] == 0x4f {
		return inputRange{length: source.size}, nil, nil
	}
	if err := h.full(signature[2:]); err != nil {
		return inputRange{}, nil, err
	}
	if string(signature[:]) != "\x00\x00\x00\x0cjP  \r\n\x87\n" {
		return inputRange{}, nil, FormatError("signature")
	}
	var header *jp2Header
	for index := 0; h.read < uint64(source.size); index++ {
		kind, end, err := h.box(uint64(source.size))
		if err != nil {
			return inputRange{}, nil, err
		}
		if index == 0 && kind != "ftyp" {
			return inputRange{}, nil, FormatError("missing file type box")
		}
		switch kind {
		case "ftyp":
			if index != 0 {
				return inputRange{}, nil, FormatError("duplicate file type box")
			}
			if err := h.fileType(end); err != nil {
				return inputRange{}, nil, err
			}
		case "jp2h":
			if header != nil {
				return inputRange{}, nil, FormatError("duplicate JP2 header")
			}
			header, err = h.containerHeader(end)
			if err != nil {
				return inputRange{}, nil, err
			}
		case "jp2c":
			if header == nil {
				return inputRange{}, nil, FormatError("missing JP2 header")
			}
			return inputRange{offset: int64(h.read), length: int64(end - h.read)}, header, nil
		default:
			if err := h.skip(end - h.read); err != nil {
				return inputRange{}, nil, err
			}
		}
	}
	return inputRange{}, nil, FormatError("missing codestream box")
}

// decodeTile 解码一个瓦片并将重建样本写入原始分量
// 入参: ctx 上下文, tile 瓦片索引, result 目标图像, limits 可用资源限制
// 返回: error 错误信息
func (d *Decoder) decodeTile(ctx context.Context, tile int, result *Raster, limits Limits) error {
	prepared, err := d.prepareTile(ctx, tile, result.supportBounds(), limits)
	if err != nil {
		return err
	}
	if prepared.memory >= limits.MaxMemoryBytes {
		return &LimitError{Resource: "tile memory", Limit: limits.MaxMemoryBytes, Required: prepared.memory + 1}
	}
	limits.MaxMemoryBytes -= prepared.memory
	return d.reconstructTile(ctx, prepared, result, limits)
}

// prepareTile 建立可复用的瓦片解码索引，不分配整幅图像样本
// 入参: ctx 上下文, tile 瓦片索引, region 降低分辨率后的参考网格区域, limits 资源限制
// 返回: *tileDecoding 瓦片解码状态, error 错误信息
func (d *Decoder) prepareTile(ctx context.Context, tile int, region image.Rectangle, limits Limits) (*tileDecoding, error) {
	budget := layoutBudget{limit: limits.MaxMemoryBytes}
	if err := budget.add(uint64(len(d.info.Components)), 256); err != nil {
		return nil, err
	}
	if err := budget.add(1, 48); err != nil {
		return nil, err
	}
	params, err := d.index.parameters(tile)
	if err != nil {
		return nil, err
	}
	layouts := make([]componentLayout, len(params.styles))
	bounds := d.index.tileBounds(tile)
	for c, style := range params.styles {
		layouts[c], err = makeComponentLayout(ctx, bounds, d.info.Components[c], style, params.quant[c], &budget)
		if err != nil {
			return nil, err
		}
		layouts[c].roi = params.roi[c]
		for r := range layouts[c].resolutions {
			for p := range layouts[c].resolutions[r].precincts {
				for b := range layouts[c].resolutions[r].precincts[p].bands {
					band := &layouts[c].resolutions[r].precincts[p].bands[b]
					band.maxPlanes += int(params.roi[c])
				}
			}
		}
		selectRegionBlocks(&layouts[c], componentBounds(region, d.info.Components[c]), d.options.Reduce)
	}
	if err := d.readTilePackets(ctx, tile, params, layouts, limits, &budget); err != nil {
		return nil, err
	}
	return &tileDecoding{params: params, layouts: layouts, memory: budget.used}, nil
}

// reconstructTile 根据瓦片索引重建目标分量，并执行分量逆变换
// 入参: ctx 上下文, tile 瓦片解码状态, result 目标图像, limits 可用资源限制
// 返回: error 错误信息
func (d *Decoder) reconstructTile(ctx context.Context, tile *tileDecoding, result *Raster, limits Limits) error {
	params, layouts := tile.params, tile.layouts
	var err error
	for first := 0; first < len(layouts); {
		count := 1
		if first == 0 && params.defaults.mct {
			count = 3
		}
		var samples [3]tileSamples
		remaining := limits
		window := result.components[first].storage
		for c := 1; c < count; c++ {
			window = window.Union(result.components[first+c].storage)
		}
		for c := range count {
			layout := layouts[first+c]
			layout.bounds = reduceBounds(layout.bounds, d.options.Reduce)
			layout.coding.levels -= d.options.Reduce
			layout.resolutions = layout.resolutions[:layout.coding.levels+1]
			area := layout.bounds.Intersect(window)
			if area == layout.bounds {
				samples[c], err = reconstructComponent(ctx, d.source, &layout, params.quant[first+c], d.info.Components[first+c].Precision, remaining, d.options.Workers)
			} else {
				samples[c], err = reconstructRegion(ctx, d.source, &layout, params.quant[first+c], d.info.Components[first+c].Precision, area, remaining, d.options.Workers)
			}
			if err != nil {
				return err
			}
			used := uint64(len(samples[c].integers)+len(samples[c].floats)) * 8
			if used >= remaining.MaxMemoryBytes {
				return &LimitError{Resource: "tile samples", Limit: remaining.MaxMemoryBytes, Required: used + 1}
			}
			remaining.MaxMemoryBytes -= used
		}
		if count == 3 {
			if params.styles[0].reversible {
				err = transformRCT(ctx, [3][]int64{samples[0].integers, samples[1].integers, samples[2].integers}, true)
			} else {
				err = transformICT(ctx, [3][]float64{samples[0].floats, samples[1].floats, samples[2].floats}, true)
			}
			if err != nil {
				return err
			}
		}
		for c := range count {
			if err := storeTileSamples(ctx, &result.components[first+c], samples[c]); err != nil {
				return err
			}
		}
		first += count
	}
	return nil
}

// readTilePackets 顺序读取各瓦片分段的数据包并保留跨段状态
// 入参: ctx 上下文, tile 瓦片索引, params 有效参数, layouts 分量分区, limits 资源限制, budget 累计预算
// 返回: error 错误信息
func (d *Decoder) readTilePackets(ctx context.Context, tile int, params tileParameters, layouts []componentLayout, limits Limits, budget *layoutBudget) error {
	ppm := len(d.index.main.packed) > 0
	tileParts := d.index.tiles[tile].parts
	packed := ppm
	for _, part := range tileParts {
		packed = packed || len(part.packed) > 0
	}
	headers := &packedHeaders{ctx: ctx, source: d.source}
	if !ppm {
		defer func() { budget.used -= uint64(cap(headers.ranges)) * 16 }()
	}
	sequence := 0
	var lengths packetLengthReader
	var mainCache, tileCache, mergeCache *packetLengthBuffer
	for _, part := range tileParts {
		if part.plm != nil || len(part.plt) > 0 {
			if err := budget.add(1, 1024); err != nil {
				return err
			}
			defer func() { budget.used -= 1024 }()
			mainCache, tileCache, mergeCache = &packetLengthBuffer{}, &packetLengthBuffer{}, &packetLengthBuffer{}
			lengths.buffer = tileCache
			break
		}
	}
	volumeCount := len(params.volumes)
	tilePOC := len(d.index.tiles[tile].header.progressions) > 0
	if tilePOC {
		volumeCount = 0
	}
	for partIndex, part := range tileParts {
		last := partIndex == len(tileParts)-1
		plm := packetLengthReader{ranges: part.plm, buffer: mainCache}
		if len(part.plt) > 0 {
			var err error
			lengths, err = mergePacketLengths(ctx, d.source, lengths, packetLengthReader{ranges: part.plt, buffer: mergeCache})
			if err != nil {
				return err
			}
			if lengths.buffer == mergeCache {
				tileCache, mergeCache = mergeCache, tileCache
			}
		}
		if tilePOC {
			volumeCount += len(part.progressions)
		}
		if ppm {
			headers = &packedHeaders{ctx: ctx, source: d.source, ranges: part.headers}
			for _, span := range part.headers {
				headers.remaining += span.length
			}
		} else if len(part.packed) > 0 {
			ranges, err := orderedPacked(part.packed, budget)
			if err != nil {
				return err
			}
			err = headers.append(ranges, budget)
			budget.used -= uint64(len(ranges)) * 16
			if err != nil {
				return err
			}
		}
		position, end := part.data.offset, part.data.offset+part.data.length
		trailing := int64(-1)
		for _, volume := range params.volumes[:volumeCount] {
			err := walkPacketVolumeBudget(ctx, layouts, volume, limits, budget, func(address packetAddress) error {
				if (!packed && position == end) || (packed && headers.remaining == 0) {
					return errTilePartEnd
				}
				if packed && !ppm && position == end && !last && (part.plm == nil || plm.empty()) {
					if part.plm != nil || lengths.empty() {
						return errTilePartEnd
					}
					if trailing < 0 {
						var err error
						trailing, err = headers.trailingEmptyPackets(lengths, tileParts[partIndex+1:], params.defaults.eph, budget, func(current packedHeaders, future []*tilePart, limit int64) (int64, error) {
							return zeroPacketPrefix(ctx, current, future, layouts, params, limits, budget, limit)
						})
						if err != nil {
							return err
						}
					}
					if trailing == 0 {
						return errTilePartEnd
					}
					trailing--
				}
				start, missingSOP := position, int64(0)
				var marker [6]byte
				if last && packed && params.defaults.sop && end-position == 1 {
					if _, err := d.source.readAt(ctx, marker[:1], position); err != nil {
						return err
					}
					if marker[0] == 255 {
						position, missingSOP = end, 5
					}
				}
				if end-position >= 2 {
					if _, err := d.source.readAt(ctx, marker[:2], position); err != nil {
						return err
					}
					if binary.BigEndian.Uint16(marker[:2]) == markerSOP {
						if !params.defaults.sop {
							return FormatError("unexpected SOP")
						}
						if end-position < 6 {
							if !last {
								return FormatError("incomplete SOP")
							}
							available := int(end - position)
							if _, err := d.source.readAt(ctx, marker[:available], position); err != nil {
								return err
							}
							if (available >= 3 && marker[2] != 0) || (available >= 4 && marker[3] != 4) || (available >= 5 && marker[4] != byte(sequence>>8)) {
								return FormatError("SOP sequence or length")
							}
							position = end
							if !packed {
								if err := checkPacketLengths(ctx, d.source, &plm, &lengths, part.plm != nil, position-start, -1, false); err != nil {
									return err
								}
								return errTilePartEnd
							}
							missingSOP = int64(6 - available)
						} else {
							if _, err := d.source.readAt(ctx, marker[:], position); err != nil {
								return err
							}
							if binary.BigEndian.Uint16(marker[2:]) != 4 || int(binary.BigEndian.Uint16(marker[4:])) != sequence&65535 {
								return FormatError("SOP sequence or length")
							}
							position += 6
						}
					}
				}
				input := &packetByteReader{ctx: ctx, source: d.source, position: position, end: end}
				var headerInput io.ByteReader = input
				if packed {
					if err := headers.beginPacket(); err != nil {
						return err
					}
					headerInput = headers
				}
				r := packetBits{source: headerInput}
				parts, err := readPacketHeader(ctx, &r, address.area.bands, address.layer, params.styles[address.component].style)
				if err != nil {
					if last && packetHeaderTruncated(err, input, headers, packed) {
						position = end
						if err := checkPacketLengths(ctx, d.source, &plm, &lengths, part.plm != nil, position-start, -1, false); err != nil {
							return err
						}
						return errTilePartEnd
					}
					return err
				}
				defer releasePacketContributions(parts)
				if params.defaults.eph {
					for i := range 2 {
						marker[i], err = headerInput.ReadByte()
						if err != nil {
							if last && packetHeaderTruncated(err, input, headers, packed) {
								position = end
								if err := checkPacketLengths(ctx, d.source, &plm, &lengths, part.plm != nil, position-start, -1, false); err != nil {
									return err
								}
								return errTilePartEnd
							}
							return err
						}
						if marker[i] != byte(uint16(markerEPH)>>uint(8*(1-i))) {
							return FormatError("missing EPH")
						}
					}
				}
				if !packed {
					position = input.position
				}
				hasLengths := part.plm != nil || !lengths.empty()
				declared := position - start + missingSOP
				if hasLengths {
					for _, contribution := range parts {
						if int64(contribution.length) > math.MaxInt64-declared {
							return FormatError("packet length overflow")
						}
						declared += int64(contribution.length)
					}
				}
				retain := (d.options.MaxLayers == 0 || address.layer < d.options.MaxLayers) && address.resolution <= params.styles[address.component].levels-d.options.Reduce
				position, err = locatePacketPrefix(parts, position, end, retain, last)
				if err != nil {
					return err
				}
				sequence++
				if !hasLengths {
					return nil
				}
				optionalSOP := last && packed && params.defaults.sop && start == end
				return checkPacketLengths(ctx, d.source, &plm, &lengths, part.plm != nil, position-start, declared, optionalSOP)
			})
			if errors.Is(err, errTilePartEnd) {
				break
			}
			if err != nil {
				return err
			}
		}
		if position != end || (ppm && headers.remaining != 0) {
			return FormatError("unconsumed tile-part data")
		}
		if !plm.empty() {
			return FormatError("unconsumed PLM packet lengths")
		}
	}
	if headers.remaining != 0 {
		return FormatError("unconsumed PPT packet headers")
	}
	if !lengths.empty() {
		return FormatError("unconsumed PLT packet lengths")
	}
	return nil
}

// packetHeaderTruncated 判断包头是否因超出声明范围而被截断
// 入参: err 包头错误, input 包内读取器, headers 集中包头, packed 是否使用集中包头
// 返回: bool 是否在声明边界处截断
func packetHeaderTruncated(err error, input *packetByteReader, headers *packedHeaders, packed bool) bool {
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false
	}
	if packed {
		return headers.remaining == 0
	}
	return input.position == input.end
}

// packetByteReader 按小块缓存数据包头，避免对文件逐字节读取
type packetByteReader struct {
	ctx           context.Context
	source        *inputSource
	position, end int64
	buffer        [256]byte
	start, count  int
}

// ReadByte 读取包头字节，不越过瓦片分段
// 返回: byte 字节, error 错误信息
func (r *packetByteReader) ReadByte() (byte, error) {
	if r.position >= r.end {
		return 0, io.EOF
	}
	if r.start == r.count {
		n := int(min(int64(len(r.buffer)), r.end-r.position))
		read, err := r.source.readAt(r.ctx, r.buffer[:n], r.position)
		if err != nil {
			return 0, err
		}
		r.start, r.count = 0, read
	}
	value := r.buffer[r.start]
	r.start++
	r.position++
	return value, nil
}

// storeTileSamples 恢复直流偏移，将重建样本限制在有效值域内并写入目标分量
// 入参: ctx 上下文, target 目标分量, source 重建样本
// 返回: error 错误信息
func storeTileSamples(ctx context.Context, target *Component, source tileSamples) error {
	bounds := source.bounds.Intersect(target.storage)
	lo, hi := sampleRange(target.info)
	offset := int64(0)
	if !target.info.Signed {
		offset = int64(1) << (target.info.Precision - 1)
	}
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			index := (y-source.bounds.Min.Y)*source.bounds.Dx() + x - source.bounds.Min.X
			var value int64
			if source.integers != nil {
				value = max(lo-offset, min(hi-offset, source.integers[index])) + offset
			} else {
				v := source.floats[index]
				if math.IsNaN(v) || math.IsInf(v, 0) {
					return FormatError("nonfinite reconstructed sample")
				}
				value = int64(math.Floor(max(float64(lo), min(float64(hi), v+float64(offset))) + 0.5))
			}
			target.setSample(x, y, value)
		}
	}
	return nil
}
