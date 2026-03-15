// fake-claude は統合テスト用の Claude Code スタブ。
// PTY 経由で起動され、シーケンスまたは確率に基づいて
// 正常終了または Usage Limit をシミュレートする。
//
// 環境変数:
//
//	FAKE_CLAUDE_SEQUENCE        カンマ区切りのシナリオ列（例: "rate_limit,success"）。
//	                            各要素は呼び出し順に適用され、末尾を超えた場合は最後の要素を繰り返す。
//	FAKE_CLAUDE_CALL_COUNT_FILE 呼び出し回数を記録するファイルのパス。
//	                            指定しない場合は常に0回目として扱う。
//	FAKE_CLAUDE_RATE_LIMIT_PROB FAKE_CLAUDE_SEQUENCE 未指定時の rate limit 発生確率（0.0〜1.0）。
//	                            デフォルト: 0.3
package main

import (
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	callNum := readAndIncrementCallCount()

	// シーケンスモード
	if seq := os.Getenv("FAKE_CLAUDE_SEQUENCE"); seq != "" {
		parts := strings.Split(seq, ",")
		idx := callNum
		if idx >= len(parts) {
			idx = len(parts) - 1
		}
		switch strings.TrimSpace(parts[idx]) {
		case "rate_limit":
			doRateLimit()
		case "rate_limit_with_time":
			doRateLimitWithTime()
		case "file_content_with_keywords":
			doFileContentWithKeywords()
		default:
			doSuccess()
		}
		return
	}

	// 確率モード
	prob := 0.3
	if p := os.Getenv("FAKE_CLAUDE_RATE_LIMIT_PROB"); p != "" {
		if v, err := strconv.ParseFloat(p, 64); err == nil {
			prob = v
		}
	}

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	if r.Float64() < prob {
		doRateLimit()
	} else {
		doSuccess()
	}
}

// readAndIncrementCallCount はカウントファイルから現在の呼び出し回数を読み、
// インクリメントして書き戻す。ファイルが指定されていない場合は 0 を返す。
func readAndIncrementCallCount() int {
	path := os.Getenv("FAKE_CLAUDE_CALL_COUNT_FILE")
	if path == "" {
		return 0
	}
	n := 0
	if data, err := os.ReadFile(path); err == nil {
		n, _ = strconv.Atoi(strings.TrimSpace(string(data)))
	}
	_ = os.WriteFile(path, []byte(strconv.Itoa(n+1)), 0o644)
	return n
}

// doRateLimit は Usage Limit メッセージを出力して終了する。
// リセット時刻は含まない（統合テストでの長時間待機を避けるため）。
// claude-infinity の fallbackWait でリトライが制御される。
// セッション ID を出力することで、再開時の --resume <UUID> をテストできる。
func doRateLimit() {
	fmt.Println("You've hit your limit")
	fmt.Printf("To resume this conversation, run: claude --resume %s\n", generateFakeUUID())
}

// doRateLimitWithTime は実際の Claude Code に近い形式のリセット時刻付き
// Usage Limit メッセージを出力する。
// PTY 上の折り返しを再現するため、時刻とタイムゾーンを別行に分けて出力する。
// 出力例:
//
//	You've hit your limit · resets 3am
//	(Asia/Tokyo)
func doRateLimitWithTime() {
	// 現在時刻+1時間を JST でのリセット時刻として出力する
	jst := time.FixedZone("Asia/Tokyo", 9*60*60)
	resetAt := time.Now().In(jst).Add(time.Hour)
	fmt.Printf("You've hit your limit · resets %s\n(%s)\n",
		formatHour(resetAt), resetAt.Location().String())
	fmt.Printf("To resume this conversation, run: claude --resume %s\n", generateFakeUUID())
}

// formatHour は time.Time を "3am" / "3pm" 形式の文字列に変換する。
func formatHour(t time.Time) string {
	h := t.Hour()
	switch {
	case h == 0:
		return "12am"
	case h < 12:
		return fmt.Sprintf("%dam", h)
	case h == 12:
		return "12pm"
	default:
		return fmt.Sprintf("%dpm", h-12)
	}
}

// generateFakeUUID はテスト用の UUID v4 風の文字列を生成する。
func generateFakeUUID() string {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		r.Int31(), r.Int31n(0xffff), r.Int31n(0xffff), r.Int31n(0xffff),
		r.Int63n(0xffffffffffff))
}

// doFileContentWithKeywords は rate limit キーワードを含むファイル内容を表示した後、
// 1KB 以上の通常出力で終了する。誤検出テスト用。
func doFileContentWithKeywords() {
	// CLAUDE.md のような内容を出力
	fmt.Println("# claude-infinity")
	fmt.Println("")
	fmt.Println("Usage Limit 時に自動で待機・再開する Claude Code ラッパーツール。")
	fmt.Println("rate limit を検出して自動リトライ。")
	fmt.Println("you've hit your limit の場合にも対応。")
	fmt.Println("")

	// 1KB 以上の通常出力を追加して、キーワードを Tail ウィンドウから押し出す
	for i := 0; i < 20; i++ {
		fmt.Printf("Normal output line %d: This is regular Claude output without any special keywords.\n", i)
	}
	fmt.Println("Done! Task completed successfully.")
}

// doSuccess は正常終了を模擬する。
func doSuccess() {
	fmt.Println("Hello from fake-claude!")
}
