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

// Package j2kgo 高性能、零依赖、纯 Go 语言 J2K 图像编解码库
package j2kgo

import "image"

// Format 图像封装格式
type Format uint8

const (
	FormatJP2 Format = iota // JP2封装
	FormatJ2K               // JPEG2000裸码流
)

// Progression 数据包渐进顺序
type Progression uint8

const (
	LRCP Progression = iota // 质量层、分辨率、分量、位置
	RLCP                    // 分辨率、质量层、分量、位置
	RPCL                    // 分辨率、位置、分量、质量层
	PCRL                    // 位置、分量、分辨率、质量层
	CPRL                    // 分量、位置、分辨率、质量层
)

// Wavelet 小波变换，零值使用可逆5/3变换
type Wavelet uint8

const (
	Wavelet53 Wavelet = iota // 可逆5/3变换
	Wavelet97                // 不可逆9/7变换
)

// CodeBlockStyle 码块编码方式，可按位组合，零值使用默认算术编码
type CodeBlockStyle uint8

const (
	CodeBlockBypass       CodeBlockStyle = 1 << iota // 选择性算术编码旁路
	CodeBlockReset                                   // 编码遍结束后重置上下文
	CodeBlockTerminate                               // 每个编码遍独立终止
	CodeBlockCausal                                  // 垂直因果上下文
	CodeBlockPredictable                             // 可预测终止
	CodeBlockSegmentation                            // 清理遍分段符号
)

// QuantizationStyle 子带量化方式
type QuantizationStyle uint8

const (
	QuantizationNone      QuantizationStyle = iota // 不量化
	QuantizationDerived                            // 由LL子带步长派生
	QuantizationExpounded                          // 各子带独立指定步长
)

// Quantization 指定量化方式、保护位数及子带参数
// Steps以LL开头，其余子带按分辨率从低到高排列HL、LH、HH；派生量化仅指定LL
type Quantization struct {
	Style     QuantizationStyle
	GuardBits uint8
	Steps     []QuantizationStep
}

// QuantizationStep 表示量化步长的指数与尾数
// Exponent取值0至31，Mantissa取值0至2047；无量化时Mantissa须为零
type QuantizationStep struct {
	Exponent uint8
	Mantissa uint16
}

// ColorSpace 图像颜色空间，零值表示未知
type ColorSpace uint32

const (
	ColorUnknown ColorSpace = 0  // 未指定颜色空间
	ColorSRGB    ColorSpace = 16 // sRGB
	ColorGray    ColorSpace = 17 // 灰度
	ColorSYCC    ColorSpace = 18 // sYCC
)

// ColorTransform 编码时使用的分量变换，作用于前三个分量，零值自动选择
type ColorTransform uint8

const (
	ColorTransformAuto         ColorTransform = iota // 按小波类型和分量布局自动选择
	ColorTransformNone                               // 不进行分量变换
	ColorTransformReversible                         // 可逆分量变换
	ColorTransformIrreversible                       // 不可逆分量变换
)

// DecodeOptions 解码选项，零值使用完整分辨率、全部质量层及默认资源限制
// Workers限制并发数，零值自动选择，内存不足时降低并发；ColorSpace仅指定裸码流的颜色空间
// 设置Warning后允许修正偏小的瓦片分段总数，并通过回调报告
type DecodeOptions struct {
	Reduce     int
	MaxLayers  int
	Workers    int
	Limits     Limits
	ColorSpace ColorSpace
	Warning    func(error)
}

// EncodeOptions 编码选项，零值输出无损JP2，分解级数为nil时自动选择
// TileOrigin指定瓦片网格起点，nil时使用图像起点，首个瓦片须覆盖图像起点
// TilePartPackets限制每个瓦片分段的数据包数，零值不分段；Quantization为nil时自动生成量化参数
// ROI指定输出参考网格中的优先编码区域，超出图像的部分不参与编码
// SOP在每个数据包前写入起始标记，EPH在每个数据包头后写入结束标记
// TLM、PLM分别在主头中写入瓦片分段长度表和数据包长度表，均需预编码以统计长度
// PLM要求每个瓦片分段的包长编码不超过255字节；PLT在瓦片分段头中写入数据包长度表
// PPT将数据包头集中写入所属瓦片分段头；PPM将全部数据包头集中写入主头，需预编码且不可与PPT同时启用
type EncodeOptions struct {
	Format              Format
	Wavelet             Wavelet
	DecompositionLevels *int
	TileSize            image.Point
	TileOrigin          *image.Point
	TilePartPackets     int
	CodeBlockSize       image.Point
	CodeBlockStyle      CodeBlockStyle
	Quantization        *Quantization
	Components          []ComponentOptions
	Tiles               []TileOptions
	PrecinctSizes       []image.Point
	Progression         Progression
	ProgressionChanges  []ProgressionChange
	ColorTransform      ColorTransform
	ROI                 []image.Rectangle
	Layers              []Layer
	SOP                 bool
	EPH                 bool
	TLM                 bool
	PLM                 bool
	PLT                 bool
	PPT                 bool
	PPM                 bool
	Limits              Limits
}

// ComponentOptions 指定单个分量的编码参数，未设置的选项沿用所在图像或瓦片的默认参数
// 分解级数与默认值不同时，若未指定PrecinctSizes，则使用32768×32768
// 默认量化参数不足以覆盖该分量的子带时，须单独设置Quantization
type ComponentOptions struct {
	Index               int
	Wavelet             *Wavelet
	DecompositionLevels *int
	CodeBlockSize       image.Point
	CodeBlockStyle      *CodeBlockStyle
	PrecinctSizes       []image.Point
	Quantization        *Quantization
}

// TileOptions 指定单个瓦片的编码参数，Index按瓦片网格从左到右、从上到下编号
// 指针为nil或尺寸为零时沿用全局选项；分解级数改变且未指定区域尺寸时使用32768×32768
// Components、ProgressionChanges、ROI和Layers为nil时沿用全局列表，非nil时替换整个列表
// 任一瓦片单独指定Layers时，各瓦片按自身像素数计算字节预算，公共头部开销按面积分摊
type TileOptions struct {
	Index               int
	Wavelet             *Wavelet
	DecompositionLevels *int
	CodeBlockSize       image.Point
	CodeBlockStyle      *CodeBlockStyle
	Quantization        *Quantization
	Components          []ComponentOptions
	PrecinctSizes       []image.Point
	Progression         *Progression
	ProgressionChanges  []ProgressionChange
	ColorTransform      *ColorTransform
	ROI                 []image.Rectangle
	Layers              []Layer
	TilePartPackets     *int
	SOP                 *bool
	EPH                 *bool
	PLT                 *bool
	PPT                 *bool
}

// ProgressionChange 指定数据包的渐进顺序和范围
// 范围包含起点、不包含终点，终点为零时使用对应上限
// 各范围按顺序处理，跳过已输出的数据包，所有范围须共同覆盖全部数据包
type ProgressionChange struct {
	Order           Progression
	ComponentStart  int
	ComponentEnd    int
	ResolutionStart int
	ResolutionEnd   int
	LayerEnd        int
}

// Layer 指定质量层的累计目标码率，各层码率递增，末层为零时保留全部编码遍
// BitsPerPixel按参考网格计算总码率，包含容器及头部开销，逐瓦片分配预算
// 使用不可逆变换时，即使保留全部编码遍也不能保证无损
// 启用ROI且未指定质量层时自动分层；末层码率为零时，层数须足以容纳全部编码遍
type Layer struct {
	BitsPerPixel float64
}
