# Модуль 002 — Context-Aware Semaphore

## Контекст

В модуле 001 семафор имеет блокирующий `Acquire()` — горутина ждёт бесконечно. В production это неприемлемо: HTTP handler с timeout 30 секунд не может ждать семафор 5 минут. Клиент уже получил 504 Gateway Timeout и ушёл, а горутина всё ещё висит на `Acquire`, удерживая ресурсы.

`context.Context` решает эту проблему. Context несёт deadline, timeout и сигнал отмены. `AcquireWithContext(ctx)` ожидает разрешение ИЛИ отмену контекста — что произойдёт раньше. Это прямое применение паттерна `select` с `ctx.Done()` из workerpool-practice (модуль 004), но теперь в контексте синхронизационного примитива.

Второй аспект модуля — корректная обработка отмены. Когда контекст отменён, горутина должна не просто вернуть ошибку, а сделать это без утечки ресурсов: не забрать разрешение, которое потом не будет возвращено.

## Что нужно реализовать

Расширение пакета `semaphore` из модуля 001.

**Новые методы `Semaphore`:**

- `AcquireWithContext(ctx context.Context) error` — ожидает разрешение с учётом контекста. Возвращает `nil` при успешном acquire, `ctx.Err()` при отмене/timeout. Если контекст уже отменён при вызове — немедленный возврат ошибки без попытки acquire.

- `AcquireWithTimeout(timeout time.Duration) error` — convenience обёртка: создаёт context с timeout, вызывает `AcquireWithContext`. Возвращает `context.DeadlineExceeded` при timeout.

- `AcquireWithDeadline(deadline time.Time) error` — convenience обёртка: создаёт context с deadline.

**Тип ошибки:**

```go
var ErrSemaphoreClosed = errors.New("semaphore closed")
```

**Метод `Close()`** — закрывает семафор. Все ожидающие `AcquireWithContext` получают `ErrSemaphoreClosed`. Новые вызовы `AcquireWithContext` немедленно возвращают `ErrSemaphoreClosed`. Реализуется через отдельный канал `done` и `sync.Once`.

**Файл `main.go`** — демонстрация:
- `AcquireWithTimeout`: успешный acquire за time < timeout
- `AcquireWithTimeout`: timeout на полном семафоре
- `AcquireWithContext` с ручной отменой (`context.WithCancel`)
- `AcquireWithContext` с уже отменённым контекстом — немедленный возврат
- `Close()`: ожидающие горутины получают ошибку
- Race detector: `go run -race`

## Требования

1. `AcquireWithContext` реализуется через `select` с двумя case: отправка в канал (acquire) и `ctx.Done()` (отмена). Ветка `default` отсутствует — операция блокирующая с возможностью отмены.

2. Быстрая проверка контекста перед `select`: если `ctx.Err() != nil` — немедленный возврат. Это оптимизация: не входить в `select` если контекст уже отменён. Без этой проверки `select` может выбрать ветку acquire (если канал не полон) даже при отменённом контексте — Go `select` при готовности нескольких веток выбирает случайно.

3. `Close()` через `sync.Once` — гарантирует однократное закрытие. Повторный `Close()` — no-op. Закрытие канала `done` (`close(done)`) — сигнал для всех ожидающих `select`: `case <-done` разблокируется у всех горутин одновременно (broadcast).

4. После `Close()`: `Acquire()` (блокирующий из модуля 001) — поведение не определено (может заблокироваться навечно). `AcquireWithContext` — немедленно возвращает `ErrSemaphoreClosed`. `TryAcquire` — возвращает `false`. `Release` — работает (разблокирует оставшиеся, если есть).

5. `AcquireWithContext` с `select` из трёх веток:
   ```go
   select {
   case s.ch <- struct{}{}:
       return nil
   case <-ctx.Done():
       return ctx.Err()
   case <-s.done:
       return ErrSemaphoreClosed
   }
   ```

6. `AcquireWithTimeout` и `AcquireWithDeadline` — thin wrappers. Не дублируют логику `select`. Создают context, defer cancel, вызывают `AcquireWithContext`.

## Структура файлов

```
sync-primitives/
├── go.mod
├── main.go
└── semaphore/
    └── semaphore.go
```

