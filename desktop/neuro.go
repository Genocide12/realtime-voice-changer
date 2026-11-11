package main

// Нейро-движок: офлайн-конверсия тембра записи по голосу-референсу (seed-vc).
// Всё ставится в <папка exe>/neuro-engine: встроенный Python + PyTorch + seed-vc.
// Референсы — обычные wav/mp3 в neuro-engine/references.

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

//go:embed neuro_convert.py
var neuroConvertScript []byte

const (
	pyEmbedURL = "https://www.python.org/ftp/python/3.12.10/python-3.12.10-embed-amd64.zip"
	getPipURL  = "https://bootstrap.pypa.io/get-pip.py"
	refURL     = "https://github.com/Plachtaa/seed-vc/raw/main/examples/reference/s1p1.wav"
	cuIndex    = "https://download.pytorch.org/whl/cu126"
	cpuIndex   = "https://download.pytorch.org/whl/cpu"
)

var (
	neuroDir   string
	neuroMu    sync.Mutex
	neuroBusy  bool
	neuroJob   *neuroJobState // текущая (одна) задача конвертации
	deviceOnce sync.Once
	deviceName string
)

func init() { neuroDir = filepath.Join(exeDir0(), "neuro-engine") }

func exeDir0() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

type neuroStatus struct {
	Backend    bool     `json:"backend"`
	Installed  bool     `json:"installed"`
	Installing bool     `json:"installing"`
	Progress   string   `json:"progress"`
	Device     string   `json:"device"`
	References []string `json:"references"`
	Busy       bool     `json:"busy"`
}

func neuroPythonPath() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(neuroDir, "py", "python.exe")
	}
	return filepath.Join(neuroDir, "venv", "bin", "python")
}

func neuroInstalled() bool {
	_, err := os.Stat(filepath.Join(neuroDir, ".installed"))
	return err == nil
}

func neuroReferences() []string {
	out := []string{}
	entries, err := os.ReadDir(filepath.Join(neuroDir, "references"))
	if err != nil {
		return out
	}
	for _, e := range entries {
		n := strings.ToLower(e.Name())
		if strings.HasSuffix(n, ".wav") || strings.HasSuffix(n, ".mp3") || strings.HasSuffix(n, ".flac") {
			out = append(out, e.Name())
		}
	}
	return out
}

func neuroRun(py string, args []string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, py, args...)
	cmd.Dir = neuroDir
	cmd.Env = append(os.Environ(), "HF_HOME="+filepath.Join(neuroDir, "hf-cache"))
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func neuroStatusHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	st := neuroStatus{Backend: true, References: []string{}}
	_, statErr := os.Stat(neuroPythonPath())
	st.Installed = statErr == nil && neuroInstalled()
	neuroMu.Lock()
	st.Installing = neuroBusy
	st.Busy = neuroJob != nil && neuroJob.State == "running"
	var last string
	if neuroJob != nil {
		last = neuroJob.Msg
	}
	neuroMu.Unlock()
	if st.Installing {
		if b, err := os.ReadFile(filepath.Join(neuroDir, "install.log")); err == nil {
			lines := strings.Split(strings.TrimSpace(string(b)), "\n")
			if len(lines) > 0 {
				st.Progress = lines[len(lines)-1]
			}
		}
	} else if st.Busy {
		st.Progress = last
	}
	if st.Installed {
		deviceOnce.Do(func() {
			out, err := neuroRun(neuroPythonPath(), []string{"-c", "import torch;print('cuda' if torch.cuda.is_available() else 'cpu')"}, 120*time.Second)
			deviceName = strings.TrimSpace(out)
			if err != nil || deviceName == "" {
				deviceName = "cpu"
			}
		})
		st.Device = deviceName
		st.References = neuroReferences()
	}
	json.NewEncoder(w).Encode(st)
}

