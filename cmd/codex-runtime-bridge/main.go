package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/wavever/CCLimitPing/internal/assistant"
)

func main() {
	// This executable belongs to the independent account-preview installation.
	os.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if assistant.RunDesktopRuntimeBridge(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr) != nil {
		fmt.Fprintln(os.Stderr, "Codex 小助手后端连接已结束。")
		os.Exit(1)
	}
}
