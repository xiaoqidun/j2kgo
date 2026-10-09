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
	"image"
)

// 不可逆9/7小波的提升系数及归一化系数
const (
	liftAlpha = -1.586134342059924
	liftBeta  = -0.052980118572961
	liftGamma = 0.882911075530934
	liftDelta = 0.443506852043971
	liftScale = 1.230174104914001
)

// waveletSample 小波变换样本类型
type waveletSample interface {
	int64 | float64
}

// wavelet53 执行单轴5/3提升变换，保留交错顺序
// 入参: values 样本, origin 轴起点, inverse 是否逆变换
func wavelet53(values []int64, origin int, inverse bool) {
	n := len(values)
	if n < 2 {
		if n == 1 && origin&1 != 0 {
			if inverse {
				values[0] >>= 1
			} else {
				values[0] <<= 1
			}
		}
		return
	}
	even := origin & 1
	odd := 1 - even
	if inverse {
		for i := even; i < n; i += 2 {
			left, right := waveletNeighbors(i, n)
			values[i] -= (values[left] + values[right] + 2) >> 2
		}
		for i := odd; i < n; i += 2 {
			left, right := waveletNeighbors(i, n)
			values[i] += (values[left] + values[right]) >> 1
		}
		return
	}
	for i := odd; i < n; i += 2 {
		left, right := waveletNeighbors(i, n)
		values[i] -= (values[left] + values[right]) >> 1
	}
	for i := even; i < n; i += 2 {
		left, right := waveletNeighbors(i, n)
		values[i] += (values[left] + values[right] + 2) >> 2
	}
}

// wavelet97 执行单轴9/7提升变换，保留交错顺序
// 入参: values 样本, origin 轴起点, inverse 是否逆变换
func wavelet97(values []float64, origin int, inverse bool) {
	n := len(values)
	if n < 2 {
		if n == 1 && origin&1 != 0 {
			if inverse {
				values[0] *= 0.5
			} else {
				values[0] *= 2
			}
		}
		return
	}
	even := origin & 1
	odd := 1 - even
	if inverse {
		for i := range values {
			if i&1 == even {
				values[i] *= liftScale
			} else {
				values[i] /= liftScale
			}
		}
		lift97(values, even, -liftDelta)
		lift97(values, odd, -liftGamma)
		lift97(values, even, -liftBeta)
		lift97(values, odd, -liftAlpha)
		return
	}
	lift97(values, odd, liftAlpha)
	lift97(values, even, liftBeta)
	lift97(values, odd, liftGamma)
	lift97(values, even, liftDelta)
	for i := range values {
		if i&1 == even {
			values[i] /= liftScale
		} else {
			values[i] *= liftScale
		}
	}
}

// lift97 执行一轮浮点提升
// 入参: values 样本, parity 待处理下标的奇偶性, factor 提升系数
func lift97(values []float64, parity int, factor float64) {
	for i := parity; i < len(values); i += 2 {
		left, right := waveletNeighbors(i, len(values))
		values[i] += factor * (values[left] + values[right])
	}
}

// waveletNeighbors 计算对称延拓后的相邻样本下标
// 入参: index 当前下标, length 样本数
// 返回: int 左邻下标, int 右邻下标
func waveletNeighbors(index, length int) (int, int) {
	left, right := index-1, index+1
	if left < 0 {
		left = 1
	}
	if right >= length {
		right = length - 2
	}
	return left, right
}

// waveletPack 在高低频交错排列与低频在前的分区排列之间转换
// 入参: values 样本, scratch 等长工作缓冲, origin 轴起点, inverse 是否恢复交错顺序
func waveletPack[T waveletSample](values, scratch []T, origin int, inverse bool) {
	low := 0
	high := len(values)/2 + (len(values)&1)*(1-(origin&1))
	for i := range values {
		index := high
		if (i+origin)&1 == 0 {
			index = low
			low++
		} else {
			high++
		}
		if inverse {
			scratch[i] = values[index]
		} else {
			scratch[index] = values[i]
		}
	}
	copy(values, scratch)
}

// transformWavelet 在调用方缓冲内执行二维多级变换
// 入参: ctx 上下文, data 样本, stride 行跨度, bounds 分量边界, levels 分解级数, inverse 是否逆变换, scratch 工作缓冲, line 单轴变换
// 返回: error 错误信息
func transformWavelet[T waveletSample](ctx context.Context, data []T, stride int, bounds image.Rectangle, levels int, inverse bool, scratch []T, line func([]T, int, bool)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w, h := bounds.Dx(), bounds.Dy()
	if levels < 0 || levels > 32 || w < 0 || h < 0 || stride < w || bounds.Min.X < 0 || bounds.Min.Y < 0 {
		return FormatError("wavelet geometry")
	}
	if w == 0 || h == 0 || levels == 0 {
		return nil
	}
	span, err := checkedProduct("wavelet samples", uint64(h-1), uint64(stride), uint64(len(data)))
	if err != nil {
		return err
	}
	if span+uint64(w) > uint64(len(data)) || len(scratch)/2 < max(w, h) {
		return FormatError("wavelet buffer size")
	}
	var stages [33]image.Rectangle
	stages[0] = bounds
	for i := 1; i <= levels; i++ {
		prev := stages[i-1]
		stages[i] = image.Rect(ceilDiv(prev.Min.X, 2), ceilDiv(prev.Min.Y, 2), ceilDiv(prev.Max.X, 2), ceilDiv(prev.Max.Y, 2))
	}
	for step := 0; step < levels; step++ {
		stage := step
		if inverse {
			stage = levels - 1 - step
		}
		b := stages[stage]
		w, h = b.Dx(), b.Dy()
		if w == 0 || h == 0 {
			continue
		}
		if inverse {
			if err := waveletRows(ctx, data, stride, b, true, scratch, line); err != nil {
				return err
			}
		}
		for x := 0; x < w; x++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			column, work := scratch[:h], scratch[h:2*h]
			for y := range h {
				column[y] = data[y*stride+x]
			}
			if inverse {
				waveletPack(column, work, b.Min.Y, true)
			}
			line(column, b.Min.Y, inverse)
			if !inverse {
				waveletPack(column, work, b.Min.Y, false)
			}
			for y := range h {
				data[y*stride+x] = column[y]
			}
		}
		if !inverse {
			if err := waveletRows(ctx, data, stride, b, false, scratch, line); err != nil {
				return err
			}
		}
	}
	return nil
}

// waveletRows 原位处理当前分解级的全部行
// 入参: ctx 上下文, data 样本, stride 行跨度, bounds 当前级边界, inverse 是否逆变换, scratch 工作缓冲, line 单轴变换
// 返回: error 错误信息
func waveletRows[T waveletSample](ctx context.Context, data []T, stride int, bounds image.Rectangle, inverse bool, scratch []T, line func([]T, int, bool)) error {
	w, h := bounds.Dx(), bounds.Dy()
	for y := range h {
		if err := ctx.Err(); err != nil {
			return err
		}
		row := data[y*stride : y*stride+w]
		if inverse {
			waveletPack(row, scratch[:w], bounds.Min.X, true)
		}
		line(row, bounds.Min.X, inverse)
		if !inverse {
			waveletPack(row, scratch[:w], bounds.Min.X, false)
		}
	}
	return nil
}
