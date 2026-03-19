# Модуль 010 — Theory: Интеграция, stress testing и профилирование

## Facade pattern: PoolManager

### Зачем фасад

Девять компонентов — девять API. Вызывающий код без фасада:

```go
if !limiter.Allow() { return ErrRateLimited }
if err := bulkhead.Acquire(ctx, "critical"); err != nil { return err }
conn, err := pool.Acquire(ctx)
if err != nil { bulkhead.Release("critical"); return err }
buf := bufPool.Get()
// ... use conn + buf ...
bufPool.Put(buf)
pool.Release(conn)
bulkhead.Release("critical")
```

Восемь строк boilerplate на каждый запрос. Ошибка на любом шаге — cleanup всех предыдущих. Пропустить одну строку cleanup — leak.

PoolManager — фасад: одна точка входа, один Release:

```go
mc, err := manager.Acquire(ctx, "critical")
if err != nil { return err }
defer mc.Release()
// ... use mc.Conn() + mc.Buffer() ...
```

### Facade vs God Object

Фасад делегирует компонентам, не дублирует их логику. PoolManager не реализует rate limiting — он вызывает `limiter.Allow()`. Не реализует pooling — вызывает `pool.Acquire()`. Каждый компонент остаётся независимым, тестируемым, заменяемым.

God Object: всё в одной структуре, 2000 строк, невозможно протестировать части. Фасад: 200 строк wiring, каждый компонент — отдельный пакет с отдельными тестами.

## Pipeline: порядок компонентов

### Acquire pipeline

```
Request
  │
  ▼
┌──────────────┐  denied
│ Rate Limiter │────────▶ ErrRateLimited (метрика +1)
└──────┬───────┘
       │ allowed
       ▼
┌──────────────┐  partition full
│  Bulkhead    │────────▶ ErrPartitionFull (метрика +1)
└──────┬───────┘
       │ slot acquired
       ▼
┌──────────────┐  pool exhausted/timeout
│    Pool      │────────▶ ErrPoolExhausted (метрика +1, bulkhead.Release)
└──────┬───────┘
       │ conn acquired
       ▼
┌──────────────┐
│  BufferPool  │ Get buffer (always succeeds — New if empty)
└──────┬───────┘
       │
       ▼
  ManagedConn returned
```

Порядок: дешёвые проверки первыми. Rate limiter: ~10ns (atomic check). Bulkhead sem: ~20ns (channel send). Pool: ~100ns-50ms (idle reuse или factory). Buffer: ~10ns (sync.Pool Get). Fail fast: rate limit denied — не тратим время на bulkhead и pool.

### Release pipeline

```
ManagedConn.Release()
  │
  ▼
┌──────────────┐
│  BufferPool  │ Put buffer (reset + return)
└──────┬───────┘
       │
       ▼
┌──────────────┐
│    Pool      │ Release conn (→ idle or close)
└──────┬───────┘
       │
       ▼
┌──────────────┐
│  Bulkhead    │ Release partition slot
└──────┬───────┘
       │
       ▼
  Metrics: record duration
```

Обратный порядок: buffer → pool → bulkhead. Каждый шаг — независимый cleanup. Ошибка в одном не блокирует остальные. `defer` на каждом шаге внутри Release — защита от panic.

### Error handling в pipeline

Acquire шаг N failed — cleanup шагов 1..N-1:

```go
func (pm *PoolManager) Acquire(ctx context.Context, partition string) (*ManagedConn, error) {
    pm.metrics.AcquireTotal.Add(1)
    
    // Step 1: Rate limit
    if !pm.limiter.Allow() {
        pm.metrics.RateLimited.Add(1)
        return nil, ErrRateLimited
    }
    
    // Step 2: Bulkhead
    if err := pm.bulkhead.Acquire(ctx, partition); err != nil {
        pm.metrics.BulkheadRejects.Add(1)
        return nil, fmt.Errorf("bulkhead: %w", err)
    }
    
    // Step 3: Pool (cleanup: bulkhead on error)
    conn, err := pm.pool.Acquire(ctx)
    if err != nil {
        pm.bulkhead.Release(partition)  // cleanup step 2
        pm.metrics.PoolWaits.Add(1)
        return nil, fmt.Errorf("pool: %w", err)
    }
    
    // Step 4: Buffer
    buf := pm.buffers.Get()
    
    return &ManagedConn{
        conn:      conn,
        buf:       buf,
        partition: partition,
        manager:   pm,
        startTime: time.Now(),
    }, nil
}
```

Каждый error path — cleanup предыдущих шагов. Без cleanup — resource leak (bulkhead slot занят без conn).

## Metrics: что измерять

### Четыре категории

**Throughput:** AcquireTotal, ReleaseTotal, DedupHits — сколько операций в секунду. Trend: растёт = больше нагрузки.

**Errors:** AcquireErrors, RateLimited, BulkheadRejects, PoolTimeouts — сколько отказов. Rate: errors/total > 5% — алерт.

