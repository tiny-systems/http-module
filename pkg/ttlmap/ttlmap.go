package ttlmap

import (
	"context"
	"sync"
	"time"
)

type item struct {
	value      interface{}
	lastAccess int64
}

type TTLMap struct {
	m map[string]*item
	l sync.RWMutex
}

func New(ctx context.Context, maxTTL int) (m *TTLMap) {
	m = &TTLMap{m: make(map[string]*item)}
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.l.Lock()
				for k, v := range m.m {
					if time.Now().Unix()-v.lastAccess > int64(maxTTL) {
						v = nil
						delete(m.m, k)
					}
				}
				m.l.Unlock()
			case <-ctx.Done():
				return
			}
		}
	}()
	return
}

func (m *TTLMap) Len() int {
	return len(m.m)
}

func (m *TTLMap) Put(k string, v interface{}) {
	m.l.Lock()
	it, ok := m.m[k]
	if !ok {
		it = &item{value: v}
		m.m[k] = it
	}
	it.lastAccess = time.Now().Unix()
	m.l.Unlock()
}

func (m *TTLMap) Delete(k string) (v interface{}) {
	m.l.Lock()
	delete(m.m, k)
	m.l.Unlock()
	return
}

func (m *TTLMap) Get(k string) (v interface{}) {
	m.l.RLock()
	if it, ok := m.m[k]; ok {
		v = it.value
		it.lastAccess = time.Now().Unix()
	}
	m.l.RUnlock()
	return
}
