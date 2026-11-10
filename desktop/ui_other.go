//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

// На не-Windows системах (для локальной проверки) открываем системный браузер.
func runUI(url string) {
	fmt.Println("Программа запущена:", url)
	fmt.Println("Нативное окно доступно в Windows-сборке; на этой системе открываю браузер.")
	openBrowser(url)
	waitInterrupt()
}

func fatal(text string) {
	fmt.Println(text)
	os.Exit(1)
}

// hideWindow на не-Windows ничего не делает (нет консольных окон).
func hideWindow() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}
