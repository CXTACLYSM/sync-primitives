# Модуль 005 — Token Bucket Rate Limiter

## Контекст

Семафор ограничивает конкурентность — "не более N одновременно". Rate limiter ограничивает частоту — "не более N операций в секунду". Это ортогональные ограничения: семафор пропускает 10 запросов одновременно, каждый длится 100ms — throughput 100 req/sec. Rate limiter ограничивает до 50 req/sec — даже если семафор позволяет больше.

Token bucket — классический алгоритм rate limiting. Метафора: ведро с токенами. Каждый запрос забирает один токен. Токены добавляются с фиксированной скоростью (rate). Если ведро пусто — запрос ждёт или отклоняется. Ведро имеет максимальную ёмкость (burst) — позволяет всплески нагрузки: после периода покоя ведро заполнено, burst запросов проходит мгновенно.

В connection pool rate limiter стоит перед семафором: сначала throttle частоту, потом ограничить конкурентность. Два уровня защиты.

## Что нужно реализовать

Новый пакет `ratelimit`.

**Тип `RateLimiter`:**

```go
type RateLimiter struct {
    mu       sync.Mutex
    rate     float64       // токенов в секунду
    burst    int           // максимальная ёмкость ведра
    tokens   float64       // текущее количество токенов
    lastTime time.Time     // момент последнего обновления
    closed   bool
    done     chan struct{}
}
```

**Конструктор `New(rate float64, burst int) *RateLimiter`** — создаёт limiter. `rate` — токенов в секунду (0 = запретить всё). `burst` — максимальное количество токенов (burst ≥ 1). Ведро начинает заполненным (tokens = burst).

**Методы:**

- `Allow() bool` — неблокирующий: есть токен → true (забрать), нет → false. Аналог TryAcquire у семафора.

- `Wait(ctx context.Context) error` — блокирующий: ждать до появления токена или отмены контекста. Возвращает `nil` при успехе, `ctx.Err()` при отмене.

- `WaitN(ctx context.Context, n int) error` — ждать N токенов. Для тяжёлых операций потребляющих несколько "единиц лимита".

- `AllowN(n int) bool` — неблокирующий: есть N токенов → true.

- `Reserve() *Reservation` — зарезервировать токен, вернуть информацию о времени ожидания. Вызывающий код решает: ждать или отклонить.

- `Tokens() float64` — текущее количество токенов (приблизительное).

- `Rate() float64` — текущая скорость.

- `Burst() int` — максимальная ёмкость.

- `SetRate(rate float64)` — динамическое изменение скорости. Под мьютексом.

- `SetBurst(burst int)` — динамическое изменение burst.

- `Close()` — закрыть limiter.

**Тип `Reservation`:**

```go
type Reservation struct {
    ok      bool          // разрешено ли
    tokens  int           // количество зарезервированных токенов
    delay   time.Duration // сколько ждать до использования
    limiter *RateLimiter
}

func (r *Reservation) OK() bool
func (r *Reservation) Delay() time.Duration
func (r *Reservation) Cancel()  // вернуть токены если не использованы
```

**Файл `main.go`** — демонстрация:
- Rate limiter: 10 req/sec, burst 5
- `Allow()`: серия быстрых вызовов — первые 5 true (burst), затем ~1 per 100ms
- `Wait(ctx)`: горутины ждут своей очереди
- `Reserve()`: получить время ожидания, решить ждать или нет
- Динамическое изменение: `SetRate(100)` — увеличить лимит
- Визуализация: вывод timestamp + allow/deny для каждого запроса

## Требования

1. Ленивое обновление токенов (lazy refill). Токены не добавляются горутиной-тикером — они вычисляются при каждом обращении: `tokens += rate × (now - lastTime)`. Это точнее и не требует фоновой горутины.

2. `tokens` — `float64`, не `int`. Rate=10/sec — за 50ms должно появиться 0.5 токена. С `int` — появится 0 (округление вниз), за 100ms — 1 (рывок). Float даёт плавное пополнение.

3. `tokens` ограничен сверху `burst`: `tokens = min(tokens + delta, float64(burst))`. Нельзя накопить больше burst — после долгого простоя ведро заполняется до burst, не больше.

4. `Allow()` и `AllowN()` — неблокирующие. Под мьютексом: обновить токены, проверить достаточно ли, если да — уменьшить, вернуть true. Если нет — не изменять, вернуть false.

5. `Wait(ctx)` — вычислить время до следующего токена: `delay = (1 - tokens) / rate`. Если `tokens ≥ 1` — немедленно. Если нет — `time.NewTimer(delay)` + `select` с `ctx.Done()`. При пробуждении — повторить (другая горутина могла забрать токен).

6. `Reserve()` — "резервация": уменьшить tokens (может стать отрицательным!), вернуть Reservation с delay. Вызывающий код сам решает ждать ли delay. `Cancel()` — вернуть зарезервированные токены если решено не ждать.

7. `SetRate` и `SetBurst` — под мьютексом, с обновлением tokens перед изменением rate (зафиксировать текущее количество до изменения скорости).

