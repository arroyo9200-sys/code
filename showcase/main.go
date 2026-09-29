// Package main is a web-based showcase browser for the "Go In Action" code
// examples. It discovers every runnable example (package main with a main
// function) in the repository, serves a single-page UI on port 3000, and
// runs examples on demand, streaming their output to the browser.
package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed index.html
var indexHTML string

// Example describes a single runnable Go program in the repo.
type Example struct {
	Chapter string   `json:"chapter"`
	Name    string   `json:"name"`
	Path    string   `json:"path"`  // relative directory, e.g. "chapter1/channels"
	Files   []string `json:"files"` // .go files in the main package
}

// buildResult is the cached outcome of compiling an example.
type buildResult struct {
	binary string
	err    error
}

var (
	repoRoot   string
	buildCache sync.Map // path -> buildResult
)

func main() {
	var err error
	repoRoot, err = os.Getwd()
	if err != nil {
		panic(err)
	}

	examples := discoverExamples()
	fmt.Printf("Go In Action showcase: %d examples found, listening on :3000\n", len(examples))

	mux := http.NewServeMux()
	mux.HandleFunc("/", serveIndex)
	mux.HandleFunc("/api/examples", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, examples)
	})
	mux.HandleFunc("/api/source", serveSource)
	mux.HandleFunc("/api/run", serveRun)
	mux.HandleFunc("/api/drive/auth", handleDriveAuth)
	mux.HandleFunc("/api/drive/callback", handleDriveCallback)
	mux.HandleFunc("/api/drive/status", handleDriveStatus)
	mux.HandleFunc("/api/drive/save", handleDriveSave)

	srv := &http.Server{
		Addr:         "0.0.0.0:3000",
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		panic(err)
	}
}

// ---------------------------------------------------------------------------
// Discovery
// ---------------------------------------------------------------------------

func discoverExamples() []Example {
	dirs := make(map[string][]string)

	_ = filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(repoRoot, path)
		if d.IsDir() {
			if shouldSkipDir(rel) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if !isMainGoFile(path) {
			return nil
		}
		dir := filepath.Dir(rel)
		dirs[dir] = append(dirs[dir], filepath.Base(path))
		return nil
	})

	var examples []Example
	for dir, files := range dirs {
		sort.Strings(files)
		parts := strings.SplitN(dir, "/", 2)
		chapter := parts[0]
		name := ""
		if len(parts) > 1 {
			name = parts[1]
		}
		examples = append(examples, Example{
			Chapter: chapter,
			Name:    name,
			Path:    dir,
			Files:   files,
		})
	}

	sort.Slice(examples, func(i, j int) bool {
		if examples[i].Chapter != examples[j].Chapter {
			return chapterOrder(examples[i].Chapter) < chapterOrder(examples[j].Chapter)
		}
		return examples[i].Name < examples[j].Name
	})
	return examples
}

func shouldSkipDir(rel string) bool {
	if rel == "." {
		return false
	}
	first := strings.SplitN(rel, "/", 2)[0]
	switch first {
	case ".git", "showcase", "bin", "vendor", "node_modules":
		return true
	}
	return strings.HasPrefix(first, ".")
}

func isMainGoFile(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	content := string(data)
	return strings.Contains(content, "package main") && strings.Contains(content, "func main(")
}

func chapterOrder(ch string) int {
	n := 0
	for _, c := range strings.TrimPrefix(ch, "chapter") {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		} else {
			break
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(indexHTML))
}

func serveSource(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if !validPath(path) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	dir := filepath.Join(repoRoot, path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeJSON(w, map[string]string{"source": "Error: " + err.Error()})
		return
	}
	var sb strings.Builder
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "// ===== %s =====\n", e.Name())
		sb.Write(data)
		if !strings.HasSuffix(sb.String(), "\n") {
			sb.WriteString("\n")
		}
	}
	writeJSON(w, map[string]string{"source": sb.String()})
}

func serveRun(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if !validPath(path) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	output, ok := runExample(path)
	writeJSON(w, map[string]any{"output": output, "ok": ok})
}

// ---------------------------------------------------------------------------
// Build & run
// ---------------------------------------------------------------------------

func getOrBuild(path string) (string, error) {
	if v, ok := buildCache.Load(path); ok {
		br := v.(buildResult)
		return br.binary, br.err
	}
	tmpBin := filepath.Join(os.TempDir(), "goaction_"+strings.ReplaceAll(path, "/", "_"))
	buildCmd := exec.Command("go", "build", "-o", tmpBin, "./"+path)
	buildCmd.Dir = repoRoot
	var buildErr bytes.Buffer
	buildCmd.Stderr = &buildErr
	if err := buildCmd.Run(); err != nil {
		br := buildResult{err: fmt.Errorf("build error:\n%s\n%s", strings.TrimSpace(buildErr.String()), err)}
		buildCache.Store(path, br)
		return "", br.err
	}
	br := buildResult{binary: tmpBin}
	buildCache.Store(path, br)
	return tmpBin, nil
}

func runExample(path string) (string, bool) {
	bin, err := getOrBuild(path)
	if err != nil {
		return err.Error(), false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin)
	cmd.Dir = repoRoot
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
	runErr := cmd.Run()
	output := buf.String()
	if ctx.Err() == context.DeadlineExceeded {
		if output != "" {
			output += "\n"
		}
		output += "[process timed out after 10s and was killed]"
	}
	return output, runErr == nil && ctx.Err() == nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func validPath(path string) bool {
	clean := filepath.Clean(path)
	if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) || clean == "." {
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
