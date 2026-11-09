package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOptionalDefaultsOnWire(t *testing.T) {
	resetEnv(t)
	var body map[string]any
	srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		body = nil
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeJSON(w, map[string]any{"data": []any{map[string]any{"b64_json": samplePNGB64}}})
	}, nil)
	for _, tc := range []struct {
		name     string
		flags    []string
		defaults string
		want     map[string]any
	}{
		{name: "omitted", want: map[string]any{"model": "m", "prompt": "猫"}},
		{name: "explicit", flags: []string{"--size", "auto", "--quality", "auto", "--count", "1", "--format", "png"}, want: map[string]any{"model": "m", "prompt": "猫", "size": "auto", "quality": "auto", "n": float64(1), "output_format": "png"}},
		{name: "config", defaults: "[defaults]\nsize = '1024x1024'\nquality = 'medium'\n", flags: []string{"--quality", "high"}, want: map[string]any{"model": "m", "prompt": "猫", "size": "1024x1024", "quality": "high"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(cfg, []byte(tc.defaults), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"generate", "--config", cfg, "--base-url", srv.URL, "--model", "m", "--output-dir", t.TempDir(), "猫"}
			stdout, err := runCLI(t, append(args, tc.flags...)...)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := json.Marshal(body)
			want, _ := json.Marshal(tc.want)
			if !bytes.Equal(got, want) {
				t.Fatalf("request = %s; want %s", got, want)
			}
			if _, err := os.Stat(string(bytes.TrimSpace([]byte(stdout)))); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestJSONResultLifecycle(t *testing.T) {
	resetEnv(t)
	for _, tc := range []struct {
		name            string
		body            any
		status          int
		count           string
		wantCode        string
		wantExit, saved int
	}{
		{"metadata", map[string]any{"created": 0, "usage": map[string]any{"total_tokens": 0}, "svg_usage": nil, "vendor": map[string]any{"exact": 9007199254740993}, "data": []any{map[string]any{"b64_json": samplePNGB64, "revised_prompt": "revised"}}}, 200, "1", "", 0, 1},
		{"upstream", map[string]any{"error": map[string]any{"code": "blocked", "type": "invalid_request_error", "param": "prompt", "message": "拒绝", "moderation_details": map[string]any{"x": true}}}, 400, "1", "upstream_error", 5, 0},
		{"partial", map[string]any{"data": []any{map[string]any{"b64_json": samplePNGB64}, map[string]any{"url": "bad://image"}}}, 200, "2", "download_failed", 4, 1},
		{"short", map[string]any{"data": []any{map[string]any{"b64_json": samplePNGB64}}}, 200, "2", "incomplete_result", 5, 1},
		{"invalid", map[string]any{"data": []any{map[string]any{"b64_json": "PGh0bWw+ZXJyb3I8L2h0bWw+"}}}, 200, "1", "invalid_image", 5, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Request-ID", "req_test")
				w.WriteHeader(tc.status)
				writeJSON(w, tc.body)
			}, nil)
			stdout, err := runCLI(t, append(generateArgs(t), "--base-url", srv.URL, "--model", "m", "--output-dir", t.TempDir(), "--count", tc.count, "--json", "猫")...)
			if tc.wantExit == 0 && err != nil {
				t.Fatal(err)
			}
			if tc.wantExit != 0 && (err == nil || exitCodeOf(err) != tc.wantExit) {
				t.Fatalf("err=%v", err)
			}
			var result struct {
				SchemaVersion int             `json:"schema_version"`
				Status        string          `json:"status"`
				RequestID     string          `json:"request_id"`
				Images        []savedImage    `json:"images"`
				Response      json.RawMessage `json:"response"`
				Error         *struct {
					Code             string                     `json:"code"`
					Stage            string                     `json:"stage"`
					GenerationStatus string                     `json:"generation_status"`
					Upstream         map[string]json.RawMessage `json:"upstream"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(stdout), &result); err != nil {
				t.Fatalf("result=%s: %v", stdout, err)
			}
			if result.SchemaVersion != 1 || result.RequestID != "req_test" || len(result.Images) != tc.saved {
				t.Fatalf("result=%s", stdout)
			}
			for _, img := range result.Images {
				raw, err := os.ReadFile(img.Path)
				if err != nil || !bytes.Equal(raw, samplePNG) {
					t.Fatalf("saved=%v, %v", img, err)
				}
			}
			if tc.wantCode == "" {
				if result.Status != "succeeded" || result.Error != nil || !bytes.Contains(result.Response, []byte("9007199254740993")) || bytes.Contains(result.Response, []byte("b64_json")) {
					t.Fatalf("metadata=%s", stdout)
				}
			} else if result.Status != "failed" || result.Error == nil || result.Error.Code != tc.wantCode {
				t.Fatalf("failure=%s", stdout)
			}
			if tc.name == "partial" && result.Error.GenerationStatus != "completed" {
				t.Fatalf("generation status=%s", stdout)
			}
			if tc.name == "upstream" && string(result.Error.Upstream["code"]) != `"blocked"` {
				t.Fatalf("upstream=%s", stdout)
			}
		})
	}
}

func TestOfficialImageParameters(t *testing.T) {
	resetEnv(t)
	svg := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><path d="M0 0h10v10z"/></svg>`)
	gif := []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;")
	for _, tc := range []struct {
		name   string
		flags  []string
		want   map[string]any
		raw    []byte
		format string
	}{
		{"svg", []string{"--format", "svg", "--svg-model", "vector-model", "--svg-reasoning-effort", "high", "--complexity", "medium", "--quality", "xhigh", "--size", "301x999", "--safety-identifier", "user-猫", "--user", "legacy-user", "--stream=false"}, map[string]any{"output_format": "svg", "svg": map[string]any{"model": "vector-model", "reasoning": map[string]any{"effort": "high"}}, "complexity": "medium", "quality": "xhigh", "size": "301x999", "safety_identifier": "user-猫", "user": "legacy-user", "stream": false}, svg, "svg"},
		{"gif", []string{"--format", "gif", "--frame-count", "16", "--frame-duration-ms", "2400", "--moderation", "low", "--background", "opaque"}, map[string]any{"output_format": "gif", "frame_count": float64(16), "frame_duration_ms": float64(2400), "moderation": "low", "background": "opaque"}, gif, "gif"},
		{"legacy", []string{"--response-format", "b64_json", "--style", "vivid", "--quality", "hd"}, map[string]any{"response_format": "b64_json", "style": "vivid", "quality": "hd"}, samplePNG, "png"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&got)
				writeJSON(w, map[string]any{"data": []any{map[string]any{"b64_json": base64.StdEncoding.EncodeToString(tc.raw)}}})
			}, nil)
			stdout, err := runCLI(t, append(append(generateArgs(t), "--base-url", srv.URL, "--model", "m", "--output-dir", t.TempDir(), "--json", "猫"), tc.flags...)...)
			if err != nil {
				t.Fatal(err)
			}
			tc.want["model"] = "m"
			tc.want["prompt"] = "猫"
			actual, _ := json.Marshal(got)
			want, _ := json.Marshal(tc.want)
			if !bytes.Equal(actual, want) {
				t.Fatalf("request=%s want=%s", actual, want)
			}
			var result struct {
				Images []savedImage `json:"images"`
			}
			if err := json.Unmarshal([]byte(stdout), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Images) != 1 || result.Images[0].Format != tc.format || filepath.Ext(result.Images[0].Path) != "."+tc.format {
				t.Fatalf("result=%s", stdout)
			}
			raw, err := os.ReadFile(result.Images[0].Path)
			if err != nil || !bytes.Equal(raw, tc.raw) {
				t.Fatalf("file: %v", err)
			}
		})
	}
}

