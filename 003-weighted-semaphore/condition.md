# Модуль 003 — Weighted Semaphore

## Контекст

В модулях 001–002 каждый Acquire забирает ровно одно разрешение. Все операции равнозначны. Но в production операции имеют разный "вес": лёгкий запрос к кэшу — 1 unit, SELECT с JOIN — 3 units, bulk INSERT на 10,000 строк — 10 units. Counting semaphore на 10 разрешений пропустит 10 bulk INSERT одновременно — база ляжет. Weighted semaphore позволяет задать вес каждой операции: bulk INSERT забирает 10 из 100 units, лёгкий запрос — 1.

Реализация weighted semaphore сложнее counting: буферизованный канал не поддерживает "отправить N элементов атомарно". Цикл из N send — не атомарен (горутина может получить 5 из 10 и заблокироваться). Нужна другая структура: мьютекс + condition variable + очередь ожидающих с разными весами.

Это первый модуль где канал-семафор из модулей 001–002 заменяется на mutex-based реализацию. Понимание обоих подходов — фундамент для проектирования синхронизационных примитивов.

## Что нужно реализовать

Новый тип `WeightedSemaphore` в пакете `semaphore`.

**Тип `WeightedSemaphore`:**

```go
type WeightedSemaphore struct {
    mu      sync.Mutex
    cond    *sync.Cond
    max     int64
    current int64
    waiters list.List  // очередь ожидающих с весами
    closed  bool
}

type waiter struct {
    weight int64
    ready  chan struct{}  // сигнал "разрешение получено"
}
```

**Конструктор `NewWeighted(maxWeight int64) *WeightedSemaphore`.**

**Методы:**

- `Acquire(ctx context.Context, weight int64) error` — забрать `weight` units. Блокируется если недостаточно свободных. Возвращает `ctx.Err()` при отмене, `ErrSemaphoreClosed` при закрытии, `ErrWeightExceeded` если `weight > max`.

- `TryAcquire(weight int64) bool` — неблокирующая попытка. `true` если `weight` units доступны и забраны.

- `Release(weight int64)` — вернуть `weight` units. Паника если `weight` больше текущего occupied (предотвращение дисбаланса).

- `Available() int64` — свободные units.

- `Close()` — закрыть семафор. Все ожидающие получают ошибку.

**Sentinel errors:**

```go
var (
    ErrSemaphoreClosed = errors.New("semaphore closed")
    ErrWeightExceeded  = errors.New("weight exceeds semaphore capacity")
)
```

**Свойство FIFO:** ожидающие обслуживаются в порядке поступления. Если горутина A запросила 5 units и ждёт, а горутина B запросила 1 unit позже — B не получит разрешение раньше A даже если 1 unit свободен. Это предотвращает starvation тяжёлых запросов.

**Файл `main.go`** — демонстрация:
- Weighted semaphore на 10 units
- Лёгкие операции (weight=1) и тяжёлые (weight=5)
- FIFO порядок: тяжёлая операция не голодает
- TryAcquire с разными весами
- Release с неправильным весом → panic
- Close с ожидающими горутинами

## Требования

1. Внутренняя структура: `sync.Mutex` для защиты состояния, `list.List` для очереди ожидающих (FIFO), `chan struct{}` на каждого waiter для индивидуальной сигнализации.

2. FIFO ordering: когда units освобождаются через Release, проверяется первый waiter в очереди. Если его weight ≤ available — разбудить. Если нет — никого не будить (даже если следующий waiter с меньшим weight поместился бы). Это strict FIFO. Альтернатива (best-fit: будить первого кто поместится) — эффективнее по throughput, но допускает starvation.

3. `Release` паникует при `current - weight < 0`. Это защита от дисбаланса: Release с весом большим чем было Acquire — программная ошибка, не штатная ситуация. Аналог `sync.Mutex.Unlock` без Lock.

4. `Acquire` с `weight > max` — немедленная ошибка `ErrWeightExceeded`. Нет смысла ждать: даже если все units свободны, запрос никогда не будет удовлетворён.

5. `Acquire` с `weight == 0` — немедленный успех без изменения состояния. Zero-weight acquire — no-op.

6. При отмене контекста: waiter удаляется из очереди. Если waiter уже получил сигнал (race между cancel и signal) — units должны быть корректно обработаны (либо использованы, либо возвращены).

7. Race detector: `go run -race` обязателен на всех демонстрациях.

## Структура файлов

```
sync-primitives/
├── go.mod
├── main.go
└── semaphore/
    ├── semaphore.go          # Semaphore (модули 001–002)
    └── weighted.go           # WeightedSemaphore
```

## Ожидаемый вывод main.go

