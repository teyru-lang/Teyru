package lsp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// frame encodes one message the way a client writes it.
func frame(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return []byte(fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(data), data))
}

func request(id int, method string, params any) map[string]any {
	m := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		m["params"] = params
	}
	return m
}

func notification(method string, params any) map[string]any {
	m := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		m["params"] = params
	}
	return m
}

// run feeds the messages to Serve and returns the decoded responses and
// notifications, plus the exit code.
func run(t *testing.T, msgs ...map[string]any) ([]map[string]any, int) {
	t.Helper()
	var in bytes.Buffer
	for _, m := range msgs {
		in.Write(frame(t, m))
	}
	var out bytes.Buffer
	code := Serve(&in, &out)
	return decodeFrames(t, out.Bytes()), code
}

func decodeFrames(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for len(data) > 0 {
		i := bytes.Index(data, []byte("\r\n\r\n"))
		if i < 0 {
			t.Fatalf("truncated frame: %q", data)
		}
		var length int
		for _, line := range strings.Split(string(data[:i]), "\r\n") {
			if _, err := fmt.Sscanf(line, "Content-Length: %d", &length); err != nil {
				t.Fatalf("bad header %q: %v", line, err)
			}
		}
		body := data[i+4 : i+4+length]
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("bad body %q: %v", body, err)
		}
		out = append(out, m)
		data = data[i+4+length:]
	}
	return out
}

func TestInitializeAndShutdown(t *testing.T) {
	msgs, code := run(t,
		request(1, "initialize", map[string]any{"processId": nil, "rootUri": nil, "capabilities": map[string]any{}}),
		notification("initialized", map[string]any{}),
		request(2, "shutdown", nil),
		notification("exit", nil),
	)
	if code != 0 {
		t.Fatalf("exit code %d after shutdown, want 0", code)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2: %v", len(msgs), msgs)
	}
	init := msgs[0]["result"].(map[string]any)
	caps := init["capabilities"].(map[string]any)
	if caps["textDocumentSync"] != float64(1) {
		t.Fatalf("textDocumentSync = %v, want 1 (full)", caps["textDocumentSync"])
	}
	if caps["definitionProvider"] != true {
		t.Fatalf("definitionProvider = %v, want true", caps["definitionProvider"])
	}
	if msgs[1]["result"] != nil {
		t.Fatalf("shutdown result = %v, want null", msgs[1]["result"])
	}
}

func TestExitWithoutShutdown(t *testing.T) {
	_, code := run(t, notification("exit", nil))
	if code != 1 {
		t.Fatalf("exit code %d without shutdown, want 1", code)
	}
}

const undeclared = `class Main {
  static void main() {
    foo()
  }
}
`

func TestPublishDiagnostics(t *testing.T) {
	msgs, _ := run(t,
		request(1, "initialize", map[string]any{}),
		notification("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{
				"uri":        "file:///tmp/undeclared.teyru",
				"languageId": "teyru",
				"version":    1,
				"text":       undeclared,
			},
		}),
		request(2, "shutdown", nil),
		notification("exit", nil),
	)
	var published map[string]any
	for _, m := range msgs {
		if m["method"] == "textDocument/publishDiagnostics" {
			published = m["params"].(map[string]any)
		}
	}
	if published == nil {
		t.Fatal("no diagnostics were published")
	}
	diags := published["diagnostics"].([]any)
	if len(diags) != 1 {
		t.Fatalf("got %d diagnostics, want 1: %v", len(diags), diags)
	}
	d := diags[0].(map[string]any)
	// A call to an unknown name is a missing method, which is the code the
	// checker uses for it; the point of the test is that the editor publishes
	// the compiler's own code rather than a guess.
	if d["code"] != "TY-TYP-0076" {
		t.Fatalf("code = %v, want TY-TYP-0076", d["code"])
	}
	if d["severity"] != float64(severityError) {
		t.Fatalf("severity = %v, want %d", d["severity"], severityError)
	}
	r := d["range"].(map[string]any)
	start := r["start"].(map[string]any)
	end := r["end"].(map[string]any)
	if start["line"] != float64(2) || start["character"] != float64(4) {
		t.Fatalf("range starts at %v:%v, want 2:4", start["line"], start["character"])
	}
	if end["line"] != float64(2) || end["character"] != float64(7) {
		t.Fatalf("range ends at %v:%v, want 2:7", end["line"], end["character"])
	}
	if !strings.Contains(d["message"].(string), "foo") {
		t.Fatalf("message %q does not name the symbol", d["message"])
	}
}

func TestDidChangeRepublishes(t *testing.T) {
	msgs, _ := run(t,
		request(1, "initialize", map[string]any{}),
		notification("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{
				"uri":        "file:///tmp/change.teyru",
				"languageId": "teyru",
				"version":    1,
				"text":       "class Main {\n  static void main() {\n  }\n}\n",
			},
		}),
		notification("textDocument/didChange", map[string]any{
			"textDocument": map[string]any{"uri": "file:///tmp/change.teyru", "version": 2},
			"contentChanges": []any{
				map[string]any{"text": undeclared},
			},
		}),
		request(2, "shutdown", nil),
		notification("exit", nil),
	)
	var publishes []map[string]any
	for _, m := range msgs {
		if m["method"] == "textDocument/publishDiagnostics" {
			publishes = append(publishes, m["params"].(map[string]any))
		}
	}
	if len(publishes) != 2 {
		t.Fatalf("got %d publishes, want 2", len(publishes))
	}
	if n := len(publishes[0]["diagnostics"].([]any)); n != 0 {
		t.Fatalf("the first buffer has %d diagnostics, want 0", n)
	}
	diags := publishes[1]["diagnostics"].([]any)
	if len(diags) != 1 {
		t.Fatalf("after the change there are %d diagnostics, want 1", len(diags))
	}
	if code := diags[0].(map[string]any)["code"]; code != "TY-TYP-0076" {
		t.Fatalf("code = %v, want TY-TYP-0076", code)
	}
}