func TestImageValidationBeforePOST(t *testing.T) {
	resetEnv(t)
	calls := 0
	srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeJSON(w, map[string]any{"data": []any{map[string]any{"b64_json": samplePNGB64}}})
	}, nil)
	cases := [][]string{
		{"--size="}, {"--quality="}, {"--format="}, {"--background="}, {"--moderation="}, {"--model="},
		{"--complexity", "extreme"}, {"--frame-count", "2"}, {"--format", "gif", "--frame-count", "17"},
		{"--format", "gif", "--frame-duration-ms", "19"}, {"--format", "gif", "--stream"},
		{"--format", "svg", "--count", "2"}, {"--format", "svg", "--stream"},
		{"--svg-model", "vector"}, {"--format", "svg", "--svg-model="},
		{"--format", "svg", "--svg-reasoning-effort", "high"},
		{"--format", "svg", "--svg-model", "vector", "--svg-reasoning-effort", "bad"},
		{"--partial-images", "0"}, {"--stream", "--partial-images", "4"},
		{"--safety-identifier", strings.Repeat("猫", 129)},
		{"--response-format", "bad"}, {"--style", "bad"},
	}
	for _, flags := range cases {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			stdout, err := runCLI(t, append(append(generateArgs(t), "--base-url", srv.URL, "--model", "m", "--output-dir", t.TempDir(), "--json", "猫"), flags...)...)
			if err == nil || exitCodeOf(err) != 2 {
				t.Fatalf("err=%v stdout=%s", err, stdout)
			}
			var result map[string]any
			if err := json.Unmarshal([]byte(stdout), &result); err != nil {
				t.Fatalf("invalid JSON=%s", stdout)
			}
			if result["status"] != "failed" {
				t.Fatal(result)
			}
		})
	}
	for _, prompt := range []string{strings.Repeat("猫", 32001), string([]byte{0xff}), "  "} {
		_, err := runCLI(t, append(generateArgs(t), "--base-url", srv.URL, "--model", "m", "--output-dir", t.TempDir(), prompt)...)
		if err == nil || exitCodeOf(err) != 2 {
			t.Fatalf("prompt of %d bytes: %v", len(prompt), err)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid requests sent: %d", calls)
	}
}

