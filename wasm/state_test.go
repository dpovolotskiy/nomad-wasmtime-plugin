package wasm

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func (ts *taskStore) size() int {
	ts.lock.RLock()
	defer ts.lock.RUnlock()

	return len(ts.store)
}

func TestNewTaskStore(t *testing.T) {
	tCases := []struct {
		name              string
		expectedStoreSize int
	}{
		{
			name:              "create new task store",
			expectedStoreSize: 0,
		},
	}

	for _, tCase := range tCases {
		t.Run(tCase.name, func(t *testing.T) {
			store := newTaskStore()

			assert.Equal(t, tCase.expectedStoreSize, store.size())
		})
	}
}

func TestSet(t *testing.T) {
	tCases := []struct {
		name      string
		keysToAdd map[string]*taskHandle
	}{
		{
			name: "don't add any keys to store",
		},
		{
			name: "add single key to store",
			keysToAdd: map[string]*taskHandle{
				"fakeKey1": {},
			},
		},
		{
			name: "add several keys parallel",
			keysToAdd: map[string]*taskHandle{
				"fakeKey1": {},
				"fakeKey2": {},
				"fakeKey3": {},
			},
		},
	}

	for _, tCase := range tCases {
		t.Run(tCase.name, func(t *testing.T) {
			store := newTaskStore()

			var wg sync.WaitGroup

			for k, v := range tCase.keysToAdd {
				key, val := k, v

				wg.Add(1)

				go func() {
					defer wg.Done()

					store.Set(key, val)
				}()
			}

			wg.Wait()

			assert.Equal(t, len(tCase.keysToAdd), store.size())

			if len(tCase.keysToAdd) > 0 {
				for k := range tCase.keysToAdd {
					if _, ok := store.Get(k); !ok {
						t.Errorf("expected key %s is absent", k)
					}
				}
			}
		})
	}
}

func TestGet(t *testing.T) {
	tCases := []struct {
		name          string
		presentKeys   map[string]*taskHandle
		expectedFound bool
		keyToGet      string
	}{
		{
			name:          "try to get absent key",
			expectedFound: false,
			keyToGet:      "absentKey",
		},
		{
			name: "try to get present key",
			presentKeys: map[string]*taskHandle{
				"fakeKey1": {},
			},
			expectedFound: true,
			keyToGet:      "fakeKey1",
		},
	}

	for _, tCase := range tCases {
		t.Run(tCase.name, func(t *testing.T) {
			store := newTaskStore()

			for k, v := range tCase.presentKeys {
				key, val := k, v

				store.Set(key, val)
			}

			_, found := store.Get(tCase.keyToGet)

			assert.Equal(t, tCase.expectedFound, found)
		})
	}
}

func TestDelete(t *testing.T) {
	tCases := []struct {
		name         string
		presentKeys  map[string]*taskHandle
		keysToDelete []string
		expectedSize int
	}{
		{
			name: "try to delete absent key",
			presentKeys: map[string]*taskHandle{
				"fakeKey1": {},
			},
			keysToDelete: []string{"absentKey"},
			expectedSize: 1,
		},
		{
			name: "try to delete present single key",
			presentKeys: map[string]*taskHandle{
				"fakeKey1": {},
			},
			keysToDelete: []string{"fakeKey1"},
			expectedSize: 0,
		},
		{
			name: "try to delete several keys parallel",
			presentKeys: map[string]*taskHandle{
				"fakeKey1": {},
				"fakeKey2": {},
				"fakeKey3": {},
				"fakeKey4": {},
			},
			keysToDelete: []string{"fakeKey1", "fakeKey2", "fakeKey3"},
			expectedSize: 1,
		},
	}

	for _, tCase := range tCases {
		t.Run(tCase.name, func(t *testing.T) {
			store := newTaskStore()

			for k, v := range tCase.presentKeys {
				key, val := k, v

				store.Set(key, val)
			}

			var wg sync.WaitGroup

			for _, k := range tCase.keysToDelete {
				key := k

				wg.Add(1)

				go func() {
					defer wg.Done()

					store.Delete(key)
				}()
			}

			wg.Wait()

			assert.Equal(t, tCase.expectedSize, store.size())

			// check that all deleted keys are absent
			for _, k := range tCase.keysToDelete {
				if _, ok := store.Get(k); ok {
					t.Errorf("found unexpected key %s (should have been deleted)", k)
				}
			}
		})
	}
}
