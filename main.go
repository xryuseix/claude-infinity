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
	regexp.MustCompile(`(?i)try\s+again\s+(later|in)`),
	regexp.MustCompile(`(?i)throttl`),
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
		return runResult{}, fmt.Errorf("claude の起動に失敗: %w", err)
	}
	defer func() { _ = ptmx.Close() }()

	// ウィンドウサイズ変更の転送
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
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
				fmt.Fprintf(os.Stderr, "\r\033[K[claude-infinity] 再開まで %d時間%02d分%02d秒 ...", h, m, s)
			} else {
				fmt.Fprintf(os.Stderr, "\r\033[K[claude-infinity] 再開まで %02d:%02d ...", m, s)
			}
		}
	}
}

// sandboxSettings は sandbox モード有効時に claude に渡すデフォルト設定
const sandboxSettings = `{"sandbox":{"enabled":true,"autoAllowBashIfSandboxed":true}}`

// runLoop はリトライループを実行し、終了コードを返す
func runLoop(ctx context.Context, r runner, w waiter, args []string, maxRetries int, fallbackWait time.Duration, noSandbox bool) int {
	fmt.Fprintf(os.Stderr, "[claude-infinity] Claude Code を起動します...\n")

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
			fmt.Fprintf(os.Stderr, "[claude-infinity] セッションを再開します (リトライ %d/%d)\n", i+1, maxRetries)
		} else {
			claudeArgs = append(defaultArgs, args...)
		}

		result, err := r.RunClaude(claudeArgs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\n[claude-infinity] エラー: %v\n", err)
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
			fmt.Fprintf(os.Stderr, "\n[claude-infinity] Usage Limit を検出しました。%s に再開します...\n",
				resetTime.Format("15:04 (MST)"))
		} else {
			// リセット時刻が取得できなかった場合はフォールバック
			resumeAt = time.Now().Add(fallbackWait)
			waitMin := int(fallbackWait.Minutes())
			fmt.Fprintf(os.Stderr, "\n[claude-infinity] Usage Limit を検出しました。%d 分後に再開します（リセット時刻を取得できませんでした）...\n", waitMin)
		}

		if !w.WaitUntil(ctx, resumeAt) {
			return 130
		}

		isResume = true
	}

	fmt.Fprintf(os.Stderr, "[claude-infinity] 最大リトライ回数(%d)に達しました。\n", maxRetries)
	return 1
}

func main() {
	waitMin := flag.Int("wait", 5, "リセット時刻を取得できなかった場合のフォールバック待機時間（分）")
	maxRetries := flag.Int("max-retries", 50, "最大リトライ回数")
	noSandbox := flag.Bool("no-sandbox", false, "サンドボックスモードを無効化する（デフォルトは有効）")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "claude-infinity: Usage Limit 時に自動で待機・再開する Claude Code ラッパー\n\n")
		fmt.Fprintf(os.Stderr, "リセット時刻が出力に含まれている場合、その時刻に合わせて自動再開します。\n")
		fmt.Fprintf(os.Stderr, "取得できない場合は --wait で指定した時間後に再開します。\n\n")
		fmt.Fprintf(os.Stderr, "使い方:\n")
		fmt.Fprintf(os.Stderr, "  claude-infinity [オプション] [-- claude の引数...]\n\n")
		fmt.Fprintf(os.Stderr, "例:\n")
		fmt.Fprintf(os.Stderr, "  claude-infinity                        # サンドボックスモードで起動（デフォルト）\n")
		fmt.Fprintf(os.Stderr, "  claude-infinity --no-sandbox           # サンドボックスなしで起動\n")
		fmt.Fprintf(os.Stderr, "  claude-infinity -- -p \"Hello\"           # プロンプトを指定\n")
		fmt.Fprintf(os.Stderr, "  claude-infinity --wait 10               # フォールバック待機を10分に\n")
		fmt.Fprintf(os.Stderr, "  claude-infinity --max-retries 100       # 最大100回リトライ\n\n")
		fmt.Fprintf(os.Stderr, "オプション:\n")
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
		fmt.Fprintf(os.Stderr, "\n[claude-infinity] 中断しました。\n")
		os.Exit(130)
	}()

	r := &ptyRunner{}
	w := &realWaiter{}
	exitCode := runLoop(ctx, r, w, args, *maxRetries, waitDuration, *noSandbox)
	os.Exit(exitCode)
}
