# Модуль 008 — Theory: Bulkhead Pattern, изоляция нагрузки и oversubscription

## Bulkhead: принцип изоляции

### Метафора

Корабль без переборок: одна пробоина — вода заливает весь корпус, корабль тонет. Корабль с переборками: пробоина в одном отсеке — вода только там, остальные сухие, корабль на плаву.

В software: один connection pool, три типа трафика (critical API, batch jobs, analytics). Batch job запускает 1000 запросов — исчерпывает все connections. Critical API — в очереди. Пользователи видят timeout. С bulkhead: batch jobs — свой лимит (3 conn), critical — свой (5 conn). Batch исчерпал свои 3 — ждёт. Critical API — свои 5 свободны, работает без задержки.

### Где применяется

1. **Connection pools.** Разные категории запросов — разные лимиты.
2. **Thread pools (Java).** Hystrix от Netflix — каждый downstream service в отдельном thread pool.
3. **HTTP clients.** Per-host connection limits: не более 10 conn к payments API, не более 50 к cache.
4. **Queue consumers.** Разные consumer groups с разными concurrency limits.
5. **CPU/Memory.** Linux cgroups — bulkhead для системных ресурсов.

### Bulkhead vs Circuit Breaker

Bulkhead — ограничение конкурентности per-category. Circuit breaker — отключение при обнаружении failures. Complementary:

```
Request → Rate Limiter → Bulkhead → Circuit Breaker → Connection Pool → Backend
```

Rate limiter: частота. Bulkhead: конкурентность per-category. Circuit breaker: health-based отключение. Pool: ресурс management.

## Архитектура

### Двухуровневое ограничение

```
Acquire("critical", ctx):
   ┌─────────────────────┐
   │ Partition Semaphore  │ ← per-category limit (быстрый, in-memory)
   │ "critical": 5 slots │
   └──────────┬──────────┘
              │ slot acquired
   ┌──────────▼──────────┐
   │   Connection Pool   │ ← shared resource limit (может быть медленным)
   │   maxSize: 15       │
   └──────────┬──────────┘
              │ conn acquired
         return conn

Release(conn, "critical"):
   ┌──────────────────────┐
   │   Connection Pool    │ ← вернуть conn первым
   │   pool.Release(conn) │
   └──────────┬───────────┘
              │
   ┌──────────▼───────────┐
   │ Partition Semaphore   │ ← освободить slot вторым
   │ sem.Release()         │
   └───────────────────────┘
```

Порядок Acquire: partition sem → pool. Порядок Release: pool → partition sem. Одинаковый порядок ресурсов в acquire предотвращает deadlock.

### Почему partition sem первым

1. **Быстрый отказ.** Partition sem — in-memory, ~10ns. Если partition полна — немедленный отказ без обращения к pool. Pool acquire может быть дорогим (factory: TCP connect, ~50ms).

2. **Защита pool.** Partition sem фильтрует трафик ДО pool. Без partition sem: все запросы попадают в pool → contention на pool мьютексе. С partition sem: только partition limit запросов достигают pool.

3. **Изоляция.** Partition A исчерпана → запросы partition A блокируются на partition sem. Pool не видит эти запросы. Partition B → pool → без contention от A.

### Release: почему pool первым

```
Acquire order:  partition_sem → pool
Release order:  pool → partition_sem
```

Почему не `partition_sem → pool` для Release тоже? Потому что Release partition sem сигнализирует: "один slot свободен". Если pool Release ещё не вызван — conn всё ещё active в pool. Другая горутина, получив partition slot, вызовет pool.Acquire — может получить другой conn (если есть idle) или заблокироваться. Это корректно, но неоптимально: conn ещё не вернулся в idle, pool может создать новый.

Pool Release первым: conn возвращается в idle. Partition sem Release вторым: slot открывается. Следующая горутина: partition sem → pool.Acquire → находит conn в idle (только что возвращённый). Оптимальнее: переиспользование вместо создания.

