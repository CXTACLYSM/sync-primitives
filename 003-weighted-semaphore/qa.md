# Модуль 003 — QA: Weighted Semaphore

## Блок 1: Атомарность и deadlock

**Q1.1:** Weighted acquire через цикл send (канал-семафор): горутина A запросила 5, горутина B запросила 5, capacity=8. A получила 3, B получила 3. Оставшиеся 2 слота — каждой нужно ещё 2. Обе заблокированы. Описать формально: почему это deadlock, а не livelock? Может ли ситуация разрешиться сама?

**A1.1:** Deadlock: обе горутины заблокированы на send в канал, ни одна не может продвинуться. Send разблокируется только если кто-то сделает receive (Release). Но Release делается после завершения acquire — а acquire не завершён. Circular dependency: A ждёт слот ← B держит слоты ← B ждёт слот ← A держит слоты. Не livelock: горутины не делают бесполезную работу, они спят. Не может разрешиться сама: нет третьей стороны которая сделает Release. Единственный выход: timeout (если AcquireWithContext) → rollback (release полученных). Но rollback сам может deadlock-нуть если обе горутины timeout одновременно и обе делают Release одновременно — тогда разрешится (обе откатят). Стохастическое разрешение, не гарантированное.

**Q1.2:** WeightedSemaphore: `Acquire(ctx, 5)` — атомарная операция. Почему под мьютексом "проверить + изменить" — атомарно, а цикл send — нет? Ведь мьютекс тоже можно потерять между check и set?

**A1.2:** Мьютекс гарантирует: между Lock и Unlock — ни одна другая горутина не выполняет код под тем же мьютексом. "Проверить (current+weight ≤ max) + изменить (current += weight)" выполняется без прерываний. Цикл send: каждый send — отдельная операция с отдельным lock канала. Между send #3 и send #4 — мьютекс канала отпускается, другая горутина может вклиниться. Ключевое отличие: мьютекс держится на протяжении всей check-and-set операции. Канал — lock берётся и отпускается на каждый send отдельно.

**Q1.3:** Можно ли реализовать weighted acquire через один send в канал с буфером capacity=1, передавая weight как payload? `ch <- weight`. Какие проблемы?

**A1.3:** Проблемы: (1) receive (Release) получит weight — но чей? Если в канале несколько send от разных горутин — FIFO канала определяет порядок, но Release не знает какой weight извлечь. (2) Канал с buffer 1: один send блокируется до receive. Вторая горутина с send — заблокирована. Это binary semaphore, не weighted. (3) Канал не считает "суммарный вес" — он считает "количество элементов". Weighted семантика (суммарный вес ≤ max) принципиально отличается от counting (количество элементов ≤ cap). Канал не подходит.

---

## Блок 2: FIFO и starvation

**Q2.1:** Strict FIFO: тяжёлый запрос (weight=8, max=10) первый в очереди. За ним 20 лёгких (weight=1). Available=7. Все 20 лёгких ждут, хотя могли бы работать. Суммарная потеря throughput: 20 запросов × время ожидания. Когда эта потеря приемлема?

**A2.1:** Приемлема когда: (1) Fairness важнее throughput — SLA гарантирует что запрос обслужен за N секунд, starvation нарушает SLA. (2) Тяжёлые запросы редки — ожидание лёгких случается нечасто, суммарная потеря мала. (3) Тяжёлые запросы критичны — batch import, миграция данных, отчётность — задержка неприемлема. Неприемлема когда: (1) Тяжёлые запросы частые и available колеблется около их weight — постоянная блокировка лёгких. (2) Real-time система — каждая миллисекунда задержки стоит денег. (3) Тяжёлые запросы могут быть разбиты на мелкие — лучше 8 запросов по weight=1 чем один weight=8.

**Q2.2:** Best-fit стратегия: Release будит первого waiter чей weight ≤ available. Поток лёгких запросов weight=1 каждые 5ms. Один тяжёлый weight=9, max=10. Среднее время Release — 20ms. Посчитать: сколько лёгких проскочит между каждым Release? Получит ли тяжёлый разрешение?

