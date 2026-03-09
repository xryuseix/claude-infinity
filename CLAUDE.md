# claude-infinity

Usage Limit 時に自動で待機・再開する Claude Code ラッパーツール。

## プロジェクト概要

- 言語: Go 1.25+
- 単一バイナリ (`main.go` のみ)
- 外部依存: `github.com/creack/pty`, `golang.org/x/term`
- 対象OS: macOS / Linux (PTY を使用するため Windows 非対応)

## アーキテクチャ

```
main.go
├── ringBuffer     - 循環バッファ (出力末尾 16KB を保持して rate limit パターンを検出)
├── runClaude      - PTY 経由で claude CLI を起動し、出力を監視
├── isRateLimited  - 正規表現パターンで Usage Limit を判定
├── waitWithCountdown - カウントダウン表示付き待機
└── main           - リトライループ (claude → 検出 → 待機 → --resume)
```

## ビルド・実行

```bash
make            # fmt → lint → build
make build      # ビルドのみ
make fmt        # gofmt
make lint       # go vet + golangci-lint
make clean      # 生成物削除
make install    # go install で $GOBIN にインストール
./claude-infinity                  # 対話モード
./claude-infinity --wait 10        # 待機時間を10分に
./claude-infinity -- -p "..."      # claude に引数を渡す
```

## コーディング規約

- コメントは日本語で記述する
- エラーメッセージ・ユーザー向け出力は日本語
- ステータス出力は stderr に出力 (`[claude-infinity]` プレフィックス付き)
- claude の出力は stdout にそのまま透過する
- ターミナル状態は必ず復元する (defer + シグナルハンドラ)

## 検出パターン (`limitPatterns`)

パターン追加時は `main.go` の `limitPatterns` スライスに正規表現を追加する。誤検出を避けるため、汎用的すぎるパターンは避けること。

## テスト時の注意

- PTY を使用するため、CI 環境では `/dev/ptmx` が利用可能である必要がある
- `claude` コマンドが PATH に存在する必要がある
- 実際の Usage Limit テストは手動確認が必要
