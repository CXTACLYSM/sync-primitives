# Модуль 004 — QA: Mutex Patterns & sync.RWMutex

## Блок 1: Mutex internals

**Q1.1:** `sync.Mutex` — 8 байтов (`state int32` + `sema uint32`). `sync.RWMutex` — 24 байта (Mutex + readerCount int32 + readerWait int32 + writerSem uint32 + readerSem uint32). Почему RWMutex больше? Что хранят дополнительные поля?

**A1.1:** `readerCount` — количество активных читателей. Атомарно инкрементируется при RLock, декрементируется при RUnlock. `readerWait` — количество читателей которые writer ждёт (зафиксировано при Lock). `writerSem` — runtime semaphore для блокировки writer до завершения всех read locks. `readerSem` — runtime semaphore для блокировки новых readers до завершения write lock. Больше полей = больше координации = больше overhead на каждую операцию. Именно поэтому RWMutex дороже Mutex при низком contention.

**Q1.2:** `sync.Mutex` Normal mode: новая горутина конкурирует с разбуженной. Starvation mode (>1ms ожидания): FIFO. Как переключение между режимами влияет на производительность? Почему не всегда FIFO?

**A1.2:** Normal mode: горутина на CPU (spinner) выигрывает у разбуженной (нужно переключение контекста ~1µs). Throughput выше — мьютекс передаётся без переключения, горутина сразу продолжает. FIFO: каждая передача = wakeup ожидающей горутины (~1-10µs). Throughput ниже. Всегда FIFO — throughput деградирует при высоком contention (бенчмарки показали до 10x разницу). Гибрид: normal для throughput, starvation mode для fairness при длительном ожидании. 1ms threshold — эмпирический: достаточно долго чтобы обнаружить starvation, достаточно коротко чтобы не стать нормой.

**Q1.3:** `sync.Mutex` нерекурсивен: `Lock(); Lock()` = deadlock. Java имеет `ReentrantLock`. Почему Go не поддерживает рекурсивный мьютекс?

**A1.3:** Позиция Go team (Russ Cox): рекурсивный мьютекс маскирует плохой дизайн. Если функция A (под Lock) вызывает функцию B (тоже требует Lock) — это признак что A знает слишком много о реализации B, или Lock слишком грубый. Решения: (1) Разделить на locked/unlocked версии: `func set(key, val)` (unlocked, internal) и `func Set(key, val)` (locked, public). (2) Сузить критическую секцию — Lock только на минимальный участок, вызовы функций — вне Lock. (3) Передать уже захваченный мьютекс через параметр (редко, но допустимо). Рекурсивный мьютекс ≈ `defer` на исправление архитектуры.

---

## Блок 2: RWMutex семантика

**Q2.1:** 5 горутин держат RLock. Writer вызывает Lock. Ещё 3 горутины вызывают RLock. Какой порядок: (a) 3 новых читателя проходят, writer ждёт всех 8? (b) 3 новых читателя блокируются, writer ждёт только 5?

**A2.1:** (b). Go RWMutex: Lock устанавливает `readerCount` в отрицательное значение (atomicAdd(-rwmutexMaxReaders)). Новые RLock видят отрицательный readerCount → блокируются на readerSem. Writer ждёт только 5 текущих читателей (readerWait=5). Когда все 5 RUnlock — writer разблокируется, выполняет write, Unlock. Unlock сбрасывает readerCount в положительное → 3 заблокированных читателя разблокируются. Порядок: [R×5] → [W ждёт 5] → [R×3 ждут W] → [5 RUnlock] → [W Lock+Unlock] → [R×3 RLock].

**Q2.2:** `RLock(); RLock()` из одной горутины — deadlock?

**A2.2:** Нет deadlock — RLock реентрантен для чтения. `readerCount` инкрементируется дважды — одна горутина считается двумя читателями. Обе RUnlock нужны. НО: если между двумя RLock writer вызвал Lock — deadlock! Writer ждёт завершения RLock#1. RLock#2 блокируется (writer waiting). Горутина держит RLock#1 и заблокирована на RLock#2 → writer ждёт горутину → горутина ждёт writer → deadlock. На практике: двойной RLock из одной горутины — баг. Как и рекурсивный Lock. Go не проверяет это — нет понятия "владения" для RLock.

**Q2.3:** RWMutex: writer Unlock. 10 ожидающих читателей и 1 ожидающий writer. Кто разблокируется первым?

**A2.3:** Все 10 читателей. Go RWMutex при writer Unlock: сбрасывает writer flag, разблокирует всех ожидающих читателей на readerSem. Следующий writer будет ждать пока все 10 читателей не сделают RUnlock. Это оптимизация throughput: читатели параллельны, нет смысла пропускать одного writer если 10 читателей ждут — все 10 пройдут параллельно. Следующий writer получит Lock когда все 10 завершат.

---

## Блок 3: SafeMap дизайн