func neuroLogf(format string, a ...interface{}) {
	f, err := os.OpenFile(filepath.Join(neuroDir, "install.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, format+"\n", a...)
}

func downloadTo(url, dest string, logName string) error {
	resp, err := http.Get(url) // #nosec G107 -- фиксированные URL дистрибутивов
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: HTTP %d", logName, resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func unzipTo(src, dest string) error {
	z, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer z.Close()
	for _, f := range z.File {
		name := filepath.Join(dest, filepath.FromSlash(f.Name))
		if !strings.HasPrefix(name, filepath.Clean(dest)+string(os.PathSeparator)) {
			continue
		}
		if f.FileInfo().IsDir() {
			os.MkdirAll(name, 0755)
			continue
		}
		os.MkdirAll(filepath.Dir(name), 0755)
		r, err := f.Open()
		if err != nil {
			return err
		}
		wf, err := os.Create(name)
		if err != nil {
			r.Close()
			return err
		}
		_, err = io.Copy(wf, r)
		r.Close()
		wf.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// pipPins — проверенная связка версий (валидирована на CPU в песочнице 2026-11-11).
func pipPins(index string) []string {
	return []string{"install", "--no-warn-script-location", "--no-input",
		"torch==2.15.0", "torchvision==0.29.2", "torchaudio==2.11.1",
		"--index-url", index}
}

func pipRest() []string {
	return []string{"install", "--no-warn-script-location", "--no-input",
		"transformers==4.49.0", "huggingface_hub==0.36.2", "seed-vc", "soundfile"}
}

func neuroInstallRun() {
	defer func() { neuroMu.Lock(); neuroBusy = false; neuroMu.Unlock() }()
	os.MkdirAll(filepath.Join(neuroDir, "references"), 0755)
	os.MkdirAll(filepath.Join(neuroDir, "hf-cache"), 0755)
	os.Remove(filepath.Join(neuroDir, ".installed"))

	py := neuroPythonPath()
	haveGPU := hasNvidia()

	// 1. Python (Windows — встроенный дистрибутив; Linux/mac — системный python3 + venv)
	if runtime.GOOS == "windows" {
		if _, err := os.Stat(py); err != nil {
			neuroLogf("1/6 Скачиваю Python 3.12 (11 МБ)…")
			zp := filepath.Join(neuroDir, "py.zip")
			if err := downloadTo(pyEmbedURL, zp, "Python"); err != nil {
				neuroLogf("ОШИБКА скачивания Python: %v", err)
				return
			}
			neuroLogf("2/6 Распаковываю Python…")
			os.MkdirAll(filepath.Join(neuroDir, "py"), 0755)
			if err := unzipTo(zp, filepath.Join(neuroDir, "py")); err != nil {
				neuroLogf("ОШИБКА распаковки: %v", err)
				return
			}
			os.Remove(zp)
			// включаем site-packages в ._pth
			pth := filepath.Join(neuroDir, "py", "python312._pth")
			if b, err := os.ReadFile(pth); err == nil {
				s := strings.ReplaceAll(string(b), "#import site", "import site\nLib/site-packages")
				os.WriteFile(pth, []byte(s), 0644)
			}
			neuroLogf("3/6 Ставлю pip…")
			gp := filepath.Join(neuroDir, "get-pip.py")
			if err := downloadTo(getPipURL, gp, "get-pip"); err != nil {
				neuroLogf("ОШИБКА get-pip: %v", err)
				return
			}
			if _, err := neuroRun(py, []string{gp, "--no-warn-script-location"}, 10*time.Minute); err != nil {
				neuroLogf("ОШИБКА pip: %v", err)
				return
			}
			os.Remove(gp)
		}
	} else {
		if _, err := exec.LookPath("python3"); err != nil {
			neuroLogf("ОШИБКА: нужен Python 3.10+ (установи python3 и повтори)")
			return
		}
		if _, err := os.Stat(py); err != nil {
			neuroLogf("1/6 Создаю окружение Python…")
			out, err := neuroRun("python3", []string{"-m", "venv", filepath.Join(neuroDir, "venv")}, 5*time.Minute)
			if err != nil {
				neuroLogf("ОШИБКА venv: %v %s", err, tail(out))
				return
			}
		}
	}

	// 2. PyTorch (CUDA или CPU)
	if haveGPU {
		neuroLogf("4/6 Скачиваю PyTorch для NVIDIA (~2.5 ГБ) — это самый долгий шаг, 5–20 минут…")
		out, err := neuroRun(py, pipPins(cuIndex), 40*time.Minute)
		if err != nil {
			neuroLogf("CUDA-сборка не встала (%s), пробую CPU-сборку…", tail(out))
			if _, err2 := neuroRun(py, pipPins(cpuIndex), 40*time.Minute); err2 != nil {
				neuroLogf("ОШИБКА PyTorch: %v", err2)
				return
			}
		}
	} else {
		neuroLogf("4/6 Скачиваю PyTorch CPU (~200 МБ)…")
		if out, err := neuroRun(py, pipPins(cpuIndex), 40*time.Minute); err != nil {
			neuroLogf("ОШИБКА PyTorch: %v %s", err, tail(out))
			return
		}
	}

	// 3. seed-vc и зависимости
	neuroLogf("5/6 Ставлю нейро-модели seed-vc (несколько минут)…")
	if out, err := neuroRun(py, pipRest(), 30*time.Minute); err != nil {
		neuroLogf("ОШИБКА seed-vc: %v %s", err, tail(out))
		return
	}

	// 4. convert.py + референсы
	os.WriteFile(filepath.Join(neuroDir, "convert.py"), neuroConvertScript, 0644)
	if _, err := os.Stat(filepath.Join(neuroDir, "references", "s1p1.wav")); err != nil {
		neuroLogf("6/6 Скачиваю голос-референс по умолчанию…")
		downloadTo(refURL, filepath.Join(neuroDir, "references", "s1p1.wav"), "референс")
	}

	// 5. проверка импорта
	if out, err := neuroRun(py, []string{"-c", "import torch, torchaudio, seed_vc; print('ok')"}, 5*time.Minute); err != nil || !strings.Contains(out, "ok") {
		neuroLogf("ОШИБКА проверки окружения: %v %s", err, tail(out))
		return
	}

	os.WriteFile(filepath.Join(neuroDir, ".installed"), []byte(time.Now().Format(time.RFC3339)), 0644)
	neuroLogf("ГОТОВО — движок установлен. Устройство: %s", map[bool]string{true: "GPU (CUDA)", false: "CPU"}[hasNvidia()])
}

func hasNvidia() bool {
	if runtime.GOOS == "windows" {
		_, err := exec.LookPath("nvidia-smi")
		if err != nil {
			for _, d := range []string{`C:\Windows\System32\nvidia-smi.exe`} {
				if _, e := os.Stat(d); e == nil {
					return true
				}
			}
			return false
		}
		return true
	}
	_, err := exec.LookPath("nvidia-smi")
	return err == nil
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return s[len(s)-300:]
	}
	return s
}

func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

type neuroJobState struct {
	ID     string
	State  string // running | done | error
	Msg    string
	Out    string
	Mode   string
	cancel func()
}

func neuroInstallHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	neuroMu.Lock()
	if neuroBusy {
		neuroMu.Unlock()
		json.NewEncoder(w).Encode(map[string]interface{}{"started": false, "reason": "уже устанавливается"})
		return
	}
	neuroBusy = true
	neuroMu.Unlock()
	go neuroInstallRun()
	json.NewEncoder(w).Encode(map[string]interface{}{"started": true})
}

func randID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func saveUpload(r *http.Request, field, dest string, maxMB int64) (string, error) {
	f, _, err := r.FormFile(field)
	if err != nil {
		return "", fmt.Errorf("нет файла %s", field)
	}
	defer f.Close()
	out, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	defer out.Close()
	n, err := io.Copy(out, io.LimitReader(f, maxMB*1024*1024+1))
	if err != nil {
		return "", err
	}
	if n > maxMB*1024*1024 {
		return "", fmt.Errorf("файл больше %d МБ", maxMB)
	}
	if n == 0 {
		return "", fmt.Errorf("пустой файл")
	}
	return dest, nil
}

func neuroConvertHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !neuroInstalled() {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "движок не установлен — сначала установка"})
		return
	}
	neuroMu.Lock()
	if neuroJob != nil && neuroJob.State == "running" {
		neuroMu.Unlock()
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "движок занят — дождись конца текущей обработки"})
		return
	}
	neuroMu.Unlock()

	r.ParseMultipartForm(220 << 20)
	jobsDir := filepath.Join(neuroDir, "jobs")
	id := time.Now().Format("20060102-150405") + "-" + randID()
	dir := filepath.Join(jobsDir, id)
	os.MkdirAll(dir, 0755)

	src, err := saveUpload(r, "file", filepath.Join(dir, "source"+fileExt(r.FormValue("name"), ".wav")), 200)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": err.Error()})
		return
	}
	ref := ""
	if refName := r.FormValue("reference"); refName != "" && !strings.Contains(refName, "/") && !strings.Contains(refName, "\\") {
		ref = filepath.Join(neuroDir, "references", refName)
		if _, err := os.Stat(ref); err != nil {
			ref = ""
		}
	}
	if ref == "" {
		if _, _, err := r.FormFile("reffile"); err == nil {
			ref, err = saveUpload(r, "reffile", filepath.Join(dir, "ref"+fileExt(r.FormValue("refname"), ".wav")), 30)
			if err != nil {
				json.NewEncoder(w).Encode(map[string]interface{}{"error": "референс: " + err.Error()})
				return
			}
		}
	}
	if ref == "" {
		def := neuroReferences()
		if len(def) == 0 {
			json.NewEncoder(w).Encode(map[string]interface{}{"error": "нет референса: добавь wav-файл голоса"})
			return
		}
		ref = filepath.Join(neuroDir, "references", def[0])
	}
	mode := r.FormValue("mode")
	if mode != "song" {
		mode = "speech"
	}
	steps := clamp(parseFloat0(r.FormValue("steps"), 25), 5, 100)
	shift := strings.TrimSpace(r.FormValue("shift"))
	if shift == "" {
		shift = "0"
	}
	for _, c := range shift {
		if c != '-' && (c < '0' || c > '9') && c != 'a' && c != 'u' && c != 't' && c != 'o' {
			shift = "0"
			break
		}
	}

	job := &neuroJobState{ID: id, State: "running", Msg: "готовлю задачу…", Mode: mode}
	neuroMu.Lock()
	neuroJob = job
	neuroMu.Unlock()

	go func() {
		py := neuroPythonPath()
		args := []string{filepath.Join(neuroDir, "convert.py"),
			"--source", src, "--reference", ref,
			"--output", filepath.Join(dir, "out.wav"),
			"--mode", mode, "--steps", fmt.Sprintf("%d", int(steps)),
			"--shift", shift,
			"--cache", filepath.Join(neuroDir, "hf-cache")}
		out, err := neuroRun(py, args, 60*time.Minute)
		if err != nil {
			job.State = "error"
			job.Msg = "ошибка конвертации: " + tail(out)
			neuroMu.Lock()
			neuroJob = job
			neuroMu.Unlock()
			return
		}
		if _, statErr := os.Stat(filepath.Join(dir, "out.wav")); statErr != nil {
			job.State = "error"
			job.Msg = "файл результата не создан: " + tail(out)
		} else {
			job.State = "done"
			job.Msg = "готово"
			job.Out = filepath.Join(dir, "out.wav")
		}
		neuroMu.Lock()
		neuroJob = job
		neuroMu.Unlock()
	}()
	json.NewEncoder(w).Encode(map[string]interface{}{"id": id})
}

func neuroJobHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	id := r.URL.Query().Get("id")
	neuroMu.Lock()
	job := neuroJob
	neuroMu.Unlock()
	if job == nil || job.ID != id {
		json.NewEncoder(w).Encode(map[string]interface{}{"state": "unknown"})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"state": job.State, "msg": job.Msg})
}

func neuroFileHandler(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" || len(id) > 40 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	for _, c := range id {
		ok := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || c == '-'
		if !ok {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
	}
	dir := filepath.Join(neuroDir, "jobs", id)
	http.ServeFile(w, r, filepath.Join(dir, "out.wav"))
}

func fileExt(name, def string) string {
	e := strings.ToLower(filepath.Ext(name))
	switch e {
	case ".wav", ".mp3", ".flac", ".ogg", ".m4a":
		return e
	}
	return def
}

func parseFloat0(s string, def float64) float64 {
	var v float64
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%f", &v); err != nil || v == 0 {
		return def
	}
	return v
}

func registerNeuroAPI(mux *http.ServeMux) {
	mux.HandleFunc("/api/neuro/status", neuroStatusHandler)
	mux.HandleFunc("/api/neuro/install", neuroInstallHandler)
	mux.HandleFunc("/api/neuro/convert", neuroConvertHandler)
	mux.HandleFunc("/api/neuro/job", neuroJobHandler)
	mux.HandleFunc("/api/neuro/file", neuroFileHandler)
}
