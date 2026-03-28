package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type Semaphore struct {
	ch chan struct{}
}

func NewSemaphore(concurrency int) *Semaphore {
	if concurrency <= 0 {
		panic("concurrency must be positive")
	}

	return &Semaphore{
		ch: make(chan struct{}, concurrency),
	}
}

func (s *Semaphore) Acquire() {
	s.ch <- struct{}{}
}

func (s *Semaphore) Release() {
	<-s.ch
}

func (s *Semaphore) TryAcquire() bool {
	select {
	case s.ch <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Semaphore) Available() int {
	return cap(s.ch) - len(s.ch)
}

func (s *Semaphore) MaxConcurrency() int {
	return cap(s.ch)
}

func main() {
	fmt.Println("=== Semaphore (max=3, workers=10) ===")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sema := NewSemaphore(3)

	var maxConcurrent atomic.Int64
	var current atomic.Int64

	ticker := time.NewTicker(50 * time.Millisecond)
	go func() {
		for {
			select {
			case <-ticker.C:
				fmt.Printf("[observer] active: %d, available: %d/%d\n",
					current.Load(), sema.Available(), sema.MaxConcurrency())
			case <-ctx.Done():
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			sema.Acquire()
			defer sema.Release()

			cur := current.Add(1)
			for {
				old := maxConcurrent.Load()
				if cur <= old || maxConcurrent.CompareAndSwap(old, cur) {
					break
				}
			}

			time.Sleep(100 * time.Millisecond)
			current.Add(-1)
		}()
	}

	wg.Wait()
	ticker.Stop()
	fmt.Printf("Max concurrent observed: %d (limit: %d)\n\n", maxConcurrent.Load(), sema.MaxConcurrency())

	fmt.Println("=== TryAcquire ===")
	sem2 := NewSemaphore(3)

	sem2.Acquire()
	sem2.Acquire()
	sem2.Acquire()
	fmt.Printf("Semaphore full: %d/%d available\n", sem2.Available(), sem2.MaxConcurrency())

	result := sem2.TryAcquire()
	fmt.Printf("TryAcquire on full: %v\n", result)

	sem2.Release()
	fmt.Println("Released one slot...")

	result = sem2.TryAcquire()
	fmt.Printf("TryAcquire after release: %v\n", result)

	sem2.Release()
	sem2.Release()
	sem2.Release()
	fmt.Println()

	fmt.Println("=== Available ===")
	sem3 := NewSemaphore(3)
	fmt.Printf("Initial: %d/%d\n", sem3.Available(), sem3.MaxConcurrency())

	sem3.Acquire()
	sem3.Acquire()
	fmt.Printf("After 2 acquires: %d/%d\n", sem3.Available(), sem3.MaxConcurrency())

	sem3.Release()
	fmt.Printf("After 1 release: %d/%d\n", sem3.Available(), sem3.MaxConcurrency())

	sem3.Release()
	fmt.Printf("After 2nd release: %d/%d\n", sem3.Available(), sem3.MaxConcurrency())

	fmt.Println("\nmain done")
}