**A2.2:** Между каждым Release (20ms): ~4 новых лёгких запроса (5ms interval). При Release одного лёгкого: available 1→2→1 (один ушёл, один пришёл). Available колеблется 1-3. Тяжёлый нуждается в 9 свободных. Для этого нужно чтобы 9 из ~10 активных лёгких завершились без замены. Вероятность: каждый лёгкий живёт ~20ms, за 20ms приходят ~4 новых. Стационарное состояние: ~4-5 активных лёгких, available ~5-6. Тяжёлый может получить разрешение в моменты когда случайно совпало завершение нескольких лёгких. Но гарантии нет — стохастически может ждать секунды или минуты. Starvation не абсолютная но латентность непредсказуемая.

**Q2.3:** Гибрид FIFO + skip: проверять первые K=3 waiters, будить подходящего. Waiter перед которым проскочили — его skip_count++. Когда skip_count > M — он становится "приоритетным" (никто не проскакивает). Описать: как это предотвращает starvation? Как выбрать M?

**A2.3:** Механизм: каждый Release проверяет первые 3 waiters. Если w1 не помещается, но w2 помещается — w2 будят, w1.skip_count++. Когда w1.skip_count > M — w1 становится head-of-line: никто не может проскочить, strict FIFO для w1. Starvation предотвращена: максимум M skip-ов, потом гарантированное обслуживание. Выбор M: M=0 → strict FIFO. M=∞ → best-fit. M=5-10 — компромисс: лёгкие проскакивают 5-10 раз, потом тяжёлый блокирует. На практике M подбирается по SLA: если max acceptable latency = 1s, средний Release interval = 50ms — M ≈ 1s/50ms = 20 skip-ов.

---

## Блок 3: sync.Cond глубже

**Q3.1:** `sync.Cond.Wait()` — "атомарно Unlock + sleep". Что значит "атомарно" здесь? Может ли другая горутина захватить мьютекс между Unlock и sleep?

**A3.1:** "Атомарно" — на уровне runtime: горутина добавляется в wait list Cond И отпускает мьютекс в одной runtime операции. Никакая другая горутина не может увидеть мьютекс unlocked без wait-горутины в wait list. Последовательность: (1) Add goroutine to Cond wait list (under Cond internal lock). (2) Unlock mutex. Другая горутина может захватить мьютекс после шага 2, но wait-горутина уже в wait list и будет разбужена Signal/Broadcast. Без атомарности: горутина отпускает мьютекс, другая горутина захватывает, делает Signal ПЕРЕД тем как первая вошла в sleep. Signal потерян — missed wakeup. Атомарность предотвращает это.

**Q3.2:** `Cond.Signal()` будит одного. `Cond.Broadcast()` будит всех. Для weighted semaphore Release освобождает W units. В очереди: w1(3), w2(5), w3(1). Signal разбудит w1 (одного). Но после пробуждения w1 (current -= 3) — может хватить и для w3. Кто разбудит w3?

**A3.2:** Никто — при использовании Signal. w1 проснётся, получит мьютекс, проверит условие (3 ≤ available) → true → acquire. w3 всё ещё спит. Units для w3 есть, но Signal был один. Решение: Broadcast вместо Signal. Все просыпаются, проверяют условие, подходящие acquire, остальные засыпают. Или: w1 после своего acquire вызывает Signal/Broadcast чтобы разбудить следующих. Это "cascading wakeup" — каждый разбуженный будит следующего. Сложнее, но точнее чем Broadcast (не будит тех кто точно не поместится).

**Q3.3:** `Cond.Wait()` не принимает context. Как добавить timeout к Cond-based semaphore?