```
=== Weighted Semaphore (max=10) ===
[light-1] acquired weight=1 (used: 1/10)
[light-2] acquired weight=1 (used: 2/10)
[heavy-1] acquired weight=5 (used: 7/10)
[light-3] acquired weight=1 (used: 8/10)
[light-4] acquired weight=1 (used: 9/10)
[heavy-2] waiting weight=5 (need 5, available 1)...
[light-5] waiting weight=1 (FIFO: behind heavy-2)...
[light-1] released weight=1 (used: 8/10)
[light-2] released weight=1 (used: 7/10)
[light-3] released weight=1 (used: 6/10)
[light-4] released weight=1 (used: 5/10)
[heavy-1] released weight=5 (used: 0/10)
[heavy-2] acquired weight=5 (used: 5/10)
[light-5] acquired weight=1 (used: 6/10)

=== FIFO demonstration ===
heavy request (weight=8) arrived first, waiting...
light request (weight=1) arrived second, waiting...
FIFO: light waits behind heavy even though space available

=== TryAcquire ===
Available: 10/10
TryAcquire(5): true, available: 5/10
TryAcquire(6): false (not enough), available: 5/10
TryAcquire(5): true, available: 0/10

=== Weight exceeded ===
Acquire(weight=11, max=10): weight exceeds semaphore capacity

=== Release panic ===
Acquired weight=3
Release(weight=5): panic: semaphore: released more than acquired

=== Close ===
2 goroutines waiting...
Close()
waiter 1: semaphore closed
waiter 2: semaphore closed
```

## Эксперимент на разрушение

**Эксперимент 1: Non-atomic acquire через канал.** Попробовать реализовать weighted acquire через counting semaphore (N отправок в канал):

```go
func (s *Semaphore) AcquireN(ctx context.Context, n int) error {
    for i := 0; i < n; i++ {
        if err := s.AcquireWithContext(ctx); err != nil {
            // Откатить уже полученные
            for j := 0; j < i; j++ {
                s.Release()
            }
            return err
        }
    }
    return nil
}
```

Запустить с 10 горутинами, каждая AcquireN(ctx, 5) на семафоре capacity=10. Зафиксировать: deadlock. Горутина A получила 3 из 5, горутина B получила 3 из 5 — суммарно 6 из 10 занято. A нужно ещё 2, B нужно ещё 2. Свободно 4. Но A заблокирована на 4-м acquire, B заблокирована на 4-м acquire. Ни одна не может завершить — classic deadlock. Rollback в catch — не помогает если timeout не настроен (вечная блокировка до rollback). С timeout — помогает, но throughput деградирует (retry storms).

Зафиксировать: weighted acquire через цикл send — фундаментально broken для конкурентного доступа. Нужна атомарная операция "забрать N units или ничего".

**Эксперимент 2: Starvation без FIFO.** Реализовать best-fit стратегию: при Release будить первого waiter чей weight ≤ available (не обязательно первый в очереди). Запустить: поток лёгких запросов (weight=1, каждые 10ms) и один тяжёлый (weight=8 из max=10). Зафиксировать: тяжёлый запрос никогда не получит разрешение — лёгкие проскакивают постоянно, available колеблется 1-3, никогда не достигает 8. Starvation.

Затем включить FIFO: тяжёлый запрос встал первым в очередь — лёгкие за ним не обслуживаются даже если место есть. Через несколько Release — available достигает 8, тяжёлый получает разрешение, за ним — лёгкие. Нет starvation, но throughput ниже (лёгкие ждут за тяжёлым).

**Эксперимент 3: Race между cancel и signal.** Weighted semaphore, max=5. Горутина A: Acquire(ctx, 3), timeout 50ms. Горутина B: через 40ms делает Release(3). Race: timeout (50ms) vs signal от Release (40ms). В 40ms Release пробует разбудить A (send в A.ready). Если A ещё в select — получит сигнал, acquire успешен. Если A уже обработала timeout — `A.ready` signal потерян? Units утекли?

Запустить 1000 раз с `-race`. Зафиксировать: корректная реализация должна обрабатывать race без утечки. При cancel: waiter удаляется из очереди, если signal уже отправлен в ready — units возвращаются обратно.

## Вопросы до написания кода

1. Почему буферизованный канал не подходит для weighted semaphore? Что конкретно делает цикл "N send" не-атомарным?

2. `sync.Cond` — condition variable. В каких случаях `Cond.Broadcast` предпочтительнее `Cond.Signal`? Для weighted semaphore — какой?

3. FIFO vs best-fit: в каких production сценариях starvation тяжёлых запросов — приемлема? Когда throughput важнее fairness?

4. `Release` паникует при over-release. Альтернатива: вернуть error. Почему паника предпочтительнее для этого случая?

5. `list.List` — linked list из стандартной библиотеки. Почему не `[]waiter` (слайс)? Какая операция критична для выбора структуры данных?

6. Waiter с индивидуальным `chan struct{}` для сигнализации. Почему не один общий канал? Почему не `sync.Cond`?