package main

import (
	"context"
	_ "embed"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"time"
)

//go:embed index.html
var indexHTML []byte

const basePort = 8765

func main() {
	if runtime.GOOS == "windows" {
		_ = exec.Command("cmd", "/c", "chcp", "65001").Run() // UTF-8 в консоли Windows
	}
	l, port := listen()
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(indexHTML)
	})
	srv := &http.Server{Handler: mux}

	fmt.Println("Голос-подменщик запущен.")
	fmt.Println("Адрес:", url)
	fmt.Println("Если браузер не открылся сам — вставь адрес в браузер вручную.")
	fmt.Println("Выключение: закрой это окно или нажми Ctrl+C.")
	go openBrowser(url)
	go func() {
		if err := srv.Serve(l); err != nil && err != http.ErrServerClosed {
			fmt.Println("Ошибка сервера:", err)
			os.Exit(1)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func listen() (net.Listener, int) {
	for p := basePort; p < basePort+50; p++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			return l, p
		}
	}
	fmt.Println("Свободный порт не найден (8765-8814).")
	os.Exit(1)
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
	if err := cmd.Start(); err != nil {
		fmt.Println("Открой вручную:", url)
	}
}
