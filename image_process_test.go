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
	"strconv"
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

// 在真实 stdin 边界发出就绪标记，避免用固定延迟猜测信号处理器是否已安装。
type promptReadyReader struct{ marker string }

func (r promptReadyReader) Read(p []byte) (int, error) {
	if err := os.WriteFile(r.marker, []byte("ready"), 0600); err != nil {
		return 0, err
	}
	return os.Stdin.Read(p)
}
func TestPromptCancellationHelper(t *testing.T) {
	marker := os.Getenv("IMAGE_GEN_TEST_PROMPT_READY")
	if marker == "" {
		return
	}
	stdinReader = promptReadyReader{marker: marker}
	if err := run([]string{"generate", "--json", "--config", filepath.Join(filepath.Dir(marker), "missing"), "-"}); err != nil {
		os.Exit(exitCodeOf(err))
	}
	os.Exit(0)
}

func TestCancelWhileReadingPrompt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows os.Process.Signal 不支持 Interrupt")
	}
	resetEnv(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ready")
	command := exec.Command(binary, "-test.run=^TestPromptCancellationHelper$")
	command.Env = append(os.Environ(), "IMAGE_GEN_TEST_PROMPT_READY="+marker)
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	completed := false
	finished := make(chan error, 1)
	go func() { finished <- command.Wait() }()
	t.Cleanup(func() {
		if !completed {
			_ = command.Process.Kill()
			<-finished
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stdin 读取未开始")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case <-time.After(3 * time.Second):
		t.Fatal("Ctrl-C 未取消等待 stdin 的命令")
	case err := <-finished:
		completed = true
		if err == nil || command.ProcessState.ExitCode() != 130 {
			t.Fatalf("exit=%v stderr=%s", err, stderr.String())
		}
		var result struct {
			Status string `json:"status"`
			Error  struct {
				Code             string `json:"code"`
				GenerationStatus string `json:"generation_status"`
			} `json:"error"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
			t.Fatalf("JSON: %v output=%s", err, stdout.String())
		}
		if result.Status != "failed" || result.Error.Code != "canceled" || result.Error.GenerationStatus != "not_started" {
			t.Fatalf("result=%s", stdout.String())
		}
	}
}

func TestAuthCancellationHelper(t *testing.T) {
	marker := os.Getenv("IMAGE_GEN_TEST_AUTH_READY")
	if marker == "" {
		return
	}
	if err := os.WriteFile(marker, []byte("ready"), 0600); err != nil {
		os.Exit(1)
	}
	time.Sleep(10 * time.Second)
	os.Exit(0)
}

func TestCancelWhileResolvingAuth(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows os.Process.Signal 不支持 Interrupt")
	}
	resetEnv(t)
	t.Setenv("IMAGE_GEN_API_KEY", "")
	binary := filepath.Join(t.TempDir(), "image-gen")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.toml")
	config := fmt.Sprintf("base_url = 'https://example.invalid'\nmodel = 'm'\n[auth]\ncommand = %s\nargs = ['-test.run=^TestAuthCancellationHelper$']\n", strconv.Quote(helper))
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ready")
	command := exec.Command(binary, "generate", "--json", "--config", configPath, "猫")
	command.Env = append(os.Environ(), "IMAGE_GEN_TEST_AUTH_READY="+marker)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	completed := false
	finished := make(chan error, 1)
	go func() { finished <- command.Wait() }()
	t.Cleanup(func() {
		if !completed {
			_ = command.Process.Kill()
			<-finished
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("取密钥命令未启动")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case <-time.After(2 * time.Second):
		t.Fatal("Ctrl-C 未取消取密钥命令")
	case err := <-finished:
		completed = true
		if err == nil || command.ProcessState.ExitCode() != 130 {
			t.Fatalf("exit=%v stderr=%s", err, stderr.String())
		}
		var result struct {
			Error struct {
				Code             string `json:"code"`
				Stage            string `json:"stage"`
				GenerationStatus string `json:"generation_status"`
			} `json:"error"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Error.Code != "canceled" || result.Error.Stage != "config" || result.Error.GenerationStatus != "not_started" {
			t.Fatalf("result=%s", stdout.String())
		}
	}
}
