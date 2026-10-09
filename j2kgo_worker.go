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
	"runtime"
	"sync"
)

// 并行解码的调度器和单个工作协程预留内存，单位为字节
const (
	decodePoolMemory   = 1024
	decodeWorkerMemory = 256
)

// decodeWork 记录码块任务数量、待处理样本数及最大工作缓冲大小
type decodeWork struct {
	count   int
	samples uint64
	largest uint64
}

// decodeTask 保存码块处理函数及其所需内存
type decodeTask struct {
	memory uint64
	run    func(context.Context) error
}

// decodePool 在共享内存预算内并发解码码块，不缓存待解码的码块数据
type decodePool struct {
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	ready      *sync.Cond
	used       uint64
	limit      uint64
	err        error
	panicValue any
	panicked   bool
	tasks      chan decodeTask
	wait       sync.WaitGroup
}

// add 汇总待解码码块并提前检查输入范围及任务预算
// 入参: source 输入源, block 码块, memory 可用内存
// 返回: error 错误信息
func (w *decodeWork) add(source *inputSource, block *packetBlock, memory uint64) error {
	need, err := packetBlockMemory(source, block, memory)
	if err != nil {
		return err
	}
	w.count++
	w.samples = min(16384, w.samples+uint64(block.bounds.Dx()*block.bounds.Dy()))
	w.largest = max(w.largest, need)
	return nil
}

// workers 根据计算量、GOMAXPROCS和可用内存确定并发上限
// 入参: requested 请求并发上限，零值自动选择, memory 可用内存
// 返回: int 实际并发上限
func (w decodeWork) workers(requested int, memory uint64) int {
	if w.count < 2 || w.samples < 16384 || memory <= w.largest || memory-w.largest <= decodePoolMemory {
		return 1
	}
	maximum := runtime.GOMAXPROCS(0)
	if requested > 0 {
		maximum = min(maximum, requested)
	}
	return max(1, min(maximum, w.count, int(min(uint64(maximum), (memory-w.largest-decodePoolMemory)/decodeWorkerMemory))))
}