## Oversubscription

### Что это

Sum(partition limits) > pool maxSize. Три partition по 10 = 30, pool maxSize=20. Не все 30 slot могут быть активны одновременно — pool ограничивает до 20.

### Когда безопасно

1. **Разные пики.** "batch" работает ночью, "api" — днём. Пики не совпадают → oversubscription безопасна.
2. **Probabilistic.** Каждая partition использует ~50% slots в среднем. 3 × 10 × 50% = 15 < 20. Oversubscription работает.
3. **With fallback.** При pool exhaustion — retry, backoff, degraded response. Не crash.

### Когда опасно

1. **Simultaneous peaks.** Black Friday: все partition на пике одновременно → pool exhaustion → cascade timeouts.
2. **No timeout.** Pool.Acquire без timeout → горутины висят навечно, consuming partition slots → deadlock-like behavior.
3. **Large oversubscription.** 3 × 100 = 300 vs pool 20 → при любом reasonable load pool exhausted.

### Мониторинг

```
pool_active / pool_max_size > 0.9  → alert: pool near exhaustion
partition_rejected_total > 0        → alert: partition overloaded
pool_wait_duration_p99 > 1s         → alert: severe contention
```

При oversubscription: `pool_active` = indicator. Если постоянно near maxSize — oversubscription слишком агрессивна. Уменьшить partition limits или увеличить pool.

## Partition как семафор

### Реализация через модуль 001-002

Каждая partition — `*semaphore.Semaphore` из модулей 001-002. Context-aware acquire, TryAcquire, Close — всё уже реализовано.

```go
type Partition struct {
    name string
    sem  *semaphore.Semaphore
    // atomic counters для метрик
    active   atomic.Int64
    total    atomic.Int64
    rejected atomic.Int64
}
```

Acquire partition:
```go
func (p *Partition) Acquire(ctx context.Context) error {
    if err := p.sem.AcquireWithContext(ctx); err != nil {
        p.rejected.Add(1)
        return fmt.Errorf("partition %q: %w", p.name, err)
    }
    p.active.Add(1)
    p.total.Add(1)
    return nil
}
```

Release partition:
```go
func (p *Partition) Release() {
    p.active.Add(-1)
    p.sem.Release()
}
```

### Lazy partition creation

Неизвестная partition: Acquire("unknown_category") → partition не зарегистрирована. Вместо ошибки — создать lazily:

```go
func (b *Bulkhead) getOrCreatePartition(name string) *Partition {
    b.mu.RLock()
    p, ok := b.partitions[name]
    b.mu.RUnlock()
    if ok {
        return p
    }
    
    b.mu.Lock()
    defer b.mu.Unlock()
    // Double-check
    if p, ok = b.partitions[name]; ok {
        return p
    }
    p = newPartition(name, b.defaultMax)
    b.partitions[name] = p
    return p
}
```

Double-checked locking (модуль 004): RLock для fast path, Lock для creation. Между RUnlock и Lock — другая горутина может создать. Double-check предотвращает дублирование.

## Dynamic resize

### Проблема

Partition "critical" maxConns=5. Под нагрузкой — не хватает. `ResizePartition("critical", 8)`. Семафор capacity 5 → 8. Но семафор из модуля 001 — `make(chan struct{}, N)` — capacity канала immutable после создания.

### Решение: замена семафора

```go
func (b *Bulkhead) ResizePartition(name string, newMax int) error {
    b.mu.Lock()
    defer b.mu.Unlock()
    
    p, ok := b.partitions[name]
    if !ok {
        return fmt.Errorf("partition %q not found", name)
    }
    
    // Создать новый семафор
    newSem := semaphore.New(newMax)
    
    // Перенести текущие active slots
    active := int(p.active.Load())
    for i := 0; i < active; i++ {
        newSem.Acquire()  // "занять" столько же слотов сколько active
    }
    
    // Заменить
    oldSem := p.sem
    p.sem = newSem
    p.maxConns = newMax
    
    // Закрыть старый (разблокировать ожидающих → они попробуют новый)
    oldSem.Close()
    
    return nil
}
```

