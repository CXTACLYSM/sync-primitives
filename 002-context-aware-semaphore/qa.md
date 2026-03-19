# Модуль 002 — QA: Context-Aware Semaphore

## Блок 1: select и конкурентный выбор

**Q1.1:** `AcquireWithContext` — `select` с тремя ветками: send в `ch`, `<-ctx.Done()`, `<-s.done`. Семафор свободен (ветка 1 ready) и контекст отменён (ветка 2 ready). `select` выбирает случайно. С вероятностью ~50% горутина получит разрешение при отменённом контексте. Описать полный сценарий: что происходит дальше? Кто вызовет Release?

**A1.1:** Горутина получает разрешение, `AcquireWithContext` возвращает `nil`. Вызывающий код проверяет `err == nil` → true → начинает работу → `defer sem.Release()`. Контекст отменён, но acquire успешен — код работает нормально. Release вызовется через defer. Нет утечки. Проблема: вызывающий код может также проверять `ctx.Err()` и решить не делать работу, но Release всё равно вызовется через defer. Это корректное поведение — но потратило один slot семафора на работу которая "не нужна". Быстрая проверка `ctx.Err()` перед select — предотвращает этот waste, но не гарантирует (между проверкой и select контекст может отмениться).

**Q1.2:** Можно ли реализовать приоритетный select в Go без вложенных select? Например: "всегда выбирать cancel если ready, независимо от других веток."

**A1.2:** Нет. Go `select` не поддерживает приоритет. Единственный механизм — двойной select:
```go
// Приоритетная проверка (неблокирующая)
select {
case <-ctx.Done():
    return ctx.Err()
default:
}
// Основной select (блокирующий)
select {
case ch <- struct{}{}:
    return nil
case <-ctx.Done():
    return ctx.Err()
}
```
Первый select с default — если cancel ready, вернуть немедленно. Если нет — перейти ко второму. Между двумя select — окно гонки (cancel может прийти), но это минимизировано. Альтернатива: `reflect.Select` позволяет программный контроль порядка, но это рефлексия — медленнее и сложнее.

**Q1.3:** `select {}` (пустой, без веток) — что произойдёт? Когда это используется?

**A1.3:** Вечная блокировка. Горутина заблокирована навсегда — нет веток, нечего выбирать. Runtime: если все горутины заблокированы — `fatal error: all goroutines are asleep - deadlock!`. Использование: `main` функция в программах которые работают через горутины (серверы): `select {}` в конце main — "ждать вечно, пока горутины работают". Менее идиоматично чем `signal.NotifyContext` + `<-ctx.Done()`, но встречается в legacy коде.

---

## Блок 2: context.Context internals

**Q2.1:** `context.WithTimeout(parent, 5*time.Second)` — что конкретно создаётся? Горутина? Таймер? Какой тип возвращается?

**A2.1:** Возвращается `*cancelCtx` (или `*timerCtx` — internal тип). Внутри: ссылка на parent, `chan struct{}` для Done(), `time.Timer` на 5 секунд. НЕ горутина — таймер реализован через runtime timer (heap-based, управляется Go scheduler). При срабатывании таймера: `cancel()` вызывается автоматически, `Done()` канал закрывается, `Err()` возвращает `DeadlineExceeded`. `cancel()` вручную: останавливает таймер, закрывает Done, устанавливает Err=Canceled. `defer cancel()` — останавливает таймер если операция завершилась раньше timeout.

**Q2.2:** `context.Background()` vs `context.TODO()`. Оба возвращают пустой контекст. В чём разница? Когда какой?

**A2.2:** Функционально идентичны — оба `emptyCtx`, никогда не отменяются, без значений. Семантическая разница: `Background()` — осознанный выбор: "мне не нужен контекст отмены здесь" (корень дерева контекстов, main, init). `TODO()` — placeholder: "здесь нужен контекст, но пока непонятно какой — доработаю позже". `TODO()` — маркер технического долга. Grep по `context.TODO()` в кодовой базе показывает где не хватает propagation контекста. В production коде `TODO()` не должно остаться.

**Q2.3:** `ctx, cancel := context.WithCancel(parent)`. `cancel()` вызван дважды. Паника? Что произойдёт?

**A2.3:** Нет паники. Второй `cancel()` — no-op. Внутри `cancel` проверяет: если `Done()` канал уже закрыт — ничего не делает. Это by design: `defer cancel()` вызовется при выходе из функции, даже если cancel уже вызван ранее (timeout сработал, или cancel вызван явно). Идемпотентность cancel — гарантия безопасности при множественных вызовах.

**Q2.4:** Цепочка контекстов: `parent` → `child1 (WithTimeout 5s)` → `child2 (WithCancel)`. Через 5 секунд parent timeout. Что произойдёт с child1 и child2?

**A2.4:** Ловушка в вопросе: parent — `context.Background()` (не timeout). `child1` — WithTimeout 5s от parent. Через 5 секунд `child1` отменяется. `child2` — потомок child1 — тоже отменяется (отмена propagates вниз). Parent не затронут. Если parent сам WithTimeout 3s → child1 отменится через 3 секунды (parent timeout раньше child), child2 — тоже. Эффективный deadline child1 = min(parent deadline, own deadline).

