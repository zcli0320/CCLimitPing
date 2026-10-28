// codex-helper is a local JSON bridge for the native window, not an HTTP
// service. Warmup commands execute a bounded isolated request; credentials are
// never exported and no listening port is opened.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/wavever/CCLimitPing/internal/assistant"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "accounts" {
		accountsCommand()
		return
	}
	if len(os.Args) >= 2 && os.Args[1] == "handoff" {
		handoffCommand()
		return
	}
	if len(os.Args) >= 2 && os.Args[1] == "handoff-preflight" {
		handoffPreflightCommand()
		return
	}
	if len(os.Args) >= 3 && os.Args[1] == "resume" {
		resumeCommand()
		return
	}
	if len(os.Args) >= 3 && os.Args[1] == "warmup" {
		warmupCommand()
		return
	}
	withQuota := len(os.Args) == 3 && os.Args[2] == "--quota"
	if (len(os.Args) != 2 && !withQuota) || os.Args[1] != "snapshot" {
		fmt.Fprintln(os.Stderr, "Usage: codex-helper snapshot [--quota]")
		os.Exit(2)
	}
	r, err := assistant.NewReader()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 17*time.Second)
	defer cancel()
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	snapshot := r.Snapshot(ctx)
	if withQuota {
		r.ReadAccountQuotas(ctx, &snapshot)
	}
	if err := enc.Encode(snapshot); err != nil {
		os.Exit(1)
	}
}

func accountsCommand() {
	r, err := assistant.NewReader()
	if err != nil {
		os.Exit(1)
	}
	if len(os.Args) == 3 && os.Args[2] == "status" {
		_ = json.NewEncoder(os.Stdout).Encode(r.AccountAllocation())
		return
	}
	if (len(os.Args) == 3 || (len(os.Args) == 4 && os.Args[3] == "--manual-switch")) && os.Args[2] == "recover-preparation" {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		var s assistant.HandoffState
		var e error
		if len(os.Args) == 4 {
			s, e = r.ReviewManualNativePreparation(ctx)
		} else {
			s, e = r.ReviewNativePreparation(ctx)
		}
		if e != nil {
			s.Message = e.Error()
		}
		_ = json.NewEncoder(os.Stdout).Encode(s)
		return
	}
	if len(os.Args) == 4 && os.Args[2] == "check" {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = json.NewEncoder(os.Stdout).Encode(r.CheckNativeAccounts(ctx, os.Args[3]))
		return
	}
	fmt.Fprintln(os.Stderr, "Usage: codex-helper accounts status | check desktop|cli | recover-preparation [--manual-switch]")
	os.Exit(2)
}