const definitions = `class Main {
  static int add(int a, int b) {
    return a + b
  }
  static void main() {
    int x = add(1, 2)
  }
}
`

func TestDefinitionOnCall(t *testing.T) {
	loc := definitionAt(t, definitions, Position{Line: 5, Character: 12})
	if loc == nil {
		t.Fatal("definition returned null")
	}
	if loc.Range.Start != (Position{Line: 1, Character: 13}) {
		t.Fatalf("definition starts at %v, want 1:13", loc.Range.Start)
	}
	if got := textAt(definitions, loc.Range); got != "add" {
		t.Fatalf("definition range covers %q, want %q", got, "add")
	}
}

func TestDefinitionOnParameter(t *testing.T) {
	loc := definitionAt(t, definitions, Position{Line: 2, Character: 11})
	if loc == nil {
		t.Fatal("definition returned null")
	}
	if got := textAt(definitions, loc.Range); got != "a" {
		t.Fatalf("definition range covers %q, want %q", got, "a")
	}
	if loc.Range.Start.Line != 1 {
		t.Fatalf("parameter declared on line %d, want 1", loc.Range.Start.Line)
	}
}

const members = `class Point {
  int x
  int y
  int sum() {
    return x + y
  }
}
class Main {
  Point p = new Point()
  void run() {
    int local = p.sum()
    p.x = local
  }
}
`

func TestDefinitionOnField(t *testing.T) {
	loc := definitionAt(t, members, Position{Line: 11, Character: 6})
	if loc == nil {
		t.Fatal("definition of a field returned null")
	}
	if got := textAt(members, loc.Range); got != "x" {
		t.Fatalf("definition range covers %q, want %q", got, "x")
	}
	if loc.Range.Start.Line != 1 {
		t.Fatalf("field declared on line %d, want 1", loc.Range.Start.Line)
	}
}

func TestDefinitionOnType(t *testing.T) {
	loc := definitionAt(t, members, Position{Line: 8, Character: 2})
	if loc == nil {
		t.Fatal("definition of a type returned null")
	}
	if got := textAt(members, loc.Range); got != "Point" {
		t.Fatalf("definition range covers %q, want %q", got, "Point")
	}
	if loc.Range.Start.Line != 0 || loc.Range.Start.Character != 6 {
		t.Fatalf("class declared at %v, want 0:6", loc.Range.Start)
	}
}

func TestDefinitionOnLocal(t *testing.T) {
	loc := definitionAt(t, members, Position{Line: 11, Character: 14})
	if loc == nil {
		t.Fatal("definition of a local returned null")
	}
	if got := textAt(members, loc.Range); got != "local" {
		t.Fatalf("definition range covers %q, want %q", got, "local")
	}
	if loc.Range.Start.Line != 10 {
		t.Fatalf("local declared on line %d, want 10", loc.Range.Start.Line)
	}
}

func TestDefinitionOnReceiverCall(t *testing.T) {
	loc := definitionAt(t, members, Position{Line: 10, Character: 18})
	if loc == nil {
		t.Fatal("definition of a method call returned null")
	}
	if got := textAt(members, loc.Range); got != "sum" {
		t.Fatalf("definition range covers %q, want %q", got, "sum")
	}
	if loc.Range.Start.Line != 3 {
		t.Fatalf("method declared on line %d, want 3", loc.Range.Start.Line)
	}
}

func TestDefinitionUnresolvedIsNull(t *testing.T) {
	if loc := definitionAt(t, undeclared, Position{Line: 2, Character: 4}); loc != nil {
		t.Fatalf("definition of an undeclared symbol = %v, want null", loc)
	}
}

func definitionAt(t *testing.T, text string, pos Position) *Location {
	t.Helper()
	msgs, _ := run(t,
		request(1, "initialize", map[string]any{}),
		notification("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{
				"uri":        "file:///tmp/def.teyru",
				"languageId": "teyru",
				"version":    1,
				"text":       text,
			},
		}),
		request(2, "textDocument/definition", map[string]any{
			"textDocument": map[string]any{"uri": "file:///tmp/def.teyru"},
			"position":     map[string]any{"line": pos.Line, "character": pos.Character},
		}),
		request(3, "shutdown", nil),
		notification("exit", nil),
	)
	var result any
	found := false
	for _, m := range msgs {
		if m["id"] == float64(2) {
			result = m["result"]
			found = true
		}
	}
	if !found {
		t.Fatal("no response to the definition request")
	}
	if result == nil {
		return nil
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var loc Location
	if err := json.Unmarshal(raw, &loc); err != nil {
		t.Fatal(err)
	}
	return &loc
}

func textAt(text string, r Range) string {
	li := newLineIndex(text)
	return text[li.offsetAt(r.Start):li.offsetAt(r.End)]
}
