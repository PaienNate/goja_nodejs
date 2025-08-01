package require

import (
	"encoding/json"
	"sync"
)

type SafeMap[K comparable, V any] struct {
	m sync.Map
}

func (m *SafeMap[K, V]) Delete(key K) { m.m.Delete(key) }
func (m *SafeMap[K, V]) Load(key K) (value V, ok bool) {
	v, ok := m.m.Load(key)
	if !ok {
		return value, ok
	}
	return v.(V), ok
}

func (m *SafeMap[K, V]) Exists(key K) bool {
	_, exists := m.Load(key)
	return exists
}

func (m *SafeMap[K, V]) LoadAndDelete(key K) (value V, loaded bool) {
	v, loaded := m.m.LoadAndDelete(key)
	if !loaded {
		return value, loaded
	}
	return v.(V), loaded
}

func (m *SafeMap[K, V]) LoadOrStore(key K, value V) (actual V, loaded bool) {
	a, loaded := m.m.LoadOrStore(key, value)
	return a.(V), loaded
}

func (m *SafeMap[K, V]) Range(f func(key K, value V) bool) {
	m.m.Range(func(key, value any) bool { return f(key.(K), value.(V)) })
}

func (m *SafeMap[K, V]) Store(key K, value V) { m.m.Store(key, value) }

func (m *SafeMap[K, V]) Len() int {
	// TOOD: 性能优化
	times := 0
	m.Range(func(_ K, _ V) bool {
		times++
		return true
	})
	return times
}

func (m *SafeMap[K, V]) MarshalJSON() ([]byte, error) {
	m2 := make(map[K]V)
	m.Range(func(key K, value V) bool {
		m2[key] = value
		return true
	})
	return json.Marshal(m2)
}

func (m *SafeMap[K, V]) UnmarshalJSON(b []byte) error {
	m2 := make(map[K]V)
	err := json.Unmarshal(b, &m2)
	if err != nil {
		return err
	}
	for k, v := range m2 {
		m.Store(k, v)
	}
	return nil
}