---

## Блок 3: Close и lifecycle

**Q3.1:** `Close()` закрывает `done` канал. 10 горутин ждут в `select`. Все 10 видят `<-s.done` как ready. Но `select` в каждой горутине имеет и другие ветки (acquire, ctx.Done). Гарантировано ли что все 10 получат `ErrSemaphoreClosed`?

**A3.1:** Не гарантировано для каждой индивидуальной горутины. Если у горутины ветка acquire тоже ready (семафор не полон) — select может выбрать acquire. Горутина получит разрешение вместо ошибки. Но: если семафор полон (все slots заняты) — acquire ветка не ready, единственные ready — ctx.Done и s.done. Между ними — 50/50. Если контекст не отменён — s.done единственная ready ветка → `ErrSemaphoreClosed`. На практике: Close вызывается при полном или частично заполненном семафоре. Ожидающие горутины заблокированы на acquire (канал полон) → s.done — единственная ready → все получат closed.

**Q3.2:** После Close: `Release()` — что произойдёт? `ch` не закрыт, в нём лежат элементы от прошлых Acquire. Release читает из `ch`.

**A3.2:** Release работает: чтение из `ch` успешно, элемент извлекается. `ch` не закрыт (close закрыл только `done`). Release не проверяет `done`. Это корректно: горутина получившая разрешение ДО close должна иметь возможность вернуть его. Без этого — утечка разрешений. Порядок: Close() → ожидающие получают ошибку → работающие горутины завершают → Release → ch опустошается. После всех Release: ch пуст, done закрыт, семафор "мёртв" но в чистом состоянии.

**Q3.3:** `Close()` вызван. Затем `Acquire()` (блокирующий, из модуля 001, без context). Что произойдёт?

**A3.3:** `Acquire()` = `s.ch <- struct{}{}`. Если канал не полон — отправка успешна, горутина получает разрешение. Acquire не знает о Close (не проверяет `done`). Если канал полон — вечная блокировка (нет `<-s.done` в select). Это by design: `Acquire()` — legacy API без context-awareness. В документации: "после Close используйте только AcquireWithContext". Или: добавить проверку `done` в `Acquire()` через select:
```go
func (s *Semaphore) Acquire() error {
    select {
    case s.ch <- struct{}{}:
        return nil
    case <-s.done:
        return ErrSemaphoreClosed
    }
}
```
Но это меняет сигнатуру (добавляет `error`). Архитектурное решение: оставить `Acquire()` без error для обратной совместимости или обновить.

---

## Блок 4: Утечки ресурсов

**Q4.1:** `AcquireWithTimeout(5*time.Second)` создаёт `context.WithTimeout`. Внутри контекста — timer. Функция вернула `context.DeadlineExceeded` (timeout). `defer cancel()` вызывается. Вопрос: если бы defer cancel НЕ вызывался — что именно утекает и на какой срок?

**A4.1:** Утекает: internal timer в runtime timer heap. Timer уже сработал (timeout), но context structure ещё жива. Без cancel: context и его Done channel остаются в памяти до GC. Parent context держит ссылку на child (для propagation). Если parent — `context.Background()` (живёт вечно) — child не собирается GC пока parent жив. `cancel()` удаляет child из parent tree + освобождает ресурсы. На практике для одного вызова — trivial leak. Для тысяч в секунду — memory grows linearly. `go vet` предупреждает о missing cancel.

**Q4.2:** Горутина вызвала `AcquireWithContext(ctx)`, получила разрешение. Затем горутина запаниковала (nil pointer dereference). `defer sem.Release()` был установлен. Разрешение вернётся?

**A4.2:** Зависит от recovery. Если паника не перехвачена (`recover`) — горутина завершается, все defer выполняются ДО завершения. `defer sem.Release()` выполнится. Разрешение вернётся. Если recovery где-то выше по стеку (middleware, pool manager) — defer тоже выполнится (defer + recover — стандартный паттерн). Единственный случай утечки: `runtime.Goexit()` БЕЗ defer (если defer не был установлен до Goexit). На практике: паника с defer Release — разрешение не утекает.

**Q4.3:** 1000 горутин вызывают `AcquireWithContext(ctx)` с timeout 1 секунда. Семафор полностью занят. Через 1 секунду все 1000 получают `DeadlineExceeded`. Проблема: 1000 горутин создались, подождали, вернули ошибку. Это thundering herd на timeout — 1000 горутин одновременно обрабатывают ошибку. Как смягчить?

**A4.3:** Варианты: (1) `TryAcquire` вместо `AcquireWithContext` — немедленный отказ без ожидания, нет накопления ожидающих. (2) Rate limiter перед семафором (модуль 005) — ограничить количество попыток в секунду. (3) Ограничить очередь ожидающих: если уже N горутин ждут — новые получают ошибку немедленно (backpressure). (4) Jitter на timeout: не все 1000 с одинаковым timeout, а разброс ±20% — timeout срабатывает не одновременно. (5) Circuit breaker: после N timeout подряд — отклонять без попытки acquire.

---

## Блок 5: Паттерны использования

