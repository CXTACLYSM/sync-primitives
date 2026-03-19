# Модуль 004 — Mutex Patterns & sync.RWMutex

## Контекст

Модули 001–003 построили семафор — примитив для ограничения конкурентности. Семафор контролирует "сколько горутин одновременно". Мьютекс решает другую задачу: защита shared state. Connection pool (модуль 006) будет хранить слайс соединений, map статусов, счётчики — всё это shared state к которому обращаются десятки горутин одновременно. Без защиты — data race, коррупция данных, crash.

`sync.Mutex` — exclusive lock: одна горутина за раз. `sync.RWMutex` — разделение читателей и писателей: множество горутин читают одновременно, но запись — эксклюзивна. Для connection pool это идеально: большинство операций — чтение (проверить статус соединения, получить idle connection), редкие — запись (добавить/удалить соединение).

Этот модуль строит `SafeMap[K, V]` — потокобезопасную обобщённую map, которая станет основой для кэша соединений в модуле 006.

## Что нужно реализовать

Новый пакет `safemap` с generic типом.

**Тип `SafeMap[K comparable, V any]`:**

```go
type SafeMap[K comparable, V any] struct {
    mu   sync.RWMutex
    data map[K]V
}
```

**Конструктор `New[K comparable, V any]() *SafeMap[K, V]`.**

**Методы (read — RLock):**

- `Get(key K) (V, bool)` — получить значение по ключу. Comma-ok idiom. RLock: множество горутин читают одновременно.

- `Len() int` — количество элементов. RLock.

- `Has(key K) bool` — проверка наличия ключа. RLock.

- `Keys() []K` — все ключи (копия). RLock.

- `Values() []V` — все значения (копия). RLock.

- `Range(fn func(key K, value V) bool)` — итерация с callback. `fn` возвращает `false` для остановки. RLock на время всей итерации.

**Методы (write — Lock):**

- `Set(key K, value V)` — установить значение. Lock (exclusive).

- `Delete(key K) (V, bool)` — удалить и вернуть. Lock.

- `GetOrSet(key K, defaultValue V) (V, bool)` — получить существующее или установить default. Lock (не RLock — может модифицировать). Второе значение: `true` если ключ уже существовал.

- `Update(key K, fn func(V, bool) V) V` — атомарное чтение-модификация-запись. `fn` получает текущее значение и bool (существовал ли). Возвращает новое значение. Lock.

- `Clear()` — удалить все элементы. Lock.

**Метод `Snapshot() map[K]V`** — копия всей map. RLock. Возвращает обычную map (не SafeMap). Для read-heavy операций: взять snapshot, работать без блокировок.

**Файл `main.go`** — демонстрация:
- Конкурентная запись из 10 горутин
- Конкурентное чтение из 100 горутин одновременно с записью
- `Update` для atomic increment
- `GetOrSet` для lazy initialization
- `Range` с ранним выходом
- `Snapshot` для длительных операций без блокировки
- Race detector: `go run -race`
- Бенчмарк: RWMutex vs Mutex (read-heavy workload)

## Требования

1. Read методы (`Get`, `Len`, `Has`, `Keys`, `Values`, `Range`, `Snapshot`) используют `RLock`/`RUnlock`. Множество горутин читают одновременно.

2. Write методы (`Set`, `Delete`, `GetOrSet`, `Update`, `Clear`) используют `Lock`/`Unlock`. Одна горутина пишет, остальные (и читатели и писатели) ждут.

3. `defer mu.RUnlock()` и `defer mu.Unlock()` — сразу после Lock. Без исключений. Паника внутри метода не должна оставить мьютекс залоченным.

4. `Range` держит RLock на время всей итерации. Callback `fn` вызывается под RLock. Это значит: `fn` не должен вызывать write-методы SafeMap (deadlock: RLock → Lock). Документировать это ограничение.

5. `GetOrSet` — одна операция под одним Lock (не RLock+Lock, что создаёт race). Проверка и установка — атомарны.

6. `Update` — read-modify-write под одним Lock. `fn` вызывается под Lock. Аналогично `Range` — `fn` не должен вызывать методы SafeMap.

7. `Keys()` и `Values()` возвращают копии — слайсы с новым backing array. Мутация возвращённых данных не влияет на SafeMap.

8. Generics: `K comparable` — constraint для ключа map. `V any` — любой тип значения. Тип инстанцируется при использовании: `safemap.New[string, *Connection]()`.

## Структура файлов

```
sync-primitives/
├── go.mod
├── main.go
├── semaphore/
│   ├── semaphore.go
│   └── weighted.go
└── safemap/
    └── safemap.go
```

## Ожидаемый вывод main.go