Ожидающие на старом семафоре получат `ErrSemaphoreClosed` → retry → Acquire на новом → успех (если slots доступны). Transient errors для ожидающих — приемлемо при resize.

Альтернатива: weighted semaphore (модуль 003) вместо channel-based. WeightedSemaphore с mutex-based state — resize проще (изменить `max` под мьютексом). Но: WeightedSemaphore дороже per-operation.

## Fallback partition

### Паттерн

Primary partition exhausted → попробовать fallback:

```go
func (b *Bulkhead) Acquire(ctx context.Context, partition string) (*connpool.Conn, error) {
    p := b.getOrCreatePartition(partition)
    
    // Попытка primary
    if err := p.Acquire(ctx); err != nil {
        // Primary failed — попробовать fallback
        if b.fallback != "" && partition != b.fallback {
            fb := b.getOrCreatePartition(b.fallback)
            if err := fb.Acquire(ctx); err != nil {
                return nil, fmt.Errorf("partition %q and fallback %q exhausted", partition, b.fallback)
            }
            // Fallback acquired — get conn from pool
            conn, err := b.pool.Acquire(ctx)
            if err != nil {
                fb.Release()
                return nil, err
            }
            return conn, nil  // caller must Release with fallback partition
        }
        return nil, err
    }
    
    // Primary acquired — get conn
    conn, err := b.pool.Acquire(ctx)
    if err != nil {
        p.Release()
        return nil, err
    }
    return conn, nil
}
```

Нюанс: caller должен знать из какой partition conn получен (primary или fallback) для корректного Release. Решение: вернуть обёртку с partition info.

### Когда fallback

1. **Critical traffic.** Primary "critical" = 5 slots. Fallback "overflow" = 3 extra slots. Critical никогда не rejected — до 8 connections при пике.
2. **Degraded service.** Primary "fast" (SSD backend). Fallback "slow" (HDD backend). Fast full → slow, но с warning.
3. **Multi-region.** Primary "us-east". Fallback "eu-west". Primary overloaded → route to fallback region.

## Partition-aware Conn

### Проблема

Acquire возвращает `*connpool.Conn`. Release требует partition name. Если caller передаст неправильную partition — slot leak в одной partition, overcounting в другой.

### Решение: wrapper

```go
type BulkheadConn struct {
    conn      *connpool.Conn
    partition *Partition
    bulkhead  *Bulkhead
    released  bool
}

func (bc *BulkheadConn) Release() {
    if bc.released {
        return  // idempotent
    }
    bc.released = true
    bc.conn.Release()        // pool release
    bc.partition.Release()   // partition sem release
}

func (bc *BulkheadConn) Resource() connpool.Resource {
    return bc.conn.Resource()
}
```

Acquire возвращает `*BulkheadConn`. Release не требует partition name — conn знает свою partition. Нет способа Release с неправильной partition.

## Вопросы для самопроверки

1. Три partition по 5, pool maxSize=10. Все три на пике: 5+5+5=15 > 10. Pool блокирует 5 запросов. Какие 5? Из какой partition? Deterministic?

2. Partition sem → pool acquire. Pool acquire timeout. Partition sem slot — занят, но conn не получен. Release partition sem без conn. Корректно? Или partition sem "утечёт"?

3. Dynamic resize: "critical" 5→3. Active=4 (больше нового max). Что происходит? Четыре conn продолжают работать, новый семафор capacity=3. Acquire → 3 slots busy + 4 on old = 7 total "critical"?

4. `BulkheadConn` wrapper — добавляет слой indirection. Для каждого Acquire/Release — два уровня Release (conn + partition). Overhead?

5. Fallback chain: A → B → C. A full → try B. B full → try C. C full → error. Три sequential semaphore attempts. Max latency если все три с timeout 1s?