8. Race detector обязателен.

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
└── ratelimit/
    └── ratelimit.go
```

## Ожидаемый вывод main.go

```
=== Allow (rate=10/sec, burst=5) ===
[00:00.000] Allow: true  (tokens: 4.0/5)  ← burst
[00:00.000] Allow: true  (tokens: 3.0/5)
[00:00.000] Allow: true  (tokens: 2.0/5)
[00:00.000] Allow: true  (tokens: 1.0/5)
[00:00.000] Allow: true  (tokens: 0.0/5)
[00:00.001] Allow: false (tokens: 0.01/5) ← burst exhausted
[00:00.050] Allow: false (tokens: 0.5/5)  ← half token
[00:00.100] Allow: true  (tokens: 0.0/5)  ← 1 token refilled

=== Wait (rate=5/sec, burst=1) ===
[00:00.000] request 1: passed (0ms wait)
[00:00.200] request 2: passed (200ms wait)
[00:00.400] request 3: passed (200ms wait)
[00:00.600] request 4: passed (200ms wait)
Throughput: 5.0 req/sec ✓

=== Reserve ===
Reservation: delay=150ms, ok=true
Decision: delay < 500ms threshold, will wait
Waited 150ms, proceeding

=== Dynamic rate ===
Before: 10 req/sec → ~10 allowed per second
SetRate(100)
After: 100 req/sec → ~100 allowed per second

=== Burst visualization ===
After 1s idle (burst=5):
  5 requests pass instantly (burst)
  request 6: wait 100ms
  request 7: wait 200ms
```

## Эксперимент на разрушение

**Эксперимент 1: Тикер вместо lazy refill.** Реализовать альтернативный rate limiter через `time.Ticker`:

```go
func NewTickerLimiter(rate int, burst int) *TickerLimiter {
    l := &TickerLimiter{tokens: make(chan struct{}, burst)}
    // Предзаполнить
    for i := 0; i < burst; i++ {
        l.tokens <- struct{}{}
    }
    // Горутина пополнения
    go func() {
        ticker := time.NewTicker(time.Second / time.Duration(rate))
        for range ticker.C {
            select {
            case l.tokens <- struct{}{}:
            default: // ведро полное
            }
        }
    }()
    return l
}
```

Сравнить с lazy refill: (a) Точность при rate=1000/sec — тикер с интервалом 1ms. CPU overhead? (b) Утечка горутины если limiter не закрыт. (c) Первый запрос после долгого простоя: тикер — burst за секунды (по одному тику), lazy — мгновенно (вычислено). (d) Изменение rate: тикер — нужно пересоздать ticker, lazy — поменять float.

Зафиксировать: lazy refill точнее, эффективнее (нет фоновой горутины), проще в управлении. Тикер — понятнее интуитивно, но хуже в production.

**Эксперимент 2: Отрицательные токены.** `Reserve()` может уменьшить tokens ниже нуля. Код:

```go
limiter := New(1, 1)  // 1 tok/sec, burst 1
r1 := limiter.Reserve()  // tokens: 1 → 0, delay=0
r2 := limiter.Reserve()  // tokens: 0 → -1, delay=1s
r3 := limiter.Reserve()  // tokens: -1 → -2, delay=2s
```

Зафиксировать: `Reserve` позволяет "занять из будущего". Три резервации: первая мгновенная, вторая через 1с, третья через 2с. Если r3 отменена (`Cancel`) — tokens: -2 → -1. Если r2 тоже — tokens: -1 → 0. Следующий запрос — мгновенный. Отрицательные токены — кредит: "будущие токены уже распределены".

**Эксперимент 3: Rate = 0.** `New(0, 5)` — rate ноль. `Allow()` — первые 5 true (burst), потом всегда false (ничего не добавляется). `Wait(ctx)` — блокируется навечно (токенов не будет). Зафиксировать: rate=0 — "запретить всё кроме burst". Полезно для emergency throttling: `SetRate(0)` — остановить все запросы. `SetRate(100)` — возобновить.

## Вопросы до написания кода

1. Token bucket vs leaky bucket — в чём разница? Какой позволяет burst, какой — нет? Для API rate limiting — какой предпочтительнее?

2. Lazy refill: `tokens += rate * elapsed.Seconds()`. Если между вызовами прошло 10 минут — `tokens += 10*60*rate`. Tokens clamp до burst. Почему clamp необходим? Что произойдёт без него?

3. `tokens` — float64. Запрос потребляет 1 токен. `tokens = 0.999` — Allow вернёт false (< 1). Через 0.1ms `tokens = 1.0` — Allow вернёт true. Гранулярность rate limiter ограничена точностью float64?

4. `Wait(ctx)` вычисляет delay и создаёт timer. Между вычислением и пробуждением timer — другая горутина могла забрать токен. Как обработать этот race?

5. `Reserve()` делает tokens отрицательными — "кредит из будущего". Зачем разрешать отрицательные? Почему не блокировать как Wait?

6. `rate.Limiter` из `golang.org/x/time/rate` — стандартная реализация. Какой API? Совпадает ли с нашим?