// Package lsp implements the Teyru language server: JSON-RPC 2.0 over stdio,
// with full-document synchronization, diagnostics from the compiler's own
// front end, and go-to-definition inside the open file.
package lsp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/teyru-lang/Teyru/internal/driver"
	"github.com/teyru-lang/Teyru/internal/lexer"
	"github.com/teyru-lang/Teyru/internal/source"
)

// Position is an LSP position: a zero-based line and a zero-based UTF-16
// column.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is an LSP range.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location is the result of a definition request.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// Diagnostic is one published compiler message.
type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity"`
	Code     string `json:"code,omitempty"`
	Source   string `json:"source,omitempty"`
	Message  string `json:"message"`
}

// Severity values from the protocol.
const (
	severityError   = 1
	severityWarning = 2
)

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// message is one JSON-RPC request, notification or response.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Server holds the open documents.
type Server struct {
	out      io.Writer
	docs     map[string]string
	shutdown bool
}

// Serve runs the server until `exit` or the end of the input. It returns the
// process exit code the protocol asks for: 0 after a shutdown, 1 otherwise.
func Serve(in io.Reader, out io.Writer) int {
	s := &Server{out: out, docs: map[string]string{}}
	r := bufio.NewReader(in)
	for {
		data, err := readMessage(r)
		if err != nil {
			if err == io.EOF {
				if s.shutdown {
					return 0
				}
				return 1
			}
			return 1
		}
		var msg message
		if err := json.Unmarshal(data, &msg); err != nil {
			// A malformed message has no id to answer, so it is dropped.
			continue
		}
		if stop, code := s.handle(&msg); stop {
			return code
		}
	}
}

// readMessage reads one Content-Length framed message.
func readMessage(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, err
			}
			length = n
		}
	}
	if length < 0 {
		return nil, errors.New("message has no Content-Length")
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}
	return data, nil
}

func writeMessage(w io.Writer, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(data))
	w.Write(data)
}

