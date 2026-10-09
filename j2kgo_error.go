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

import "fmt"

// FormatError 码流或容器格式错误
type FormatError string

// Error 返回错误描述
// 返回: string 错误描述
func (e FormatError) Error() string { return "j2kgo: invalid format: " + string(e) }

// UnsupportedError 表示尚不支持的格式特性
type UnsupportedError string

// Error 返回错误描述
// 返回: string 错误描述
func (e UnsupportedError) Error() string { return "j2kgo: unsupported: " + string(e) }

// LimitError 表示资源用量超出限制
type LimitError struct {
	Resource string
	Limit    uint64
	Required uint64
}

// Error 返回错误描述
// 返回: string 错误描述
func (e *LimitError) Error() string {
	return fmt.Sprintf("j2kgo: %s limit exceeded: need %d, limit %d", e.Resource, e.Required, e.Limit)
}
