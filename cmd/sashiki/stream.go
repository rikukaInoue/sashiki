// ダンプ / baseline ストリームの入出力先を「ローカルファイル」「標準入出力」
// 「S3」で共通に扱うヘルパ(#242 / #243)。
//
// 14GB のダンプが S3 にある、といった運用でホストに落とさず投入できるように
// するのが目的。S3 は aws CLI に委譲する — 認証(インスタンスプロファイル /
// SSO / MFA)や再開・マルチパートの面倒を自前で持たないため。CLI が無い環境では
// その場で分かるエラーにする。
package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// isS3 は s3:// URL かどうか。
func isS3(path string) bool { return strings.HasPrefix(path, "s3://") }

// isStdio は "-"(標準入出力)かどうか。
func isStdio(path string) bool { return path == "-" }

// requireAWSCLI は aws CLI の存在を確認する。
func requireAWSCLI() error {
	if _, err := exec.LookPath("aws"); err != nil {
		return fmt.Errorf("s3:// を使うには aws CLI が必要です(未インストール): %w", err)
	}
	return nil
}

// openSource は読み出し元を開く。ローカルパス / "-"(stdin) / s3:// に対応。
// 戻り値の closer は必ず閉じること(S3 の場合は子プロセスの後始末も行う)。
func openSource(path string) (io.ReadCloser, error) {
	switch {
	case isStdio(path):
		return io.NopCloser(os.Stdin), nil
	case isS3(path):
		if err := requireAWSCLI(); err != nil {
			return nil, err
		}
		// aws s3 cp <url> - で標準出力へ流す。
		cmd := exec.Command("aws", "s3", "cp", path, "-")
		cmd.Stderr = os.Stderr
		out, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("aws s3 cp %s: %w", path, err)
		}
		return &procReader{r: out, cmd: cmd, what: "aws s3 cp " + path}, nil
	default:
		return os.Open(path)
	}
}

// createSink は書き込み先を開く。ローカルパス / "-"(stdout) / s3:// に対応。
func createSink(path string) (io.WriteCloser, error) {
	switch {
	case isStdio(path):
		return nopWriteCloser{os.Stdout}, nil
	case isS3(path):
		if err := requireAWSCLI(); err != nil {
			return nil, err
		}
		// aws s3 cp - <url> で標準入力から吸わせる。
		cmd := exec.Command("aws", "s3", "cp", "-", path)
		cmd.Stderr = os.Stderr
		in, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("aws s3 cp - %s: %w", path, err)
		}
		return &procWriter{w: in, cmd: cmd, what: "aws s3 cp - " + path}, nil
	default:
		return os.Create(path)
	}
}

// procReader は子プロセスの標準出力を読み、Close で終了を待つ。
// 待たないと「読み終えたが子プロセスは失敗していた」を取りこぼす。
type procReader struct {
	r    io.ReadCloser
	cmd  *exec.Cmd
	what string
}

func (p *procReader) Read(b []byte) (int, error) { return p.r.Read(b) }

func (p *procReader) Close() error {
	_ = p.r.Close()
	if err := p.cmd.Wait(); err != nil {
		return fmt.Errorf("%s: %w", p.what, err)
	}
	return nil
}

// procWriter は子プロセスの標準入力へ書き、Close で入力を閉じてから終了を待つ。
type procWriter struct {
	w    io.WriteCloser
	cmd  *exec.Cmd
	what string
}

func (p *procWriter) Write(b []byte) (int, error) { return p.w.Write(b) }

func (p *procWriter) Close() error {
	// 先に stdin を閉じないと子プロセスが EOF を受け取れず Wait が返らない。
	if err := p.w.Close(); err != nil {
		_ = p.cmd.Wait()
		return err
	}
	if err := p.cmd.Wait(); err != nil {
		return fmt.Errorf("%s: %w", p.what, err)
	}
	return nil
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// materialize は「ローカルのパスが必要な処理」のために、必要なら一時ファイルへ
// 落としてそのパスを返す。ローカルパスならそのまま返す(コピーしない)。
// cleanup は必ず呼ぶこと(一時ファイルを消す)。
//
// mysql の投入経路は標準入力から流せるが、pg_restore のカスタム/ディレクトリ形式は
// シーク可能なファイルを要求するため、この経路が要る。
func materialize(path, pattern string) (local string, cleanup func(), err error) {
	if !isS3(path) && !isStdio(path) {
		return path, func() {}, nil
	}
	src, err := openSource(path)
	if err != nil {
		return "", func() {}, err
	}
	defer func() { _ = src.Close() }()

	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", func() {}, err
	}
	tmp := f.Name()
	cleanup = func() { _ = os.Remove(tmp) }
	if _, err := io.Copy(f, src); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("%s の取得に失敗: %w", path, err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return tmp, cleanup, nil
}
