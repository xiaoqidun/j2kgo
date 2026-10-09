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

import "math"

// Limits 限制库内资源用量，零值使用默认限制，不限制调用方或整个进程的内存
type Limits struct {
	MaxInputBytes  uint64
	MaxSamples     uint64
	MaxMemoryBytes uint64
}

// normalized 补充未设置的资源限制
// 返回: Limits 资源限制
func (l Limits) normalized() Limits {
	if l.MaxInputBytes == 0 {
		l.MaxInputBytes = 1 << 30
	}
	if l.MaxSamples == 0 {
		l.MaxSamples = 1 << 30
	}
	if l.MaxMemoryBytes == 0 {
		l.MaxMemoryBytes = 512 << 20
	}
	return l
}

// checkedProduct 检查尺寸乘积及资源上限
// 入参: resource 资源名称, a 乘数, b 乘数, limit 上限
// 返回: uint64 乘积, error 错误信息
func checkedProduct(resource string, a, b, limit uint64) (uint64, error) {
	if b != 0 && a > math.MaxUint64/b {
		return 0, &LimitError{Resource: resource, Limit: limit, Required: math.MaxUint64}
	}
	n := a * b
	if n > limit {
		return 0, &LimitError{Resource: resource, Limit: limit, Required: n}
	}
	return n, nil
}

// checkTotal 检查累计资源用量
// 入参: resource 资源名称, used 当前用量, added 新增用量, limit 上限
// 返回: uint64 累计用量, error 错误信息
func checkTotal(resource string, used, added, limit uint64) (uint64, error) {
	if added > math.MaxUint64-used {
		return 0, &LimitError{Resource: resource, Limit: limit, Required: math.MaxUint64}
	}
	total := used + added
	if total > limit {
		return 0, &LimitError{Resource: resource, Limit: limit, Required: total}
	}
	return total, nil
}

// appendBudgeted 在预算内追加切片元素，计入扩容时新旧缓冲的峰值内存
// 入参: values 原切片, added 新增元素, budget 内存预算, size 单项字节上限
// 返回: []T 追加后的切片, error 错误信息
func appendBudgeted[T any](values, added []T, budget *layoutBudget, size uint64) ([]T, error) {
	if budget == nil || len(added) <= cap(values)-len(values) {
		return append(values, added...), nil
	}
	result, err := growBudgeted(values, len(added), budget, size)
	if err != nil {
		return values, err
	}
	return append(result, added...), nil
}

// growBudgeted 为切片预留新增元素的容量，将扩容峰值计入内存预算
// 入参: values 原切片, added 新增元素数量, budget 内存预算, size 单项字节上限
// 返回: []T 长度不变的切片, error 错误信息
func growBudgeted[T any](values []T, added int, budget *layoutBudget, size uint64) ([]T, error) {
	if added <= cap(values)-len(values) {
		return values, nil
	}
	maximum := uint64(^uint(0)>>1) / max(size, 1)
	count, err := checkTotal("buffer entries", uint64(len(values)), uint64(added), maximum)
	if err != nil {
		return values, err
	}
	capacity := max(count, min(uint64(cap(values))*2, maximum))
	if err := budget.add(capacity, size); err != nil {
		capacity = count
		if err := budget.add(capacity, size); err != nil {
			return values, err
		}
	}
	result := make([]T, len(values), int(capacity))
	copy(result, values)
	budget.used -= uint64(cap(values)) * size
	return result, nil
}