**Q3.1:** `Get` возвращает `(V, bool)` под RLock. Если `V = *Connection` — вызывающий код получает указатель. Мутация `conn.Status = "closed"` — вне RLock. Другая горутина вызывает `Get` тоже получает указатель, читает `conn.Status`. Data race? Как SafeMap может защитить от этого?

**A3.1:** Data race: да. SafeMap защищает доступ к map, не к объектам на которые map ссылается. `conn.Status = "closed"` — запись вне мьютекса. Другая горутина читает `conn.Status` — чтение вне мьютекса. Concurrent read+write без синхронизации = data race. Решения: (1) Connection сама потокобезопасна (внутренний мьютекс или atomic поля). (2) SafeMap хранит значения, не указатели: `SafeMap[string, Connection]` — Get возвращает копию. Мутация копии не влияет на original. Но: копирование дорого для больших struct. (3) Functional update: `sm.Update(key, func(conn *Connection, exists bool) *Connection { conn.Status = "closed"; return conn })` — мутация под Lock. SafeMap обеспечивает атомарность update.

**Q3.2:** `Range` под RLock. Callback модифицирует external state (append в слайс, запись в channel). Это потокобезопасно?

**A3.2:** Depends. `append` в локальный слайс — безопасно (только одна горутина). `append` в shared слайс без собственной синхронизации — data race. Запись в buffered channel — безопасно (channel потокобезопасен). Запись в unbuffered channel может блокировать → RLock держится дольше → writers ждут. Правило: callback в Range должен быть быстрым и неблокирующим. Для тяжёлой обработки: Snapshot → обработка вне Lock.

**Q3.3:** `Clear()` реализация: `m.data = make(map[K]V)` vs `for k := range m.data { delete(m.data, k) }`. Разница?

**A3.3:** `make(map[K]V)` — новая map, старая — garbage (GC соберёт). O(1). Но: все существующие ссылки на старую map (snapshots, etc.) — не затронуты (у них свои map). `delete` в цикле — мутация существующей map. O(n). Memory: map не shrinks при delete (buckets остаются). После delete всех элементов — map пуста но занимает память от пиковой capacity. `make` — чисто: новая map, минимальная memory. Для `Clear()` — `make` предпочтительнее.

---

## Блок 4: Deadlock и копирование

**Q4.1:** Range callback вызывает Delete:
```go
sm.Range(func(k string, v int) bool {
    if v < 0 {
        sm.Delete(k)  // Lock inside RLock
    }
    return true
})
```
Точный механизм deadlock. Сколько горутин заблокировано? Обнаружит ли Go runtime?

**A4.1:** Одна горутина: (1) Вызывает Range → RLock (readerCount=1). (2) Callback → sm.Delete → Lock. (3) Lock видит readerCount > 0, ждёт readerSem. (4) RUnlock никогда не вызовется — Range ещё не вернулся. Одна горутина заблокирована Lock, ожидая себя саму RUnlock. Runtime НЕ обнаружит: deadlock detection проверяет "все горутины спят". Если есть другие активные горутины (HTTP server, timer) — не все спят → нет диагностики. Программа зависает бесшумно. Обнаружение: timeout тестов, watchdog в production, pprof goroutine dump покажет горутину заблокированную на `sync.RWMutex.Lock`.

**Q4.2:** `sm2 := *sm1` — копирование SafeMap (struct с RWMutex). Что `go vet` скажет? Что конкретно копируется? sm1 Lock → sm2 операции — корректны?

**A4.2:** `go vet`: `copies lock value: sync.RWMutex`. Копируется: struct целиком, включая RWMutex с текущим state (если locked — копия тоже "locked"). `sm1.mu.Lock()` → sm1.mu.state = locked. `*sm1` → sm2.mu.state = locked (скопировано). `sm2.mu.Unlock()` — может отработать (state = unlocked), но семантически неверно: никто не делал Lock на sm2. `sm2.mu.Lock()` — если state=locked (от копирования) — deadlock. Поведение undefined: internal semaphore не скопирован корректно (sema — runtime-managed). Правило: SafeMap возвращается по указателю из конструктора → копирование указателя, не структуры.

**Q4.3:** `defer mu.Unlock()` — Unlock вызывается при panic. Но если Lock был на mu, а Unlock — на скопированном mu2 (ошибка) — мьютекс mu остаётся locked. Как `go vet` помогает? Какие ошибки не ловит?

**A4.3:** `go vet` ловит: копирование struct с Mutex (`x = *y` где y содержит Mutex). Не ловит: Unlock на другом мьютексе (логическая ошибка, не синтаксическая), забытый Unlock на одном из путей (defer обычно решает), deadlock из-за Lock ordering. Статический анализ мьютексов — hard problem (halting problem). `go vet` ловит низковисящие фрукты, остальное — code review, тесты, `-race`.

---

## Блок 5: Производительность

**Q5.1:** Бенчмарк: SafeMap с RWMutex vs SafeMap с Mutex. 95% reads, 5% writes. 1 горутина: RWMutex медленнее (overhead). 16 горутин: RWMutex быстрее (параллельные reads). При каком количестве горутин crossover?

