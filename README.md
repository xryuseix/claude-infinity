# claude-infinity

Claude Code の Usage Limit 時に自動待機・再開するラッパーツール。

## 仕組み

PTY 経由で `claude` を起動し、出力を監視。Usage Limit を検出するとリセット時刻まで自動待機し、`claude --resume` でセッションを再開します。リセット時刻を取得できない場合は `--wait` 分だけ待機します。

## インストール

**前提:** Go 1.21+、`claude` コマンドが PATH に通っていること、macOS / Linux

```bash
git clone https://github.com/xryuseix/claude-infinity.git
cd claude-infinity
make          # fmt → lint → build
# または
go install github.com/xryuseix/claude-infinity@latest
```

## 使い方

```bash
claude-infinity                              # 対話モード
claude-infinity -- -p "レビューして"           # claude に引数を渡す
```

## 注意事項

- 待機中に別セッションを開くと `--resume` が意図しないセッションを再開する可能性あり
- Windows 非対応 (PTY 使用のため)

## ライセンス

MIT
