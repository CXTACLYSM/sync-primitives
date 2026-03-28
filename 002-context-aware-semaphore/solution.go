package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var ErrSemaphoreClosed = errors.New("semaphore closed")

type Semaphore struct {
	ch   chan struct{}
	done chan struct{}
	once sync.Once
}

func NewSemaphore(concurrency int) *Semaphore {
	if concurrency <= 0 {
		panic("concurrency must be positive")
	}

	return &Semaphore{
		ch:   make(chan struct{}, concurrency),
		done: make(chan struct{}),
	}
}

func (s *Semaphore) Acquire() {
	s.ch <- struct{}{}
}

func (s *Semaphore) AcquireWithContext(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return ErrSemaphoreClosed
	default:
	}

	select {
	case s.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return ErrSemaphoreClosed
	}
}

func (s *Semaphore) AcquireWithTimeout(duration time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	return s.AcquireWithContext(ctx)
}

func (s *Semaphore) AcquireWithDeadline(deadline time.Time) error {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	return s.AcquireWithContext(ctx)
}

func (s *Semaphore) Release() {
	<-s.ch
}

func (s *Semaphore) TryAcquire() (bool, error) {
	select {
	case <-s.done:
		return false, ErrSemaphoreClosed
	default:
	}

	select {
	case s.ch <- struct{}{}:
		return true, nil
	default:
		return false, nil
	}
}

func (s *Semaphore) Available() int {
	return cap(s.ch) - len(s.ch)
}

func (s *Semaphore) MaxConcurrency() int {
	return cap(s.ch)
}

func (s *Semaphore) Close() {
	s.once.Do(func() {
		close(s.done)
	})
}

func main() {
	fmt.Println("=== AcquireWithTimeout: success ===")
	sem1 := NewSemaphore(3)

	start := time.Now()
	err := sem1.AcquireWithTimeout(1 * time.Second)
	elapsed := time.Since(start)

	fmt.Printf("Result: %v, elapsed: %v\n", err, elapsed.Truncate(time.Millisecond))
	sem1.Release()
	fmt.Printf("Available after release: %d/%d\n\n", sem1.Available(), sem1.MaxConcurrency())

	fmt.Println("=== AcquireWithTimeout: timeout ===")
	sem2 := NewSemaphore(2)

	sem2.Acquire()
	sem2.Acquire()
	fmt.Printf("Semaphore full: %d/%d available\n", sem2.Available(), sem2.MaxConcurrency())

	start = time.Now()
	err = sem2.AcquireWithTimeout(200 * time.Millisecond)
	elapsed = time.Since(start)

	fmt.Printf("Result: %v, elapsed: %v\n", err, elapsed.Truncate(time.Millisecond))

	sem2.Release()
	sem2.Release()
	fmt.Println()

	fmt.Println("=== AcquireWithContext: manual cancel ===")
	sem3 := NewSemaphore(1)
	sem3.Acquire()

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(150 * time.Millisecond)
		fmt.Println("Cancelling context...")
		cancel()
	}()

	start = time.Now()
	err = sem3.AcquireWithContext(ctx)
	elapsed = time.Since(start)

	fmt.Printf("Result: %v, elapsed: %v\n", err, elapsed.Truncate(time.Millisecond))
	sem3.Release()
	fmt.Println()

	fmt.Println("=== AcquireWithContext: already cancelled ===")
	sem4 := NewSemaphore(3)

	ctx, cancel = context.WithCancel(context.Background())
	cancel()

	start = time.Now()
	err = sem4.AcquireWithContext(ctx)
	elapsed = time.Since(start)

	fmt.Printf("Result: %v, elapsed: %v (immediate)\n", err, elapsed.Truncate(time.Millisecond))
	fmt.Println()

	fmt.Println("=== Close: waiting goroutines get error ===")
	sem5 := NewSemaphore(1)
	sem5.Acquire()

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := sem5.AcquireWithTimeout(5 * time.Second)
			if err != nil {
				fmt.Printf("  goroutine %d: %v\n", i, err)
				return
			}
			defer sem5.Release()
		}()
	}

	time.Sleep(50 * time.Millisecond)

	fmt.Println("Closing semaphore...")
	sem5.Close()

	wg.Wait()
	sem5.Release()

	err = sem5.AcquireWithContext(context.Background())
	fmt.Printf("Acquire after close: %v\n\n", err)

	fmt.Println("=== Race detector stress test ===")
	sem6 := NewSemaphore(5)

	var wg2 sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			err := sem6.AcquireWithTimeout(100 * time.Millisecond)
			if err != nil {
				return
			}
			defer sem6.Release()
			time.Sleep(10 * time.Millisecond)
		}()
	}

	wg2.Wait()
	fmt.Printf("Final state: %d/%d available\n", sem6.Available(), sem6.MaxConcurrency())
	fmt.Println("Race detector: OK")
}
