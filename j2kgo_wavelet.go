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
	"runtime"
	"sync"
)

// 不可逆9/7小波的提升系数及归一化系数
const (
	liftAlpha = -1.586134342059924
	liftBeta  = -0.052980118572961
	liftGamma = 0.882911075530934
	liftDelta = 0.443506852043971
	liftScale = 1.230174104914001
)

// 小波垂直列分块宽度及启用并行的最小样本数
const (
	waveletColStrip        = 8
	waveletParallelSamples = 16384
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
		update53(values, even, true)
		predict53(values, odd, true)
		return
	}
	predict53(values, odd, false)
	update53(values, even, false)
}

// update53 执行5/3小波更新步骤
// 入参: values 样本, parity 待处理下标的奇偶性, inverse 是否逆变换
func update53(values []int64, parity int, inverse bool) {
	last := len(values) - 1
	if inverse {
		if parity == 0 {
			values[0] -= (values[1] + values[1] + 2) >> 2
			for i := 2; i < last; i += 2 {
				values[i] -= (values[i-1] + values[i+1] + 2) >> 2
			}
			if last&1 == 0 {
				values[last] -= (values[last-1] + values[last-1] + 2) >> 2
			}
			return
		}
		for i := 1; i < last; i += 2 {
			values[i] -= (values[i-1] + values[i+1] + 2) >> 2
		}
		if last&1 == 1 {
			values[last] -= (values[last-1] + values[last-1] + 2) >> 2
		}
		return
	}
	if parity == 0 {
		values[0] += (values[1] + values[1] + 2) >> 2
		for i := 2; i < last; i += 2 {
			values[i] += (values[i-1] + values[i+1] + 2) >> 2
		}
		if last&1 == 0 {
			values[last] += (values[last-1] + values[last-1] + 2) >> 2
		}
		return
	}
	for i := 1; i < last; i += 2 {
		values[i] += (values[i-1] + values[i+1] + 2) >> 2
	}
	if last&1 == 1 {
		values[last] += (values[last-1] + values[last-1] + 2) >> 2
	}
}

