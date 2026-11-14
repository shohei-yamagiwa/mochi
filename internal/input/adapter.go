package input

import (
	"fmt"
	"sync"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
)

// Adapter aliases domain.InputAdapter for callers within the input package.
type Adapter = domain.InputAdapter

// Registry stores named InputAdapter implementations.
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

// NewRegistry creates an empty InputAdapter registry.
func NewRegistry() *Registry {
	return &Registry{
		adapters: make(map[string]Adapter),
	}
}

// Register registers an InputAdapter by its Name().
func (r *Registry) Register(adapter Adapter) error {
	if adapter == nil {
		return fmt.Errorf("cannot register nil input adapter")
	}
	name := adapter.Name()
	if name == "" {
		return fmt.Errorf("input adapter name must not be empty")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.adapters[name]; exists {
		return fmt.Errorf("input adapter %q already registered", name)
	}
	r.adapters[name] = adapter
	return nil
}

// Get retrieves a registered InputAdapter by name.
func (r *Registry) Get(name string) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[name]
	return a, ok
}
