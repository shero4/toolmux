package web

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// flash is a one-time message shown on the next page load. It travels through
// an opaque id in the URL rather than the message itself, so secrets such as a
// freshly issued token never appear in browser history or server logs.
type flash struct {
	Kind    string // ok, error, or info
	Message string
	Details []string
	Token   string
	Setup   *setupGuide
}

const flashTTL = 10 * time.Minute

type flashStore struct {
	mu    sync.Mutex
	items map[string]flashEntry
}

type flashEntry struct {
	flash   flash
	expires time.Time
}

func newFlashStore() *flashStore {
	return &flashStore{items: make(map[string]flashEntry)}
}

func (f *flashStore) put(fl flash) string {
	raw := make([]byte, 18)
	_, _ = rand.Read(raw)
	id := base64.RawURLEncoding.EncodeToString(raw)
	now := time.Now()
	f.mu.Lock()
	defer f.mu.Unlock()
	for key, entry := range f.items {
		if entry.expires.Before(now) {
			delete(f.items, key)
		}
	}
	f.items[id] = flashEntry{flash: fl, expires: now.Add(flashTTL)}
	return id
}

// take returns the message once and forgets it.
func (f *flashStore) take(id string) *flash {
	if id == "" {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.items[id]
	if !ok {
		return nil
	}
	delete(f.items, id)
	if entry.expires.Before(time.Now()) {
		return nil
	}
	return &entry.flash
}

func okFlash(message string) flash    { return flash{Kind: "ok", Message: message} }
func errorFlash(message string) flash { return flash{Kind: "error", Message: message} }