**A5.1:** Зависит от hardware (cache coherence protocol, number of cores) и длительности критической секции. Типично: crossover при 2-4 горутинах. При 1 горутине: нет contention, RWMutex overhead (атомарные операции на readerCount) проигрывает простому Mutex CAS. При 2+ горутинах с 95% reads: параллельное чтение компенсирует overhead. Бенчмарк: `go test -bench=. -benchmem -cpu=1,2,4,8,16`. Конкретные числа: Mutex ~50ns/op at 1 CPU, RWMutex ~80ns/op. At 16 CPU: Mutex ~500ns/op (contention), RWMutex ~100ns/op (parallel reads). Crossover: ~2-3 CPU.

**Q5.2:** `SafeMap.Get` под RLock: `m.mu.RLock(); defer m.mu.RUnlock(); return m.data[key]`. Map lookup = ~50-100ns. RLock+RUnlock = ~30-50ns. Overhead от lock = 30-50% от полезной работы. Как уменьшить?

**A5.2:** (1) Batching: `GetMultiple(keys []K) map[K]V` — один RLock, несколько lookups. Amortize lock cost. (2) Sharding: `[N]SafeMap` — разбить на N independent maps по hash(key). Contention на каждом мьютексе в N раз меньше. Java ConcurrentHashMap использует sharding. (3) Read-copy-update (RCU): хранить `atomic.Pointer[map[K]V]`. Reads: `atomic.Load` (no lock). Writes: copy map, modify, `atomic.Store`. Lock-free reads. (4) sync.Map: оптимизирован для read-heavy, amortized lock-free reads. Для текущего проекта: SafeMap с RWMutex — достаточно. Sharding — в проекте 10 (Traffic Arbitration) если profiling покажет bottleneck.

**Q5.3:** `Snapshot()` копирует map. Для map с 100K entries: одна аллокация `make(map[K]V, 100000)` + копирование. Время? Можно ли сделать incremental snapshot?

**A5.3:** `make(map[K]V, 100000)` — одна аллокация buckets array. Копирование 100K entries: O(n), ~1-10ms зависит от размера key/value. RLock держится всё это время — writers заблокированы. Incremental snapshot: нет стандартного способа. Альтернативы: (1) Versioned map: каждый entry имеет version. Snapshot = "все entries с version ≤ snapshot_version". Reads не требуют копирования. (2) Copy-on-write: при модификации — копия только изменённых entries. (3) Sharded snapshot: по одному shard за раз, lock каждый shard отдельно. Для 100K entries × ~100 bytes/entry = ~10MB — копирование за <10ms, приемлемо для non-hot path.

---

## Блок 6: Generics и type safety

**Q6.1:** `SafeMap[string, any]` — ключ string, значение any. Чем это хуже `SafeMap[string, *Connection]`?

**A6.1:** `any` теряет type safety: можно `Set("key", 42)` и `Set("key", "hello")` — разные типы для одного ключа. `Get` возвращает `any` — нужен type assertion. Ошибка типа — runtime panic, не compile error. `SafeMap[string, *Connection]`: `Set("key", 42)` — ошибка компиляции. `Get` возвращает `*Connection` — прямой доступ к полям без assertion. Правило: конкретные типы > any. Использовать any только когда map действительно хранит разнотипные значения (конфигурация, JSON-like структуры).

**Q6.2:** `K comparable` — можно ли использовать `interface{}` как ключ? `safemap.New[any, string]()` — скомпилируется?

**A6.2:** `any` (`interface{}`) является `comparable` — `any` включён в constraint. Скомпилируется. Но: runtime behavior зависит от конкретного типа значения в interface. Если в interface лежит slice — `==` вызовет runtime panic (slice not comparable). `safemap.New[any, string]()` — компилируется, но `sm.Set([]int{1,2}, "val")` — runtime panic при internal map operation (map key comparison). Ложная безопасность: компилятор пропустил, runtime упадёт.

**Q6.3:** Метод `Update(key K, fn func(V, bool) V)`. `fn` вызывается под Lock. `fn` занимает 100ms (тяжёлое вычисление). Все операции на SafeMap заблокированы 100ms. Как вынести тяжёлую работу из-под Lock?

**A6.3:** Паттерн: read → compute outside lock → CAS-like update:
```go
v, exists := sm.Get(key)        // RLock, fast
newV := heavyCompute(v, exists) // no lock, 100ms
sm.Set(key, newV)               // Lock, fast
```
Проблема: между Get и Set — другая горутина могла изменить значение. Решение: `Update` с проверкой:
```go
sm.Update(key, func(current V, exists bool) V {
    if current != expectedOld {
        return current  // кто-то изменил, не перезаписывать
    }
    return newV  // precomputed
})
```
Или: optimistic locking с version counter. Compute → attempt Update → if version changed → retry. Для текущего проекта: если fn быстрый (<1µs) — под Lock. Если медленный — precompute + conditional update.