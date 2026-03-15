package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/mock/gomock"
)

// ============================================================
// ringBuffer テスト
// ============================================================

func TestRingBuffer_Basic(t *testing.T) {
	rb := newRingBuffer(10)
	if _, err := rb.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	got := string(rb.Bytes())
	if got != "hello" {
		t.Errorf("want %q, got %q", "hello", got)
	}
}

func TestRingBuffer_Overflow(t *testing.T) {
	rb := newRingBuffer(5)
	if _, err := rb.Write([]byte("abcdefgh")); err != nil { // 8 bytes into 5-cap buffer
		t.Fatal(err)
	}
	got := string(rb.Bytes())
	if got != "defgh" {
		t.Errorf("want %q, got %q", "defgh", got)
	}
}

func TestRingBuffer_ExactCapacity(t *testing.T) {
	rb := newRingBuffer(5)
	if _, err := rb.Write([]byte("abcde")); err != nil {
		t.Fatal(err)
	}
	got := string(rb.Bytes())
	if got != "abcde" {
		t.Errorf("want %q, got %q", "abcde", got)
	}
}

func TestRingBuffer_Empty(t *testing.T) {
	rb := newRingBuffer(10)
	got := rb.Bytes()
	if len(got) != 0 {
		t.Errorf("want empty, got %q", got)
	}
}

// ============================================================
// ringBuffer.Tail テスト
// ============================================================

func TestRingBuffer_Tail_Basic(t *testing.T) {
	rb := newRingBuffer(10)
	if _, err := rb.Write([]byte("hello world")); err != nil {
		t.Fatal(err)
	}
	// バッファには "ello world" (10バイト) が入っている
	got := string(rb.Tail(5))
	if got != "world" {
		t.Errorf("Tail(5) = %q, want %q", got, "world")
	}
}

func TestRingBuffer_Tail_LargerThanContent(t *testing.T) {
	rb := newRingBuffer(10)
	if _, err := rb.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	got := string(rb.Tail(100))
	if got != "hi" {
		t.Errorf("Tail(100) = %q, want %q", got, "hi")
	}
}

func TestRingBuffer_Tail_ExactSize(t *testing.T) {
	rb := newRingBuffer(10)
	if _, err := rb.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	got := string(rb.Tail(10))
	if got != "0123456789" {
		t.Errorf("Tail(10) = %q, want %q", got, "0123456789")
	}
}

func TestRingBuffer_Tail_Overflow(t *testing.T) {
	rb := newRingBuffer(5)
	if _, err := rb.Write([]byte("abcdefgh")); err != nil { // バッファに "defgh" が残る
		t.Fatal(err)
	}
	got := string(rb.Tail(3))
	if got != "fgh" {
		t.Errorf("Tail(3) = %q, want %q", got, "fgh")
	}
}

func TestRingBuffer_Tail_Empty(t *testing.T) {
	rb := newRingBuffer(10)
	got := rb.Tail(5)
	if len(got) != 0 {
		t.Errorf("Tail(5) on empty buffer = %q, want empty", got)
	}
}

// ============================================================
// isRateLimited テスト
// ============================================================

