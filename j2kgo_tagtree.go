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

// tagNode 标签树节点及渐进查询状态
type tagNode struct {
	parent int
	value  uint32
	low    uint32
	known  bool
}

// tagTree 记录码块首次出现的质量层或前导零位平面数
type tagTree struct {
	nodes  []tagNode
	leaves int
}

// newTagTree 创建标签树，values为nil时用于解码
// 入参: width 叶层宽度, height 叶层高度, values 叶层编码值, limits 资源限制
// 返回: *tagTree 标签树, error 错误信息
func newTagTree(width, height int, values []uint32, limits Limits) (*tagTree, error) {
	limits = limits.normalized()
	total, err := tagTreeNodeCount(width, height, limits.MaxMemoryBytes/32)
	if err != nil {
		return nil, err
	}
	if values != nil && len(values) != width*height {
		return nil, FormatError("tag tree value count")
	}
	tree := &tagTree{nodes: make([]tagNode, int(total)), leaves: width * height}
	for i := range tree.nodes {
		tree.nodes[i].value, tree.nodes[i].parent = math.MaxUint32, -1
	}
	for i, value := range values {
		if value == math.MaxUint32 {
			return nil, FormatError("tag tree value overflow")
		}
		tree.nodes[i].value = value
	}
	w, h, offset := width, height, 0
	for w != 1 || h != 1 {
		parentOffset, parentWidth := offset+w*h, ceilDiv(w, 2)
		for y := range h {
			for x := range w {
				node := &tree.nodes[offset+y*w+x]
				node.parent = parentOffset + (y/2)*parentWidth + x/2
				tree.nodes[node.parent].value = min(tree.nodes[node.parent].value, node.value)
			}
		}
		offset, w, h = parentOffset, parentWidth, ceilDiv(h, 2)
	}
	return tree, nil
}

// tagTreeNodeCount 按各层实际尺寸计算节点数并检查预算
// 入参: width 叶层宽度, height 叶层高度, limit 节点上限
// 返回: int 节点数, error 错误信息
func tagTreeNodeCount(width, height int, limit uint64) (int, error) {
	if width < 1 || height < 1 {
		return 0, FormatError("tag tree dimensions")
	}
	w, h := width, height
	var total uint64
	for {
		n, err := checkedProduct("tag tree nodes", uint64(w), uint64(h), min(uint64(^uint(0)>>1), limit))
		if err != nil {
			return 0, err
		}
		total, err = checkTotal("tag tree nodes", total, n, min(uint64(^uint(0)>>1), limit))
		if err != nil {
			return 0, err
		}
		if w == 1 && h == 1 {
			return int(total), nil
		}
		w, h = ceilDiv(w, 2), ceilDiv(h, 2)
	}
}

// path 生成叶节点到根节点的访问路径
// 入参: leaf 叶节点索引, result 路径缓冲
// 返回: int 路径长度, error 错误信息
func (t *tagTree) path(leaf int, result *[65]int) (int, error) {
	if leaf < 0 || leaf >= t.leaves {
		return 0, FormatError("tag tree leaf index")
	}
	length := 0
	for node := leaf; node >= 0; node = t.nodes[node].parent {
		result[length] = node
		length++
	}
	return length, nil
}

// decode 查询标签值是否小于阈值，保留跨质量层状态
// 入参: r 数据包位流, leaf 叶节点索引, threshold 查询阈值
// 返回: bool 是否小于阈值, error 错误信息
func (t *tagTree) decode(r *packetBits, leaf int, threshold uint32) (bool, error) {
	var path [65]int
	length, err := t.path(leaf, &path)
	if err != nil {
		return false, err
	}
	low := uint32(0)
	for i := length - 1; i >= 0; i-- {
		node := &t.nodes[path[i]]
		node.low = max(node.low, low)
		for node.low < threshold && node.low < node.value {
			bit, err := r.bit()
			if err != nil {
				return false, err
			}
			if bit != 0 {
				node.value = node.low
			} else {
				node.low++
			}
		}
		low = node.low
	}
	return t.nodes[leaf].value < threshold, nil
}

// encode 编码标签值与阈值的比较结果，保留跨质量层状态
// 入参: w 数据包位流, leaf 叶节点索引, threshold 查询阈值
// 返回: error 错误信息
func (t *tagTree) encode(w *packetWriter, leaf int, threshold uint32) error {
	var path [65]int
	length, err := t.path(leaf, &path)
	if err != nil {
		return err
	}
	low := uint32(0)
	for i := length - 1; i >= 0; i-- {
		node := &t.nodes[path[i]]
		node.low = max(node.low, low)
		for node.low < threshold {
			if node.low >= node.value {
				if !node.known {
					w.bit(1)
					node.known = true
				}
				break
			}
			w.bit(0)
			node.low++
		}
		low = node.low
	}
	return nil
}