## Ожидаемый вывод main.go

```
=== AcquireWithTimeout: success ===
Acquired in 0ms (timeout: 1s)

=== AcquireWithTimeout: timeout ===
Semaphore full (3/3)
AcquireWithTimeout(200ms): context deadline exceeded (waited 200ms)

=== AcquireWithContext: cancel ===
Started waiting for semaphore...
Cancelled after 100ms
AcquireWithContext: context canceled

=== AcquireWithContext: already cancelled ===
AcquireWithContext with cancelled ctx: context canceled (immediate return)

=== Close ===
3 goroutines waiting on full semaphore...
Closing semaphore...
goroutine 1: semaphore closed
goroutine 2: semaphore closed
goroutine 3: semaphore closed
New AcquireWithContext after close: semaphore closed

=== Race detector ===
All operations completed with -race: OK
```

## Эксперимент на разрушение

**Эксперимент 1: Контекст отменён, но acquire успел.** Семафор на 1. Контекст с timeout 100ms. Горутина делает `AcquireWithContext`. Семафор свободен — acquire мгновенный, timeout не срабатывает. Затем: горутина делает тяжёлую работу 200ms, вызывает `Release`. К моменту Release контекст уже отменён. Вопрос: Release работает корректно? Контекст влияет на Release?

Зафиксировать: контекст влияет ТОЛЬКО на Acquire. Release не принимает контекст и не проверяет его. Это by design: если разрешение получено — оно должно быть возвращено, независимо от состояния контекста. Привязка Release к контексту — баг (разрешение "утечёт" если контекст отменён до Release).

**Эксперимент 2: Race между acquire и cancel.** Семафор на 1, занят. 100 горутин вызывают `AcquireWithContext(ctx)` с timeout 50ms. Одновременно — горутина с Release каждые 10ms. Некоторые горутины успеют acquire, некоторые — timeout. Запустить с `-race`. Проверить: (a) нет data races, (b) количество successful acquires + timeouts = 100, (c) ни одно разрешение не утекло (в конце `Available() == MaxConcurrency()`).

Зафиксировать: `select` с двумя ready ветками — Go выбирает случайно. Горутина может получить timeout даже когда семафор освободился в тот же момент. Это не баг — это fair scheduling.

**Эксперимент 3: Close без ожидающих.** Семафор на 5, все свободны. `Close()`. Затем `AcquireWithContext(ctx)`. Зафиксировать: `ErrSemaphoreClosed` без блокировки. Затем: семафор на 5, 3 разрешения заняты (3 горутины работают). `Close()`. 3 горутины завершают работу, вызывают Release. Проверить: Release после Close — не паника? Не блокировка?

Зафиксировать поведение Release после Close: чтение из канала `ch` — если в канале есть элементы (от Acquire) — успешно. Если канал пуст — блокировка (баг вызывающего кода, не семафора). Close не закрывает `ch` — только `done`. Это важно: закрытие `ch` при наличии ожидающих send — panic.

## Вопросы до написания кода

1. `select` с двумя готовыми ветками — какая будет выбрана? Можно ли контролировать приоритет? Как это влияет на поведение `AcquireWithContext` когда и канал свободен, и контекст отменён одновременно?

2. `context.WithTimeout(parent, 5*time.Second)` возвращает `(ctx, cancel)`. Почему `cancel` нужно вызывать даже если timeout сработал? Что утекает если не вызвать?

3. `Close()` закрывает канал `done`, но не канал `ch` (семафор). Почему нельзя закрыть `ch`? Что произойдёт если горутина делает `ch <- struct{}{}` (Acquire) на закрытом канале?

4. `sync.Once` для Close — зачем? Что произойдёт при двойном `close(done)` без Once? А при двойном Close с Once?

5. Быстрая проверка `ctx.Err() != nil` перед `select` — зачем? Ведь `select` и так проверит `ctx.Done()`. В каком сценарии без этой проверки горутина получит разрешение при отменённом контексте?

6. `AcquireWithTimeout` создаёт `context.WithTimeout` и defer cancel. Если acquire успешен за 1ms при timeout 5s — cancel вызовется через defer при выходе. Утечёт ли горутина таймера на 5 секунд?