// run 执行码块任务并等待全部工作协程退出，内存不足以并发时自动串行
// 工作协程的panic在任务全部退出后由调用协程重新触发
// 入参: ctx 上下文, workers 并发上限, memory 可用内存, produce 逐个提交任务
// 返回: error 取消、任务执行或任务提交错误
func (w decodeWork) run(ctx context.Context, workers int, memory uint64, produce func(func(uint64, func(context.Context) error) error) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	workers = w.workers(workers, memory)
	if workers == 1 {
		return produce(func(need uint64, run func(context.Context) error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if need > memory {
				return &LimitError{Resource: "decode task memory", Limit: memory, Required: need}
			}
			return run(ctx)
		})
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	p := &decodePool{ctx: child, cancel: cancel, limit: memory - decodePoolMemory - uint64(workers)*decodeWorkerMemory, tasks: make(chan decodeTask)}
	p.ready = sync.NewCond(&p.mu)
	wakeDone := make(chan struct{})
	stopWake := context.AfterFunc(child, func() {
		p.mu.Lock()
		p.ready.Broadcast()
		p.mu.Unlock()
		close(wakeDone)
	})
	defer func() {
		if !stopWake() {
			<-wakeDone
		}
	}()
	p.wait.Add(workers)
	for range workers {
		go p.work()
	}
	finished := false
	defer func() {
		if !finished {
			cancel()
			close(p.tasks)
			p.wait.Wait()
		}
	}()
	err := produce(p.submit)
	if err != nil {
		cancel()
	}
	close(p.tasks)
	p.wait.Wait()
	finished = true
	if p.panicked {
		panic(p.panicValue)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if p.err != nil {
		return p.err
	}
	return err
}

// submit 等待可用内存和空闲工作协程，提交单个任务
// 入参: memory 任务所需内存, run 任务函数
// 返回: error 错误信息
func (p *decodePool) submit(memory uint64, run func(context.Context) error) error {
	if memory > p.limit {
		return &LimitError{Resource: "decode task memory", Limit: p.limit, Required: memory}
	}
	p.mu.Lock()
	for memory > p.limit-p.used && p.ctx.Err() == nil {
		p.ready.Wait()
	}
	if err := p.ctx.Err(); err != nil {
		p.mu.Unlock()
		return err
	}
	p.used += memory
	p.mu.Unlock()
	select {
	case p.tasks <- decodeTask{memory: memory, run: run}:
		return nil
	case <-p.ctx.Done():
		p.mu.Lock()
		p.used -= memory
		p.ready.Broadcast()
		p.mu.Unlock()
		return p.ctx.Err()
	}
}

// work 依次处理接收到的任务，任务通道关闭后退出
func (p *decodePool) work() {
	defer p.wait.Done()
	for task := range p.tasks {
		p.execute(task)
	}
}

// execute 执行任务并归还内存预算，发生错误或panic时记录原因并取消其余任务
// 入参: task 解码任务
func (p *decodePool) execute(task decodeTask) {
	var err error
	finished := false
	defer func() {
		value := recover()
		p.mu.Lock()
		p.used -= task.memory
		if !finished && !p.panicked {
			p.panicked, p.panicValue = true, value
			p.cancel()
		} else if err != nil && p.err == nil && p.ctx.Err() == nil {
			p.err = err
			p.cancel()
		}
		p.ready.Broadcast()
		p.mu.Unlock()
	}()
	err = p.ctx.Err()
	if err == nil {
		err = task.run(p.ctx)
	}
	finished = true
}

// packetBlockMemory 估算码块解码所需内存，包含码字和工作缓冲
// 入参: source 输入源, block 码块, limit 内存上限
// 返回: uint64 所需内存, error 错误信息
func packetBlockMemory(source *inputSource, block *packetBlock, limit uint64) (uint64, error) {
	memory, err := checkedProduct("code-block memory", uint64(block.bounds.Dx()*block.bounds.Dy()), 11, limit)
	if err != nil {
		return 0, err
	}
	memory, err = checkTotal("code-block memory", memory, 512, limit)
	if err != nil {
		return 0, err
	}
	for _, segment := range block.segments {
		if segment.retained == 0 {
			break
		}
		memory, err = checkTotal("codeword memory", memory, 32, limit)
		if err != nil {
			return 0, err
		}
		for _, part := range segment.parts {
			if part.offset < 0 || part.length < 0 || part.offset > source.size || part.length > source.size-part.offset {
				return 0, FormatError("codeword input extent")
			}
			memory, err = checkTotal("codeword memory", memory, uint64(part.length), limit)
			if err != nil {
				return 0, err
			}
		}
	}
	return memory, nil
}

// decodePacketBlock 在独立任务预算内读取并解码码块，恢复感兴趣区域系数
// 入参: ctx 上下文, source 输入源, block 码块, band 子带方向, coding 编码方式, roi ROI移位量, precision 量化幅度位数, memory 任务内存预算
// 返回: *codeBlock 解码系数, error 错误信息
func decodePacketBlock(ctx context.Context, source *inputSource, block *packetBlock, band bandOrientation, coding CodeBlockStyle, roi uint8, precision int, memory uint64) (*codeBlock, error) {
	work := uint64(block.bounds.Dx()*block.bounds.Dy())*11 + 512
	if work >= memory {
		return nil, &LimitError{Resource: "code-block memory", Limit: memory, Required: work + 1}
	}
	encoded, err := loadPacketBlock(ctx, source, block, Limits{MaxMemoryBytes: memory - work})
	if err != nil {
		return nil, err
	}
	decoded, err := decodeCodeBlockPrecision(ctx, encoded, roi, precision, block.bounds.Dx(), block.bounds.Dy(), band, coding, Limits{MaxMemoryBytes: work})
	if err != nil {
		return nil, err
	}
	return decoded, nil
}
