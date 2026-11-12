package httpguard

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// AudioSampleLimit 同时限制忽略 Range 的上游响应和解码器输入。
// 解码器只输出短 PCM 样本，不能自行打开媒体地址或外部文件。
const AudioSampleLimit = 256 << 10

type audioError string

func (e audioError) Error() string          { return string(e) }
func (e audioError) DiagnosticCode() string { return string(e) }

const (
	ErrInvalidAudio audioError = "invalid_audio"
	// ErrIncompleteAudio 表示片段能解码，但整份文件明显不是这首歌的完整音频：
	// 试听片段、付费提示音，或文件头声明的时长远大于实际字节数的截断文件。
	ErrIncompleteAudio    audioError = "incomplete_audio"
	ErrDecoderUnavailable audioError = "decoder_unavailable"
)

const (
	// 实际时长比歌曲时长短出 max(incompleteSlack, 1/incompleteRatio) 以上才判为不完整，
	// 容忍元数据误差和片头片尾差异。
	incompleteSlack = 15 * time.Second
	incompleteRatio = 5
	// 文件头声明的时长与总字节数折算出的平均码率低于该值，说明正文被截断。
	truncatedKbps = 40
)

var audioDecoders = make(chan struct{}, max(1, min(4, runtime.NumCPU())))

// VerifyAudio 读取有界起始片段并实际解码，HTTP 状态、MIME 和文件签名均不能单独通过验证。
// expected 为歌曲元数据中的时长，大于零且响应从文件头开始时，还会拒绝试听片段和截断文件。
// 消费的前缀会放回原响应流，以便管理员明确选择代理时完整交付音频。
func VerifyAudio(ctx context.Context, response *http.Response, expected time.Duration) error {
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
	facts, err := decodeAudio(ctx, sample)
	if err != nil {
		return err
	}
	if MediaStartsAtZero(response) {
		total, _ := MediaLength(response)
		if err := facts.complete(total, expected); err != nil {
			return err
		}
	}
	response.Body = &audioPrefixBody{Reader: io.MultiReader(bytes.NewReader(sample), body), original: body}
	if contentType := facts.contentType(); contentType != "" {
		response.Header.Set("Content-Type", contentType)
	}
	return nil
}

// MediaLength 返回整份音频的字节数：200 取 Content-Length，206 取 Content-Range 的总长。
// 未提供或不合法时保持未知，不能把探测片段的长度当作完整大小。
func MediaLength(response *http.Response) (int64, bool) {
	if response.StatusCode == http.StatusOK {
		length, err := strconv.ParseUint(response.Header.Get("Content-Length"), 10, 63)
		return int64(length), err == nil
	}
	if response.StatusCode != http.StatusPartialContent {
		return 0, false
	}
	value, ok := strings.CutPrefix(response.Header.Get("Content-Range"), "bytes ")
	span, total, hasTotal := strings.Cut(value, "/")
	first, last, hasEnd := strings.Cut(span, "-")
	if !ok || !hasTotal || !hasEnd {
		return 0, false
	}
	start, startErr := strconv.ParseUint(first, 10, 63)
	end, endErr := strconv.ParseUint(last, 10, 63)
	length, lengthErr := strconv.ParseUint(total, 10, 63)
	return int64(length), startErr == nil && endErr == nil && lengthErr == nil && end >= start && length > end
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

func decodeAudio(ctx context.Context, sample []byte) (*audioMetadata, error) {
	select {
	case audioDecoders <- struct{}{}:
		defer func() { <-audioDecoders }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	program, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, ErrDecoderUnavailable
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
		return nil, ctx.Err()
	}
	// 必须解码出真实样本且解码过程没有错误，不能只看 ffprobe 元数据或进程退出。
	if err == nil && output.bytes >= 512 {
		return metadata, nil
	}
	var exitError *exec.ExitError
	if err != nil && !errors.As(err, &exitError) {
		return nil, ErrDecoderUnavailable
	}
	return nil, ErrInvalidAudio
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

// duration 与 kbps 取自解码器对输入的描述：有文件头时长（FLAC、带 Xing 的 MP3、M4A、WAV）时直接可信；
// 没有时长的恒定码率文件由调用方用总字节数和码率折算。取不到的值为零。
func (w *audioMetadata) facts() (duration time.Duration, kbps int64) {
	for _, line := range strings.Split(string(w.data), "\n") {
		line = strings.TrimSpace(line)
		if value, ok := strings.CutPrefix(line, "Duration: "); ok && duration == 0 {
			clock, _, _ := strings.Cut(value, ",")
			var hours, minutes int64
			var seconds float64
			if parts := strings.Split(clock, ":"); len(parts) == 3 {
				hours, _ = strconv.ParseInt(parts[0], 10, 64)
				minutes, _ = strconv.ParseInt(parts[1], 10, 64)
				seconds, _ = strconv.ParseFloat(parts[2], 64)
			}
			duration = time.Duration((float64(hours*3600+minutes*60) + seconds) * float64(time.Second))
			kbps = kbpsIn(value)
		}
		// 流自身的码率来自帧头，比容器估算更可靠。
		if strings.HasPrefix(line, "Stream #0") && strings.Contains(line, "Audio:") {
			if rate := kbpsIn(line); rate > 0 {
				kbps = rate
				break
			}
		}
	}
	return duration, kbps
}

// kbpsIn 取出 "… 320 kb/s" 中的数值，没有时返回零。
func kbpsIn(line string) int64 {
	head, _, ok := strings.Cut(line, " kb/s")
	if !ok {
		return 0
	}
	rate, _ := strconv.ParseInt(head[strings.LastIndexByte(head, ' ')+1:], 10, 64)
	return rate
}

// complete 判断整份文件是否像完整歌曲。total 为文件总字节数，expected 为歌曲时长；任一未知时不作对应判断。
func (w *audioMetadata) complete(total int64, expected time.Duration) error {
	duration, kbps := w.facts()
	if duration > 0 && total > 0 && total*8/1000 < int64(duration/time.Second)*truncatedKbps {
		return ErrIncompleteAudio
	}
	if duration == 0 && total > 0 && kbps > 0 {
		duration = time.Duration(total*8/kbps) * time.Millisecond
	}
	if expected > 0 && duration > 0 && expected-duration > max(incompleteSlack, expected/incompleteRatio) {
		return ErrIncompleteAudio
	}
	return nil
}

// MediaStartsAtZero 防止把拖动得到的中间字节当作文件头；非零范围独立验证起始片段。
func MediaStartsAtZero(response *http.Response) bool {
	if response.StatusCode == http.StatusOK {
		return true
	}
	return strings.HasPrefix(response.Header.Get("Content-Range"), "bytes 0-")
}
