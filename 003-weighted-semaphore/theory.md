# Модуль 003 — Theory: Weighted Semaphore, sync.Cond и FIFO fairness

## Проблема: атомарность N-acquire

### Почему канал не работает

Counting semaphore через канал: один send = один acquire. Для weighted acquire (N units) — N send. Но N send — не атомарная операция:

```go
// Горутина A: acquire 5 из 10
for i := 0; i < 5; i++ {
    ch <- struct{}{}  // каждый send — отдельная операция
}
```

Между send #3 и send #4 горутина может быть preempted. Другая горутина может сделать свои send. Две горутины, каждая запрашивает 5 из 10:

```
A: send #1 → OK (total: 1/10)
B: send #1 → OK (total: 2/10)
A: send #2 → OK (total: 3/10)
B: send #2 → OK (total: 4/10)
A: send #3 → OK (total: 5/10)
B: send #3 → OK (total: 6/10)
A: send #4 → OK (total: 7/10)
B: send #4 → OK (total: 8/10)
A: send #5 → OK (total: 9/10)
B: send #5 → OK (total: 10/10)
```

В этом случае — повезло, обе получили по 5. Но:

```
A: send #1..#3 → OK (total: 3/10)
B: send #1..#3 → OK (total: 6/10)
C: send #1..#3 → OK (total: 9/10)
A: send #4 → OK (total: 10/10)
A: send #5 → БЛОКИРОВКА (канал полон)
B: send #4 → БЛОКИРОВКА
C: send #4 → БЛОКИРОВКА
```

Deadlock: A занял 4 слота, B — 3, C — 3. Каждому нужно ещё. Свободных — 0. Никто не может завершить acquire, никто не вызовет release.

### Решение: атомарная проверка + ожидание

Weighted acquire должен проверить "достаточно ли units" и "забрать их" как одну атомарную операцию:

```go
func (s *WeightedSemaphore) tryAcquireUnlocked(weight int64) bool {
    if s.current + weight <= s.max && s.waiters.Len() == 0 {
        s.current += weight
        return true
    }
    return false
}
```

Под мьютексом: проверка + модификация — атомарны. Если не хватает — встать в очередь и ждать. При Release — проверить первого в очереди.

## Архитектура WeightedSemaphore

### Структура

```go
type WeightedSemaphore struct {
    mu      sync.Mutex
    max     int64
    current int64        // занято units
    waiters list.List    // очередь ожидающих (FIFO)
    closed  bool
}

type waiter struct {
    weight int64
    ready  chan struct{}  // индивидуальный сигнал
}
```

`mu` защищает всё состояние. `current` — сколько units занято. `waiters` — linked list ожидающих. Каждый waiter имеет свой `ready` канал для индивидуальной сигнализации.

### Acquire: алгоритм

```
1. Lock mutex
2. Если weight > max → unlock, return ErrWeightExceeded
3. Если weight == 0 → unlock, return nil
4. Если closed → unlock, return ErrSemaphoreClosed
5. Если current + weight ≤ max И очередь пуста → current += weight, unlock, return nil
6. Создать waiter{weight, make(chan struct{})}, добавить в конец очереди
7. Unlock mutex
8. select { case <-waiter.ready: return nil; case <-ctx.Done(): cancel waiter }
```

Шаг 5: проверка "очередь пуста" — часть FIFO. Если в очереди кто-то ждёт — новый запрос встаёт за ним, даже если units достаточно. Без этой проверки — новый лёгкий запрос проскочит мимо ожидающего тяжёлого → starvation.

Шаг 8: горутина ждёт на `ready` канале. Release отправит в `ready` когда units освободятся.

### Release: алгоритм

```
1. Lock mutex
2. current -= weight
3. Если current < 0 → panic (over-release)
4. Пока очередь не пуста:
   a. Взять первого waiter
   b. Если current + waiter.weight ≤ max:
      - current += waiter.weight
      - Удалить из очереди
      - close(waiter.ready)  ← разбудить
   c. Иначе: break (не хватает, перестать проверять)
5. Unlock mutex
```

Шаг 4c: strict FIFO — если первый waiter не помещается, не проверять остальных. Даже если второй с меньшим weight поместился бы.

