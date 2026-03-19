# Модуль 004 — Theory: Мьютексы, RWMutex и защита shared state

## sync.Mutex: exclusive lock

### Суть

`sync.Mutex` — простейший примитив синхронизации. Два состояния: locked и unlocked. Два метода: `Lock()` (заблокировать) и `Unlock()` (разблокировать). Между Lock и Unlock — критическая секция: только одна горутина за раз.

```go
var mu sync.Mutex
var counter int

mu.Lock()
counter++  // критическая секция — только одна горутина
mu.Unlock()
```

### Внутренняя реализация

`sync.Mutex` — это `int32` (state) + semaphore:

```go
type Mutex struct {
    state int32
    sema  uint32
}
```

`state` кодирует: locked/unlocked, есть ли ожидающие, находится ли в starvation mode. `sema` — runtime semaphore для блокировки ожидающих горутин.

**Fast path** (без contention): `Lock` пытается `atomic.CompareAndSwapInt32(&m.state, 0, locked)`. Если успех — мьютекс захвачен за одну атомарную операцию (~1-2 наносекунды). Unlock: `atomic.AddInt32(&m.state, -locked)`.

**Slow path** (contention): CAS не удался (мьютекс уже locked). Горутина: (1) спинит (spin) несколько итераций на multicore CPU — надежда что holder отпустит быстро. (2) Если спин не помог — паркуется через runtime semaphore (переход в kernel, ~microseconds).

### Normal mode vs Starvation mode

Go 1.9 добавил fairness. Два режима:

**Normal mode:** новые горутины конкурируют с только что разбуженными. Новые горутины имеют преимущество — они уже на CPU, разбуженная горутина ещё получает CPU time. Это максимизирует throughput (горутина на CPU быстрее разбуженной), но может starve ожидающих.

**Starvation mode:** если горутина ждёт Lock > 1ms — мьютекс переключается в starvation mode. Новые горутины не конкурируют — Unlock передаёт владение первому в очереди (FIFO). Throughput ниже, но no starvation. Когда очередь пуста или первый ожидающий ждал < 1ms — обратно в normal mode.

Это всё скрыто за API `Lock()/Unlock()`. Разработчику не нужно выбирать режим — мьютекс адаптируется автоматически.

### Правила использования

1. **Lock и Unlock в одной горутине.** Go runtime проверяет (начиная с Go 1.8+): Unlock из другой горутины — `fatal error`. Это отличие от семафора (модуль 001).

2. **Не рекурсивен.** `Lock(); Lock()` — deadlock. Горутина ждёт сама себя. Go намеренно не поддерживает recursive mutex (в отличие от Java `ReentrantLock`) — это design smell.

3. **defer Unlock.** Всегда `mu.Lock(); defer mu.Unlock()`. Ранний return, паника — defer сработает. Без defer: одна забытая ветка `if` → мьютекс залочен навсегда.

4. **Не копировать.** `go vet` предупреждает: `copies lock value`. Копирование мьютекса — undefined behavior.

## sync.RWMutex: readers-writer lock

### Суть

`sync.RWMutex` разделяет доступ:
- `RLock()/RUnlock()` — read lock. Множество горутин одновременно.
- `Lock()/Unlock()` — write lock. Exclusive: блокирует и читателей, и писателей.

```go
var mu sync.RWMutex
var data map[string]int

// Читатель:
mu.RLock()
v := data["key"]  // безопасно — другие читатели тоже здесь
mu.RUnlock()

// Писатель:
mu.Lock()
data["key"] = 42  // exclusive — ни читателей, ни писателей
mu.Unlock()
```

### Когда RWMutex лучше Mutex

RWMutex выгоден при:
- Чтение значительно чаще записи (>90% reads)
- Критическая секция чтения не тривиальна (>100ns)
- Много конкурентных читателей

