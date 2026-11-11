package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

//go:embed index.html
var indexHTML []byte

func main() {
	l, port := listen()
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(indexHTML)
	})
	registerAPI(mux)

	srv := &http.Server{Handler: mux}
	go func() {
		if err := srv.Serve(l); err != nil && err != http.ErrServerClosed {
			fmt.Println("Ошибка сервера:", err)
			os.Exit(1)
		}
	}()

	runUI(url) // блокируется, пока открыто окно (или фолбэк-браузер)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

/* ============ системные голоса (SAPI через PowerShell) ============ */

type sapiVoice struct {
	Name   string `json:"name"`
	Lang   string `json:"lang"`
	Gender string `json:"gender"`
}

var (
	voicesCache []sapiVoice
	voicesOnce  sync.Once
	voicesErr   error
)

func sapiVoices() ([]sapiVoice, error) {
	voicesOnce.Do(func() {
		if runtime.GOOS != "windows" {
			voicesCache = []sapiVoice{}
			return
		}
		ps := `$v = New-Object -ComObject SAPI.SpVoice; $v.GetVoices() | ForEach-Object { $n = $_.GetAttribute('Name'); $l = $_.GetAttribute('Language'); $g = $_.GetAttribute('Gender'); $l = if ($l) { [System.Globalization.CultureInfo]::new([int]('0x'+$l)).Name } else { '' }; "{0}|{1}|{2}" -f ($n -replace '^Microsoft ',''), $l, $g }`
		out, err := runPS(ps, 20*time.Second)
		if err != nil {
			voicesErr = err
			return
		}
		for _, line := range bytes.Split(out, []byte("\n")) {
			parts := bytes.Split(bytes.TrimSpace(line), []byte("|"))
			if len(parts) != 3 {
				continue
			}
			name := string(bytes.Trim(parts[0], " \r"))
			if name == "" {
				continue
			}
			voicesCache = append(voicesCache, sapiVoice{
				Name:   name,
				Lang:   string(bytes.Trim(parts[1], " \r")),
				Gender: string(bytes.Trim(parts[2], " \r")),
			})
		}
		if len(voicesCache) == 0 {
			voicesErr = fmt.Errorf("голоса не найдены")
		}
	})
	return voicesCache, voicesErr
}

func runPS(script string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.SysProcAttr = hideWindow()
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func registerAPI(mux *http.ServeMux) {
	mux.HandleFunc("/api/voices", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		vs, err := sapiVoices()
		if err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"voices": []sapiVoice{}, "error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"voices": vs})
	})

	mux.HandleFunc("/api/tts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		var req struct {
			Text   string  `json:"text"`
			Voice  string  `json:"voice"`
			Rate   float64 `json:"rate"`
			Pitch  float64 `json:"pitch"`
			Volume float64 `json:"volume"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		wav, err := sapiTTS(req.Text, req.Voice, req.Rate, req.Pitch, req.Volume)
		if err != nil {
			http.Error(w, "tts failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write(wav)
	})
}

func sapiTTS(text, voice string, rate, pitch, volume float64) ([]byte, error) {
	if runtime.GOOS != "windows" {
		return nil, fmt.Errorf("только в Windows-сборке")
	}
	tmp, err := os.MkdirTemp("", "vctts")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	wavPath := filepath.ToSlash(filepath.Join(tmp, "out.wav"))

	// SpMemoryStream: рендер в память -> wav-файл, без всплывающих окон.
	ps := fmt.Sprintf(`
$s = New-Object -ComObject SAPI.SpVoice
$s.Rate = %d
$s.Volume = %d
try { $s.SelectVoice('%s') } catch {}
$m = New-Object -ComObject SAPI.SpMemoryStream
$s.AudioOutputStream = $m
[void]$s.Speak([Console]::In.ReadToEnd())
[IO.File]::WriteAllBytes('%s', $m.GetData())
`,
		int(clamp(rate*10-10, -10, 10)), int(clamp(volume*100, 0, 100)), psEscape(voice), wavPath)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", ps)
	cmd.SysProcAttr = hideWindow()
	cmd.Stdin = bytes.NewReader([]byte(text))
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return os.ReadFile(wavPath)
}

func psEscape(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\'' {
			out = append(out, '\'')
		}
		out = append(out, r)
	}
	return string(out)
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}


func listen() (net.Listener, int) {
	for p := 8765; p < 8815; p++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			return l, p
		}
	}
	fatal("Не найден свободный порт (8765-8814).")
	return nil, 0
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func waitInterrupt() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
}