Цикл в шаге 4: один Release может разбудить нескольких waiters. Если освободилось 10 units, а в очереди waiters с weights 3, 3, 3 — все три будут разбужены.

### Cancel: удаление из очереди

Контекст отменён, горутина в select получила `<-ctx.Done()`:

```go
case <-ctx.Done():
    s.mu.Lock()
    // Удалить waiter из очереди
    for e := s.waiters.Front(); e != nil; e = e.Next() {
        if e.Value.(*waiter) == w {
            s.waiters.Remove(e)
            break
        }
    }
    s.mu.Unlock()
    return ctx.Err()
```

Race condition: между `<-ctx.Done()` и `s.mu.Lock()` — Release мог уже разбудить этого waiter (send в `ready`). Нужна проверка: если `ready` уже получен сигнал — units уже выделены, нельзя просто return error. Нужно либо использовать units, либо вернуть их:

```go
case <-ctx.Done():
    s.mu.Lock()
    select {
    case <-w.ready:
        // Race: уже разбужен — units выделены. Вернуть.
        s.current -= w.weight
        s.notifyWaitersLocked()
    default:
        // Не разбужен — удалить из очереди
        // ... remove from list
    }
    s.mu.Unlock()
    return ctx.Err()
```

Это один из самых тонких моментов в реализации — race между cancel и signal.

## sync.Cond: альтернативный подход

### Что такое Cond

`sync.Cond` — condition variable. Позволяет горутине ждать выполнения условия, блокируясь и освобождая мьютекс:

```go
cond := sync.NewCond(&mu)

// Ожидающий:
mu.Lock()
for !condition {
    cond.Wait()  // атомарно: unlock mu + sleep; при пробуждении: lock mu
}
// condition == true, mu locked
mu.Unlock()

// Сигнализатор:
mu.Lock()
condition = true
cond.Signal()  // разбудить одного ожидающего
mu.Unlock()
```

### Cond для weighted semaphore

```go
func (s *WeightedSemaphore) Acquire(weight int64) {
    s.mu.Lock()
    for s.current + weight > s.max {
        s.cond.Wait()
    }
    s.current += weight
    s.mu.Unlock()
}

func (s *WeightedSemaphore) Release(weight int64) {
    s.mu.Lock()
    s.current -= weight
    s.cond.Broadcast()  // разбудить всех ожидающих
    s.mu.Unlock()
}
```

Проще, но проблемы:

1. **Нет FIFO.** `Broadcast` будит всех. Первая горутина которая получит мьютекс и увидит достаточно units — забирает. Порядок пробуждения не гарантирован → starvation возможен.

2. **Нет context support.** `Cond.Wait()` не принимает context. Нет timeout, нет cancel. Для production — критичный недостаток.

3. **Thundering herd.** `Broadcast` будит ALL ожидающих. Если 100 горутин ждут, а освободился 1 unit — все 100 просыпаются, проверяют условие, 99 снова засыпают. O(N) wakeups при каждом Release.

### Почему индивидуальные каналы лучше

Каждый waiter с `chan struct{}`:
- FIFO: Release будит первого в очереди (не всех)
- Context: `select { case <-ready: case <-ctx.Done(): }` — нативная поддержка
- Нет thundering herd: один Release будит одного (или нескольких, ровно столько сколько поместится)
- Точечное пробуждение: не 100 горутин, а 1–3 которые реально получат units

Недостаток: аллокация канала на каждый waiter. Для тысяч одновременных waiters — ощутимо. Для десятков — несущественно.

## FIFO vs Best-Fit

### FIFO (strict ordering)

Release проверяет первого в очереди. Если не помещается — не проверять остальных. Тяжёлый запрос первый в очереди → лёгкие за ним ждут.

**Плюсы:** нет starvation, предсказуемая латентность (время в очереди пропорционально позиции).
**Минусы:** throughput ниже. 8 свободных units, первый waiter хочет 10, второй — 1. Второй мог бы работать сейчас, но ждёт.

### Best-Fit (max throughput)

Release проверяет всех waiters, будит первого кто помещается. Маленький запрос проскакивает мимо большого.

**Плюсы:** максимальный throughput, utilization семафора ближе к 100%.
**Минусы:** starvation больших запросов. Постоянный поток мелких — большой никогда не получит разрешение.

### Гибрид: FIFO с bounded skip

