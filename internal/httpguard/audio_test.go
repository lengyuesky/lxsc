package httpguard

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

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
			if err := VerifyAudio(context.Background(), response); err != nil {
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
		if err := VerifyAudio(context.Background(), response); !errors.Is(err, ErrInvalidAudio) {
			t.Fatalf("响应头或假签名被当成可播放音频：%v", err)
		}
		response.Body.Close()
	}
}

func TestVerifyAudioCannotOpenExternalPlaylistResources(t *testing.T) {
	body := []byte("#EXTM3U\n#EXTINF:30,untrusted\nfile:///etc/passwd\n")
	response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: io.NopCloser(bytes.NewReader(body))}
	defer response.Body.Close()
	if err := VerifyAudio(context.Background(), response); !errors.Is(err, ErrInvalidAudio) {
		t.Fatal("音频检查接受了需要打开额外文件的播放列表", err)
	}
}

func TestVerifyAudioMissingDecoderCannotBecomeSuccess(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/wav"}}, Body: io.NopCloser(bytes.NewReader(testaudio.WAV()))}
	defer response.Body.Close()
	if err := VerifyAudio(context.Background(), response); !errors.Is(err, ErrDecoderUnavailable) {
		t.Fatal("没有解码器仍被当成验证成功", err)
	}
}
