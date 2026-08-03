// Package testutil contains small in-process HTTP fixtures shared by package
// tests. It intentionally never exposes or persists authorization headers.
package testutil

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
)

type ResponsesServer struct {
	Server  *httptest.Server
	Mu      sync.Mutex
	Calls   int
	Bodies  [][]byte
	Status  int
	Body    any
	Handler func(http.ResponseWriter, *http.Request)
}

func NewResponsesServer() *ResponsesServer {
	m := &ResponsesServer{Status: http.StatusOK, Body: map[string]any{
		"output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "Visible text:\nfixture\n\nVisual description:\nfixture"}}}},
	}}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.Mu.Lock()
		m.Calls++
		m.Bodies = append(m.Bodies, append([]byte(nil), body...))
		m.Mu.Unlock()
		if m.Handler != nil {
			m.Handler(w, r)
			return
		}
		w.WriteHeader(m.Status)
		if m.Body != nil {
			_ = json.NewEncoder(w).Encode(m.Body)
		}
	}))
	return m
}

func (m *ResponsesServer) Count() int {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	return m.Calls
}

func (m *ResponsesServer) RequestBodies() [][]byte {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	result := make([][]byte, len(m.Bodies))
	for i := range m.Bodies {
		result[i] = append([]byte(nil), m.Bodies[i]...)
	}
	return result
}