Проверять первые K waiters, будить первого подходящего среди них. K=1 — strict FIFO. K=∞ — best-fit. K=3–5 — компромисс: проверить несколько, starvation ограничена (максимум K-1 мелких проскочат перед крупным).

Для текущего проекта: strict FIFO. Это проще, предсказуемее, и starvation — неприемлемый failure mode для connection pool.

## list.List: linked list для очереди

### Почему не слайс

Очередь waiters — insert в конец, remove из начала или середины (при cancel).

Слайс: insert O(1) amortized (`append`), remove из начала O(n) (сдвиг), remove из середины O(n). Для частых remove из начала (каждый Release) — O(n) на каждый Release.

Linked list: insert O(1), remove O(1) (если есть pointer на element). `list.List` возвращает `*Element` при insert — сохранить его в waiter, использовать для O(1) remove.

```go
elem := s.waiters.PushBack(w)
// ... позже при cancel:
s.waiters.Remove(elem)  // O(1)
```

### list.List API

```go
import "container/list"

l := list.New()
e := l.PushBack(value)    // добавить в конец, вернуть *Element
l.PushFront(value)         // добавить в начало
l.Remove(e)                // удалить элемент
front := l.Front()         // первый элемент (nil если пуст)
l.Len()                    // количество элементов
e.Value                    // значение (interface{})
e.Next()                   // следующий элемент
```

`e.Value` — `interface{}` (pre-generics API). Нужен type assertion: `w := e.Value.(*waiter)`. С Go 1.18+ можно обернуть в generic type, но `list.List` сам не параметризован.

## Release: пробуждение нескольких waiters

### Каскадное пробуждение

Один Release может разбудить нескольких waiters:

```
Очередь: [w1(3), w2(2), w3(5), w4(1)]
current=10, max=10
Release(7) → current=3

Проверка w1: 3+3=6 ≤ 10 → будить. current=6
Проверка w2: 6+2=8 ≤ 10 → будить. current=8
Проверка w3: 8+5=13 > 10 → стоп (FIFO: не проверять w4)
```

Результат: w1 и w2 разбужены, w3 и w4 ждут. current=8. При следующем Release(2+): w3 сможет пройти.

### close(ready) vs send

Для сигнализации waiter можно использовать `close(ready)` или `ready <- struct{}{}`:

- `close`: broadcast всем читающим из ready. Но у ready один читатель — OK. Нельзя повторно использовать канал.
- `send`: unicast. Можно повторно использовать если нужно (не нужно для waiter).

`close` предпочтительнее: не блокируется если читатель ещё не вошёл в select (буфер не нужен). `send` в небуферизованный канал блокируется если читатель не готов. Для waiter.ready: `make(chan struct{})` (unbuffered) + `close` — безопасно.

Альтернатива: `make(chan struct{}, 1)` (buffered) + `send` — тоже безопасно, send не блокируется.

## Вопросы для самопроверки

1. WeightedSemaphore max=10. Горутина A: Acquire(ctx, 7). Горутина B: Acquire(ctx, 7). Обе ждут. Release(4): current=10→6. W1(7): 6+7=13>10 — нет. Никто не разбужен. Ещё Release(4): current=6→2. W1(7): 2+7=9≤10 — будить. A получает 7. current=9. W2(7): 9+7=16>10 — ждёт. Корректно ли это поведение?

2. FIFO гарантирует порядок обслуживания. Но не гарантирует порядок завершения: w1(3) разбужен первым, но его работа длится 10 секунд. w2(1) разбужен вторым, работа 1ms. w2 завершится и вызовет Release раньше w1. Нарушает ли это FIFO свойство семафора?

3. `Release(weight)` паникует при over-release. Горутина не ловит панику. Весь процесс упадёт. Альтернатива: вернуть error. Почему для синхронизационного примитива паника лучше чем error?

4. `list.List` — не generic. `e.Value` — `interface{}`. Каждый доступ — type assertion. С Go 1.18+ есть generics. Можно ли создать `type TypedList[T any]` обёртку? Стоит ли?

5. Waiter отменил контекст. Release уже отправил signal в ready. Race: горутина видит и `<-ctx.Done()` и `<-ready` в select. Если выбран ctx.Done — units "потеряны" (allocated но не используются). Описать механизм возврата units при этом race.