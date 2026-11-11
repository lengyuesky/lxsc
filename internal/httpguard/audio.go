package httpguard

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
)

// AudioSampleLimit 同时限制忽略 Range 的上游响应和解码器输入。
// 解码器只输出短 PCM 样本，不能自行打开媒体地址或外部文件。
const AudioSampleLimit = 256 << 10

type audioError string

func (e audioError) Error() string          { return string(e) }
func (e audioError) DiagnosticCode() string { return string(e) }

const (
	ErrInvalidAudio       audioError = "invalid_audio"
	ErrDecoderUnavailable audioError = "decoder_unavailable"
)

var audioDecoders = make(chan struct{}, max(1, min(4, runtime.NumCPU())))

// VerifyAudio 读取有界起始片段并实际解码，HTTP 状态、MIME 和文件签名均不能单独通过验证。
// 消费的前缀会放回原响应流，以便管理员明确选择代理时完整交付音频。
func VerifyAudio(ctx context.Context, response *http.Response) error {
	if err := CheckMediaResponse(response); err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		return nil
	}
	if response.Body == nil {
		return ErrInvalidAudio
	}
	body := response.Body
	stop := context.AfterFunc(ctx, func() { _ = body.Close() })
	sample, err := io.ReadAll(io.LimitReader(body, AudioSampleLimit))
	if !stop() || ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || len(sample) == 0 {
		return ErrInvalidAudio
	}
	contentType, err := decodeAudio(ctx, sample)
	if err != nil {
		return err
	}
	response.Body = &audioPrefixBody{Reader: io.MultiReader(bytes.NewReader(sample), body), original: body}
	if contentType != "" {
		response.Header.Set("Content-Type", contentType)
	}
	return nil
}

// VerifyMediaReadable 在原文件起始片段已经解码验证后，拒绝空或读取失败的拖动响应。
func VerifyMediaReadable(ctx context.Context, response *http.Response) error {
	body := response.Body
	stop := context.AfterFunc(ctx, func() { _ = body.Close() })
	sample, err := io.ReadAll(io.LimitReader(body, 1))
	if !stop() || ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || len(sample) != 1 {
		return ErrInvalidAudio
	}
	response.Body = &audioPrefixBody{Reader: io.MultiReader(bytes.NewReader(sample), body), original: body}
	return nil
}

func decodeAudio(ctx context.Context, sample []byte) (string, error) {
	select {
	case audioDecoders <- struct{}{}:
		defer func() { <-audioDecoders }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	program, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", ErrDecoderUnavailable
	}
	command := exec.CommandContext(ctx, program,
		"-hide_banner", "-loglevel", "info", "-nostdin",
		"-max_alloc", "16777216", "-threads", "1", "-filter_threads", "1",
		"-protocol_whitelist", "pipe", "-format_whitelist", "mp3,flac,ogg,wav,aac,mov,ape,wv,aiff,au",
		"-probesize", "262144", "-analyzeduration", "1000000",
		"-err_detect", "explode", "-xerror",
		"-i", "pipe:0", "-map", "0:a:0", "-t", "0.05",
		"-ac", "1", "-ar", "8000", "-f", "s16le", "pipe:1")
	command.Stdin = bytes.NewReader(sample)
	// 不向解码进程继承服务端凭据，且固定日志语言/颜色以便识别实际容器。
	command.Env = []string{"LC_ALL=C", "AV_LOG_FORCE_NOCOLOR=1"}
	output := &audioPCM{}
	metadata := &audioMetadata{}
	command.Stdout, command.Stderr = output, metadata
	err = command.Run()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	// 必须解码出真实样本且解码过程没有错误，不能只看 ffprobe 元数据或进程退出。
	if err == nil && output.bytes >= 512 {
		return metadata.contentType(), nil
	}
	var exitError *exec.ExitError
	if err != nil && !errors.As(err, &exitError) {
		return "", ErrDecoderUnavailable
	}
	return "", ErrInvalidAudio
}

type audioPCM struct{ bytes int }

func (w *audioPCM) Write(data []byte) (int, error) {
	w.bytes += min(len(data), 4096-w.bytes)
	return len(data), nil
}

type audioPrefixBody struct {
	io.Reader
	original io.ReadCloser
}

func (b *audioPrefixBody) Close() error { return b.original.Close() }

// 解码器诊断只在有界内存中解析，不写入日志或返回自由文本。
// 媒体类型取自实际解码器识别的容器，而非上游 MIME 或可伪造的文件签名。
type audioMetadata struct{ data []byte }

func (w *audioMetadata) Write(data []byte) (int, error) {
	w.data = append(w.data, data[:min(len(data), 8192-len(w.data))]...)
	return len(data), nil
}

func (w *audioMetadata) contentType() string {
	for _, line := range strings.Split(string(w.data), "\n") {
		format, ok := strings.CutPrefix(line, "Input #0, ")
		if !ok {
			continue
		}
		format, _, _ = strings.Cut(format, ",")
		return map[string]string{"mp3": "audio/mpeg", "flac": "audio/flac", "ogg": "audio/ogg", "wav": "audio/wav", "aac": "audio/aac", "mov": "audio/mp4", "ape": "audio/ape", "wv": "audio/wavpack", "aiff": "audio/aiff", "au": "audio/basic"}[format]
	}
	return ""
}

// MediaStartsAtZero 防止把拖动得到的中间字节当作文件头；非零范围独立验证起始片段。
func MediaStartsAtZero(response *http.Response) bool {
	if response.StatusCode == http.StatusOK {
		return true
	}
	return strings.HasPrefix(response.Header.Get("Content-Range"), "bytes 0-")
}