func TestIsRateLimited(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"usage limit", "You hit your usage limit", true},
		{"rate limit", "rate limit exceeded", true},
		{"too many requests", "too many requests, please wait", true},
		{"quota exceeded", "quota exceeded for today", true},
		{"you've hit", "you've hit the limit", true},
		{"you've reached", "You've reached the limit", true},
		{"limit reached", "limit reached", true},
		{"request throttled", "request throttled", true},
		{"requests throttled", "requests throttled by server", true},
		{"resource exhausted", "resource exhausted", true},
		{"usage_limit underscore", "usage_limit", true},
		{"rate-limit hyphen", "rate-limit", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRateLimited([]byte(tt.input))
			if got != tt.want {
				t.Errorf("isRateLimited(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// TestIsRateLimited_FalsePositive_FileContent は、ファイル内容に rate limit キーワードが
// 含まれていても、Tail(1024) で検出されないことを確認する。
func TestIsRateLimited_FalsePositive_FileContent(t *testing.T) {
	// CLAUDE.md のような内容を含むファイル表示をシミュレート
	fileContent := []byte("Usage Limit 時に自動で待機・再開する Claude Code ラッパーツール。\n" +
		"rate limit を検出して自動リトライ。\n" +
		"you've hit your limit の場合にも対応。\n")

	// ファイル内容の後に 1KB 以上の通常出力が続く
	normalOutput := make([]byte, 1200)
	for i := range normalOutput {
		normalOutput[i] = 'x'
	}

	rb := newRingBuffer(16384)
	if _, err := rb.Write(fileContent); err != nil {
		t.Fatal(err)
	}
	if _, err := rb.Write(normalOutput); err != nil {
		t.Fatal(err)
	}

	// Tail(1024) では通常出力のみが含まれるため、誤検出しない
	if isRateLimited(rb.Tail(tailCheckSize)) {
		t.Error("isRateLimited should return false when file content with keywords is outside Tail window")
	}

	// Bytes() 全体だと検出してしまう（修正前の動作）
	if !isRateLimited(rb.Bytes()) {
		t.Error("isRateLimited(Bytes()) should return true (demonstrates the old bug)")
	}
}

// TestIsRateLimited_TruePositive_RecentRateLimit は、直近出力に rate limit メッセージが
// ある場合に Tail(1024) で検出されることを確認する。
func TestIsRateLimited_TruePositive_RecentRateLimit(t *testing.T) {
	rb := newRingBuffer(16384)
	// 先に通常出力
	if _, err := rb.Write([]byte("normal output from claude...\n")); err != nil {
		t.Fatal(err)
	}
	// 直近に rate limit メッセージ
	if _, err := rb.Write([]byte("You\u2019ve hit your usage limit\n")); err != nil {
		t.Fatal(err)
	}

	if !isRateLimited(rb.Tail(tailCheckSize)) {
		t.Error("isRateLimited should return true for recent rate limit message in Tail window")
	}
}

// TestIsRateLimited_UnescapedDotFixed は、パターン5の正規表現修正を確認する。
// 修正前: `you.ve` のドットがワイルドカードとして "youXve" 等にもマッチしていた。
func TestIsRateLimited_UnescapedDotFixed(t *testing.T) {
	// "youXve hit" は誤マッチしないこと
	if isRateLimited([]byte("youXve hit the limit")) {
		t.Error("isRateLimited should not match 'youXve' (dot was a wildcard bug)")
	}

	// 正当なアポストロフィはマッチすること
	cases := []string{
		"you've hit the limit",      // ASCII アポストロフィ
		"you\u2019ve hit the limit", // Unicode 右引用符 '
		"you\u2018ve hit the limit", // Unicode 左引用符 '
	}
	for _, c := range cases {
		if !isRateLimited([]byte(c)) {
			t.Errorf("isRateLimited(%q) should return true", c)
		}
	}
}

func TestIsRateLimited_NoMatch(t *testing.T) {
	inputs := []string{
		"normal output",
		"everything is fine",
		"claude is running",
		"please try again later",     // 汎用的な「後で再試行」はマッチしない
		"try again in 5 minutes",     // 同上
		"CPU throttling detected",    // CPU 制限はマッチしない
		"network throttling enabled", // ネットワーク制限はマッチしない
		"",
	}
	for _, input := range inputs {
		if isRateLimited([]byte(input)) {
			t.Errorf("isRateLimited(%q) = true, want false", input)
		}
	}
}

// TestIsRateLimited_CamelCase_NoMatch はキャメルケースの識別子が誤検出されないことを確認する。
// rateLimitTier を含む Python コード表示で誤検出した問題の再発防止テスト。
func TestIsRateLimited_CamelCase_NoMatch(t *testing.T) {
	inputs := []string{
		// キャメルケースの変数名・JSON キー
		`rateLimitTier`,
		`isRateLimited`,
		`rateLimited`,
		`usageLimitExceeded`,
		`quotaExceededError`,
		`resourceExhaustedErr`,
		`requestThrottledCount`,
		// 関数呼び出し
		`getRateLimit()`,
		`checkUsageLimit()`,
		// JSON 出力
		`{"rateLimitTier": "free"}`,
		`"quotaExceededAt": "2025-01-01"`,
		// 実際に誤検出を引き起こしたコマンド表示
		`oauth.get("rateLimitTier","?")`,
		`print(f'Tier: {oauth.get("rateLimitTier","?")}')`,
		// スネークケースで別の識別子に埋め込まれたケース
		`check_rate_limit_status`,
		`is_usage_limit_enabled`,
	}
	for _, input := range inputs {
		if isRateLimited([]byte(input)) {
			t.Errorf("isRateLimited(%q) = true, want false (camelCase/identifier false positive)", input)
		}
	}
}

// TestIsRateLimited_RealMessages は Claude Code が実際に出力するメッセージで検出できることを確認する。
func TestIsRateLimited_RealMessages(t *testing.T) {
	inputs := []string{
		"Claude usage limit reached. Your limit will reset at 7pm (Asia/Tokyo).",
		"You've hit your rate limit. Please wait before trying again.",
		"Too many requests. Please slow down.",
		"Rate limit exceeded for this API key.",
		"You've reached your usage limit for the day.",
		"Your quota exceeded. Upgrade your plan for more.",
		"Request throttled due to high traffic.",
		"Resource exhausted: out of tokens.",
	}
	for _, input := range inputs {
		if !isRateLimited([]byte(input)) {
			t.Errorf("isRateLimited(%q) = false, want true (real message not detected)", input)
		}
	}
}

// ============================================================
// parseResetTimeAt テスト
// ============================================================

func TestParseResetTimeAt_ValidFormats(t *testing.T) {
	now := time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		input    string
		wantHour int
		wantMin  int
	}{
		{"7pm", "Your limit will reset at 7pm (UTC)", 19, 0},
		{"7:30pm", "Your limit will reset at 7:30pm (UTC)", 19, 30},
		{"12am", "Your limit will reset at 12am (UTC)", 0, 0},
		{"12:00pm", "Your limit will reset at 12:00pm (UTC)", 12, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseResetTimeAt([]byte(tt.input), now)
			if !ok {
				t.Fatalf("parseResetTimeAt returned false")
			}
			if got.Hour() != tt.wantHour || got.Minute() != tt.wantMin {
				t.Errorf("got %02d:%02d, want %02d:%02d", got.Hour(), got.Minute(), tt.wantHour, tt.wantMin)
			}
		})
	}
}

func TestParseResetTimeAt_Timezones(t *testing.T) {
	now := time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		tz   string
	}{
		{"Asia/Tokyo", "Asia/Tokyo"},
		{"US/Eastern", "US/Eastern"},
		{"Europe/London", "Europe/London"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := "Your limit will reset at 7pm (" + tt.tz + ")"
			got, ok := parseResetTimeAt([]byte(input), now)
			if !ok {
				t.Fatalf("parseResetTimeAt returned false for tz=%s", tt.tz)
			}
			loc, _ := time.LoadLocation(tt.tz)
			if got.Location().String() != loc.String() {
				t.Errorf("location = %s, want %s", got.Location(), loc)
			}
		})
	}
}

