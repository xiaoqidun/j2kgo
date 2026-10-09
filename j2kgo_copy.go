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

import "context"

// copyContext 分段复制切片，复制长度不超过任一切片长度，支持取消操作
// 入参: ctx 上下文, dst 目标缓冲, src 原始数据
// 返回: error 错误信息
func copyContext[T any](ctx context.Context, dst, src []T) error {
	count := min(len(dst), len(src))
	for start := 0; start < count; {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := start + min(8192, count-start)
		copy(dst[start:end], src[start:end])
		start = end
	}
	return ctx.Err()
}

// cloneSliceContext 分配独立切片并分段复制，空切片返回nil
// 入参: ctx 上下文, src 原始数据
// 返回: []T 独立副本, error 错误信息
func cloneSliceContext[T any](ctx context.Context, src []T) ([]T, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(src) == 0 {
		return nil, nil
	}
	dst := make([]T, len(src))
	if err := copyContext(ctx, dst, src); err != nil {
		return nil, err
	}
	return dst, nil
}