func TestEditReferenceEncodingAndOrder(t *testing.T) {
	resetEnv(t)
	local := filepath.Join(t.TempDir(), "a.png")
	if err := os.WriteFile(local, samplePNG, 0600); err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/edits" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request=%s %s", r.URL, r.Header)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		writeJSON(w, map[string]any{"data": []any{map[string]any{"b64_json": samplePNGB64}}})
	}))
	defer srv.Close()
	stdout, err := runCLI(t, "edit", "猫", "--image", local, "--image-file-id", "file_b", "--image-url", "https://example.test/c.png", "--mask-file-id", "file_mask", "--input-fidelity", "high", "--output-dir", t.TempDir(), "--base-url", srv.URL, "--config", filepath.Join(t.TempDir(), "missing"), "--json")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["model"]; ok {
		t.Fatalf("unexpected model: %s", got["model"])
	}
	if string(got["input_fidelity"]) != `"high"` || string(got["mask"]) != `{"file_id":"file_mask"}` {
		t.Fatalf("request=%v", got)
	}
	var refs []map[string]string
	if err := json.Unmarshal(got["images"], &refs); err != nil {
		t.Fatal(err)
	}
	if len(refs) != 3 || refs[0]["image_url"] != "data:image/png;base64,"+samplePNGB64 || refs[1]["file_id"] != "file_b" || refs[2]["image_url"] != "https://example.test/c.png" {
		t.Fatalf("order=%v", refs)
	}
	var result struct {
		Model  *string      `json:"model"`
		Images []savedImage `json:"images"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if result.Model != nil || len(result.Images) != 1 {
		t.Fatalf("result=%s", stdout)
	}
	raw, err := os.ReadFile(result.Images[0].Path)
	if err != nil || !bytes.Equal(raw, samplePNG) {
		t.Fatal(err)
	}
}

func TestMarkupAndReferenceValidation(t *testing.T) {
	resetEnv(t)
	source := writeEditPNG(t, "source.png", 2, 1, true)
	markup := writeEditPNG(t, "markup.png", 2, 1, false)
	wrong := writeEditPNG(t, "wrong.png", 1, 1, false)
	opaque := writeEditPNG(t, "opaque.png", 2, 1, true)
	var jpegBytes bytes.Buffer
	if err := jpeg.Encode(&jpegBytes, image.NewRGBA(image.Rect(0, 0, 2, 1)), nil); err != nil {
		t.Fatal(err)
	}
	exif, _ := hex.DecodeString("ffe1002245786966000049492a0008000000010012010300010000000600000000000000")
	oriented := writeEditFile(t, "rotated.jpg", append(append([]byte{0xff, 0xd8}, exif...), jpegBytes.Bytes()[2:]...))
	orientedMarkup := writeEditPNG(t, "oriented.png", 1, 2, false)
	rawMarkup, _ := os.ReadFile(markup)
	large := writeEditFile(t, "large.png", samplePNG)
	if err := os.Truncate(large, 50_000_000); err != nil {
		t.Fatal(err)
	}
	longURI := "data:image/png;base64," + strings.Repeat("A", 20971520)
	cases := []struct {
		name     string
		flags    []string
		good     bool
		encoding string
	}{
		{"multipart", []string{"--image", source, "--markup", markup}, true, "multipart/form-data"},
		{"oriented", []string{"--image", oriented, "--markup", orientedMarkup}, true, "multipart/form-data"},
		{"data", []string{"--image", source, "--markup-url", "data:image/png;base64," + base64.StdEncoding.EncodeToString(rawMarkup)}, true, "application/json"},
		{"svg JSON", []string{"--image", source, "--format", "svg", "--svg-reasoning-effort", "high"}, true, "application/json"},
		{"both overlays", []string{"--image", source, "--mask", markup, "--markup", markup}, false, ""},
		{"mask sources", []string{"--image", source, "--mask", markup, "--mask-file-id", "file_mask"}, false, ""},
		{"markup sources", []string{"--image", source, "--markup", markup, "--markup-file-id", "file_markup"}, false, ""},
		{"opaque markup", []string{"--image", source, "--markup", opaque}, false, ""},
		{"wrong size", []string{"--image", source, "--markup", wrong}, false, ""},
		{"json wrong mask", []string{"--image", source, "--image-file-id", "f", "--mask", wrong}, false, ""},
		{"not png mask", []string{"--image", oriented, "--mask", oriented, "--image-file-id", "f"}, false, ""},
		{"markup HTTP", []string{"--image", source, "--markup-url", "http://example.test/m.png"}, false, ""},
		{"bad data MIME", []string{"--image-url", "data:image/jpeg;base64," + samplePNGB64}, false, ""},
		{"bad base64", []string{"--image-url", "data:image/png;base64,???"}, false, ""},
		{"SVG input", []string{"--image-url", "data:image/svg+xml;base64,PHN2Zy8+"}, false, ""},
		{"empty ID", []string{"--image-file-id", " "}, false, ""},
		{"max bytes", []string{"--image", large}, false, ""},
		{"URI length", []string{"--image-url", longURI}, false, ""},
		{"gif mask", []string{"--image-file-id", "f", "--mask-file-id", "m", "--format", "gif"}, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if !strings.HasPrefix(r.Header.Get("Content-Type"), tc.encoding) {
					t.Errorf("encoding=%s", r.Header.Get("Content-Type"))
				}
				if tc.encoding == "multipart/form-data" {
					if err := r.ParseMultipartForm(1 << 20); err != nil {
						t.Error(err)
					} else {
						defer r.MultipartForm.RemoveAll()
						if len(r.MultipartForm.File["markup"]) != 1 {
							t.Error("missing markup")
						}
					}
				}
				if tc.name == "svg JSON" {
					var body map[string]json.RawMessage
					_ = json.NewDecoder(r.Body).Decode(&body)
					if string(body["svg"]) != `{"reasoning":{"effort":"high"}}` {
						t.Errorf("nested svg=%s", body["svg"])
					}
				}
				writeJSON(w, map[string]any{"data": []any{map[string]any{"b64_json": samplePNGB64}}})
			}))
			defer srv.Close()
			stdout, err := runCLI(t, append([]string{"edit", "猫", "--config", filepath.Join(t.TempDir(), "missing"), "--base-url", srv.URL, "--output-dir", t.TempDir()}, tc.flags...)...)
			if tc.good {
				if err != nil || requests != 1 {
					t.Fatalf("err=%v requests=%d", err, requests)
				}
				path := strings.Split(stdout, "\n")[0]
				if _, err := os.Stat(path); err != nil {
					t.Fatal(err)
				}
			} else if err == nil || exitCodeOf(err) != 2 || requests != 0 {
				t.Fatalf("err=%v requests=%d", err, requests)
			}
		})
	}
}

func TestQuotaAndRetryAfter(t *testing.T) {
	resetEnv(t)
	for _, tc := range []struct {
		name, code, kind, after, wantCode, scope string
		status                                   int
	}{
		{"quota", "insufficient_quota", "insufficient_quota", "", "upstream_error", "none", 429},
		{"credit", "credit_balance_exhausted", "invalid_request_error", "", "upstream_error", "none", 429},
		{"rate wait", "slow_down", "rate_limit_error", "1", "upstream_error", "request", 429},
		{"server", "internal_error", "server_error", "", "upstream_error", "none", 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Retry-After", tc.after)
				w.Header().Set("X-Request-ID", "req_retry")
				w.WriteHeader(tc.status)
				writeJSON(w, map[string]any{"error": map[string]any{"code": tc.code, "type": tc.kind, "message": "try later"}})
			}, nil)
			stdout, err := runCLI(t, append(generateArgs(t), "--base-url", srv.URL, "--model", "m", "--output-dir", t.TempDir(), "--json", "--timeout", "50ms", "猫")...)
			if err == nil || exitCodeOf(err) != 5 || requests.Load() != 1 {
				t.Fatalf("err=%v requests=%d", err, requests.Load())
			}
			var result struct {
				Error struct {
					Code  string `json:"code"`
					Retry struct {
						Scope string   `json:"scope"`
						After *float64 `json:"after_seconds"`
					} `json:"retry"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(stdout), &result); err != nil {
				t.Fatal(err)
			}
			if result.Error.Code != tc.wantCode || result.Error.Retry.Scope != tc.scope {
				t.Fatalf("result=%s", stdout)
			}
			if tc.after != "" && (result.Error.Retry.After == nil || *result.Error.Retry.After != 1) {
				t.Fatalf("retry-after=%s", stdout)
			}
		})
	}
}

