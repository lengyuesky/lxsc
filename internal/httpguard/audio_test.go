package httpguard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"lxsc/internal/testaudio"
)

func TestVerifyAudioRequiresDecodedSamplesAndRestoresBody(t *testing.T) {
	for _, test := range []struct {
		name, declared, actual string
		body                   []byte
	}{
		{"PCM", "audio/wav", "audio/wav", testaudio.WAV()},
		{"QQ误标FLAC", "audio/x-ogg", "audio/flac", testaudio.FLAC},
		{"MP3", "application/octet-stream", "audio/mpeg", testaudio.MP3},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := &http.Response{StatusCode: 206, Header: http.Header{"Content-Type": {test.declared}}, Body: io.NopCloser(bytes.NewReader(test.body))}
			if err := VerifyAudio(context.Background(), response, 0); err != nil {
				t.Fatal("真实合成音频不能验证", err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || !bytes.Equal(body, test.body) || response.Header.Get("Content-Type") != test.actual {
				t.Fatal("验证消耗的字节未恢复，或实际音频类型没有纠正")
			}
		})
	}
}

func TestVerifyAudioRejectsHeaderOnlyEmptyAndFakeAudio(t *testing.T) {
	for _, body := range [][]byte{nil, []byte("{\"error\":\"denied\"}"), []byte("fLaC this is not a FLAC stream"), []byte("ID3 encrypted-or-corrupt-content"), bytes.Repeat([]byte{0x9a, 0x17}, 4096)} {
		response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/x-ogg"}}, Body: io.NopCloser(bytes.NewReader(body))}
		if err := VerifyAudio(context.Background(), response, 0); !errors.Is(err, ErrInvalidAudio) {
			t.Fatalf("响应头或假签名被当成可播放音频：%v", err)
		}
		response.Body.Close()
	}
}

func TestVerifyAudioCannotOpenExternalPlaylistResources(t *testing.T) {
	body := []byte("#EXTM3U\n#EXTINF:30,untrusted\nfile:///etc/passwd\n")
	response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: io.NopCloser(bytes.NewReader(body))}
	defer response.Body.Close()
	if err := VerifyAudio(context.Background(), response, 0); !errors.Is(err, ErrInvalidAudio) {
		t.Fatal("音频检查接受了需要打开额外文件的播放列表", err)
	}
}

func TestVerifyAudioMissingDecoderCannotBecomeSuccess(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/wav"}}, Body: io.NopCloser(bytes.NewReader(testaudio.WAV()))}
	defer response.Body.Close()
	if err := VerifyAudio(context.Background(), response, 0); !errors.Is(err, ErrDecoderUnavailable) {
		t.Fatal("没有解码器仍被当成验证成功", err)
	}
}

// synthesize 用解码器现场合成噪声音频；不含真实歌曲。
func synthesize(t *testing.T, seconds int, arguments ...string) []byte {
	t.Helper()
	// 写入真实文件而不是管道，编码器才能回填文件头里的时长。
	path := filepath.Join(t.TempDir(), "audio")
	command := exec.Command("ffmpeg", append(append([]string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", fmt.Sprintf("anoisesrc=d=%d:c=pink:r=44100", seconds)}, arguments...), path)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatal("无法合成测试音频", err, string(output))
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		t.Fatal("无法合成测试音频", err)
	}
	return data
}

// probeResponse 模拟对整份文件的起始片段探测：正文只有前 256 KiB，总长由 Content-Range 声明。
func probeResponse(file []byte, total int) *http.Response {
	sample := file[:min(len(file), AudioSampleLimit)]
	return &http.Response{StatusCode: 206, Header: http.Header{"Content-Range": {fmt.Sprintf("bytes 0-%d/%d", len(sample)-1, total)}}, Body: io.NopCloser(bytes.NewReader(sample))}
}

func TestVerifyAudioRejectsPreviewClipsAndTruncatedFiles(t *testing.T) {
	// 没有 Xing 头的恒定码率 MP3：时长只能由总字节数和码率折算。
	mp3 := synthesize(t, 60, "-b:a", "128k", "-write_xing", "0", "-f", "mp3")
	// FLAC 文件头自带准确时长。
	flac := synthesize(t, 60, "-f", "flac")
	song := 4 * time.Minute
	for _, test := range []struct {
		name     string
		response *http.Response
		expected time.Duration
		want     error
	}{
		{"完整 MP3", probeResponse(mp3, len(mp3)), time.Minute, nil},
		{"完整 FLAC", probeResponse(flac, len(flac)), time.Minute, nil},
		{"元数据略有出入仍算完整", probeResponse(mp3, len(mp3)), 70 * time.Second, nil},
		{"歌曲时长未知时不判断", probeResponse(mp3, len(mp3)), 0, nil},
		{"MP3 试听片段", probeResponse(mp3, len(mp3)), song, ErrIncompleteAudio},
		{"FLAC 试听片段", probeResponse(flac, len(flac)), song, ErrIncompleteAudio},
		{"几秒钟的提示音", probeResponse(testaudio.MP3, len(testaudio.MP3)), song, ErrIncompleteAudio},
		{"文件头完整但正文被截断", probeResponse(flac, 200<<10), time.Minute, ErrIncompleteAudio},
		{"总长未知时只凭文件头时长", &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(flac[:AudioSampleLimit]))}, time.Minute, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			defer test.response.Body.Close()
			if err := VerifyAudio(context.Background(), test.response, test.expected); !errors.Is(err, test.want) || (test.want == nil && err != nil) {
				t.Fatalf("got %v want %v", err, test.want)
			}
		})
	}
}

func TestVerifyAudioSkipsCompletenessForSeekedRanges(t *testing.T) {
	// 拖动得到的中间片段不代表整份文件，完整性只在从文件头开始的响应上判断。
	response := &http.Response{StatusCode: 206, Header: http.Header{"Content-Range": {"bytes 100-200/300"}}, Body: io.NopCloser(bytes.NewReader(testaudio.MP3))}
	defer response.Body.Close()
	if err := VerifyAudio(context.Background(), response, time.Hour); err != nil {
		t.Fatal(err)
	}
}
