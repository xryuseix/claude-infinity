# claude2

Claude Code の Usage Limit に達した際、自動で待機し、制限解除後にセッションを再開するラッパーツールです。

## 動作の仕組み

1. PTY（擬似端末）経由で `claude` を起動し、ターミナルの色やインタラクティブ機能を維持
2. 出力を循環バッファ（16KB）で常時監視し、Usage Limit 関連のパターンを正規表現で検出
3. `"Your limit will reset at 7pm (Asia/Tokyo)"` のようなリセット時刻を出力からパースし、その時刻まで自動待機
4. リセット時刻を取得できない場合は `--wait` で指定した時間分待機（デフォルト: 5分）
5. `claude --resume` で前回のセッションを自動再開
6. 最大リトライ回数に達するまで繰り返し

## インストール

### 前提条件

- Go 1.21 以上
- Claude Code がインストール済みで `claude` コマンドに PATH が通っていること
- macOS または Linux

### ビルド

```bash
git clone https://github.com/r-ishikawa/claude2.git
cd claude2
make
```

`make` を実行すると、フォーマット → lint → ビルドの順に実行され、`claude2` バイナリが生成されます。

PATH の通った場所にインストールする場合:

```bash
make install
```

### Makefile ターゲット

| ターゲット | 説明 |
|:--|:--|
| `make` / `make all` | fmt → lint → build を順に実行 |
| `make build` | `claude2` バイナリをビルド |
| `make fmt` | `gofmt` でコードをフォーマット |
| `make lint` | `go vet` + `golangci-lint`（インストール済みの場合）を実行 |
| `make vet` | `go vet` を実行 |
| `make clean` | ビルド生成物を削除 |
| `make install` | ビルドして `/usr/local/bin/` にコピー |

## 使い方

```bash
# 通常の対話モードで起動
claude2

# claude に引数を渡す
claude2 -- -p "このコードをレビューして"

# フォールバック待機時間を10分に設定（デフォルト: 5分）
claude2 --wait 10

# 最大リトライ回数を変更（デフォルト: 50回）
claude2 --max-retries 100
```

## オプション

| オプション | デフォルト | 説明 |
|:--|:--|:--|
| `--wait` | `5` | リセット時刻を取得できなかった場合のフォールバック待機時間（分） |
| `--max-retries` | `50` | 最大リトライ回数 |

## リセット時刻の自動検出

Claude Code が Usage Limit に達すると、以下のようなメッセージが表示されます:

```
Claude usage limit reached. Your limit will reset at 7pm (Asia/Tokyo).
```

claude2 はこのメッセージからリセット時刻とタイムゾーンを自動的にパースし、その時刻 + 1分後に `claude --resume` で再開します。リセット時刻を取得できなかった場合は `--wait` の値をフォールバックとして使用します。

## 検出するパターン

以下のパターンを出力から検出した場合、Usage Limit と判定します:

- `usage limit`
- `rate limit`
- `too many requests`
- `quota exceeded`
- `you've hit / reached`
- `limit reached`
- `try again later / in`
- `throttle`（部分一致）
- `resource exhausted`

## 注意事項

- 待機中に別の Claude Code セッションを開くと、`--resume` が意図しないセッションを再開する可能性があります
- 検出パターンは Claude Code の出力メッセージに依存するため、アップデートにより変更される可能性があります
- 待機中に Ctrl+C で中断できます
- Windows は非対応です（PTY を使用するため）

## ライセンス

MIT