func TestStreamingLifecycle(t *testing.T) {
	resetEnv(t)
	for _, tc := range []struct {
		name             string
		events           []string
		count            string
		wantCode         string
		finals, previews int
		jsonl            bool
	}{
		{"multi", []string{"partial", "unknown", "partial", "completed", "completed"}, "2", "", 2, 2, true},
		{"large", []string{"large", "completed"}, "1", "", 1, 1, false},
		{"no preview", []string{"completed"}, "1", "", 1, 0, true},
		{"incomplete", []string{"partial", "completed", "done"}, "2", "incomplete_stream", 1, 1, true},
		{"error", []string{"partial", "error"}, "1", "upstream_error", 0, 1, false},
		{"mismatch", []string{"mismatch"}, "1", "invalid_response", 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Header.Get("Accept") != "text/event-stream" {
					t.Error("missing SSE Accept")
				}
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body["stream"] != true || body["partial_images"] != float64(0) {
					t.Errorf("request=%v", body)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("X-Request-ID", "req_stream")
				_, _ = fmt.Fprint(w, ": heartbeat\r\n\r\n")
				for _, event := range tc.events {
					if event == "done" {
						_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
						continue
					}
					typ := "image_generation." + event
					obj := map[string]any{"type": typ, "b64_json": samplePNGB64, "output_format": "png"}
					switch event {
					case "partial", "large":
						typ = "image_generation.partial_image"
						obj["type"] = typ
						obj["partial_image_index"] = 0
						if event == "large" {
							obj["vendor_padding"] = strings.Repeat("x", 70000)
						}
					case "completed":
						obj["usage"] = map[string]any{"total_tokens": 0}
					case "unknown":
						typ = "vendor.progress"
						obj = map[string]any{"type": typ, "progress": 50}
					case "error":
						typ = "error"
						obj = map[string]any{"type": "error", "error": map[string]any{"code": "stream_failed", "message": "失败"}}
					case "mismatch":
						typ = "image_generation.completed"
						obj["type"] = "image_edit.completed"
					}
					raw, _ := json.Marshal(obj)
					// 合法多行 data，在首个逗号处分开，再模拟网络分块。
					first, rest, ok := strings.Cut(string(raw), ",")
					message := "event: " + typ + "\r\ndata: " + first
					if ok {
						message += ",\r\ndata: " + rest
					}
					message += "\r\n\r\n"
					for len(message) > 0 {
						n := min(4000, len(message))
						_, _ = io.WriteString(w, message[:n])
						message = message[n:]
						w.(http.Flusher).Flush()
					}
				}
			}, nil)
			mode := "--json"
			if tc.jsonl {
				mode = "--jsonl"
			}
			stdout, err := runCLI(t, append(generateArgs(t), "--base-url", srv.URL, "--model", "m", "--output-dir", t.TempDir(), "--stream", "--partial-images", "0", "--count", tc.count, mode, "猫")...)
			if tc.wantCode == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantCode != "" && (err == nil || exitCodeOf(err) != 5) {
				t.Fatalf("err=%v", err)
			}
			// captureRun 混合 warning stderr；仅机器输出行参与解析。
			var result map[string]json.RawMessage
			terminal := 0
			sequence := 0
			for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
				if strings.HasPrefix(line, "image-gen:") {
					continue
				}
				var row map[string]json.RawMessage
				if err := json.Unmarshal([]byte(line), &row); err != nil {
					t.Fatalf("invalid JSON line: %v", err)
				}
				if !tc.jsonl {
					result = row
					terminal++
					continue
				}
				var seq int
				_ = json.Unmarshal(row["sequence"], &seq)
				if seq != sequence+1 {
					t.Errorf("sequence %d after %d", seq, sequence)
				}
				sequence = seq
				if string(row["type"]) == `"result"` {
					_ = json.Unmarshal(row["result"], &result)
					terminal++
				}
			}
			if terminal != 1 {
				t.Fatalf("terminal events=%d", terminal)
			}
			var finals []savedImage
			var previews []struct {
				Path    string `json:"path"`
				Partial int    `json:"partial_image_index"`
			}
			var events []map[string]json.RawMessage
			_ = json.Unmarshal(result["images"], &finals)
			_ = json.Unmarshal(result["previews"], &previews)
			_ = json.Unmarshal(result["events"], &events)
			if len(finals) != tc.finals || len(previews) != tc.previews || requests.Load() != 1 {
				t.Fatalf("counts finals=%d previews=%d requests=%d", len(finals), len(previews), requests.Load())
			}
			paths := map[string]bool{}
			for _, img := range finals {
				raw, err := os.ReadFile(img.Path)
				if err != nil || !bytes.Equal(raw, samplePNG) {
					t.Fatal(err)
				}
				paths[img.Path] = true
			}
			for _, preview := range previews {
				if preview.Partial != 0 || paths[preview.Path] {
					t.Fatalf("preview=%+v", preview)
				}
				paths[preview.Path] = true
				if _, err := os.Stat(preview.Path); err != nil {
					t.Fatal(err)
				}
			}
			for _, event := range events {
				if _, ok := event["b64_json"]; ok {
					t.Error("base64 leaked to metadata")
				}
			}
			if string(result["response"]) != "null" {
				t.Error("SSE fabricated JSON response")
			}
			if tc.wantCode != "" {
				var failure struct {
					Code string `json:"code"`
				}
				_ = json.Unmarshal(result["error"], &failure)
				if failure.Code != tc.wantCode {
					t.Fatalf("error=%s", result["error"])
				}
			}
		})
	}
}

