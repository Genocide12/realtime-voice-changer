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

func main() {
	l, port := listen()
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(indexHTML)
	})
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