**Latency:** AcquireDuration (время от начала Acquire до возврата ManagedConn), UseDuration (время между Acquire и Release). Percentiles: p50, p95, p99.

**Saturation:** Pool.Active/Pool.MaxSize, Partition.Active/Partition.Max, RateLimiter.Tokens/Burst. > 80% — near capacity.

### atomic vs mutex для метрик

Все метрики — `atomic.Int64`. Почему не mutex:

1. **Hot path.** Metrics инкрементируются на каждом Acquire/Release — горячий путь. Mutex: Lock + increment + Unlock = ~30-50ns с contention. Atomic: Add = ~5-10ns, no lock, no contention.

2. **No compound operations.** Каждая метрика — независимый счётчик. Не нужно атомарно обновлять несколько метрик одновременно. Для compound (AcquireTotal++ AND AcquireErrors++ atomically) — нужен mutex. Для independent — atomic.

3. **Read scalability.** Stats() читает все метрики. Atomic read — no lock, concurrent reads free. Mutex: RLock на все reads.

### Latency tracking

```go
type ManagedConn struct {
    startTime time.Time
    // ...
}

func (mc *ManagedConn) Release() {
    duration := time.Since(mc.startTime)
    mc.manager.metrics.UseDurationNs.Add(duration.Nanoseconds())
    mc.manager.metrics.UseCount.Add(1)
    // ... release resources ...
}
```

Среднее: `UseDurationNs.Load() / UseCount.Load()`. Для percentiles нужна более сложная структура (histogram, HDR histogram). Для production: Prometheus Histogram.

## Stress testing

### Принципы

Stress test — не unit test. Цель: обнаружить проблемы которые проявляются только под нагрузкой: data races, deadlocks, goroutine leaks, memory leaks, starvation.

```go
func TestStress(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping stress test in short mode")
    }
    
    manager := NewPoolManager(mockFactory,
        WithMaxConns(20),
        WithRateLimit(1000, 50),
        WithPartition("critical", 5),
        WithPartition("normal", 10),
        WithPartition("background", 3),
    )
    defer manager.Close()
    
    goroutinesBefore := runtime.NumGoroutine()
    
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    
    var wg sync.WaitGroup
    var acquireCount, errorCount atomic.Int64
    
    partitions := []string{"critical", "normal", "background"}
    
    for i := 0; i < 200; i++ {
        wg.Add(1)
        go func(id int) {
            defer wg.Done()
            partition := partitions[id%3]
            
            for ctx.Err() == nil {
                mc, err := manager.Acquire(ctx, partition)
                acquireCount.Add(1)
                if err != nil {
                    errorCount.Add(1)
                    continue
                }
                
                // Simulate work
                time.Sleep(time.Duration(rand.Intn(10)) * time.Millisecond)
                mc.Release()
            }
        }(i)
    }
    
    wg.Wait()
    
    // Assertions
    stats := manager.Stats()
    
    if stats.Pool.Active != 0 {
        t.Errorf("pool leak: active=%d, expected 0", stats.Pool.Active)
    }
    
    goroutinesAfter := runtime.NumGoroutine()
    if goroutinesAfter > goroutinesBefore+5 {
        t.Errorf("goroutine leak: before=%d, after=%d", goroutinesBefore, goroutinesAfter)
    }
    
    errorRate := float64(errorCount.Load()) / float64(acquireCount.Load())
    if errorRate > 0.1 {
        t.Errorf("error rate too high: %.2f%%", errorRate*100)
    }
    
    t.Logf("Completed: %d acquires, %d errors (%.2f%%)",
        acquireCount.Load(), errorCount.Load(), errorRate*100)
}
```

### Race detector

`go test -race -run TestStress` — обязателен. Race detector добавляет ~2-10x slowdown, но обнаруживает data races с высокой вероятностью. Stress test с -race: длительность × 5, но confidence в отсутствии races.

Race detector не обнаруживает: deadlocks (не race), goroutine leaks (не race), logic errors (не race). Для deadlocks: timeout на тест. Для leaks: NumGoroutine check. Для logic: assertions на metrics.

### Goroutine leak detection

```go
goroutinesBefore := runtime.NumGoroutine()
// ... test ...
goroutinesAfter := runtime.NumGoroutine()

// Допуск: +5 (GC goroutines, finalizers, runtime overhead)
if goroutinesAfter > goroutinesBefore + 5 {
    // Leak detected
    pprof.Lookup("goroutine").WriteTo(os.Stderr, 1)  // dump all goroutines
}
```

Альтернатива: `go.uber.org/goleak` — более точный: фильтрует runtime горутины, сравнивает с baseline.

## pprof: профилирование

### Включение

```go
import _ "net/http/pprof"

go func() {
    log.Println(http.ListenAndServe("localhost:6060", nil))
}()
```