func (s *Server) respond(id json.RawMessage, result any) {
	writeMessage(s.out, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

func (s *Server) respondError(id json.RawMessage, code int, text string) {
	writeMessage(s.out, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   &rpcError{Code: code, Message: text},
	})
}

func (s *Server) notify(method string, params any) {
	writeMessage(s.out, map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

func (s *Server) handle(msg *message) (stop bool, code int) {
	request := len(msg.ID) > 0
	switch msg.Method {
	case "initialize":
		s.respond(msg.ID, map[string]any{
			"capabilities": map[string]any{
				// 1 is Full: every change sends the whole document.
				"textDocumentSync":   1,
				"definitionProvider": true,
			},
			"serverInfo": map[string]any{"name": "teyru", "version": driver.Version()},
		})
	case "initialized":
		// no state to set up
	case "shutdown":
		s.shutdown = true
		if request {
			s.respond(msg.ID, nil)
		}
	case "exit":
		if s.shutdown {
			return true, 0
		}
		return true, 1
	case "textDocument/didOpen":
		var p struct {
			TextDocument struct {
				URI  string `json:"uri"`
				Text string `json:"text"`
			} `json:"textDocument"`
		}
		if json.Unmarshal(msg.Params, &p) == nil && p.TextDocument.URI != "" {
			s.docs[p.TextDocument.URI] = p.TextDocument.Text
			s.publish(p.TextDocument.URI, p.TextDocument.Text)
		}
	case "textDocument/didChange":
		var p struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
			ContentChanges []struct {
				Text string `json:"text"`
			} `json:"contentChanges"`
		}
		if json.Unmarshal(msg.Params, &p) == nil && p.TextDocument.URI != "" && len(p.ContentChanges) > 0 {
			text := p.ContentChanges[len(p.ContentChanges)-1].Text
			s.docs[p.TextDocument.URI] = text
			s.publish(p.TextDocument.URI, text)
		}
	case "textDocument/didClose":
		var p struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		if json.Unmarshal(msg.Params, &p) == nil {
			delete(s.docs, p.TextDocument.URI)
		}
	case "textDocument/definition":
		var p struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
			Position Position `json:"position"`
		}
		if json.Unmarshal(msg.Params, &p) != nil {
			s.respondError(msg.ID, -32602, "invalid definition parameters")
			break
		}
		s.respond(msg.ID, s.definition(p.TextDocument.URI, p.Position))
	default:
		if request {
			s.respondError(msg.ID, -32601, "method not found: "+msg.Method)
		}
	}
	return false, 0
}

// publish runs the compiler's front end on the buffer and sends its
// diagnostics for this document.
func (s *Server) publish(uri, text string) {
	diags, _ := driver.Analyze(uri, text)
	li := newLineIndex(text)
	out := make([]Diagnostic, 0, len(diags.List))
	for _, d := range diags.List {
		end := d.End
		if end <= d.Pos.Off {
			end = tokenEnd(text, d.Pos.Off)
		}
		severity := severityError
		if d.Severity == source.Warning {
			severity = severityWarning
		}
		out = append(out, Diagnostic{
			Range:    Range{Start: li.positionAt(d.Pos.Off), End: li.positionAt(end)},
			Severity: severity,
			Code:     d.Code,
			Source:   "teyru",
			Message:  d.Message,
		})
	}
	s.notify("textDocument/publishDiagnostics", map[string]any{
		"uri":         uri,
		"diagnostics": out,
	})
}

// definition resolves the identifier at a position to a declaration in the
// same document, or answers null when there is none.
func (s *Server) definition(uri string, pos Position) any {
	text, ok := s.docs[uri]
	if !ok {
		return nil
	}
	off := newLineIndex(text).offsetAt(pos)
	_, file := driver.Analyze(uri, text)
	toks := lexer.Lex(source.NewFile(uri, text), &source.Diagnostics{})
	t, ok := resolveDefinition(file, toks, off)
	if !ok {
		return nil
	}
	li := newLineIndex(text)
	return Location{
		URI: uri,
		Range: Range{
			Start: li.positionAt(t.pos.Off),
			End:   li.positionAt(t.pos.Off + t.length),
		},
	}
}

// tokenEnd extends off over the identifier or single character there, so a
// diagnostic with no end offset still has a visible range.
func tokenEnd(text string, off int) int {
	if off >= len(text) {
		return off
	}
	i := off
	for i < len(text) {
		r, n := utf8.DecodeRuneInString(text[i:])
		if !(r == '_' || r == '$' || isLetter(r) || isDigit(r)) {
			break
		}
		i += n
	}
	if i == off {
		_, n := utf8.DecodeRuneInString(text[off:])
		i = off + n
	}
	return i
}

func isLetter(r rune) bool { return unicode.IsLetter(r) }

func isDigit(r rune) bool { return unicode.IsDigit(r) }

// lineIndex converts between byte offsets and LSP positions. LSP columns count
// UTF-16 code units by default, so a rune outside the BMP counts as two.
type lineIndex struct {
	text   string
	starts []int
}

func newLineIndex(text string) *lineIndex {
	starts := []int{0}
	for i := range text {
		if text[i] == '\n' {
			starts = append(starts, i+1)
		}
	}
	return &lineIndex{text: text, starts: starts}
}

func (li *lineIndex) positionAt(off int) Position {
	if off < 0 {
		off = 0
	}
	if off > len(li.text) {
		off = len(li.text)
	}
	line := sort.Search(len(li.starts), func(i int) bool { return li.starts[i] > off }) - 1
	if line < 0 {
		line = 0
	}
	start := li.starts[line]
	col := 0
	for i := start; i < off; {
		r, n := utf8.DecodeRuneInString(li.text[i:])
		if r > 0xFFFF {
			col += 2
		} else {
			col++
		}
		i += n
	}
	return Position{Line: line, Character: col}
}

func (li *lineIndex) offsetAt(p Position) int {
	if p.Line < 0 {
		return 0
	}
	if p.Line >= len(li.starts) {
		return len(li.text)
	}
	start := li.starts[p.Line]
	end := len(li.text)
	if p.Line+1 < len(li.starts) {
		end = li.starts[p.Line+1]
	}
	col := 0
	for i := start; i < end; {
		if col >= p.Character {
			return i
		}
		r, n := utf8.DecodeRuneInString(li.text[i:])
		if r > 0xFFFF {
			col += 2
		} else {
			col++
		}
		i += n
	}
	return end
}
