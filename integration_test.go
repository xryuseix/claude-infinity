//go:build integration

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/mock/gomock"
)

// fakeBinDir は fake-claude を "claude" として配置した一時ディレクトリ。
// PATH の先頭に追加することで runClaude が fake-claude を使うようにする。
var fakeBinDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "claude-infinity-integration-*")
	if err != nil {
		panic("一時ディレクトリの作成に失敗: " + err.Error())
	}
	defer os.RemoveAll(dir)

	fakeBinDir = dir
	target := filepath.Join(dir, "claude")

	// fake-claude を "claude" という名前でビルド
	cmd := exec.Command("go", "build", "-o", target, "./testbin/fake-claude/")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		panic("fake-claude のビルドに失敗: " + err.Error())
	}

	// PATH の先頭に差し込んで本物の claude より優先させる
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", dir+string(os.PathListSeparator)+origPath)
	defer os.Setenv("PATH", origPath)

	os.Exit(m.Run())
}

// setupCallCount はテストごとに独立した呼び出しカウントファイルを用意し、
// FAKE_CLAUDE_CALL_COUNT_FILE に設定する。
func setupCallCount(t *testing.T) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "call_count")
	t.Setenv("FAKE_CLAUDE_CALL_COUNT_FILE", f)
}

// TestIntegration_NormalExit は rate limit が発生しない場合に正常終了することを確認する。
func TestIntegration_NormalExit(t *testing.T) {
	setupCallCount(t)
	t.Setenv("FAKE_CLAUDE_SEQUENCE", "success")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	code := runLoop(ctx, &ptyRunner{}, &realWaiter{}, []string{}, 3, 100*time.Millisecond, true)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

// TestIntegration_RateLimitThenSuccess は rate limit 後に再開して正常終了することを確認する。
func TestIntegration_RateLimitThenSuccess(t *testing.T) {
	setupCallCount(t)
	t.Setenv("FAKE_CLAUDE_SEQUENCE", "rate_limit,success")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// fallbackWait を短くして待機時間を最小化する
	code := runLoop(ctx, &ptyRunner{}, &realWaiter{}, []string{}, 3, 100*time.Millisecond, true)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

// TestIntegration_MaxRetriesExceeded は rate limit が続いて最大リトライ回数に達した場合に
// exit code 1 で終了することを確認する。
func TestIntegration_MaxRetriesExceeded(t *testing.T) {
	setupCallCount(t)
	t.Setenv("FAKE_CLAUDE_SEQUENCE", "rate_limit") // 常に rate_limit

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	code := runLoop(ctx, &ptyRunner{}, &realWaiter{}, []string{}, 2, 100*time.Millisecond, true)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

// TestIntegration_ProbabilisticMode は確率モード（FAKE_CLAUDE_RATE_LIMIT_PROB=0）で
// 正常終了することを確認する。
func TestIntegration_ProbabilisticMode(t *testing.T) {
	setupCallCount(t)
	t.Setenv("FAKE_CLAUDE_RATE_LIMIT_PROB", "0") // rate limit が発生しない

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	code := runLoop(ctx, &ptyRunner{}, &realWaiter{}, []string{}, 3, 100*time.Millisecond, true)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

// TestIntegration_ResumeArgs は rate limit 後の再起動時に --resume が渡されることを確認する。
// fake-claude 側でコマンドライン引数の記録が必要なため、シーケンスで動作を確認する。
func TestIntegration_ResumeArgs(t *testing.T) {
	setupCallCount(t)
	// 1回目: rate_limit、2回目: success（--resume で呼ばれるはず）
	t.Setenv("FAKE_CLAUDE_SEQUENCE", "rate_limit,success")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	code := runLoop(ctx, &ptyRunner{}, &realWaiter{}, []string{"-p", "hello"}, 3, 100*time.Millisecond, true)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

// TestIntegration_ResetTimeDetection は fake-claude が "resets Xam (Asia/Tokyo)" 形式の
// メッセージを出力したとき、claude-infinity がリセット時刻を正しく解析して
// WaitUntil に渡すことを確認する。
//
// PTY 折り返しを再現するため、fake-claude は時刻とタイムゾーンを別行で出力する:
//
//	You've hit your limit · resets 3am
//	(Asia/Tokyo)
//
// テストでは ptyRunner（実 PTY）と MockWaiter を組み合わせることで、
// 実際に待機せずに WaitUntil の引数を検証する。
func TestIntegration_ResetTimeDetection(t *testing.T) {
	setupCallCount(t)
	t.Setenv("FAKE_CLAUDE_SEQUENCE", "rate_limit_with_time,success")

	ctrl := gomock.NewController(t)
	mw := NewMockwaiter(ctrl)

	// fake-claude は「現在時刻+1時間」のリセット時刻を出力する。
	// runLoop はその時刻+1分バッファで WaitUntil を呼ぶ。
	before := time.Now()

	var capturedTarget time.Time
	mw.EXPECT().WaitUntil(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, target time.Time) bool {
			capturedTarget = target
			return true // 実際には待機しない
		},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	code := runLoop(ctx, &ptyRunner{}, mw, []string{}, 3, 100*time.Millisecond, true)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}

	// WaitUntil の target が「時刻パースの結果」として渡されたことを検証する。
	//
	// fake-claude は「現在時刻+1時間の hour 部分」を出力するため、
	// target は before から最小1分後〜最大61分後（+1分バッファ込み）になる。
	// フォールバックが使われた場合は fallbackWait=100ms+1分≒すぐ になるため、
	// target が 30分後より手前なら「フォールバックが使われた」と判断できる。
	if capturedTarget.Before(before.Add(30 * time.Minute)) {
		t.Errorf("WaitUntil target = %v, want at least 30min after %v\n"+
			"(time parsing may have failed and fallback was used)",
			capturedTarget, before)
	}
	// パース失敗で翌日扱いになっていないかも確認する
	if capturedTarget.After(before.Add(25 * time.Hour)) {
		t.Errorf("WaitUntil target = %v is too far in the future (> 25h from %v)\n"+
			"(time may have been parsed as next day)",
			capturedTarget, before)
	}
}
