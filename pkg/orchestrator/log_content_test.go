package orchestrator

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestContentForLogCarriesNoContentByDefault(t *testing.T) {
	SetLogCallContent(false)
	text := "Me llamo Ana García, mi teléfono es 612 345 678"
	got := ContentForLog(text)
	for _, w := range strings.Fields(text) {
		if strings.Contains(got, w) && len(w) > 2 {
			t.Fatalf("ContentForLog(%q) = %q: carries %q", text, got, w)
		}
	}
	if !strings.HasPrefix(got, "[47 chars, 10 words, h:") {
		t.Fatalf("ContentForLog = %q, want length, word count and hash", got)
	}
	if ContentForLog(text) != got {
		t.Fatal("the same text must hash the same within a process, or two lines cannot be matched")
	}
	if ContentForLog(text+".") == got {
		t.Fatal("different texts produced the same summary")
	}
	if ContentForLog("") != "" {
		t.Fatal("an empty transcript must still read as empty")
	}
}

func TestContentForLogShowsTheTextWhenEnabled(t *testing.T) {
	SetLogCallContent(true)
	defer SetLogCallContent(false)
	if got := ContentForLog("hola"); got != "hola" {
		t.Fatalf("ContentForLog with LOG_CALL_CONTENT on = %q, want the text", got)
	}
	long := strings.Repeat("a", 500)
	if got := ContentForLog(long); len([]rune(got)) != 401 {
		t.Fatalf("enabled content is clipped to 400 runes, got %d", len([]rune(got)))
	}
}

// Every log line in this package that names call content under one of these keys passes it through
// ContentForLog. A new line that logs a transcript or a reply directly fails here.
func TestLogLinesPassCallContentThroughContentForLog(t *testing.T) {
	contentKeys := map[string]bool{
		"transcript": true, "text": true, "before": true, "after": true, "previous": true,
		"continuation": true, "merged": true, "joint": true, "kept": true, "dropped": true,
		"summary": true, "bot_was_saying": true, "key": true, "prompt": true, "reply": true,
		"response": true, "args": true, "arguments": true, "result": true, "partial": true,
		"utterance": true, "seed": true,
	}
	levels := map[string]bool{"Debug": true, "Info": true, "Warn": true, "Error": true}
	files, _ := filepath.Glob("*.go")
	fs := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fs, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := c.Fun.(*ast.SelectorExpr)
			if !ok || !levels[sel.Sel.Name] || len(c.Args) < 3 {
				return true
			}
			for i := 1; i+1 < len(c.Args); i += 2 {
				lit, ok := c.Args[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				k, _ := strconv.Unquote(lit.Value)
				if !contentKeys[k] {
					continue
				}
				if v, ok := c.Args[i+1].(*ast.CallExpr); ok {
					if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "ContentForLog" {
						checked++
						continue
					}
				}
				t.Errorf("%s: log key %q is not passed through ContentForLog", fs.Position(c.Args[i+1].Pos()), k)
			}
			return true
		})
	}
	if checked < 30 {
		t.Fatalf("saw only %d content values in log lines; the scan is not reading this package", checked)
	}
}
