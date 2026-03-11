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
		{"limit reached", "limit reached, try again later", true},
		{"try again later", "please try again later", true},
		{"try again in", "try again in 5 minutes", true},
		{"throttled", "request throttled", true},
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

func TestIsRateLimited_NoMatch(t *testing.T) {
	inputs := []string{
		"normal output",
		"everything is fine",
		"claude is running",
		"",
	}
	for _, input := range inputs {
		if isRateLimited([]byte(input)) {
			t.Errorf("isRateLimited(%q) = true, want false", input)
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

	mr.EXPECT().RunClaude([]string{"-p", "hello"}).Return(runResult{
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
	first := mr.EXPECT().RunClaude([]string{"-p", "hello"}).Return(runResult{
		rateLimited: true,
		exitCode:    0,
		outputData:  []byte("usage limit hit"),
	}, nil)

	// WaitUntil が呼ばれる（成功で返す）
	mw.EXPECT().WaitUntil(gomock.Any(), gomock.Any()).Return(true)

	// 2回目: --resume で再開、成功
	mr.EXPECT().RunClaude([]string{"--resume"}).Return(runResult{
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
	first := mr.EXPECT().RunClaude([]string{}).Return(runResult{
		rateLimited: true,
		exitCode:    0,
		outputData:  []byte("Your limit will reset at 7pm (UTC)"),
	}, nil)

	// WaitUntil が呼ばれる
	mw.EXPECT().WaitUntil(gomock.Any(), gomock.Any()).Return(true)

	// 2回目: 成功
	mr.EXPECT().RunClaude([]string{"--resume"}).Return(runResult{
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
	mr.EXPECT().RunClaude([]string{}).Return(runResult{
		rateLimited: true,
		exitCode:    0,
		outputData:  []byte("rate limit"),
	}, nil)

	// 2回目・3回目: --resume で rate limited
	mr.EXPECT().RunClaude([]string{"--resume"}).Return(runResult{
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

	mr.EXPECT().RunClaude([]string{}).Return(runResult{}, errors.New("failed to start"))

	ctx := context.Background()
	code := runLoop(ctx, mr, mw, []string{}, 5, 5*time.Minute, true)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

func TestRunLoop_ContextCancelled(t *testing.T) {
	ctrl := gomock.NewController(t)

	mr := NewMockrunner(ctrl)
	mw := NewMockwaiter(ctrl)

	// rate limited
	mr.EXPECT().RunClaude([]string{}).Return(runResult{
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
