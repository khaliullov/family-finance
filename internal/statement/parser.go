package statement

import (
	"sync"
)

// ParserRegistry manages supported statement parser plugins.
type ParserRegistry struct {
	mu      sync.RWMutex
	parsers map[string]StatementParser
}

func NewParserRegistry() *ParserRegistry {
	return &ParserRegistry{
		parsers: make(map[string]StatementParser),
	}
}

// NewDefaultParserRegistry creates a registry pre-loaded with official statement parsers.
func NewDefaultParserRegistry() *ParserRegistry {
	r := NewParserRegistry()
	r.Register(NewTBankCSVParser())
	return r
}

// Register registers a format parser plugin.
func (r *ParserRegistry) Register(p StatementParser) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.parsers[p.Profile().FormatID] = p
}

// Get retrieves a parser by its explicit format ID.
func (r *ParserRegistry) Get(formatID string) (StatementParser, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.parsers[formatID]
	return p, ok
}

// Sniff attempts to detect a compatible parser from file magic bytes or header text.
func (r *ParserRegistry) Sniff(headerChunk []byte) (StatementParser, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.parsers {
		if p.Sniff(headerChunk) {
			return p, true
		}
	}
	return nil, false
}