RWMutex хуже Mutex при:
- Примерно равном количестве read/write — overhead RWMutex (два atomic + state management) превышает выгоду параллельного чтения
- Критическая секция < 100ns — overhead lock/unlock доминирует
- Мало горутин — нет contention, нет выгоды от параллелизма

Эмпирическое правило: начать с `sync.Mutex`. Перейти на `RWMutex` после профилирования если lock contention — bottleneck и reads доминируют.

### Writer starvation prevention

Naive RWMutex: пока есть активные читатели — новые читатели проходят, writer ждёт. Постоянный поток читателей → writer starves.

Go `sync.RWMutex`: когда writer вызывает `Lock()`:
1. Устанавливает флаг "writer waiting"
2. Ждёт завершения текущих читателей
3. Новые `RLock()` блокируются (видят флаг writer waiting)

Это гарантирует: writer ждёт не дольше чем max(длительность текущих read locks). Новые читатели не проскакивают мимо ждущего writer.

### RLock + Lock = deadlock

```go
mu.RLock()
mu.Lock()  // DEADLOCK
```

Горутина держит RLock. Lock ждёт завершения ВСЕХ RLock. Текущая горутина — один из читателей. Lock ждёт текущую горутину → горутина ждёт сама себя → deadlock.

Go runtime НЕ обнаруживает этот deadlock (одна горутина заблокирована, но другие могут быть активны). Программа зависает без диагностики. Только code review и тестирование обнаруживают.

## Паттерн: defer Unlock

### Обязательность

```go
func (m *SafeMap[K, V]) Set(key K, value V) {
    m.mu.Lock()
    defer m.mu.Unlock()
    m.data[key] = value
}
```

Без defer:
```go
func (m *SafeMap[K, V]) BrokenSet(key K, value V) {
    m.mu.Lock()
    if key == "" {
        return  // мьютекс залочен навсегда!
    }
    m.data[key] = value
    m.mu.Unlock()
}
```

Одна забытая ветка = production incident. `defer` — единственная надёжная гарантия Unlock при любом пути выполнения.

### Производительность defer

До Go 1.14: `defer` добавлял ~35ns overhead (heap allocation для defer record). С Go 1.14: open-coded defers — ~0-5ns для простых случаев (defer в прямолинейной функции без циклов). Для мьютексов: Lock/Unlock сами стоят ~10-20ns (fast path). Defer overhead — сопоставим или меньше. Экономия на defer — premature optimization с катастрофическим downside.

## Generics: SafeMap[K, V]

### Type constraints

```go
type SafeMap[K comparable, V any] struct {
    mu   sync.RWMutex
    data map[K]V
}
```

`K comparable` — constraint: тип K должен поддерживать `==`. Это требование map в Go — ключ map должен быть comparable. `comparable` — встроенный constraint, включает: все числовые типы, string, bool, pointer, channel, interface, array из comparable, struct из comparable. Исключает: slice, map, function.

`V any` — без ограничений. Значение map может быть любым типом.

### Инстанцирование

```go
strings := safemap.New[string, int]()           // map[string]int
conns := safemap.New[string, *Connection]()     // map[string]*Connection
metrics := safemap.New[string, []MetricPoint]() // map[string][]MetricPoint
```

Компилятор генерирует специализированный код для каждой комбинации K, V. Нет runtime overhead от generics (в отличие от Java erasure).

## Паттерн: check-and-act под одним lock

### Проблема: TOCTOU

```go
// BROKEN: два отдельных lock
func (m *SafeMap[K, V]) BrokenGetOrSet(key K, def V) (V, bool) {
    m.mu.RLock()
    v, ok := m.data[key]
    m.mu.RUnlock()
    
    if ok {
        return v, true
    }
    
    // RACE: между RUnlock и Lock — другая горутина может Set
    m.mu.Lock()
    m.data[key] = def
    m.mu.Unlock()
    return def, false
}
```

Между `RUnlock` и `Lock` — окно: другая горутина может `Set` тот же ключ. Результат: значение перезаписано, `loaded=false` возвращено обеим горутинам, одно значение потеряно.