```
=== Concurrent writes ===
10 goroutines writing 100 keys each...
Final map size: 1000 (no lost writes)

=== Concurrent read + write ===
100 readers, 10 writers running simultaneously...
All operations completed, race detector: clean

=== Update (atomic increment) ===
Counter after 1000 concurrent increments: 1000

=== GetOrSet (lazy init) ===
First call: created new connection (loaded=false)
Second call: returned existing (loaded=true)

=== Range ===
Iterating with early exit at key > 5:
  key=1, value=one
  key=2, value=two
  key=3, value=three
  (stopped early)

=== Snapshot ===
Took snapshot of 1000 entries
Processing snapshot without holding lock...
Writes during processing: 50 new entries
Snapshot unaffected: still 1000 entries

=== Benchmark: RWMutex vs Mutex (95% reads) ===
RWMutex: 1,234,567 ops/sec
Mutex:     456,789 ops/sec
RWMutex advantage: 2.7x for read-heavy workload
```

## Эксперимент на разрушение

**Эксперимент 1: Deadlock через Lock inside RLock.** Код:

```go
sm.Range(func(key string, value int) bool {
    if value > 100 {
        sm.Delete(key)  // Lock внутри RLock → deadlock
    }
    return true
})
```

`Range` держит RLock. `Delete` вызывает Lock. `RWMutex`: Lock ждёт пока ВСЕ RLock освобождены. Текущая горутина держит RLock → Lock ждёт текущую горутину → deadlock. Зафиксировать: Go runtime не обнаружит этот deadlock (не все горутины спят — одна горутина заблокирована сама на себе). Программа зависает без сообщения. Объяснить: почему runtime не ловит self-deadlock. Как избежать: собрать ключи для удаления в слайс, удалить после Range.

**Эксперимент 2: Mutex копирование.** Код:

```go
sm1 := safemap.New[string, int]()
sm1.Set("a", 1)
sm2 := *sm1  // копирование struct с Mutex внутри
sm2.Set("b", 2)
```

`go vet` предупредит: `copies lock value`. Что конкретно происходит при копировании `sync.RWMutex`? Два мьютекса разделяют состояние? Или каждый независим? Запустить с `-race` — что поймает?

Зафиксировать: копирование мьютекса копирует его внутреннее состояние (locked/unlocked, waiter count). Если оригинал locked — копия тоже locked, но никто не держит lock на копию → Unlock копии → panic. Даже если unlocked — два мьютекса с общей историей → undefined behavior. Конструктор возвращает `*SafeMap` (указатель) — копирование указателя, не структуры.

**Эксперимент 3: RWMutex writer starvation.** Нагрузка: 1000 горутин читают непрерывно (RLock, sleep 1ms, RUnlock, repeat). 1 горутина пишет (Lock, write, Unlock). Измерить: сколько времени писатель ждёт Lock? Происходит ли starvation?

Зафиксировать: Go `sync.RWMutex` предотвращает writer starvation. Когда writer ожидает Lock — новые RLock блокируются (не пропускаются мимо writer). Существующие RLock завершаются, writer получает Lock, затем накопившиеся читатели. Это отличается от naive RWMutex где readers могут starve writer. Измерить: максимальное время ожидания writer = max(time of longest active RLock).

**Эксперимент 4: Забытый Unlock.** Метод без defer:

```go
func (m *SafeMap[K, V]) BrokenSet(key K, value V) {
    m.mu.Lock()
    if key == "" {
        return  // ЗАБЫЛИ Unlock!
    }
    m.data[key] = value
    m.mu.Unlock()
}
```

Вызвать `BrokenSet("", 0)`. Следующий вызов любого метода — вечная блокировка. Зафиксировать: без `defer mu.Unlock()` любой ранний return оставляет мьютекс залоченным. Все последующие операции — deadlock. В production: сервер "зависает", метрики перестают обновляться, health check timeout → restart.

## Вопросы до написания кода

1. `sync.Mutex` vs `sync.RWMutex` — когда RWMutex медленнее Mutex? При каком соотношении read/write RWMutex оправдан?

2. Можно ли RLock дважды из одной горутины? `mu.RLock(); mu.RLock()` — deadlock? А `mu.Lock(); mu.Lock()`?

3. `GetOrSet` — почему нельзя реализовать через `RLock` + проверку + `Lock` + установку? Какой race condition возникнет?

4. `Range` держит RLock на время всей итерации. Если map содержит 1 миллион элементов и callback медленный — все writers заблокированы на время итерации. Как решить?

5. Generics: `K comparable` — почему не `K any`? Какие типы не являются `comparable` и не могут быть ключами map?

6. `SafeMap` потокобезопасен. Но `Get` возвращает `V` по значению. Если `V` — указатель (`*Connection`) — вызывающий код получает указатель на объект внутри map. Мутация через указатель — не защищена мьютексом. Это проблема?