# Модуль 008 — Bulkhead Pattern

## Контекст

Connection pool из модуля 006 — один общий ресурс для всех. Один тяжёлый клиент (batch import, analytics query) может исчерпать все соединения, заблокировав лёгкие запросы (health check, user-facing API). Bulkhead решает эту проблему: пул разделён на изолированные секции, каждая категория трафика получает свой лимит.

Метафора из кораблестроения: переборки (bulkheads) делят корпус на водонепроницаемые отсеки. Пробоина в одном отсеке не затапливает весь корабль. В software: перегрузка в одной категории трафика не исчерпывает ресурсы для остальных.

Bulkhead — это слой ПОВЕРХ connection pool, а не замена. Внутри — тот же Pool из модуля 006. Снаружи — маршрутизация по категориям с отдельными семафорами.

## Что нужно реализовать

Новый пакет `bulkhead` поверх `connpool`.

**Тип `Bulkhead`:**

```go
type Bulkhead struct {
    mu         sync.RWMutex
    partitions map[string]*Partition
    pool       *connpool.Pool
    defaultMax int
    fallback   string  // имя fallback partition ("" = no fallback)
    closed     bool
}

type Partition struct {
    name      string
    sem       *semaphore.Semaphore
    maxConns  int
    active    atomic.Int64
    total     atomic.Int64
    rejected  atomic.Int64
}
```

**Конструктор:**

```go
func New(pool *connpool.Pool, opts ...Option) *Bulkhead
```

**Options:**

```go
WithPartition(name string, maxConns int) Option   // добавить partition
WithDefaultMax(n int) Option                       // лимит для неизвестных категорий
WithFallback(partitionName string) Option          // fallback partition при exhaustion
```

**Методы Bulkhead:**

- `Acquire(ctx context.Context, partition string) (*connpool.Conn, error)` — получить соединение в рамках указанной partition. Сначала семафор partition (ограничение категории), потом pool.Acquire (получение соединения).

- `Release(conn *connpool.Conn, partition string)` — вернуть соединение. Освободить слот семафора partition, вернуть conn в pool.

- `TryAcquire(partition string) (*connpool.Conn, bool)` — неблокирующий acquire.

- `Stats() BulkheadStats` — статистика по каждой partition.

- `AddPartition(name string, maxConns int) error` — динамическое добавление partition.

- `ResizePartition(name string, maxConns int) error` — изменение лимита partition.

- `Close() error` — закрыть bulkhead и underlying pool.

**Тип `BulkheadStats`:**

```go
type BulkheadStats struct {
    Partitions map[string]PartitionStats
    PoolStats  connpool.PoolStats
}

type PartitionStats struct {
    Name     string
    MaxConns int
    Active   int64
    Total    int64
    Rejected int64
}
```

**Файл `main.go`** — демонстрация:
- Три partition: "critical" (5 conn), "normal" (10 conn), "background" (3 conn)
- Pool maxSize=15 (меньше суммы partition → shared underlying pool)
- Critical запросы проходят даже когда normal exhausted
- Background перегрузка не влияет на critical
- TryAcquire для background (fail fast)
- Fallback: normal exhausted → попробовать "overflow" partition
- Stats по каждой partition
- Dynamic resize: увеличить critical с 5 до 8

## Требования

1. Двухуровневое ограничение: partition семафор (ограничение категории) + pool (ограничение общих ресурсов). Acquire: сначала partition sem, потом pool. Release: сначала pool, потом partition sem. Порядок обратный для предотвращения deadlock при nested locks.

2. Сумма partition limits может превышать pool maxSize. Три partition по 10 = 30, pool maxSize=20. Это допустимо: не все partition будут на пике одновременно. Oversubscription — нормальная практика (как overbooking авиабилетов). Но: при одновременном пике — pool.Acquire заблокируется даже если partition sem пропустил.

3. Сумма partition limits может быть меньше pool maxSize. Три partition по 3 = 9, pool maxSize=20. 11 слотов pool — не используются partition. Для запросов без partition (или default partition) — доступны.

4. Неизвестная partition при Acquire: если partition name не зарегистрирована — использовать default partition (с `defaultMax` лимитом). Создаётся lazily при первом обращении.

5. Fallback partition: если основная partition exhausted (sem full) — попробовать fallback. Для критического трафика: если "critical" полон → попробовать "overflow". Overflow — shared partition с мягким лимитом. Опционально, по конфигурации.

6. `ResizePartition` — изменить maxConns. Реализация: создать новый семафор с новым capacity, закрыть старый. Active connections на старом семафоре — завершатся естественно (Release уменьшит счётчик).

7. Метрики `Rejected` — количество раз когда partition sem отклонил запрос (TryAcquire returned false, или timeout на AcquireWithContext). Для alerting: `rejected_total{partition="critical"} > 0` — тревога.

8. Race detector обязателен. Atomic для counters (active, total, rejected) — без мьютекса.

## Структура файлов