func TestStreamingFallbackAndTimeout(t *testing.T) {
	resetEnv(t)
	for _, tc := range []struct {
		name     string
		fallback bool
		mode     string
	}{
		{"fallback JSON", true, "--json"}, {"fallback JSONL", true, "--jsonl"}, {"timeout", false, "--json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if tc.fallback {
					writeJSON(w, map[string]any{"data": []any{map[string]any{"b64_json": samplePNGB64}}})
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"type\":\"image_generation.partial_image\",\"partial_image_index\":0,\"b64_json\":%q}\n\n", samplePNGB64)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}, nil)
			stdout, err := runCLI(t, append(generateArgs(t), "--base-url", srv.URL, "--model", "m", "--output-dir", t.TempDir(), "--stream", tc.mode, "--timeout", "100ms", "猫")...)
			if tc.fallback {
				if err != nil || !strings.Contains(stdout, "stream_ignored") {
					t.Fatalf("err=%v result=%s", err, stdout)
				}
			} else if err == nil || exitCodeOf(err) != 4 || !strings.Contains(stdout, `"code":"request_timeout"`) {
				t.Fatalf("err=%v result=%s", err, stdout)
			}
			if requests.Load() != 1 {
				t.Fatal("repeated POST")
			}
		})
	}
}

func TestDownloadRetriesKeepGeneratedResult(t *testing.T) {
	resetEnv(t)
	var posts, downloads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/asset" {
			if r.Header.Get("Authorization") != "" {
				t.Error("API key leaked to asset")
			}
			if downloads.Add(1) == 1 {
				w.WriteHeader(503)
				return
			}
			_, _ = w.Write(samplePNG)
			return
		}
		posts.Add(1)
		writeJSON(w, map[string]any{"data": []any{map[string]any{"url": "http://" + r.Host + "/asset"}}})
	}))
	defer srv.Close()
	stdout, err := runCLI(t, append(generateArgs(t), "--base-url", srv.URL, "--model", "m", "--output-dir", t.TempDir(), "--json", "猫")...)
	if err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 1 || downloads.Load() != 2 {
		t.Fatalf("posts=%d downloads=%d", posts.Load(), downloads.Load())
	}
	var result struct {
		Images []savedImage `json:"images"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(result.Images[0].Path)
	if err != nil || !bytes.Equal(raw, samplePNG) {
		t.Fatal(err)
	}
}

func TestNormalizedFormatsAndMachineFailures(t *testing.T) {
	resetEnv(t)
	var calls atomic.Int32
	srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["output_format"] != "svg" {
			t.Errorf("format=%v", body["output_format"])
		}
		writeJSON(w, map[string]any{"data": []any{map[string]any{"b64_json": base64.StdEncoding.EncodeToString([]byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`))}}})
	}, nil)
	stdout, err := runCLI(t, append(generateArgs(t), "--base-url", srv.URL, "--model", "m", "--output-dir", t.TempDir(), "--format", " SVG ", "--svg-model", "v", "--json", "猫")...)
	if err != nil || strings.Contains(stdout, "output_format_mismatch") {
		t.Fatalf("normalized format err=%v result=%s", err, stdout)
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"uppercase constraint", []string{"--format", "SVG", "--count", "2"}, "invalid_argument"},
		{"missing auth", []string{}, "auth_resolution_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "missing auth" {
				t.Setenv("IMAGE_GEN_API_KEY", "")
			}
			out, err := runCLI(t, append(append(generateArgs(t), "--base-url", srv.URL, "--model", "m", "--output-dir", t.TempDir(), "--json", "猫"), tc.args...)...)
			if err == nil || !strings.Contains(out, `"code":"`+tc.want+`"`) {
				t.Fatalf("err=%v output=%s", err, out)
			}
		})
	}
	if calls.Load() != 1 {
		t.Fatalf("invalid calls: %d", calls.Load())
	}
}

