# Модуль 002 — Theory: Context, отмена и управление временем жизни

## context.Context: зачем и как

### Проблема

Горутина заблокирована на `Acquire()`. Клиент HTTP запроса отключился. Сервер завершается (graceful shutdown). Timeout SLA истёк. Во всех случаях — горутина должна прекратить ожидание. Но `Acquire()` не знает о внешних событиях: он блокируется на канале и ждёт бесконечно.

`context.Context` — механизм Go для передачи сигналов отмены, deadline и request-scoped данных через call chain. Горутина проверяет `ctx.Done()` — канал, который закрывается при отмене контекста.

### Иерархия контекстов

```go
ctx := context.Background()                        // корень, никогда не отменяется
ctx, cancel := context.WithCancel(ctx)              // отменяется вызовом cancel()
ctx, cancel := context.WithTimeout(ctx, 5*time.Second)  // отменяется через 5 сек или cancel()
ctx, cancel := context.WithDeadline(ctx, time.Now().Add(5*time.Second))  // то же, абсолютное время
```

Контексты образуют дерево. Отмена родителя отменяет всех потомков. Отмена потомка не затрагивает родителя:

```
Background
├── WithTimeout(5s)      ← отмена через 5с отменяет всё ниже
│   ├── WithCancel()     ← ручная отмена, отменяет только этот и ниже
│   │   └── горутина A
│   └── горутина B
└── горутина C            ← не затронута отменой WithTimeout
```

### ctx.Done() — канал для select

`ctx.Done()` возвращает `<-chan struct{}` — канал который закрывается при отмене контекста. Закрытие канала — broadcast: все горутины заблокированные на чтении из него разблокируются одновременно.

```go
select {
case s.ch <- struct{}{}:
    return nil          // acquire успешен
case <-ctx.Done():
    return ctx.Err()    // контекст отменён
}
```

`ctx.Err()` возвращает причину отмены:
- `context.Canceled` — вызван `cancel()`
- `context.DeadlineExceeded` — истёк timeout или deadline

### ctx.Err() перед select: зачем

```go
func (s *Semaphore) AcquireWithContext(ctx context.Context) error {
    // Быстрая проверка
    if ctx.Err() != nil {
        return ctx.Err()
    }
    
    select {
    case s.ch <- struct{}{}:
        return nil
    case <-ctx.Done():
        return ctx.Err()
    }
}
```

Зачем проверка перед `select`? Если контекст уже отменён И канал не полон — `select` видит две готовые ветки. Go выбирает случайно. С вероятностью ~50% горутина ПОЛУЧИТ разрешение при отменённом контексте. Это приведёт к:

1. Acquire возвращает `nil` (успех), но контекст отменён
2. Вызывающий код возможно уже обработал отмену и не вызовет Release
3. Утечка разрешения

Быстрая проверка `ctx.Err() != nil` исключает этот сценарий: если контекст мёртв — не пытаться acquire.

Контраргумент: иногда хочется получить разрешение даже при отменённом контексте (если операция всё равно нужна). В таком случае быструю проверку убирают осознанно. Для семафора — проверка правильнее: отменённый контекст = "отмените операцию".

## select с несколькими ветками

### Семантика выбора

`select` с несколькими готовыми ветками — Go runtime выбирает случайно (uniform random). Не по порядку, не по приоритету. Это сознательное решение для предотвращения starvation:

```go
select {
case s.ch <- struct{}{}:   // ветка 1: acquire
    return nil
case <-ctx.Done():          // ветка 2: cancel
    return ctx.Err()
case <-s.done:              // ветка 3: closed
    return ErrSemaphoreClosed
}
```

Если все три готовы — каждая выбирается с вероятностью 1/3.

### Приоритет через вложенный select

Если нужен приоритет (сначала проверить cancel, потом acquire):

```go
// Проверка контекста — приоритетна
select {
case <-ctx.Done():
    return ctx.Err()
case <-s.done:
    return ErrSemaphoreClosed
default:
    // ни cancel, ни close — попробовать acquire
}

// Acquire с отменой
select {
case s.ch <- struct{}{}:
    return nil
case <-ctx.Done():
    return ctx.Err()
case <-s.done:
    return ErrSemaphoreClosed
}
```

Первый `select` с `default` — неблокирующая проверка. Если cancel/close — немедленный возврат. Если нет — переходим ко второму `select` (блокирующий, без default). Это двойной-select паттерн: проверить приоритетные условия, затем ждать.

## close(channel) как broadcast

### Механизм

`close(ch)` — специальная операция на канале. После close:
- Отправка → panic
- Чтение → немедленно возвращает zero value (для `chan struct{}` — `struct{}{}`)
- Все горутины заблокированные на чтении — разблокируются одновременно

Это broadcast: один сигнал, все получатели. Обычная отправка в канал — unicast: один сигнал, один получатель.

### Применение для Close() семафора

```go
type Semaphore struct {
    ch   chan struct{}
    done chan struct{}
    once sync.Once
}

func (s *Semaphore) Close() {
    s.once.Do(func() {
        close(s.done)
    })
}
```