### Решение: один Lock

```go
func (m *SafeMap[K, V]) GetOrSet(key K, def V) (V, bool) {
    m.mu.Lock()
    defer m.mu.Unlock()
    
    if v, ok := m.data[key]; ok {
        return v, true
    }
    m.data[key] = def
    return def, false
}
```

Один Lock (не RLock) — проверка и установка атомарны. Никто не может вклиниться между проверкой и записью.

Недостаток: даже чтение (ключ существует) проходит через exclusive Lock, не RLock. Читатели блокируют друг друга. Для GetOrSet с частыми hit (ключ обычно существует): можно оптимизировать двойной проверкой:

```go
func (m *SafeMap[K, V]) GetOrSet(key K, def V) (V, bool) {
    // Fast path: RLock, проверить
    m.mu.RLock()
    if v, ok := m.data[key]; ok {
        m.mu.RUnlock()
        return v, true
    }
    m.mu.RUnlock()
    
    // Slow path: Lock, проверить снова, установить
    m.mu.Lock()
    defer m.mu.Unlock()
    if v, ok := m.data[key]; ok {
        return v, true  // другая горутина установила между RUnlock и Lock
    }
    m.data[key] = def
    return def, false
}
```

Double-checked locking: RLock для быстрой проверки, Lock только если нужна запись. Вторая проверка под Lock — на случай race между RUnlock и Lock.

## Update: read-modify-write

### Атомарность

```go
func (m *SafeMap[K, V]) Update(key K, fn func(V, bool) V) V {
    m.mu.Lock()
    defer m.mu.Unlock()
    
    old, exists := m.data[key]
    new := fn(old, exists)
    m.data[key] = new
    return new
}
```

`fn` вызывается под Lock. Read (получить текущее значение) → modify (fn вычисляет новое) → write (записать) — одна атомарная операция. Без Update:

```go
v, _ := sm.Get("counter")   // RLock
v++
sm.Set("counter", v)          // Lock — race: другая горутина Get/Set между
```

С Update:
```go
sm.Update("counter", func(v int, exists bool) int {
    return v + 1
})
```

Один Lock, нет race.

## Snapshot: escape from lock

### Проблема длительного чтения

`Range` держит RLock на время всей итерации. Для миллиона элементов с медленным callback — все writers заблокированы секунды.

### Решение: snapshot

```go
func (m *SafeMap[K, V]) Snapshot() map[K]V {
    m.mu.RLock()
    defer m.mu.RUnlock()
    
    result := make(map[K]V, len(m.data))
    for k, v := range m.data {
        result[k] = v
    }
    return result
}
```

RLock на время копирования (O(n), но без callback). Возвращённая map — обычная, без блокировок. Вызывающий код обрабатывает snapshot без влияния на SafeMap.

Trade-off: аллокация + копирование. Для маленьких map (<1000 entries) — быстро. Для больших — ощутимо. Но: предсказуемое время lock (пропорционально n), вместо непредсказуемого (зависит от callback в Range).

## Вопросы для самопроверки

1. `sync.Mutex` fast path: `atomic.CompareAndSwapInt32(&m.state, 0, locked)`. Одна атомарная операция. Почему этого достаточно для корректности? Что если две горутины одновременно вызывают CAS?

2. RWMutex: 10 горутин держат RLock. Writer вызывает Lock. Что видят новые горутины вызывающие RLock? Блокируются или проходят?

3. `SafeMap.Get` возвращает `V` по значению. Если `V = *Connection` — вызывающий код получает указатель. `conn.status = "closed"` — модификация через указатель не под мьютексом. Это data race?

4. `Range(fn)` под RLock. `fn` вызывает `Get` (тоже RLock). Deadlock? Почему или почему нет?

5. `sync.Map` — стандартная конкурентная map. В каких сценариях она быстрее `SafeMap` с RWMutex? В каких медленнее?