```
sync-primitives/
├── go.mod
├── main.go
├── semaphore/
│   ├── semaphore.go
│   └── weighted.go
├── safemap/
│   └── safemap.go
├── ratelimit/
│   └── ratelimit.go
├── connpool/
│   ├── pool.go
│   ├── conn.go
│   ├── options.go
│   └── stats.go
├── bufpool/
│   ├── buffer.go
│   ├── slice.go
│   └── bench_test.go
└── bulkhead/
    ├── bulkhead.go     # Bulkhead, Acquire, Release
    ├── partition.go    # Partition, stats
    └── options.go      # functional options
```

## Ожидаемый вывод main.go

```
=== Bulkhead Setup ===
Pool: maxSize=15
Partitions:
  critical:   5 max conns
  normal:     10 max conns
  background: 3 max conns

=== Isolation test ===
Background: 3 goroutines acquired (3/3)
Background: goroutine #4 → rejected (partition full)
Critical:   goroutine #1 → acquired (1/5) ← NOT affected by background
Normal:     goroutine #1 → acquired (1/10) ← NOT affected by background

=== Overload in normal ===
Normal: 10 goroutines acquired (10/10)
Normal: goroutine #11 → waiting...
Critical: goroutine #1 → acquired instantly (1/5) ← isolated from normal

=== Pool exhaustion ===
Critical: 5 acquired, Normal: 10 acquired = 15 total = pool maxSize
Background: goroutine → waiting on POOL (partition has slots, pool full)

=== Fallback ===
Normal: 10/10 (full)
Normal with fallback → trying "overflow" partition → acquired (1/5)

=== Stats ===
critical:   active=2, total=150, rejected=0
normal:     active=8, total=1230, rejected=47
background: active=1, total=890, rejected=312

=== Dynamic resize ===
ResizePartition("critical", 8)
critical: maxConns 5 → 8
New critical goroutines: acquired (6/8, 7/8, 8/8) ← 3 extra slots

=== Close ===
Bulkhead closed. All partitions drained.
```

## Эксперимент на разрушение

**Эксперимент 1: Deadlock от неправильного порядка lock.** Acquire: pool.Acquire → partition.sem.Acquire. Release: partition.sem.Release → pool.Release. Горутина A: pool.Acquire (получила conn) → partition.sem.Acquire (ждёт). Горутина B: partition.sem.Acquire (получила slot) → pool.Acquire (ждёт). A держит pool slot, ждёт partition. B держит partition slot, ждёт pool. Deadlock. Зафиксировать: порядок acquire MUST быть одинаковым. Правильно: partition sem → pool (или pool → partition sem, но одинаково в Acquire и не инвертировано).

Зафиксировать правильный порядок: partition sem первым (дешёвый, быстрый), pool вторым (дорогой, может создать conn). При Release: pool первым (вернуть conn), partition sem вторым (освободить slot). Нет circular dependency: оба ресурса захватываются в одном порядке.

**Эксперимент 2: Oversubscription deadlock.** Три partition по 10, pool maxSize=10. Partition A: 10 sem slots acquired → 10 pool.Acquire. Pool полон. Partition B: sem slot acquired → pool.Acquire → блокировка (pool полон). Partition A не Release потому что работает. B ждёт pool → timeout. Зафиксировать: oversubscription + все partition на пике = pool bottleneck. Partition sem пропускает, pool блокирует. Решение: sum(partition limits) ≤ pool maxSize для hard guarantee. Или: timeout на pool.Acquire после partition sem.

**Эксперимент 3: Partition leak.** Acquire("critical") → получил conn. Release с неправильной partition: Release(conn, "normal"). Partition "critical" sem slot не освобождён. Partition "normal" sem получил лишний Release. Со временем: "critical" исчерпан (leaking slots), "normal" — overcounted. Зафиксировать: conn должен знать свою partition. Или: Acquire возвращает обёртку с partition info.

## Вопросы до написания кода

1. Bulkhead partition sem → pool acquire. Partition sem пропустил (slot есть). Pool acquire заблокировался (pool полон). Горутина держит partition slot и ждёт pool. Другие горутины той же partition — ждут partition sem (slot занят ожидающей). Cascade blocking?

2. Oversubscription: sum(partition limits) > pool maxSize. Когда это безопасно? Когда опасно? Как мониторить?

3. Fallback partition: "normal" полон → попробовать "overflow". "overflow" тоже полон → ошибка. Два sequential sem.Acquire. Первый failed, второй attempted. Время ожидания удваивается?

4. Dynamic resize: partition "critical" 5→8. Три новых горутины acquire. Старый семафор capacity 5 — не вмещает. Как реализовать resize без закрытия существующих connections?

5. Conn должен знать свою partition для корректного Release. Как хранить: поле в Conn? Wrapper? Context value?

6. Partition "critical" пуста (0 active). Pool health check закрыл idle conn. Critical acquire → pool создаёт новый. Latency: partition sem (0ms) + pool factory (~50ms). Как pre-warm critical partition?