func TestPromptCodepointBoundaryAndConfigFailure(t *testing.T) {
	resetEnv(t)
	prompt := strings.Repeat("猫🙂", 16000)
	srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["prompt"] != prompt {
			t.Error("prompt changed")
		}
		writeJSON(w, map[string]any{"data": []any{map[string]any{"b64_json": samplePNGB64}}})
	}, nil)
	for _, stdin := range []bool{false, true} {
		t.Run(fmt.Sprint(stdin), func(t *testing.T) {
			p := "  " + prompt + "  "
			if stdin {
				oldReader, oldTTY := stdinReader, stdinTTY
				stdinReader = strings.NewReader(p)
				stdinTTY = func() bool { return false }
				t.Cleanup(func() { stdinReader, stdinTTY = oldReader, oldTTY })
				p = "-"
			}
			_, err := runCLI(t, append(generateArgs(t), "--base-url", srv.URL, "--model", "m", "--output-dir", t.TempDir(), p)...)
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	cfg := writeEditFile(t, "bad.toml", []byte("invalid = ["))
	stdout, err := runCLI(t, "generate", "--config", cfg, "--json", "猫")
	if err == nil || exitCodeOf(err) != 3 || !strings.Contains(stdout, `"code":"invalid_config"`) {
		t.Fatalf("err=%v result=%s", err, stdout)
	}
}

func TestUnexpectedExtraImagesArePreserved(t *testing.T) {
	resetEnv(t)
	second := append(append([]byte{}, samplePNG...), []byte("second-image")...)
	srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []any{map[string]any{"b64_json": samplePNGB64}, map[string]any{"b64_json": base64.StdEncoding.EncodeToString(second)}}})
	}, nil)
	stdout, err := runCLI(t, append(generateArgs(t), "--base-url", srv.URL, "--model", "m", "--output-dir", t.TempDir(), "--json", "猫")...)
	if err == nil || exitCodeOf(err) != 5 {
		t.Fatalf("err=%v", err)
	}
	var result struct {
		Images []savedImage `json:"images"`
		Error  struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if result.Error.Code != "incomplete_result" || len(result.Images) != 2 || result.Images[0].Path == result.Images[1].Path {
		t.Fatalf("result=%s", stdout)
	}
	for i, want := range [][]byte{samplePNG, second} {
		raw, err := os.ReadFile(result.Images[i].Path)
		if err != nil || !bytes.Equal(raw, want) {
			t.Fatalf("image %d: %v", i, err)
		}
	}
}

