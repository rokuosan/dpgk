# dpgk

**ドーパミン中毒のガキ** 専用、問答無用でターミナルを虹色にするやつ。

```
$ dpgk ls -la
$ dpgk vim main.go
$ dpgk --speed=0 htop
```

名前の由来は `dpkg` のパロディです。dpgkはドーパミン中毒のガキの略です。だから虹色になります。

## 使い方

```bash
dpgk [options] <command> [args...]
```

### オプション

| フラグ | デフォルト | 説明 |
|--------|-----------|------|
| `--speed` | `20` | 虹のアニメーション速度（Hz）。0で静止します。 |
| `--freq` | `1.0` | 虹の周波数／広がり（0.1-10.0） |
| `--redraw` | `0` | 静止画面の再描画アニメーション（Hz）。1-5推奨。TUIはちらつくかもしれません |
| `--version` | | バージョン表示 |

### 使用例

```bash
# 基本的な使い方
dpgk ls -la

# スクロール出力も虹色
dpgk dmesg -w

# vim/htop も虹色
dpgk vim main.go

# アニメーションなし
dpgk --speed=0 htop

# 静止画面も呼吸する虹色（実験的）
dpgk --redraw=2 htop

# パイプのときは自動で素通り（虹色オフ）
echo hello | dpgk cat
```

## インストール

### バイナリをダウンロード

[Releases](https://github.com/shibadogcap/dpgk/releases) から各プラットフォームのバイナリをダウンロードし、パスの遠た場所に配置してください。

### 自分でビルド

```bash
git clone https://github.com/shibadogcap/dpgk.git
cd dpgk
go build -o dpgk .
```

クロスコンパイル:

```bash
GOOS=linux   GOARCH=amd64 go build -ldflags="-s -w" -o dpgk-linux .
GOOS=darwin  GOARCH=amd64 go build -ldflags="-s -w" -o dpgk-macos .
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o dpgk.exe .
```

## 仕組み

```
dpgk <command>
  │
  ├── ptyx.NewConsole()        ← ローカル端末を掌握
  ├── console.MakeRaw()        ← Rawモードでキーを素通り
  ├── ptyx.Spawn(cmd, PTY)    ← 子プロセスを擬似端末で起動
  │
  ├── stdin  ─────────────────→ PTY（キー入力そのまま）
  ├── PTY ───→ RainbowTransformer ───→ stdout（虹色）
  ├── resize ─────────────────→ PTY（端末リサイズ転送）
  └── signal ─────────────────→ PTY（Ctrl+C 等を転送）
```

**RainbowTransformer** がANSIエスケープシーケンスをパース:
- SGRの色指定（fg/bg/truecolor）だけを剥がして虹色に置換
- カーソル移動、画面消去、alternate screen、スタイル属性（bold/italic等）は素通し
- カーソル位置を追跡して斜めグラデーションを計算
- 画面バッファを保持し、任意で定周期再描画（`--redraw`）

## 必須環境

- Go 1.24+（ビルド用）
- Linux / macOS / Windows（Windows は ConPTY）

## 謝辞

- [`ptyx`](https://github.com/KennethanCeyer/ptyx) — クロスプラットフォームPTYライブラリ
- [`lolcat`](https://github.com/busyloop/lolcat) — 虹色出力のインスピレーション元
- `dpkg` — 名前をインスパイアしました
