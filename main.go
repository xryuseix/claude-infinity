package main

//go:generate go run go.uber.org/mock/mockgen -source=main.go -destination=mock_test.go -package=main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// runner は Claude CLI の実行を抽象化する
type runner interface {
	RunClaude(args []string) (runResult, error)
}

// waiter は指定時刻までの待機を抽象化する
type waiter interface {
	WaitUntil(ctx context.Context, target time.Time) bool
}

// ptyRunner は runner の実装で、PTY 経由で claude を実行する
type ptyRunner struct{}

func (p *ptyRunner) RunClaude(args []string) (runResult, error) {
	return runClaude(args)
}

// realWaiter は waiter の実装で、実際に時刻まで待機する
type realWaiter struct{}

func (rw *realWaiter) WaitUntil(ctx context.Context, target time.Time) bool {
	return waitUntil(ctx, target)
}

// savedTerm はシグナルハンドラからターミナル状態を復元するためのグローバル変数
var (
	savedTermState *term.State
	savedTermFd    int
	termMu         sync.Mutex
)

func restoreTerm() {
	termMu.Lock()
	defer termMu.Unlock()
	if savedTermState != nil {
		_ = term.Restore(savedTermFd, savedTermState)
		savedTermState = nil
	}
}

// ringBuffer は固定サイズの循環バッファ。出力の末尾 N バイトを保持する。
type ringBuffer struct {
	mu   sync.Mutex
	data []byte
	pos  int
	full bool
}

func newRingBuffer(size int) *ringBuffer {
	return &ringBuffer{data: make([]byte, size)}
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range p {
		r.data[r.pos] = b
		r.pos = (r.pos + 1) % len(r.data)
		if r.pos == 0 {
			r.full = true
		}
	}
	return len(p), nil
}

func (r *ringBuffer) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		out := make([]byte, r.pos)
		copy(out, r.data[:r.pos])
		return out
	}
	size := len(r.data)
	out := make([]byte, size)
	n := copy(out, r.data[r.pos:])
	copy(out[n:], r.data[:r.pos])
	return out
}