`close(s.done)` разблокирует ВСЕ горутины ожидающие в `select { case <-s.done: }`. Каждая получит `ErrSemaphoreClosed`. Без `sync.Once` — повторный `close(s.done)` — panic: `close of closed channel`.

### Почему не закрывать ch

`s.ch` — канал семафора. На нём горутины выполняют send (Acquire). `close(s.ch)` → все ожидающие send получат panic (`send on closed channel`). Это катастрофа, не graceful shutdown. `done` — отдельный канал только для сигнализации закрытия. Разделение ответственности: `ch` — для семафорной логики, `done` — для lifecycle.

## sync.Once: однократное выполнение

### Проблема

Несколько горутин одновременно вызывают `Close()`. Без синхронизации — `close(done)` выполнится несколько раз → panic. `sync.Mutex` — работает, но verbose:

```go
s.mu.Lock()
if !s.closed {
    close(s.done)
    s.closed = true
}
s.mu.Unlock()
```

### sync.Once — элегантное решение

```go
s.once.Do(func() {
    close(s.done)
})
```

`Do` гарантирует: функция выполняется ровно один раз, даже при конкурентных вызовах. Все вызывающие блокируются до завершения первого выполнения. После первого — `Do` возвращается немедленно без вызова функции.

Внутри: `once.Do` использует atomic для fast path (проверка "уже выполнено" — одна атомарная операция) и mutex для slow path (синхронизация конкурентных первых вызовов).

### Once и паника

Если функция внутри `Do` паникует — `once` считает выполнение завершённым. Повторный `Do` не вызовет функцию снова. Паника propagates к вызывающему. Это важно: если `close(done)` паникует (невозможно если done не nil, но гипотетически) — семафор остаётся "наполовину закрытым".

## context.WithTimeout: таймер и утечка

### Создание и cancel

```go
ctx, cancel := context.WithTimeout(parentCtx, 5*time.Second)
defer cancel()  // ОБЯЗАТЕЛЬНО
```

`WithTimeout` создаёт внутренний таймер (`time.AfterFunc`). Если timeout срабатывает — контекст отменяется. Если операция завершается раньше — таймер всё ещё активен. `cancel()` останавливает таймер и освобождает ресурсы.

### Утечка без cancel

```go
ctx, _ := context.WithTimeout(parentCtx, 5*time.Second)
// забыли cancel
```

Таймер живёт 5 секунд. Горутина таймера удерживает ссылку на context и его children. GC не может собрать context до срабатывания таймера. Для одного вызова — не проблема. Для тысяч вызовов в секунду — тысячи живых таймеров, утечка памяти и горутин.

`go vet` предупреждает: `the cancel function returned by context.WithTimeout should be called, not discarded`. `defer cancel()` — обязательный паттерн, аналог `defer f.Close()`.

### cancel после успешного acquire

```go
func (s *Semaphore) AcquireWithTimeout(timeout time.Duration) error {
    ctx, cancel := context.WithTimeout(context.Background(), timeout)
    defer cancel()  // вызовется даже при успешном acquire
    return s.AcquireWithContext(ctx)
}
```

Acquire за 1ms, timeout 5s. `defer cancel()` срабатывает при выходе из функции — останавливает 5-секундный таймер. Без defer — таймер живёт 5 секунд впустую.

## Convenience wrappers: тонкие обёртки

### Принцип

`AcquireWithTimeout` и `AcquireWithDeadline` — не новая логика, а convenience API:

```go
func (s *Semaphore) AcquireWithTimeout(timeout time.Duration) error {
    ctx, cancel := context.WithTimeout(context.Background(), timeout)
    defer cancel()
    return s.AcquireWithContext(ctx)
}

func (s *Semaphore) AcquireWithDeadline(deadline time.Time) error {
    ctx, cancel := context.WithDeadline(context.Background(), deadline)
    defer cancel()
    return s.AcquireWithContext(ctx)
}
```

Вся логика — в `AcquireWithContext`. Обёртки создают контекст и делегируют. Дублирования кода нет. Изменения в логике acquire — в одном месте.

### Когда обёртки оправданы

Обёртки добавляют удобство: `sem.AcquireWithTimeout(5*time.Second)` — одна строка вместо трёх (create context, defer cancel, acquire). Для частого использования — экономит boilerplate. Для редкого — можно вызывать `AcquireWithContext` напрямую с кастомным контекстом.

## Вопросы для самопроверки

1. `AcquireWithContext` — `select` с тремя ветками. Все три готовы. Каков механизм выбора? Можно ли гарантировать что cancel будет выбран раньше acquire?

2. `context.WithTimeout(ctx, 0)` — timeout ноль секунд. Контекст уже отменён при создании? Или отменяется "на следующем тике"?

3. `Close()` вызван. 5 горутин ждут в `select`. Все 5 разблокируются одновременно? Или по одной? Какой механизм обеспечивает broadcast?

4. После `Close()` вызывается `AcquireWithContext(ctx)`. `select` видит `<-s.done` (ready) и `s.ch <- struct{}{}` (ready, канал не полон). Какая ветка выбрана — случайно. Может ли горутина получить разрешение после Close?

5. `sync.Once.Do(f)` — `f` паникует. Второй `Do(f)` — вызовет ли `f` снова? Что увидит вызывающий код?