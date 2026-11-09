package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCancelStreamingSubprocess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows os.Process.Signal 不支持 Interrupt")
	}
	resetEnv(t)
	binary := filepath.Join(t.TempDir(), "image-gen")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	var requests atomic.Int32
	disconnected := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: image_edit.partial_image\ndata: {\"type\":\"image_edit.partial_image\",\"b64_json\":%q,\"partial_image_index\":0}\n\n", samplePNGB64)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			disconnected <- struct{}{}
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()
	tmp := t.TempDir()
	outDir := t.TempDir()
	source := writeEditPNG(t, "input.png", 2, 1, true)
	cmd := exec.Command(binary, "edit", "猫", "--image", source, "--base-url", srv.URL, "--config", filepath.Join(t.TempDir(), "missing"), "--output-dir", outDir, "--stream", "--jsonl")
	cmd.Env = append(os.Environ(), "TMPDIR="+tmp, "TMP="+tmp, "TEMP="+tmp)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	lines := make(chan string, 8)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	var previewLine string
	select {
	case previewLine = <-lines:
	case <-time.After(10 * time.Second):
		t.Fatal("no preview")
	}
	var preview struct {
		Type    string `json:"type"`
		Preview struct {
			Path string `json:"path"`
		} `json:"preview"`
	}
	if err := json.Unmarshal([]byte(previewLine), &preview); err != nil || preview.Type != "image.partial" {
		t.Fatalf("preview=%s stderr=%s", previewLine, stderr.String())
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	var terminal string
	select {
	case terminal = <-lines:
	case <-time.After(5 * time.Second):
		t.Fatal("no terminal result")
	}
	var result struct {
		Type   string `json:"type"`
		Result struct {
			Status string `json:"status"`
			Error  struct {
				Code             string `json:"code"`
				GenerationStatus string `json:"generation_status"`
			} `json:"error"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(terminal), &result); err != nil || result.Type != "result" || result.Result.Status != "failed" || result.Result.Error.Code != "canceled" || result.Result.Error.GenerationStatus != "unknown" {
		t.Fatalf("terminal=%s", terminal)
	}
	if err := cmd.Wait(); err == nil || cmd.ProcessState.ExitCode() != 130 {
		t.Fatalf("exit=%v stderr=%s", err, stderr.String())
	}
	select {
	case <-disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request not canceled")
	}
	if requests.Load() != 1 {
		t.Fatal("repeated edit POST")
	}
	if _, err := os.Stat(preview.Preview.Path); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(tmp)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "image-gen-upload-") {
			t.Fatalf("upload leaked: %s", entry.Name())
		}
	}
}