**A3.3:** Нет встроенного способа. Workarounds: (1) Отдельная горутина-таймер: `go func() { time.Sleep(timeout); cond.Broadcast() }()` — будит все ожидающие, они проверяют timeout, таймаутнувшие выходят. Грубо, но работает. (2) Использовать канал вместо Cond (подход из condition.md). (3) Периодический wakeup: `for !condition && time.Now().Before(deadline) { cond.Wait() }` — но Wait блокируется навсегда без внешнего Signal. Нужен фоновый ticker с Broadcast. (4) `x/sync/semaphore` — использует внутреннюю реализацию с timeout, не Cond. Именно поэтому в текущем проекте — каналы для waiter signaling, не Cond.

---

## Блок 4: Race между cancel и signal

**Q4.1:** Горутина в select: `case <-ready` и `case <-ctx.Done()`. Release делает `close(ready)`. Одновременно ctx отменяется. Обе ветки ready. Select выбирает ctx.Done. Горутина возвращает error. Но units уже выделены (Release увеличил current и закрыл ready). Куда делись units?

**A4.1:** Units потеряны — allocated но не используются, Release не будет вызван (горутина вернула ошибку). Утечка: current увеличен на weight, но Release не вызовется. Семафор постепенно "высыхает". Решение: при cancel, проверить ready канал:
```go
case <-ctx.Done():
    s.mu.Lock()
    select {
    case <-w.ready:
        // Signal уже отправлен — units выделены. Вернуть.
        s.current -= w.weight
        s.notifyWaitersLocked()
    default:
        // Signal не отправлен — удалить из очереди
        s.removeWaiter(w)
    }
    s.mu.Unlock()
    return ctx.Err()
```
Под мьютексом: если ready закрыт — units allocated, вернуть (current -= weight) и разбудить следующих. Если not closed — удалить из очереди, units не были выделены.

**Q4.2:** Почему `close(ready)` а не `ready <- struct{}{}`? Что произойдёт если горутина ещё не вошла в select когда Release делает signal?

**A4.2:** `close(ready)`: read из закрытого канала — немедленно возвращает zero value. Горутина войдёт в select позже — `<-ready` сразу ready. Не потеряется. `ready <- struct{}{}` в unbuffered: если горутина не в select — send блокируется. Release под мьютексом → deadlock (Release держит мьютекс, ждёт send, горутина ждёт мьютекс). С buffered(1): send не блокируется, горутина получит позже. ОК, но `close` проще и идиоматичнее для one-shot signal. `close` → broadcast (но один reader → OK). `send` → unicast. Для one-shot one-reader: оба работают, `close` безопаснее.

**Q4.3:** Stress test: 100 горутин, weighted semaphore max=20, каждая Acquire(ctx, random 1-10) с timeout 100ms. Горутина-verifier каждые 10ms проверяет: `current ≤ max`. В конце (все горутины завершились): `current == 0`. Если current ≠ 0 — утечка. Как написать этот тест? Что ловит `-race`?

**A4.3:** Тест:
```go
func TestNoLeak(t *testing.T) {
    sem := NewWeighted(20)
    var wg sync.WaitGroup
    for i := 0; i < 100; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            w := rand.Int63n(10) + 1
            ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
            defer cancel()
            if err := sem.Acquire(ctx, w); err == nil {
                time.Sleep(time.Duration(rand.Intn(50)) * time.Millisecond)
                sem.Release(w)
            }
        }()
    }
    wg.Wait()
    if sem.current != 0 {
        t.Fatalf("leak: current=%d, expected 0", sem.current)
    }
}
```
`-race` ловит: конкурентный доступ к `current` без мьютекса (если забыт), чтение/запись `waiters` list без мьютекса, доступ к `closed` без синхронизации. Не ловит: логические ошибки (утечку units при cancel race) — для этого assertion `current == 0` в конце.

---

## Блок 5: Сравнение с x/sync/semaphore

**Q5.1:** `golang.org/x/sync/semaphore.Weighted` — стандартная реализация. Описать как она обрабатывает cancel race. Использует ли она Cond или каналы?