func TestStreamingResponseResourceLimit(t *testing.T) {
	resetEnv(t)
	var requests atomic.Int32
	srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		line := ":" + strings.Repeat(" ", 8190) + "\n"
		for i := 0; i < 24577; i++ {
			if _, err := io.WriteString(w, line); err != nil {
				return
			}
		}
	}, nil)
	stdout, err := runCLI(t, append(generateArgs(t), "--base-url", srv.URL, "--model", "m", "--output-dir", t.TempDir(), "--stream", "--json", "猫")...)
	if err == nil || exitCodeOf(err) != 1 || !strings.Contains(stdout, `"code":"response_limit_exceeded"`) || requests.Load() != 1 {
		t.Fatalf("err=%v result=%s", err, stdout)
	}
}

func TestJSONSyntaxErrorAfterModeAndClosedOutput(t *testing.T) {
	resetEnv(t)
	stdout, err := runCLI(t, "generate", "--json", "--count", "invalid", "猫")
	if err == nil || exitCodeOf(err) != 2 || !strings.Contains(stdout, `"status":"failed"`) {
		t.Fatalf("err=%v output=%s", err, stdout)
	}
	srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []any{map[string]any{"b64_json": samplePNGB64}}})
	}, nil)
	closed, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	original := os.Stdout
	os.Stdout = closed
	defer func() { os.Stdout = original }()
	err = run(append(generateArgs(t), "--base-url", srv.URL, "--model", "m", "--output-dir", t.TempDir(), "--json", "猫"))
	if err == nil || exitCodeOf(err) != 1 || !strings.Contains(err.Error(), "stdout") {
		t.Fatalf("err=%v", err)
	}
}