func TestParseResetTimeAt_PastTime(t *testing.T) {
	// now が 20:00 UTC で、リセット時刻が 7pm (19:00) UTC → 翌日になるべき
	now := time.Date(2025, 1, 15, 20, 0, 0, 0, time.UTC)
	input := "Your limit will reset at 7pm (UTC)"
	got, ok := parseResetTimeAt([]byte(input), now)
	if !ok {
		t.Fatal("parseResetTimeAt returned false")
	}
	wantDay := 16
	if got.Day() != wantDay {
		t.Errorf("got day %d, want %d (should be next day)", got.Day(), wantDay)
	}
}

func TestParseResetTimeAt_InvalidTz(t *testing.T) {
	now := time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)
	input := "Your limit will reset at 7pm (Invalid/Zone)"
	_, ok := parseResetTimeAt([]byte(input), now)
	if ok {
		t.Error("parseResetTimeAt should return false for invalid timezone")
	}
}

func TestParseResetTimeAt_NoMatch(t *testing.T) {
	now := time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)
	inputs := []string{
		"normal output with no reset time",
		"limit will reset sometime",
		"",
	}
	for _, input := range inputs {
		_, ok := parseResetTimeAt([]byte(input), now)
		if ok {
			t.Errorf("parseResetTimeAt(%q) should return false", input)
		}
	}
}

// ============================================================
// runLoop モックベーステスト
// ============================================================