// predict53 执行5/3小波预测步骤
// 入参: values 样本, parity 待处理下标的奇偶性, inverse 是否逆变换
func predict53(values []int64, parity int, inverse bool) {
	last := len(values) - 1
	if inverse {
		if parity == 0 {
			values[0] += (values[1] + values[1]) >> 1
			for i := 2; i < last; i += 2 {
				values[i] += (values[i-1] + values[i+1]) >> 1
			}
			if last&1 == 0 {
				values[last] += (values[last-1] + values[last-1]) >> 1
			}
			return
		}
		for i := 1; i < last; i += 2 {
			values[i] += (values[i-1] + values[i+1]) >> 1
		}
		if last&1 == 1 {
			values[last] += (values[last-1] + values[last-1]) >> 1
		}
		return
	}
	if parity == 0 {
		values[0] -= (values[1] + values[1]) >> 1
		for i := 2; i < last; i += 2 {
			values[i] -= (values[i-1] + values[i+1]) >> 1
		}
		if last&1 == 0 {
			values[last] -= (values[last-1] + values[last-1]) >> 1
		}
		return
	}
	for i := 1; i < last; i += 2 {
		values[i] -= (values[i-1] + values[i+1]) >> 1
	}
	if last&1 == 1 {
		values[last] -= (values[last-1] + values[last-1]) >> 1
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
		for i := even; i < n; i += 2 {
			values[i] *= liftScale
		}
		for i := odd; i < n; i += 2 {
			values[i] /= liftScale
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
	for i := even; i < n; i += 2 {
		values[i] /= liftScale
	}
	for i := odd; i < n; i += 2 {
		values[i] *= liftScale
	}
}

// lift97 执行一轮浮点提升
// 入参: values 样本, parity 待处理下标的奇偶性, factor 提升系数
func lift97(values []float64, parity int, factor float64) {
	last := len(values) - 1
	if parity == 0 {
		values[0] += factor * (values[1] + values[1])
		for i := 2; i < last; i += 2 {
			values[i] += factor * (values[i-1] + values[i+1])
		}
		if last&1 == 0 {
			values[last] += factor * (values[last-1] + values[last-1])
		}
		return
	}
	for i := 1; i < last; i += 2 {
		values[i] += factor * (values[i-1] + values[i+1])
	}
	if last&1 == 1 {
		values[last] += factor * (values[last-1] + values[last-1])
	}
}

// waveletUnpack 将低频在前的分区排列解交错写入目标缓冲
// 入参: dst 交错输出缓冲, src 分区输入样本, origin 轴起点
func waveletUnpack[T waveletSample](dst, src []T, origin int) {
	n := len(src)
	even := origin & 1
	lowCount := (n + 1 - even) / 2
	highCount := n - lowCount
	lo, hi := src[:lowCount], src[lowCount:]
	if even == 0 {
		for j := range highCount {
			dst[2*j] = lo[j]
			dst[2*j+1] = hi[j]
		}
		if lowCount > highCount {
			dst[n-1] = lo[highCount]
		}
		return
	}
	for j := range lowCount {
		dst[2*j] = hi[j]
		dst[2*j+1] = lo[j]
	}
	if highCount > lowCount {
		dst[n-1] = hi[lowCount]
	}
}

// waveletRepack 将交错排列按低频在前写入目标缓冲
// 入参: dst 分区输出缓冲, src 交错输入样本, origin 轴起点
func waveletRepack[T waveletSample](dst, src []T, origin int) {
	n := len(src)
	even := origin & 1
	lowCount := (n + 1 - even) / 2
	highCount := n - lowCount
	lo, hi := dst[:lowCount], dst[lowCount:]
	if even == 0 {
		for j := range highCount {
			lo[j] = src[2*j]
			hi[j] = src[2*j+1]
		}
		if lowCount > highCount {
			lo[highCount] = src[n-1]
		}
		return
	}
	for j := range lowCount {
		hi[j] = src[2*j]
		lo[j] = src[2*j+1]
	}
	if highCount > lowCount {
		hi[lowCount] = src[n-1]
	}
}

// transformWavelet 在调用方缓冲内执行二维多级变换
// 入参: ctx 上下文, data 样本, stride 行跨度, bounds 分量边界, levels 分解级数, inverse 是否逆变换, scratch 工作缓冲, line 单轴变换
// 返回: error 错误信息
func transformWavelet[T waveletSample](ctx context.Context, data []T, stride int, bounds image.Rectangle, levels int, inverse bool, scratch []T, line func([]T, int, bool)) error {
	return transformWaveletPlan(ctx, data, stride, bounds, levels, inverse, scratch, 0, 1, line)
}

// transformWaveletWorkers 在内存预算内使用列分块和并发协程执行二维多级变换
// 入参: ctx 上下文, data 样本, stride 行跨度, bounds 分量边界, levels 分解级数, inverse 是否逆变换, budget 可用工作内存字节数, workers 并发上限, line 单轴变换
// 返回: error 错误信息
func transformWaveletWorkers[T waveletSample](ctx context.Context, data []T, stride int, bounds image.Rectangle, levels int, inverse bool, budget uint64, workers int, line func([]T, int, bool)) error {
	return transformWaveletPlan(ctx, data, stride, bounds, levels, inverse, nil, budget, workers, line)
}

// transformWaveletPlan 校验几何参数并在预算内执行二维多级小波变换
// 入参: ctx 上下文, data 样本, stride 行跨度, bounds 分量边界, levels 分解级数, inverse 是否逆变换, scratch 可选预分配缓冲, budget 额外可用字节数, workers 并发上限, line 单轴变换
// 返回: error 错误信息
func transformWaveletPlan[T waveletSample](ctx context.Context, data []T, stride int, bounds image.Rectangle, levels int, inverse bool, scratch []T, budget uint64, workers int, line func([]T, int, bool)) error {
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
	if span+uint64(w) > uint64(len(data)) {
		return FormatError("wavelet buffer size")
	}
	if scratch != nil {
		if len(scratch)/2 < max(w, h) {
			return FormatError("wavelet buffer size")
		}
		budget = uint64(len(scratch)) * 8
	} else if budget/16 < uint64(max(w, h)) {
		return FormatError("wavelet buffer size")
	}
	nWorkers := 1
	if workers != 1 && uint64(w)*uint64(h) >= waveletParallelSamples {
		nWorkers = runtime.GOMAXPROCS(0)
		if workers > 0 {
			nWorkers = min(nWorkers, workers)
		}
	}
	strip := min(waveletColStrip, w)
	perWorker := max(w, strip*h)
	if nWorkers > 1 && uint64(nWorkers)*uint64(perWorker)*8 > budget {
		nWorkers = max(1, int(budget/(uint64(perWorker)*8)))
	}
	if nWorkers == 1 && uint64(perWorker)*8 > budget {
		strip = max(1, int((budget/8)/uint64(h)))
		perWorker = max(w, strip*h)
	}
	workBuf := scratch
	if nWorkers > 1 || len(workBuf) < perWorker {
		workBuf = make([]T, nWorkers*perWorker)
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
		cw, ch := b.Dx(), b.Dy()
		if cw == 0 || ch == 0 {
			continue
		}
		stageWorkers := 1
		if nWorkers > 1 && uint64(cw)*uint64(ch) >= waveletParallelSamples {
			stageWorkers = nWorkers
		}
		if inverse {
			if err := waveletRows(ctx, data, stride, b, true, workBuf, perWorker, stageWorkers, line); err != nil {
				return err
			}
		}
		if err := waveletCols(ctx, data, stride, b, inverse, workBuf, perWorker, strip, stageWorkers, line); err != nil {
			return err
		}
		if !inverse {
			if err := waveletRows(ctx, data, stride, b, false, workBuf, perWorker, stageWorkers, line); err != nil {
				return err
			}
		}
	}
	return nil
}

// waveletRows 原位处理当前分解级的全部行
// 入参: ctx 上下文, data 样本, stride 行跨度, bounds 当前级边界, inverse 是否逆变换, workBuf 工作缓冲, perWorker 单协程缓冲长度, workers 并发数, line 单轴变换
// 返回: error 错误信息
func waveletRows[T waveletSample](ctx context.Context, data []T, stride int, bounds image.Rectangle, inverse bool, workBuf []T, perWorker, workers int, line func([]T, int, bool)) error {
	w, h := bounds.Dx(), bounds.Dy()
	if w == 1 && bounds.Min.X&1 == 0 {
		return ctx.Err()
	}
	active := min(workers, max(1, h/4))
	return runWaveletWorkers(ctx, active, h, func(wctx context.Context, id, start, end int) error {
		scratch := workBuf[id*perWorker : id*perWorker+w]
		for y := start; y < end; y++ {
			if (y-start)&31 == 0 {
				if err := wctx.Err(); err != nil {
					return err
				}
			}
			row := data[y*stride : y*stride+w]
			if w == 1 {
				line(row, bounds.Min.X, inverse)
				continue
			}
			if inverse {
				waveletUnpack(scratch, row, bounds.Min.X)
				line(scratch, bounds.Min.X, true)
				copy(row, scratch)
				continue
			}
			line(row, bounds.Min.X, false)
			waveletRepack(scratch, row, bounds.Min.X)
			copy(row, scratch)
		}
		return wctx.Err()
	})
}

// waveletCols 按列分块转置执行当前分解级的垂直变换
// 入参: ctx 上下文, data 样本, stride 行跨度, bounds 当前级边界, inverse 是否逆变换, workBuf 工作缓冲, perWorker 单协程缓冲长度, strip 分块列数, workers 并发数, line 单轴变换
// 返回: error 错误信息
func waveletCols[T waveletSample](ctx context.Context, data []T, stride int, bounds image.Rectangle, inverse bool, workBuf []T, perWorker, strip, workers int, line func([]T, int, bool)) error {
	w, h := bounds.Dx(), bounds.Dy()
	if h == 1 {
		if bounds.Min.Y&1 == 0 {
			return ctx.Err()
		}
		for x := range w {
			if x&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			line(data[x:x+1], bounds.Min.Y, inverse)
		}
		return ctx.Err()
	}
	blocks := (w + strip - 1) / strip
	even := bounds.Min.Y & 1
	odd := 1 - even
	lowCount := (h + 1 - even) / 2
	return runWaveletWorkers(ctx, min(workers, blocks), blocks, func(wctx context.Context, id, start, end int) error {
		block := workBuf[id*perWorker : id*perWorker+strip*h]
		for blk := start; blk < end; blk++ {
			if err := wctx.Err(); err != nil {
				return err
			}
			x0 := blk * strip
			cols := min(strip, w-x0)
			if inverse {
				for y := range lowCount {
					dstY := 2*y + even
					row := data[y*stride+x0 : y*stride+x0+cols]
					for c := range cols {
						block[c*h+dstY] = row[c]
					}
				}
				for y := lowCount; y < h; y++ {
					dstY := 2*(y-lowCount) + odd
					row := data[y*stride+x0 : y*stride+x0+cols]
					for c := range cols {
						block[c*h+dstY] = row[c]
					}
				}
				for c := range cols {
					line(block[c*h:(c+1)*h], bounds.Min.Y, true)
				}
				for y := range h {
					row := data[y*stride+x0 : y*stride+x0+cols]
					for c := range cols {
						row[c] = block[c*h+y]
					}
				}
				continue
			}
			for y := range h {
				row := data[y*stride+x0 : y*stride+x0+cols]
				for c := range cols {
					block[c*h+y] = row[c]
				}
			}
			for c := range cols {
				line(block[c*h:(c+1)*h], bounds.Min.Y, false)
			}
			for y := range lowCount {
				srcY := 2*y + even
				row := data[y*stride+x0 : y*stride+x0+cols]
				for c := range cols {
					row[c] = block[c*h+srcY]
				}
			}
			for y := lowCount; y < h; y++ {
				srcY := 2*(y-lowCount) + odd
				row := data[y*stride+x0 : y*stride+x0+cols]
				for c := range cols {
					row[c] = block[c*h+srcY]
				}
			}
		}
		return wctx.Err()
	})
}

// runWaveletWorkers 将连续区间切分给多个工作协程并行执行
// 入参: ctx 上下文, workers 并发数, count 总任务数, fn 区间处理函数
// 返回: error 错误信息
func runWaveletWorkers(ctx context.Context, workers, count int, fn func(context.Context, int, int, int) error) error {
	if workers <= 1 || count <= 1 {
		return fn(ctx, 0, 0, count)
	}
	n := min(workers, count)
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup
	run := func(id, start, end int) {
		if err := fn(child, id, start, end); err != nil {
			mu.Lock()
			if firstErr == nil {
				firstErr = err
				cancel()
			}
			mu.Unlock()
		}
	}
	base, rem := count/n, count%n
	first := base
	if rem > 0 {
		first++
	}
	pos := first
	for id := 1; id < n; id++ {
		size := base
		if id < rem {
			size++
		}
		start, end := pos, pos+size
		pos = end
		wg.Add(1)
		go func(id, start, end int) {
			defer wg.Done()
			run(id, start, end)
		}(id, start, end)
	}
	run(0, 0, first)
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return firstErr
}