**A5.1:** `x/sync/semaphore` использует `sync.Mutex` + `list.List` waiters + channel per waiter — тот же подход что в этом модуле. При cancel: под мьютексом проверяет, получил ли waiter уже signal. Если да — units были allocated, вызывает внутренний `notifyWaiters` чтобы вернуть. Если нет — удаляет из очереди. Не использует `sync.Cond` — именно из-за отсутствия context support. Код: `golang.org/x/sync/semaphore/semaphore.go` — ~100 строк, хорошо документирован, рекомендуется к изучению после написания своей реализации.

**Q5.2:** Своя реализация vs `x/sync/semaphore`. Когда использовать свою?

**A5.2:** `x/sync/semaphore` — для production. Протестирован, отлажен, поддерживается Go team. Свою — для обучения (этот проект) и когда нужна кастомная функциональность: Close() (x/sync не имеет), FIFO с bounded skip, метрики (wait time, acquire count), bulkhead naming. Правило: не изобретать примитивы синхронизации для production если есть проверенная реализация. Исключение: специфические требования не покрытые стандартной.

**Q5.3:** `x/sync/semaphore.Acquire(ctx, 0)` — weight=0. Что произойдёт? А `Release(0)`?

**A5.3:** `Acquire(ctx, 0)` — немедленный возврат nil. Zero weight acquire — no-op (не меняет state, не блокируется). Полезно для conditional acquire: `weight := calculateWeight(req); sem.Acquire(ctx, weight)` — если weight=0, не блокировать. `Release(0)` — no-op, не паникует. Consistent: acquire 0 = release 0 = nothing.

---

## Блок 6: Дизайн и архитектура

**Q6.1:** WeightedSemaphore хранит `current int64` (занято). Альтернатива: `available int64` (свободно). Плюсы/минусы каждого?

**A6.1:** `current` (occupied): `Acquire: current += weight`, `Release: current -= weight`, check: `current + weight ≤ max`. `available` (free): `Acquire: available -= weight`, `Release: available += weight`, check: `available ≥ weight`. Эквивалентны математически (`available = max - current`). `current` интуитивнее для метрик: "сколько ресурсов используется". `available` интуитивнее для check: "хватает ли". Over-release: `current` → `current < 0` → panic. `available` → `available > max` → менее очевидный инвариант. `x/sync/semaphore` использует `cur` (occupied). Выбор — вопрос конвенции, не корректности.

**Q6.2:** `Release(weight)` паникует при over-release. В production: горутина запаниковала, recovery в HTTP handler поймал. Но семафор в inconsistent state? Или паника в Release — unrecoverable?

**A6.2:** Семафор в consistent state: паника происходит ДО модификации current (`if current - weight < 0 { panic }` ПЕРЕД `current -= weight`). Мьютекс: паника внутри Lock/Unlock — мьютекс остаётся locked. `defer mu.Unlock()` решает: defer выполняется при панике, мьютекс освобождается. Если panic без defer Unlock — мьютекс locked навсегда → все последующие Acquire deadlock. Правило: всегда `s.mu.Lock(); defer s.mu.Unlock()` — даже если паника "невозможна".

**Q6.3:** WeightedSemaphore max=100. В пике: 50 горутин с weight=1 (total=50) и 5 горутин с weight=10 (total=50). Суммарно=100=max. Все работают. Новый запрос weight=1 — ждёт. Один из weight=10 завершается → Release(10). Под FIFO: новый weight=1 получит 1 из 10 освободившихся. Оставшиеся 9 — idle до следующего waiter. Utilization: 91/100. Это проблема?

**A6.3:** Utilization 91% — хороший показатель. "Idle" 9 units — не потеря: они доступны для следующего запроса. Проблема была бы если первый waiter — weight=15 (не помещается, 10 < 15). Тогда 10 units idle пока не накопится 15 свободных. FIFO + крупные запросы → периодическое underutilization. Для metrics: отслеживать `available / max` — если часто > 50% при наличии waiters → вес запросов слишком велик для capacity, нужно увеличить max или уменьшить weight (разбить тяжёлые операции).