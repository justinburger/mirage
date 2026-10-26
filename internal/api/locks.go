package api

import "sync"

// keyedMutex is a set of mutexes addressed by key. Entries exist only while
// somebody holds or waits for them, so the set does not grow with the number
// of distinct keys ever used.
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*keyedEntry
}

type keyedEntry struct {
	mu   sync.Mutex
	refs int
}

// Lock acquires the mutex for key and returns the function that releases it.
func (k *keyedMutex) Lock(key string) (unlock func()) {
	k.mu.Lock()
	if k.m == nil {
		k.m = make(map[string]*keyedEntry)
	}
	e := k.m[key]
	if e == nil {
		e = new(keyedEntry)
		k.m[key] = e
	}
	e.refs++
	k.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		if e.refs--; e.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}

// size reports how many keys are currently tracked (for tests).
func (k *keyedMutex) size() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.m)
}