// Usage Limit / Rate Limit を示す出力パターン
var limitPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)usage[\s_-]*limit`),
	regexp.MustCompile(`(?i)rate[\s_-]*limit`),
	regexp.MustCompile(`(?i)too\s+many\s+requests`),
	regexp.MustCompile(`(?i)quota[\s_-]*exceeded`),
	regexp.MustCompile(`(?i)you.ve\s+(hit|reached)`),
	regexp.MustCompile(`(?i)limit\s+reached`),
	regexp.MustCompile(`(?i)requests?\s+throttled`),
	regexp.MustCompile(`(?i)resource[\s_-]*exhausted`),
}

// resetTimePattern はリセット時刻を抽出する
// 対応形式:
//   - "Your limit will reset at 7pm (Asia/Tokyo)"
//   - "You've hit your limit · resets 3am (Asia/Tokyo)"  ※改行をまたぐ場合も考慮
var resetTimePattern = regexp.MustCompile(
	`(?i)(?:limit\s+will\s+reset\s+at|resets)\s+(\d{1,2}(?::\d{2})?\s*(?:am|pm))[\s\S]*?\(([^)]+)\)`,
)

// timePartPattern は "7pm", "7:30pm", "12:00am" のような時刻文字列をパースする
var timePartPattern = regexp.MustCompile(`(?i)(\d{1,2})(?::(\d{2}))?\s*(am|pm)`)

func isRateLimited(data []byte) bool {
	for _, p := range limitPatterns {
		if p.Match(data) {
			return true
		}
	}
	return false
}

// parseResetTimeAt は出力からリセット時刻を抽出し、now を基準に time.Time に変換する
// 例: "Your limit will reset at 7pm (Asia/Tokyo)" → 当日 19:00 JST
// リセット時刻が now より過去の場合は翌日として扱う
func parseResetTimeAt(data []byte, now time.Time) (time.Time, bool) {
	matches := resetTimePattern.FindSubmatch(data)
	if matches == nil {
		return time.Time{}, false
	}

	timeStr := string(matches[1])
	tzStr := strings.TrimSpace(string(matches[2]))

	loc, err := time.LoadLocation(tzStr)
	if err != nil {
		return time.Time{}, false
	}

	tMatches := timePartPattern.FindStringSubmatch(timeStr)
	if tMatches == nil {
		return time.Time{}, false
	}

	h, _ := strconv.Atoi(tMatches[1])
	m := 0
	if tMatches[2] != "" {
		m, _ = strconv.Atoi(tMatches[2])
	}
	ampm := strings.ToLower(tMatches[3])

	if ampm == "pm" && h != 12 {
		h += 12
	} else if ampm == "am" && h == 12 {
		h = 0
	}

	nowInLoc := now.In(loc)
	target := time.Date(nowInLoc.Year(), nowInLoc.Month(), nowInLoc.Day(), h, m, 0, 0, loc)

	// リセット時刻が過去なら翌日
	if target.Before(nowInLoc) {
		target = target.Add(24 * time.Hour)
	}

	return target, true
}

// parseResetTime は parseResetTimeAt のラッパーで、現在時刻を基準にする
func parseResetTime(data []byte) (time.Time, bool) {
	return parseResetTimeAt(data, time.Now())
}

type runResult struct {
	rateLimited bool
	exitCode    int
	outputData  []byte // リセット時刻パース用の出力データ
}

// runClaude は claude CLI を PTY 経由で起動し、出力を監視する
func runClaude(args []string) (runResult, error) {
	cmd := exec.Command("claude", args...)

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return runResult{}, fmt.Errorf("failed to start claude: %w", err)
	}
	defer func() { _ = ptmx.Close() }()

	// ウィンドウサイズ変更の転送
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer func() {
		signal.Stop(winch)
		close(winch) // goroutine を終了させる
	}()
	go func() {
		for range winch {
			_ = pty.InheritSize(os.Stdin, ptmx)
		}
	}()
	_ = pty.InheritSize(os.Stdin, ptmx)

	// ターミナルを raw モードに設定
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		old, err := term.MakeRaw(fd)
		if err == nil {
			termMu.Lock()
			savedTermState = old
			savedTermFd = fd
			termMu.Unlock()
			defer func() {
				_ = term.Restore(fd, old)
				termMu.Lock()
				savedTermState = nil
				termMu.Unlock()
			}()
		}
	}

	// 出力を監視するための ringBuffer（末尾 16KB を保持）
	ring := newRingBuffer(16384)
	mw := io.MultiWriter(os.Stdout, ring)

	// stdin → PTY
	go func() {
		_, _ = io.Copy(ptmx, os.Stdin)
	}()

	// PTY → stdout + ringBuffer
	copyDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(mw, ptmx)
		close(copyDone)
	}()

	waitErr := cmd.Wait()

	// 残りの出力がフラッシュされるのを待つ
	select {
	case <-copyDone:
	case <-time.After(500 * time.Millisecond):
	}

	result := runResult{exitCode: 0}
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			result.exitCode = exitErr.ExitCode()
		}
	}

	ringData := ring.Bytes()
	result.rateLimited = isRateLimited(ringData)
	result.outputData = ringData
	return result, nil
}

// waitUntil は指定時刻までカウントダウンを表示しながら待機する
func waitUntil(ctx context.Context, target time.Time) bool {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case now := <-ticker.C:
			rem := target.Sub(now)
			if rem <= 0 {
				fmt.Fprintf(os.Stderr, "\r\033[K")
				return true
			}
			h := int(rem.Hours())
			m := int(rem.Minutes()) % 60
			s := int(rem.Seconds()) % 60
			if h > 0 {
				fmt.Fprintf(os.Stderr, "\r\033[K[claude-infinity] Resuming in %dh %02dm %02ds ...", h, m, s)
			} else {
				fmt.Fprintf(os.Stderr, "\r\033[K[claude-infinity] Resuming in %02d:%02d ...", m, s)
			}
		}
	}
}

// sandboxSettings は sandbox モード有効時に claude に渡すデフォルト設定
const sandboxSettings = `{"sandbox":{"enabled":true,"autoAllowBashIfSandboxed":true}}`

// runLoop はリトライループを実行し、終了コードを返す
func runLoop(ctx context.Context, r runner, w waiter, args []string, maxRetries int, fallbackWait time.Duration, noSandbox bool) int {
	fmt.Fprintf(os.Stderr, "[claude-infinity] Starting Claude Code...\n")

	// デフォルトでサンドボックスモードを有効化する引数
	defaultArgs := []string{}
	if !noSandbox {
		defaultArgs = []string{"--settings", sandboxSettings}
	}

	isResume := false
	for i := 0; i < maxRetries; i++ {
		var claudeArgs []string
		if isResume {
			claudeArgs = append(defaultArgs, "--resume")
			fmt.Fprintf(os.Stderr, "[claude-infinity] Resuming session (retry %d/%d)...\n", i+1, maxRetries)
		} else {
			claudeArgs = append(defaultArgs, args...)
		}

		result, err := r.RunClaude(claudeArgs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\n[claude-infinity] Error: %v\n", err)
			return 1
		}

		if !result.rateLimited {
			return result.exitCode
		}

		// リセット時刻のパースを試みる
		var resumeAt time.Time
		if resetTime, ok := parseResetTime(result.outputData); ok {
			// リセット時刻 + 1分のバッファ
			resumeAt = resetTime.Add(1 * time.Minute)
			fmt.Fprintf(os.Stderr, "\n[claude-infinity] Usage limit detected. Resuming at %s...\n",
				resetTime.Format("15:04 (MST)"))
		} else {
			// リセット時刻が取得できなかった場合はフォールバック
			resumeAt = time.Now().Add(fallbackWait)
			waitMin := int(fallbackWait.Minutes())
			fmt.Fprintf(os.Stderr, "\n[claude-infinity] Usage limit detected. Resuming in %d min (reset time unavailable)...\n", waitMin)
		}

		if !w.WaitUntil(ctx, resumeAt) {
			return 130
		}

		isResume = true
	}

	fmt.Fprintf(os.Stderr, "[claude-infinity] Max retries (%d) reached.\n", maxRetries)
	return 1
}

func main() {
	waitMin := flag.Int("wait", 60, "fallback wait time in minutes when reset time is unavailable")
	maxRetries := flag.Int("max-retries", 50, "maximum number of retries")
	noSandbox := flag.Bool("no-sandbox", false, "disable sandbox mode (enabled by default)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "claude-infinity: Claude Code wrapper that auto-waits and resumes on usage limits\n\n")
		fmt.Fprintf(os.Stderr, "When a reset time is found in the output, resumes at that time.\n")
		fmt.Fprintf(os.Stderr, "Otherwise waits for the duration specified by --wait.\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  claude-infinity [options] [-- claude args...]\n\n")
		fmt.Fprintf(os.Stderr, "Examples:\n")
		fmt.Fprintf(os.Stderr, "  claude-infinity                        # start in sandbox mode (default)\n")
		fmt.Fprintf(os.Stderr, "  claude-infinity --no-sandbox           # start without sandbox\n")
		fmt.Fprintf(os.Stderr, "  claude-infinity -- -p \"Hello\"           # pass prompt to claude\n")
		fmt.Fprintf(os.Stderr, "  claude-infinity --wait 10               # set fallback wait to 10 min\n")
		fmt.Fprintf(os.Stderr, "  claude-infinity --max-retries 100       # retry up to 100 times\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	args := flag.Args()
	waitDuration := time.Duration(*waitMin) * time.Minute

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// シグナルハンドラ（待機中の Ctrl+C で終了）
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		restoreTerm()
		cancel()
		fmt.Fprintf(os.Stderr, "\n[claude-infinity] Interrupted.\n")
		os.Exit(130)
	}()

	r := &ptyRunner{}
	w := &realWaiter{}
	exitCode := runLoop(ctx, r, w, args, *maxRetries, waitDuration, *noSandbox)
	os.Exit(exitCode)
}