Blank import `_ "net/http/pprof"` — в `init()` регистрирует handlers на `http.DefaultServeMux`:
- `/debug/pprof/` — index
- `/debug/pprof/profile` — CPU profile (30s по умолчанию)
- `/debug/pprof/heap` — heap profile
- `/debug/pprof/goroutine` — goroutine dump
- `/debug/pprof/mutex` — mutex contention
- `/debug/pprof/block` — block events

### CPU profiling

```bash
go tool pprof http://localhost:6060/debug/pprof/profile?seconds=10

(pprof) top 10          # top 10 функций по CPU
(pprof) web             # граф вызовов в браузере
(pprof) list Acquire    # исходный код с аннотацией CPU time
```

Для PoolManager: ожидаемые hotspots — `sync.Mutex.Lock` (pool), atomic operations (metrics), channel operations (semaphore). Если `Lock` доминирует — contention, нужна оптимизация (sharding, RWMutex, lock-free).

### Heap profiling

```bash
go tool pprof http://localhost:6060/debug/pprof/heap

(pprof) top 10 -cum     # top 10 по cumulative allocations
(pprof) list BufferPool  # аллокации в BufferPool
```

Для PoolManager: sync.Pool должен минимизировать heap allocations для буферов. Если `bufpool.Get` → `New` (allocation) при каждом вызове — pool не работает (GC слишком агрессивен, или pool misused).

### Production safety

`net/http/pprof` на `localhost:6060` — безопасно (только локальный доступ). На `0.0.0.0:6060` — ОПАСНО: любой может снять CPU profile (CPU overhead), heap dump (содержит данные из памяти), goroutine dump (leak info). В production: отдельный порт, firewall, или auth middleware.

## Graceful shutdown

### Порядок закрытия

```go
func (pm *PoolManager) Close() error {
    if !pm.closed.CompareAndSwap(false, true) {
        return nil  // already closed
    }
    
    var errs []error
    
    // 1. Rate limiter: прекратить пропускать новые запросы
    pm.limiter.Close()
    
    // 2. Bulkhead: прекратить выдавать partition slots
    if err := pm.bulkhead.Close(); err != nil {
        errs = append(errs, fmt.Errorf("bulkhead: %w", err))
    }
    
    // 3. Pool: закрыть idle, дождаться active
    if err := pm.pool.Close(); err != nil {
        errs = append(errs, fmt.Errorf("pool: %w", err))
    }
    
    // 4. Log final stats
    pm.logger.Info("pool manager closed", "stats", pm.Stats())
    
    return errors.Join(errs...)
}
```

Порядок: снаружи внутрь. Rate limiter первым — новые запросы отклоняются. Bulkhead — ожидающие partition slots получают ошибку. Pool — idle закрываются, active завершатся через Release. Active conns: не закрываются принудительно, их владельцы вызовут Release → pool увидит closed → close conn.

### CAS для idempotent Close

`pm.closed.CompareAndSwap(false, true)` — атомарная проверка и установка. Первый Close: CAS success, выполняет shutdown. Второй Close: CAS fail, return nil. Без мьютекса, без sync.Once (atomic CAS — проще для boolean flag).

## DedupDo: singleflight для операций с conn

### Паттерн

```go
func (pm *PoolManager) DedupDo(ctx context.Context, key string, fn func(*ManagedConn) (any, error), partition string) (any, error) {
    v, err, shared := pm.dedup.Do(key, func() (any, error) {
        mc, err := pm.Acquire(ctx, partition)
        if err != nil {
            return nil, err
        }
        defer mc.Release()
        
        result, err := fn(mc)
        return result, err
    })
    
    if shared {
        pm.metrics.DedupHits.Add(1)
    } else {
        pm.metrics.DedupMisses.Add(1)
    }
    
    return v, err
}
```

Leader: Acquire → fn(conn) → Release → result shared. Followers: ждут result, не acquire conn. Одна conn, одна операция, результат разделён. Корректно для read-only idempotent операций.

## Вопросы для самопроверки

1. Pipeline: rate limiter Allow → bulkhead Acquire → pool Acquire. Rate limiter consumed token. Bulkhead rejected. Token потрачен, запрос не обслужен. Accounting correct?

2. Stress test 200 горутин × 30 секунд. `go test -race` — estimated time? Race detector overhead 2-10x. 30s × 10 = 5 minutes. Acceptable for CI?

3. PoolManager.Close() закрывает rate limiter первым. In-flight Acquire: уже прошёл rate limiter, ждёт pool. Close pool → in-flight получает ErrPoolClosed. Rate limiter already consumed token. Cleanup correct?

4. Metrics: `AcquireTotal - AcquireErrors == successful releases`. Но: DedupDo followers не Acquire (ждут leader). DedupHits не входит в AcquireTotal. Accounting?

5. `pprof.Lookup("goroutine").WriteTo(os.Stderr, 1)` — выводит все горутины с стектрейсами. Формат? Как найти "заблокирована на sync.Mutex.Lock" в выводе?