func TestErrorResponseMetadataAndStreamFallback(t *testing.T) {
	resetEnv(t)
	for _, tc := range []struct {
		name           string
		status         int
		body, wantCode string
	}{
		{"http JSON", 400, `{"error":{"code":"bad_input","message":"bad"},"vendor_trace":"trace-1"}`, "upstream_error"},
		{"application error", 200, `{"error":{"code":"bad_input","message":"bad"},"vendor_trace":"trace-1"}`, "upstream_error"},
		{"invalid fallback", 200, `<html>bad</html>`, "invalid_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}, nil)
			stdout, err := runCLI(t, append(generateArgs(t), "--base-url", srv.URL, "--model", "m", "--output-dir", t.TempDir(), "--stream", "--json", "猫")...)
			if err == nil || exitCodeOf(err) != 5 {
				t.Fatalf("err=%v", err)
			}
			var result struct {
				Response map[string]json.RawMessage `json:"response"`
				Warnings []json.RawMessage          `json:"warnings"`
				Error    struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(strings.Split(stdout, "\n")[0]), &result); err != nil {
				t.Fatal(err)
			}
			if result.Error.Code != tc.wantCode || len(result.Warnings) != 0 {
				t.Fatalf("result=%s", stdout)
			}
			if tc.name != "invalid fallback" && string(result.Response["vendor_trace"]) != `"trace-1"` {
				t.Fatalf("lost response=%s", stdout)
			}
		})
	}
}

func TestRequestHeaderSurvivesErrorBodyTimeout(t *testing.T) {
	resetEnv(t)
	srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-ID", "req_timeout")
		w.WriteHeader(429)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}, nil)
	stdout, err := runCLI(t, append(generateArgs(t), "--base-url", srv.URL, "--model", "m", "--output-dir", t.TempDir(), "--json", "--timeout", "50ms", "猫")...)
	if err == nil || exitCodeOf(err) != 4 || !strings.Contains(stdout, `"request_id":"req_timeout"`) || !strings.Contains(stdout, `"generation_status":"not_started"`) {
		t.Fatalf("err=%v result=%s", err, stdout)
	}
}

func TestRejectsIncompleteGIFSignature(t *testing.T) {
	resetEnv(t)
	srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []any{map[string]any{"b64_json": base64.StdEncoding.EncodeToString([]byte("GIF8upstream error: generation failed"))}}})
	}, nil)
	outDir := t.TempDir()
	stdout, err := runCLI(t, append(generateArgs(t), "--base-url", srv.URL, "--model", "m", "--output-dir", outDir, "--format", "gif", "--json", "猫")...)
	if err == nil || exitCodeOf(err) != 5 || !strings.Contains(stdout, `"code":"invalid_image"`) {
		t.Fatalf("err=%v result=%s", err, stdout)
	}
	if files := listFiles(t, outDir); len(files) != 0 {
		t.Fatalf("invalid output files: %v", files)
	}
}

func TestMalformedRowsDoNotLeakImageData(t *testing.T) {
	resetEnv(t)
	for _, status := range []int{200, 400} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv := newGateway(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				writeJSON(w, map[string]any{"data": []any{map[string]any{"b64_json": samplePNGB64, "url": "https://example.test/result.png", "extra": 7}, "invalid row"}, "vendor": true})
			}, nil)
			stdout, err := runCLI(t, append(generateArgs(t), "--base-url", srv.URL, "--model", "m", "--output-dir", t.TempDir(), "--json", "猫")...)
			if err == nil || exitCodeOf(err) != 5 {
				t.Fatalf("err=%v", err)
			}
			var result struct {
				Response map[string]json.RawMessage `json:"response"`
			}
			if err := json.Unmarshal([]byte(stdout), &result); err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(result.Response["data"], []byte("b64_json")) || bytes.Contains(result.Response["data"], []byte(samplePNGB64)) {
				t.Fatalf("Base64 leaked in response=%s", result.Response["data"])
			}
			var items []json.RawMessage
			if err := json.Unmarshal(result.Response["data"], &items); err != nil {
				t.Fatal(err)
			}
			var first map[string]any
			_ = json.Unmarshal(items[0], &first)
			if first["url"] != "https://example.test/result.png" || first["extra"] != float64(7) || string(items[1]) != `"invalid row"` || string(result.Response["vendor"]) != "true" {
				t.Fatalf("metadata changed: %s", stdout)
			}
		})
	}
}