**Q5.1:** HTTP handler:
```go
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    if err := h.sem.AcquireWithContext(r.Context()); err != nil {
        http.Error(w, "service busy", http.StatusServiceUnavailable)
        return
    }
    defer h.sem.Release()
    // ... обработка запроса
}
```
`r.Context()` отменяется при отключении клиента. Клиент отключился во время ожидания семафора. Что получит клиент? Что произойдёт с горутиной?

**A5.1:** Клиент уже отключён — ничего не получит (TCP connection closed). Горутина: `AcquireWithContext` видит `ctx.Done()` → возвращает `context.Canceled`. Handler вызывает `http.Error(w, ...)` — запись в закрытый ResponseWriter. Ошибка записи — молча игнорируется (net/http обрабатывает). Горутина завершается. Семафор: разрешение НЕ было получено (acquire вернул ошибку) → Release НЕ вызывается (defer не достигнут) → нет утечки. Чистое завершение.

**Q5.2:** Горутина получила разрешение. Работает 5 секунд. Контекст (timeout 3s) отменяется через 3 секунды. Горутина всё ещё работает (не проверяет ctx.Done внутри). Через 5 секунд — Release. Проблема?

**A5.2:** Формально нет проблемы с семафором: разрешение получено, работа выполнена, Release вызван. Семафор корректен. Проблема с бизнес-логикой: работа заняла 5 секунд при timeout 3 — клиент уже получил таймаут. Результат работы — бесполезен. Семафор был занят 2 лишних секунды (3→5). Решение: внутри тяжёлой работы проверять `ctx.Done()` и прерываться рано:
```go
for _, item := range items {
    select {
    case <-ctx.Done():
        return ctx.Err()  // ранний выход
    default:
    }
    process(item)
}
```

**Q5.3:** Семафор как middleware:
```go
func SemaphoreMiddleware(sem *Semaphore) Middleware {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            if err := sem.AcquireWithContext(r.Context()); err != nil {
                http.Error(w, "busy", 503)
                return
            }
            defer sem.Release()
            next.ServeHTTP(w, r)
        })
    }
}
```
Как это связано с middleware из модуля 009 проекта 1? Куда в chain поставить этот middleware — до или после Recovery?

**A5.3:** Это middleware pattern из проекта 1 (модуль 009): `func(Handler) Handler`, замыкание захватывает `sem`. В chain: `Chain(Logging, Recovery, SemaphoreMiddleware(sem), ...)`. Semaphore ПОСЛЕ Recovery: если handler паникует — Recovery перехватит, вернёт 500, defer Release сработает (паника → defer → Release). Если semaphore ДО Recovery: паника внутри handler → defer Release → ОК, но если паника в самом middleware (маловероятно) — Recovery не перехватит. Безопаснее: Recovery первым (ближе к handler), Semaphore — вторым.

---

## Блок 6: Сравнения и edge cases

**Q6.1:** `AcquireWithContext(context.Background())` — Background никогда не отменяется. Эквивалентно ли это `Acquire()` из модуля 001?

**A6.1:** Почти, но не полностью. `Acquire()` = `ch <- struct{}{}` — один send. `AcquireWithContext(Background())` = `select { case ch <- ...: case <-ctx.Done(): case <-s.done: }` — select с тремя ветками. Background.Done() возвращает nil channel — чтение из nil channel блокируется навсегда → ветка никогда не ready. `s.done` — если не закрыт, тоже не ready. Эффективно — `select` с одной ready-able веткой = эквивалентно прямому send. Но: overhead select (~20-50ns vs ~10ns для прямого send). Для горячих путей — может быть измеримо. Также: `AcquireWithContext` проверяет `Close()` через `s.done`, `Acquire` — нет.

**Q6.2:** `context.WithTimeout(ctx, -1*time.Second)` — отрицательный timeout. Что произойдёт?

**A6.2:** Контекст создаётся уже отменённым. `ctx.Err()` == `context.DeadlineExceeded` немедленно. `ctx.Done()` — уже закрытый канал. `AcquireWithContext(ctx)` — быстрая проверка `ctx.Err() != nil` → немедленный возврат `DeadlineExceeded`. Горутина не блокируется. Это не panic, не undefined behavior — корректно обрабатывается. Полезно знать для defensive programming: валидировать timeout перед созданием контекста.

**Q6.3:** Семафор на 1. `AcquireWithContext` — горутина A получила разрешение. Горутина B ждёт. Горутина A паникует, `defer Release()` срабатывает. Горутина B получает разрешение. Между panic А и acquire B — есть ли окно когда семафор "пуст" (разрешение возвращено, но B ещё не получил)?

**A6.3:** Да. Release A → элемент извлечён из ch → ch пуст. B заблокирована на send в select. Runtime разблокирует B: она отправляет в ch. Между read (Release A) и send (Acquire B) — окно где `Available() == 1`. Другая горутина C с `TryAcquire` в этот момент — успеет. Это TOCTOU: Release и следующий Acquire — не атомарная операция. Для семафора это нормально (не нарушает инвариант max concurrency). Для strict ordering — нужна очередь (FIFO semaphore, модуль 003).