func handoffCommand() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: codex-helper handoff begin|cli <session> <threshold> [authorization] | disable | acknowledge | status")
		os.Exit(2)
	}
	r, err := assistant.NewReader()
	if err != nil {
		fmt.Fprintln(os.Stderr, "用户目录不可用")
		os.Exit(1)
	}
	if ((os.Args[2] == "cli" || os.Args[2] == "desktop-run") && len(os.Args) == 6) || ((os.Args[2] == "begin" || os.Args[2] == "desktop-begin") && len(os.Args) == 5) {
		threshold, parseErr := strconv.ParseFloat(os.Args[4], 64)
		if parseErr != nil {
			fmt.Fprintln(os.Stderr, "阈值无效")
			os.Exit(2)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
		defer stop()
		if os.Args[2] == "begin" || os.Args[2] == "desktop-begin" {
			ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			var state assistant.HandoffState
			var e error
			if os.Args[2] == "begin" {
				state, e = r.PrepareCLIHandoff(ctx, os.Args[3], threshold)
			} else {
				state, e = r.PrepareDesktopHandoff(ctx, os.Args[3], threshold)
			}
			if e != nil {
				state.Enabled = false
				state.Message = e.Error()
			}
			if json.NewEncoder(os.Stdout).Encode(state) != nil {
				os.Exit(1)
			}
			return
		}
		if os.Args[2] == "cli" {
			err = r.RunCLIHandoff(ctx, os.Args[3], threshold, os.Args[5])
		} else {
			err = r.RunDesktopHandoff(ctx, os.Args[3], threshold, os.Args[5])
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 3 {
		os.Exit(2)
	}
	var state assistant.HandoffState
	switch os.Args[2] {
	case "status":
		state = r.HandoffStatus()
	case "disable":
		state, err = r.ConfigureHandoff("", "", 0, false)
	case "acknowledge":
		state, err = r.AcknowledgeHandoff()
	default:
		os.Exit(2)
	}
	if err != nil {
		state.Message = err.Error()
	}
	if json.NewEncoder(os.Stdout).Encode(state) != nil {
		os.Exit(1)
	}
}

func handoffPreflightCommand() {
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "Usage: codex-helper handoff-preflight desktop|cli <session> <threshold>")
		os.Exit(2)
	}
	threshold, err := strconv.ParseFloat(os.Args[4], 64)
	if err != nil {
		fmt.Fprintln(os.Stderr, "阈值不是有效数字")
		os.Exit(2)
	}
	r, err := assistant.NewReader()
	if err != nil {
		fmt.Fprintln(os.Stderr, "用户目录不可用")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	report, err := r.CheckHandoffPreflight(ctx, os.Args[2], os.Args[3], threshold)
	if err != nil {
		report.Message = err.Error()
	}
	if report.Limitations == nil {
		report.Limitations = []string{}
	}
	if json.NewEncoder(os.Stdout).Encode(report) != nil {
		os.Exit(1)
	}
}

func resumeCommand() {
	r, err := assistant.NewReader()
	if err != nil {
		fmt.Fprintln(os.Stderr, "用户目录不可用")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if os.Args[2] == "cli" && len(os.Args) == 4 {
		if err = r.RunCLIResume(ctx, os.Args[3]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var report assistant.ResumeReport
	switch os.Args[2] {
	case "status":
		report = r.ResumeStatus()
	case "disable":
		report, err = r.ConfigureResume(ctx, "", "", false)
	case "enable":
		if len(os.Args) != 5 {
			os.Exit(2)
		}
		report, err = r.ConfigureResume(ctx, os.Args[3], os.Args[4], true)
	case "check":
		report, err = r.CheckResume(ctx)
	default:
		os.Exit(2)
	}
	if err != nil {
		report = r.ResumeStatus()
		report.Message = err.Error()
		report.Status = "error"
	}
	if json.NewEncoder(os.Stdout).Encode(report) != nil {
		os.Exit(1)
	}
}

func warmupCommand() {
	r, err := assistant.NewReader()
	if err != nil {
		fmt.Fprintln(os.Stderr, "用户目录不可用")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 210*time.Second)
	defer cancel()
	var report assistant.WarmupReport
	switch os.Args[2] {
	case "status":
		report = r.WarmupStatus()
	case "enable":
		report, err = r.ConfigureWarmup(ctx, true)
	case "disable":
		report, err = r.ConfigureWarmup(ctx, false)
	case "check":
		report, err = r.CheckWarmup(ctx, "")
	case "refresh":
		report, err = r.RefreshWarmup(ctx)
	case "schedule":
		if len(os.Args) != 6 || (os.Args[3] != "on" && os.Args[3] != "off") {
			fmt.Fprintln(os.Stderr, "Usage: codex-helper warmup schedule on|off HH:MM HH:MM")
			os.Exit(2)
		}
		report, err = r.ConfigureWarmupSchedule(os.Args[3] == "on", os.Args[4], os.Args[5])
	case "probe":
		if len(os.Args) != 4 {
			fmt.Fprintln(os.Stderr, "warmup probe requires a managed account identifier")
			os.Exit(2)
		}
		report, err = r.CheckWarmup(ctx, os.Args[3])
	default:
		fmt.Fprintln(os.Stderr, "Usage: codex-helper warmup status|enable|disable|check|refresh|probe <account>")
		os.Exit(2)
	}
	if err != nil {
		report = r.WarmupStatus()
		report.Message = err.Error()
		report.Error = err.Error()
	}
	if report.Accounts == nil {
		report.Accounts = []assistant.WarmupAccount{}
	}
	if json.NewEncoder(os.Stdout).Encode(report) != nil {
		os.Exit(1)
	}
}