func TestRunLoop_NormalExit(t *testing.T) {
	ctrl := gomock.NewController(t)

	mr := NewMockrunner(ctrl)
	mw := NewMockwaiter(ctrl)

	mr.EXPECT().RunClaude([]string{"--permission-mode", "auto", "-p", "hello"}).Return(runResult{
		rateLimited: false,
		exitCode:    0,
	}, nil)

	ctx := context.Background()
	code := runLoop(ctx, mr, mw, []string{"-p", "hello"}, 5, 5*time.Minute, true)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

func TestRunLoop_RateLimitThenSuccess(t *testing.T) {
	ctrl := gomock.NewController(t)

	mr := NewMockrunner(ctrl)
	mw := NewMockwaiter(ctrl)

	// 1回目: rate limited（リセット時刻なし）
	first := mr.EXPECT().RunClaude([]string{"--permission-mode", "auto", "-p", "hello"}).Return(runResult{
		rateLimited: true,
		exitCode:    0,
		outputData:  []byte("usage limit hit"),
	}, nil)

	// WaitUntil が呼ばれる（成功で返す）
	mw.EXPECT().WaitUntil(gomock.Any(), gomock.Any()).Return(true)

	// 2回目: --resume で再開、成功
	mr.EXPECT().RunClaude([]string{"--permission-mode", "auto", "--resume"}).Return(runResult{
		rateLimited: false,
		exitCode:    0,
	}, nil).After(first)

	ctx := context.Background()
	code := runLoop(ctx, mr, mw, []string{"-p", "hello"}, 5, 5*time.Minute, true)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

func TestRunLoop_WithResetTime(t *testing.T) {
	ctrl := gomock.NewController(t)

	mr := NewMockrunner(ctrl)
	mw := NewMockwaiter(ctrl)

	// rate limited + リセット時刻あり
	first := mr.EXPECT().RunClaude([]string{"--permission-mode", "auto"}).Return(runResult{
		rateLimited: true,
		exitCode:    0,
		outputData:  []byte("Your limit will reset at 7pm (UTC)"),
	}, nil)

	// WaitUntil が呼ばれる
	mw.EXPECT().WaitUntil(gomock.Any(), gomock.Any()).Return(true)

	// 2回目: 成功
	mr.EXPECT().RunClaude([]string{"--permission-mode", "auto", "--resume"}).Return(runResult{
		rateLimited: false,
		exitCode:    0,
	}, nil).After(first)

	ctx := context.Background()
	code := runLoop(ctx, mr, mw, []string{}, 5, 5*time.Minute, true)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

func TestRunLoop_MaxRetriesExceeded(t *testing.T) {
	ctrl := gomock.NewController(t)

	mr := NewMockrunner(ctrl)
	mw := NewMockwaiter(ctrl)

	maxRetries := 3

	// 1回目: 初期引数で rate limited
	mr.EXPECT().RunClaude([]string{"--permission-mode", "auto"}).Return(runResult{
		rateLimited: true,
		exitCode:    0,
		outputData:  []byte("rate limit"),
	}, nil)

	// 2回目・3回目: --resume で rate limited
	mr.EXPECT().RunClaude([]string{"--permission-mode", "auto", "--resume"}).Return(runResult{
		rateLimited: true,
		exitCode:    0,
		outputData:  []byte("rate limit"),
	}, nil).Times(2)

	// WaitUntil は毎回成功
	mw.EXPECT().WaitUntil(gomock.Any(), gomock.Any()).Return(true).Times(3)

	ctx := context.Background()
	code := runLoop(ctx, mr, mw, []string{}, maxRetries, 5*time.Minute, true)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

func TestRunLoop_RunError(t *testing.T) {
	ctrl := gomock.NewController(t)

	mr := NewMockrunner(ctrl)
	mw := NewMockwaiter(ctrl)

	mr.EXPECT().RunClaude([]string{"--permission-mode", "auto"}).Return(runResult{}, errors.New("failed to start"))

	ctx := context.Background()
	code := runLoop(ctx, mr, mw, []string{}, 5, 5*time.Minute, true)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

// TestRunLoop_RateLimitWithSessionID は rate limit 時にセッション ID が含まれている場合に
// 次の再開で --resume <UUID> が使われることを確認する。
func TestRunLoop_RateLimitWithSessionID(t *testing.T) {
	ctrl := gomock.NewController(t)

	mr := NewMockrunner(ctrl)
	mw := NewMockwaiter(ctrl)

	sessionID := "4fd16842-bfcb-41b1-bc20-6509b3eb0bdb"

	// 1回目: rate limited + セッション ID あり
	first := mr.EXPECT().RunClaude([]string{"--permission-mode", "auto", "-p", "hello"}).Return(runResult{
		rateLimited: true,
		exitCode:    0,
		outputData:  []byte("usage limit hit\nclaude --resume " + sessionID),
		sessionID:   sessionID,
	}, nil)

	// WaitUntil が呼ばれる
	mw.EXPECT().WaitUntil(gomock.Any(), gomock.Any()).Return(true)

	// 2回目: --resume <UUID> で再開、成功
	mr.EXPECT().RunClaude([]string{"--permission-mode", "auto", "--resume", sessionID}).Return(runResult{
		rateLimited: false,
		exitCode:    0,
	}, nil).After(first)

	ctx := context.Background()
	code := runLoop(ctx, mr, mw, []string{"-p", "hello"}, 5, 5*time.Minute, true)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

// TestRunLoop_SessionIDUpdatedOnRetry は連続する rate limit でセッション ID が
// 更新された場合に最新の ID が使われることを確認する。
func TestRunLoop_SessionIDUpdatedOnRetry(t *testing.T) {
	ctrl := gomock.NewController(t)

	mr := NewMockrunner(ctrl)
	mw := NewMockwaiter(ctrl)

	id1 := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	id2 := "11111111-2222-3333-4444-555555555555"

	// 1回目: rate limited + セッション ID1
	first := mr.EXPECT().RunClaude([]string{"--permission-mode", "auto"}).Return(runResult{
		rateLimited: true,
		exitCode:    0,
		outputData:  []byte("rate limit\nclaude --resume " + id1),
		sessionID:   id1,
	}, nil)

	mw.EXPECT().WaitUntil(gomock.Any(), gomock.Any()).Return(true)

	// 2回目: --resume id1 で再開、再度 rate limited + セッション ID2
	second := mr.EXPECT().RunClaude([]string{"--permission-mode", "auto", "--resume", id1}).Return(runResult{
		rateLimited: true,
		exitCode:    0,
		outputData:  []byte("rate limit\nclaude --resume " + id2),
		sessionID:   id2,
	}, nil).After(first)

	mw.EXPECT().WaitUntil(gomock.Any(), gomock.Any()).Return(true)

	// 3回目: --resume id2 で再開、成功
	mr.EXPECT().RunClaude([]string{"--permission-mode", "auto", "--resume", id2}).Return(runResult{
		rateLimited: false,
		exitCode:    0,
	}, nil).After(second)

	ctx := context.Background()
	code := runLoop(ctx, mr, mw, []string{}, 5, 5*time.Minute, true)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

// ============================================================
// sessionIDPattern テスト
// ============================================================

func TestSessionIDPattern(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			"standard UUID",
			"To resume this conversation, run: claude --resume 4fd16842-bfcb-41b1-bc20-6509b3eb0bdb",
			"4fd16842-bfcb-41b1-bc20-6509b3eb0bdb",
		},
		{
			"UUID in multiline output",
			"Some output\nclaude --resume aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee\nMore output",
			"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := sessionIDPattern.FindSubmatch([]byte(tt.input))
			if m == nil {
				t.Fatalf("sessionIDPattern did not match %q", tt.input)
			}
			got := string(m[1])
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSessionIDPattern_NoMatch(t *testing.T) {
	inputs := []string{
		"normal output",
		"claude --resume",          // UUID なし
		"claude --resume not-uuid", // UUID 形式でない
		"",
	}
	for _, input := range inputs {
		m := sessionIDPattern.FindSubmatch([]byte(input))
		if m != nil {
			t.Errorf("sessionIDPattern should not match %q, got %q", input, string(m[1]))
		}
	}
}

func TestRunLoop_ContextCancelled(t *testing.T) {
	ctrl := gomock.NewController(t)

	mr := NewMockrunner(ctrl)
	mw := NewMockwaiter(ctrl)

	// rate limited
	mr.EXPECT().RunClaude([]string{"--permission-mode", "auto"}).Return(runResult{
		rateLimited: true,
		exitCode:    0,
		outputData:  []byte("rate limit"),
	}, nil)

	// WaitUntil がキャンセルされる
	mw.EXPECT().WaitUntil(gomock.Any(), gomock.Any()).Return(false)

	ctx := context.Background()
	code := runLoop(ctx, mr, mw, []string{}, 5, 5*time.Minute, true)
	if code != 130 {
		t.Errorf("exit code = %d, want 130", code)
	}
}
