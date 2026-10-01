// Package pending: registro en memoria de llamadas MCP bloqueadas esperando la respuesta del jefe.
// Clave = report_id. Cuando el gateway entrega la respuesta, se resuelve el canal y la llamada MCP retorna.
package pending

import "sync"

type Response struct {
	Action  string `json:"action"`
	Message string `json:"message"`
}

type Registry struct {
	mu      sync.Mutex
	waiters map[string]chan Response
}

func New() *Registry {
	return &Registry{waiters: make(map[string]chan Response)}
}

// Register crea el canal de espera para un report_id.
func (r *Registry) Register(id string) chan Response {
	ch := make(chan Response, 1)
	r.mu.Lock()
	r.waiters[id] = ch
	r.mu.Unlock()
	return ch
}

// Resolve entrega la respuesta a la llamada pendiente. Devuelve false si ya no había nadie esperando.
func (r *Registry) Resolve(id string, resp Response) bool {
	r.mu.Lock()
	ch, ok := r.waiters[id]
	if ok {
		delete(r.waiters, id)
	}
	r.mu.Unlock()
	if !ok {
		return false
	}
	ch <- resp
	return true
}

// Cancel descarta el canal (p. ej. al expirar el timeout).
func (r *Registry) Cancel(id string) {
	r.mu.Lock()
	if ch, ok := r.waiters[id]; ok {
		delete(r.waiters, id)
		close(ch)
	}
	r.mu.Unlock()
}
