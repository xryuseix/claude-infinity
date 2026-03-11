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
func doRateLimit() {
	fmt.Println("You've hit your limit")
}

// doSuccess は正常終了を模擬する。
func doSuccess() {
	fmt.Println("Hello from fake-claude!")
}
