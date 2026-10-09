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
	"cmp"
	"encoding/binary"
	"fmt"
	"slices"
)

// setProgressions 校验并复制渐进顺序及适用范围，确保覆盖全部数据包
// 入参: changes 渐进顺序变更列表
// 返回: error 错误信息
func (p *encodingPlan) setProgressions(changes []ProgressionChange) error {
	if len(changes) == 0 {
		return nil
	}
	levels := p.maxLevels()
	width := 7
	if len(p.info.Components) >= 257 {
		width = 9
	}
	if len(changes) > 65533/width {
		return fmt.Errorf("j2kgo: too many progression changes")
	}
	memory := uint64(len(changes)) * 48
	if memory*2 >= p.limits.MaxMemoryBytes {
		return &LimitError{Resource: "progression memory", Limit: p.limits.MaxMemoryBytes, Required: memory*2 + 1}
	}
	volumes := make([]progressionVolume, len(changes))
	for i, change := range changes {
		if change.ComponentEnd == 0 {
			change.ComponentEnd = len(p.info.Components)
		}
		if change.ResolutionEnd == 0 {
			change.ResolutionEnd = levels + 1
		}
		if change.LayerEnd == 0 {
			change.LayerEnd = p.layerCount
		}
		if change.Order > CPRL || change.ComponentStart < 0 || change.ComponentStart >= change.ComponentEnd || change.ComponentEnd > len(p.info.Components) || change.ResolutionStart < 0 || change.ResolutionStart >= change.ResolutionEnd || change.ResolutionEnd > 33 || change.LayerEnd < 1 || change.LayerEnd > p.layerCount {
			return fmt.Errorf("j2kgo: invalid progression change %d", i)
		}
		if change.Order == RPCL || change.Order == PCRL {
			for _, c := range p.info.Components[change.ComponentStart:change.ComponentEnd] {
				if !powerOfTwo(int(c.XStep)) || !powerOfTwo(int(c.YStep)) {
					return fmt.Errorf("j2kgo: progression requires power-of-two sampling")
				}
			}
		}
		volumes[i] = progressionVolume{order: change.Order, componentStart: change.ComponentStart, componentEnd: change.ComponentEnd, resolutionStart: change.ResolutionStart, resolutionEnd: change.ResolutionEnd, layerEnd: change.LayerEnd}
	}
	ordered := slices.Clone(volumes)
	slices.SortFunc(ordered, func(a, b progressionVolume) int { return cmp.Compare(a.componentStart, b.componentStart) })
	for resolution := 0; resolution <= levels; resolution++ {
		end := 0
		for _, volume := range ordered {
			if volume.layerEnd == p.layerCount && volume.resolutionStart <= resolution && volume.resolutionEnd > resolution {
				if volume.componentStart > end {
					for end < volume.componentStart && p.componentCoding(end).levels < resolution {
						end++
					}
					if volume.componentStart > end {
						break
					}
				}
				end = max(end, volume.componentEnd)
			}
		}
		for end < len(p.info.Components) && p.componentCoding(end).levels < resolution {
			end++
		}
		if end != len(p.info.Components) {
			return fmt.Errorf("j2kgo: progression changes do not cover all packets")
		}
	}
	p.volumes = volumes
	p.limits.MaxMemoryBytes -= memory
	return nil
}

// encodeProgressions 写入POC参数，单字节分量索引的终点256以零表示
// 入参: volumes 渐进范围, count 分量总数
// 返回: []byte 标记参数
func encodeProgressions(volumes []progressionVolume, count int) []byte {
	width := 7
	if count >= 257 {
		width = 9
	}
	data := make([]byte, 0, len(volumes)*width)
	for _, volume := range volumes {
		data = append(data, byte(volume.resolutionStart))
		data = appendComponentIndex(data, volume.componentStart, count)
		data = binary.BigEndian.AppendUint16(data, uint16(volume.layerEnd))
		data = append(data, byte(volume.resolutionEnd))
		data = appendComponentIndex(data, volume.componentEnd, count)
		data = append(data, byte(volume.order))
	}
	return data
}
