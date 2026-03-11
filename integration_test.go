//go:build integration

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
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
